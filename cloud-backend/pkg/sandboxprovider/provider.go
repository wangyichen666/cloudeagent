// Package sandbox 定义「沙箱提供方」抽象：每用户一个隔离运行环境的生命周期管理。
//
// 控制面（controlplane）只依赖本包的 Provider 接口，不感知具体实现。
// 目前提供：
//   - e2b：OpenKruise Agents / E2B SDK（生产推荐，不直接操作 K8s CRD）
//   - process：本地进程（开发调试）
//   - docker：本地容器（最接近 StatefulSet+PVC 语义）
//
// 后续接入 fly.io、裸 K8s、Firecracker 等，只需新增子包实现 Provider 接口。
package sandboxprovider

import (
	"context"
	"fmt"
	"regexp"
)

// Info 描述一个正在运行的沙箱实例。
type Info struct {
	UserID    string // 所属用户
	Workspace string // 工作区定位符（本地路径 / 卷名 / 远端标识）
	Endpoint  string // 数据面访问地址（http://host:port 或 gateway 转发地址）
	Port      int    // 沙箱内 agent-runtime 监听端口
	SandboxID string // 提供方返回的稳定标识（E2B sandboxID / 容器名 / 进程 pid 文件），空表示不支持
}

// Provider 是沙箱生命周期的统一抽象。
// 所有实现必须幂等：重复 Create/Start/Stop/Delete 不应报错。
type Provider interface {
	// Name 返回提供方名称（用于日志/可观测）。
	Name() string
	// Create 创建并启动一个沙箱，返回可访问的 Info。
	// 若已存在运行中的实例，应直接返回其 Info（幂等）。
	Create(ctx context.Context, userID string) (*Info, error)
	// Start 唤醒已休眠的沙箱；若正在运行则直接返回。
	Start(ctx context.Context, userID string) (*Info, error)
	// Stop 休眠沙箱（释放 CPU/内存，保留工作区）。
	Stop(ctx context.Context, userID string) error
	// Delete 销毁沙箱及关联资源（工作区按实现配置决定是否保留）。
	Delete(ctx context.Context, userID string) error
}

// Seeder 是可选接口：控制面重启后从 Store 恢复 userID→沙箱标识映射。
// 实现了该接口的 Provider 会在启动时被调用 Seed。
type Seeder interface {
	Seed(mapping map[string]string)
}

var userIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// ValidateUserID 防止用户输入被拼进路径/容器名/DNS/沙箱名。
func ValidateUserID(userID string) error {
	if !userIDPattern.MatchString(userID) || len(userID) > 63 {
		return fmt.Errorf("invalid user_id %q (仅允许字母数字、点、下划线、中划线，长度<=63)", userID)
	}
	return nil
}
