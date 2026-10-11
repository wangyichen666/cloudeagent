package agentruntime

import (
	"context"
	"log"
	"time"

	"cloude-agent/internal/domain"
	"cloude-agent/internal/engine"
)

// Daemon 是 Agent 实例运行时的业务核心：
// 管理配置热加载、会话生命周期、引擎调度。
// HTTP/WS 协议适配在 internal/api/agent 包。
type Daemon struct {
	cfg     *engine.ConfigManager
	session *Session
	started time.Time
}

// NewDaemon 创建实例运行时。
// engineName 指定首选引擎（如 "qwenpaw"）；为空则默认 qwenpaw，不可用回退 mock。
func NewDaemon(workspace, configFile, engineName string) (*Daemon, error) {
	cfg := engine.NewConfigManager(configFile, func(c *engine.Config) {
		log.Printf("[agent] 模型配置热加载: model=%s provider=%s", c.Model, c.Provider)
	})
	session, err := NewSession(workspace, engineName, cfg)
	if err != nil {
		return nil, err
	}
	return &Daemon{
		cfg:     cfg,
		session: session,
		started: time.Now(),
	}, nil
}

// Health 返回健康状态摘要。
func (d *Daemon) Health() (map[string]any, error) {
	idx, err := d.session.MessageIndex()
	if err != nil {
		return nil, err
	}
	cfg := d.cfg.Get()
	return map[string]any{
		"status":    "ok",
		"workspace": d.session.Workspace(),
		"messages":  idx,
		"model":     cfg.Model,
		"provider":  cfg.Provider,
		"kernel":    d.session.KernelStatus(),
		"uptime_s":  int(time.Since(d.started).Seconds()),
	}, nil
}

// GetConfig 返回当前配置（脱敏）。
func (d *Daemon) GetConfig() *engine.Config {
	return d.cfg.Get().Masked()
}

// SetConfig 热更新模型配置。
func (d *Daemon) SetConfig(cfg *engine.Config) *engine.Config {
	applied := d.cfg.Apply(cfg)
	go d.session.SyncConfig(context.Background())
	return applied
}

// Chat 同步对话。
func (d *Daemon) Chat(ctx context.Context, req *domain.ChatRequest) (*domain.ChatResponse, error) {
	return d.session.Chat(ctx, req)
}

// ChatStream 流式对话。
func (d *Daemon) ChatStream(ctx context.Context, req *domain.ChatRequest, emit StreamEmitter) (*domain.ChatResponse, error) {
	return d.session.ChatStream(ctx, req, emit)
}

// Workspace 返回工作区摘要。
func (d *Daemon) Workspace() (map[string]any, error) {
	return d.session.WorkspaceSummary()
}

// History 返回对话历史。
func (d *Daemon) History(limit int, sessionID string) (map[string]any, error) {
	entries, err := d.session.History(limit, sessionID)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"messages": entries,
		"total":    len(entries),
		"path":     d.session.LogPath(),
	}, nil
}

// NewSession 开启新会话。
func (d *Daemon) NewSession(ctx context.Context) (string, error) {
	return d.session.NewSession(ctx)
}

// ActiveSession 返回当前活动会话 id。
func (d *Daemon) ActiveSession() string {
	return d.session.ActiveSession()
}

// Close 停止引擎（实例退出时调用）。
func (d *Daemon) Close() {
	d.session.Close()
}
