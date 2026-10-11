// Package domain 定义纯领域模型：无任何外部依赖，是整个系统的最底层。
// 所有上层（store/gateway/controlplane/agentruntime/api）都只能依赖 domain，
// domain 不依赖任何上层。
package domain

import "time"

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
// 对应 agent_instances 表（内存存储时驻留内存，PostgreSQL 存储时落库）。
type Instance struct {
	UserID        string         `json:"user_id"`
	InstanceName  string         `json:"instance_name"`             // agent-<userID>
	Status        InstanceStatus `json:"status"`                    // running/suspended/failed/none
	Workspace     string         `json:"workspace"`                 // 本地路径或 Docker volume / PVC 名
	Endpoint      string         `json:"endpoint"`                  // http://127.0.0.1:<port> 或 Pod DNS / Pod IP
	Port          int            `json:"port"`                      // 本地模式的稳定路由端口
	SandboxID     string         `json:"sandbox_id,omitempty"`      // E2B sandbox ID
	Error         string         `json:"error,omitempty"`           // failed 时的原因
	LastActiveAt  time.Time      `json:"last_active_at"`            // 供 idle reaper 判定休眠
	WSConnections int            `json:"ws_connections"`            // 活跃 WS 连接数
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
}
