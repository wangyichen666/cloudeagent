// Package mock 是零依赖的本地回退引擎实现。
// 当 qwenpaw 等真实引擎不可用时，自动回退到 mock，保证平台可用。
package mock

import (
	"context"
	"fmt"
	"strings"
	"time"

	"cloude-agent/internal/engine"
)

func init() {
	engine.Register("mock", New)
}

// Engine 是 mock 引擎。
type Engine struct {
	cfg       *engine.Config
	workspace string
}

// New 创建 mock 引擎。
func New(name, workspace string, cfg *engine.Config) (engine.Engine, error) {
	if cfg == nil {
		cfg = engine.DefaultConfig()
	}
	return &Engine{cfg: cfg, workspace: workspace}, nil
}

func (e *Engine) Name() string { return "mock" }

func (e *Engine) Ready() (bool, string) { return true, "" }

func (e *Engine) Chat(ctx context.Context, sessionKey, message string, cb *engine.Callbacks) (string, error) {
	lines := []string{
		fmt.Sprintf("【mock 回复 · 模型 %s · provider %s】", e.cfg.Model, e.cfg.Provider),
		fmt.Sprintf("你刚才说：%s", message),
		"（这是本地 mock 引擎的输出。配置真实引擎后即可切换。）",
	}
	reply := strings.Join(lines, "\n")
	if cb != nil && cb.OnDelta != nil {
		for _, chunk := range chunkText(reply, 24) {
			cb.OnDelta(chunk)
		}
	}
	return reply, nil
}

func (e *Engine) NewSession(ctx context.Context) (string, error) {
	return fmt.Sprintf("mock-s-%d", time.Now().UnixNano()), nil
}

func (e *Engine) CloseSession(sessionKey string) {}

func (e *Engine) SyncConfig(ctx context.Context) {}

func (e *Engine) Close() {}

func (e *Engine) Info() (name, version string) { return "mock-engine", "0.1.0" }

func (e *Engine) Connected() bool { return true }

func chunkText(s string, n int) []string {
	runes := []rune(s)
	var out []string
	for len(runes) > 0 {
		if len(runes) < n {
			n = len(runes)
		}
		out = append(out, string(runes[:n]))
		runes = runes[n:]
	}
	return out
}
