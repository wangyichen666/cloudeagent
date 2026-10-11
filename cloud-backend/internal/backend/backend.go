package backend

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Info 描述一个正在运行的实例：工作区定位符 + 稳定路由地址。
// 本地模式 Endpoint 形如 http://127.0.0.1:<port>，
// K8s 生产路径形如 http://agent-<userID>-0.agent-svc.<ns>.svc.cluster.local:18585。
type Info struct {
	UserID    string
	Workspace string
	Endpoint  string
	Port      int
	// SandboxID 仅 E2B/KruiseAgents 后端使用：E2B 返回的沙箱长 ID，
	// control-plane 重启后据此 reconnect / pause / kill。
	SandboxID string
}

// InstanceBackend 抽象「数据面」：每用户一个 Agent 运行时的创建/休眠/唤醒/销毁。
//   - process 后端：本地进程 + 工作区目录（对应 Pod 进程 + PVC）
//   - docker  后端：每用户容器 + 命名卷（本地最接近 StatefulSet+PVC 的语义）
//   - k8s     后端：StatefulSet scale 0/1 + PVC（生产路径，见 k8sbackend/）
type InstanceBackend interface {
	Name() string
	Create(ctx context.Context, userID string) (*Info, error)
	Start(ctx context.Context, userID string) (*Info, error) // 唤醒
	Stop(ctx context.Context, userID string) error           // 休眠（保留工作区）
	Delete(ctx context.Context, userID string) error         // 销毁（含工作区，按配置）
}

var userIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// ValidateUserID 防止用户输入被拼进路径/容器名/DNS。
func ValidateUserID(userID string) error {
	if !userIDPattern.MatchString(userID) || len(userID) > 63 {
		return fmt.Errorf("invalid user_id %q (仅允许字母数字、点、下划线、中划线，长度<=63)", userID)
	}
	return nil
}

// WaitHealth 轮询实例 /health 直到就绪或超时（对应「等 Pod Ready」）。
// E2B gateway 地址（含 /sandboxes/）由 envd 异步注册，本地 kind 环境注册可能延迟，
// 此时 waitReady 已通过 GetInfo 确认 sandbox 运行态，直接放行不阻塞 create。
func WaitHealth(ctx context.Context, endpoint string, timeout time.Duration) error {
	if strings.Contains(endpoint, "/sandboxes/") {
		return nil
	}
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/health", nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	return fmt.Errorf("instance not ready within %s", timeout)
}
