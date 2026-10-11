package sandbox

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// WaitHealth 轮询实例 /health 直到就绪或超时。
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
