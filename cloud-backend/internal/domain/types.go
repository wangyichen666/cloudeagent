// Package domain 定义纯领域模型：无任何外部依赖，是整个系统的最底层。
// 所有上层（store/controlplane/agentruntime/api）都只能依赖 domain，
// domain 不依赖任何上层。
package domain

import "time"

// --- 实例领域 ---

// InstanceStatus 对应实例生命周期状态机：
//
//	[None] -> [Running] <-> [Suspended] -> [None]
type InstanceStatus string

const (
	StatusNone      InstanceStatus = "none"
	StatusRunning   InstanceStatus = "running"
	StatusSuspended InstanceStatus = "suspended"
	StatusFailed    InstanceStatus = "failed"
)

// Instance 是「用户 ↔ 实例 ↔ 工作区 ↔ 状态」编排的唯一事实来源。
type Instance struct {
	UserID        string         `json:"user_id"`
	InstanceName  string         `json:"instance_name"`
	Status        InstanceStatus `json:"status"`
	Workspace     string         `json:"workspace"`
	Endpoint      string         `json:"endpoint"`
	Port          int            `json:"port"`
	SandboxID     string         `json:"sandbox_id,omitempty"`
	Error         string         `json:"error,omitempty"`
	LastActiveAt  time.Time      `json:"last_active_at"`
	WSConnections int            `json:"ws_connections"`
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
}

// --- 会话领域 ---

// ChatRequest 是会话输入。
type ChatRequest struct {
	Message   string `json:"message"`
	SessionID string `json:"session_id,omitempty"`
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

// --- 模型配置领域 ---

// ModelConfig 是运行时动态注入的模型配置。
// base_url/api_key 只在内存/临时配置，休眠后丢弃，唤醒时由控制面重新注入；
// 绝不落入 PostgreSQL/PVC/代码仓库。
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

// --- 代码审查领域 ---

// Review 对应 code_reviews 表，由控制面的 CodeReview Worker 异步执行。
type Review struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	Repo      string    `json:"repo"`
	PRNumber  int       `json:"pr_number"`
	Status    string    `json:"status"`
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
	Level   string `json:"level"`
	Message string `json:"message"`
}

// --- 用户领域 ---

// VCSToken 对应 user_vcs_tokens 表（生产环境加密存储）。
type VCSToken struct {
	UserID   string `json:"user_id"`
	Platform string `json:"platform"`
	Token    string `json:"token"`
}
