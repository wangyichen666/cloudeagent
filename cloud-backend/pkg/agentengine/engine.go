// Package engine 定义「Agent 引擎」抽象：屏蔽具体的编码 Agent 内核实现。
//
// 控制面/运行时只依赖 Engine 接口，不感知 qwenpaw / codex / pi0-agent 等具体内核。
// 目前提供：
//   - qwenpaw：QwenPaw ACP（stdio JSON-RPC，生产推荐）
//   - mock：零依赖本地回退（开发调试）
//
// 后续接入 codex、pi0-agent 等，只需新增子包实现 Engine 接口。
package agentengine

import (
	"context"
	"fmt"
)

// Config 是引擎运行时模型配置（由控制面注入，不落盘）。
type Config struct {
	BaseURL       string
	APIKey        string
	Model         string
	Models        []string
	Provider      string
	ToolGuardMode string // "" / "default" / "bypass"
}

// Callbacks 是一次 Chat 调用的流式回调。
// 所有回调均为可选（nil 表示忽略该类事件）。
type Callbacks struct {
	// OnDelta 收到正文增量（agent_message_chunk）。
	OnDelta func(text string)
	// OnThought 收到思考过程增量（agent_thought_chunk）。
	OnThought func(text string)
	// OnTool 收到工具调用事件（tool_call / tool_call_update）。
	OnTool func(update map[string]any)
	// OnUsage 收到用量统计事件。
	OnUsage func(update map[string]any)
}

// Engine 是 Agent 内核的统一抽象。
// 一个 Engine 实例绑定一个工作区，生命周期与沙箱一致。
type Engine interface {
	// Name 返回引擎名称（用于日志/可观测/健康检查）。
	Name() string

	// Ready 报告引擎是否可用：返回 (true, "") 表示可接受请求；
	// 返回 (false, reason) 表示不可用及原因（会回退到 mock）。
	Ready() (bool, string)

	// Chat 同步执行一次会话请求，流式事件通过 callbacks 推送。
	// 返回最终聚合的回复文本。
	Chat(ctx context.Context, sessionKey, message string, callbacks *Callbacks) (string, error)

	// NewSession 开启一个新会话（释放旧上下文）。返回新会话 key。
	NewSession(ctx context.Context) (string, error)

	// CloseSession 关闭指定会话（best-effort）。
	CloseSession(sessionKey string)

	// SyncConfig 在模型配置变化后调用：热切换凭证/模型。
	// 实现可选择重启子进程或动态重载。
	SyncConfig(ctx context.Context)

	// Close 停止引擎（沙箱休眠/删除时调用），释放子进程/连接。
	Close()

	// Info 返回 initialize 阶段声明的内核信息（name, version）。
	Info() (name, version string)

	// Connected 报告引擎是否已完成握手并可接受请求。
	Connected() bool
}

// Factory 根据名称创建引擎实例。
// workspace 必须是绝对路径；cfg 提供初始模型配置（可后续 SyncConfig 热切换）。
type Factory func(name, workspace string, cfg *Config) (Engine, error)

// registry 是引擎注册表。
var registry = map[string]Factory{}

// Register 注册一个引擎工厂。通常在 init() 中调用。
func Register(name string, f Factory) {
	registry[name] = f
}

// New 创建指定名称的引擎。
func New(name, workspace string, cfg *Config) (Engine, error) {
	f, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("unknown engine: %q（已注册: %v）", name, registeredNames())
	}
	return f(name, workspace, cfg)
}

// Names 返回所有已注册引擎名称。
func Names() []string { return registeredNames() }

func registeredNames() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	return out
}
