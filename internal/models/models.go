package models

import "time"

// InstanceStatus 对应文档中的生命周期状态机：
//   [None] -> [Running] <-> [Suspended] -> [None]
type InstanceStatus string

const (
	StatusNone      InstanceStatus = "none"
	StatusRunning   InstanceStatus = "running"
	StatusSuspended InstanceStatus = "suspended"
	StatusFailed    InstanceStatus = "failed"
)

// Instance 是「用户 ↔ 实例 ↔ 工作区 ↔ 状态」编排的唯一事实来源。
// 对应 agent_instances 表（内存存储时驻留内存，PostgreSQL 存储时落库）。
type Instance struct {
	UserID        string         `json:"user_id"`
	InstanceName  string         `json:"instance_name"`            // agent-<userID>，对应 StatefulSet 名
	Status        InstanceStatus `json:"status"`                   // running/suspended/failed/none
	Workspace     string         `json:"workspace"`                // 本地路径或 Docker volume / PVC 名
	Endpoint      string         `json:"endpoint"`                 // http://127.0.0.1:<port> 或 Pod DNS
	Port          int            `json:"port"`                     // 本地模式的稳定路由端口（≈ Pod DNS）
	Error         string         `json:"error,omitempty"`          // failed 时的原因
	LastActiveAt  time.Time      `json:"last_active_at"`           // 供 idle reaper 判定休眠
	WSConnections int            `json:"ws_connections"`           // 活跃 WS 连接数（对应 agent_activity.ws_connections）
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
}

// ModelConfig 是运行时动态注入的模型配置。
// 关键设计决策（文档 6.2）：base_url/api_key 只在内存/临时配置，
// 休眠后丢弃，唤醒时由控制面重新注入；绝不落入 PostgreSQL/PVC/代码仓库。
type ModelConfig struct {
	BaseURL  string   `json:"base_url"`
	APIKey   string   `json:"api_key,omitempty"`
	Model    string   `json:"model"`
	Models   []string `json:"models,omitempty"`
	Provider string   `json:"provider,omitempty"`
}

// Masked 返回不含完整 api_key 的副本，用于可观测/审计输出。
func (c *ModelConfig) Masked() *ModelConfig {
	cp := *c
	if cp.APIKey != "" {
		if len(cp.APIKey) >= 4 {
			cp.APIKey = "sk-****" + cp.APIKey[len(cp.APIKey)-4:]
		} else {
			cp.APIKey = "****"
		}
	}
	return &cp
}

// Review 对应 code_reviews 表，由控制面的 CodeReview Worker 异步执行。
type Review struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	Repo      string    `json:"repo"`
	PRNumber  int       `json:"pr_number"`
	Status    string    `json:"status"` // pending/running/completed/failed
	Model     string    `json:"model"`
	Findings  []Finding `json:"findings,omitempty"`
	Error     string    `json:"error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Finding 是本地 CodeReview 的实现产物（离线扫描 TODO/FIXME 等）。
type Finding struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Level   string `json:"level"` // info/warning/error
	Message string `json:"message"`
}

// VCSToken 对应 user_vcs_tokens 表（生产环境加密存储，见文档）。
type VCSToken struct {
	UserID   string `json:"user_id"`
	Platform string `json:"platform"` // github/gitlab
	Token    string `json:"token"`
}

// ChatResponse 是控制面与实例之间的会话应答（REST 与 WS done 消息共用）。
type ChatResponse struct {
	Reply        string `json:"reply"`
	Model        string `json:"model"`
	Provider     string `json:"provider"`
	Mock         bool   `json:"mock"`
	MessageIndex int    `json:"message_index"`
	SessionID    string `json:"session_id"`
}
