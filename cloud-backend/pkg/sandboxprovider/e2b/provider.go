// Package e2b 是 OpenKruise Agents / E2B SDK 的沙箱提供方实现。
//
// 对沙箱的一切操作（创建/唤醒/暂停/销毁）都走 E2B 兼容 SDK
//（github.com/openkruise/agents-api/e2b），不直接操作 agents.kruise.io 任何 CRD。
//
// 生命周期映射：
//   - Create = e2b.Create(template=SandboxSet名)，从预热池领取一个沙箱
//   - Start  = e2b.Connect(sandboxID)，自动 resume paused 沙箱
//   - Stop   = e2b.Connect(sandboxID) 后 sbx.Pause()
//   - Delete = e2b.Connect(sandboxID) 后 sbx.Kill()
package e2b

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	e2bsdk "github.com/openkruise/agents-api/e2b"

	"cloude-agent/pkg/sandboxprovider"
)

// Config 是 E2B 提供方的连接与行为配置。
type Config struct {
	APIURL         string        // sandbox-manager API 地址
	SandboxBaseURL string        // sandbox-gateway 数据面地址
	APIKey         string        // E2B API Key
	Scheme         string        // http / https
	Template       string        // SandboxSet 预热池名（如 agent-pool）
	TimeoutSec     int32         // 沙箱空闲 TTL（秒）
	AgentPort      int           // 沙箱内 agent-runtime 端口
	WaitReady      time.Duration // 等沙箱就绪超时
}

// Provider 实现 sandboxprovider.Provider。
type Provider struct {
	cfg  Config
	mu   sync.Mutex
	sbxs map[string]string // userID -> sandboxID
}

// New 构造 E2B 提供方。
func New(cfg Config) (*Provider, error) {
	if cfg.APIURL == "" {
		return nil, fmt.Errorf("e2b provider: APIURL 未配置")
	}
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("e2b provider: APIKey 未配置")
	}
	if cfg.Template == "" {
		cfg.Template = "agent-pool"
	}
	if cfg.Scheme == "" {
		cfg.Scheme = "http"
	}
	if cfg.AgentPort == 0 {
		cfg.AgentPort = 18585
	}
	if cfg.TimeoutSec == 0 {
		cfg.TimeoutSec = 1800
	}
	if cfg.WaitReady == 0 {
		cfg.WaitReady = 60 * time.Second
	}
	return &Provider{cfg: cfg, sbxs: make(map[string]string)}, nil
}

func (p *Provider) Name() string { return "e2b" }

// Seed 实现 sandboxprovider.Seeder：控制面重启后恢复映射。
func (p *Provider) Seed(m map[string]string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for uid, sid := range m {
		if sid != "" {
			p.sbxs[uid] = sid
		}
	}
	log.Printf("[e2b] seed: 恢复 %d 个沙箱映射", len(m))
}

func (p *Provider) connOpts() []e2bsdk.ConnectionConfigOption {
	opts := []e2bsdk.ConnectionConfigOption{
		e2bsdk.WithAPIKey(p.cfg.APIKey),
		e2bsdk.WithScheme(p.cfg.Scheme),
		e2bsdk.WithAPIURL(p.cfg.APIURL),
	}
	if p.cfg.SandboxBaseURL != "" {
		opts = append(opts, e2bsdk.WithSandboxBaseURL(p.cfg.SandboxBaseURL))
	}
	return opts
}

func (p *Provider) remember(userID, sandboxID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sbxs[userID] = sandboxID
}

func (p *Provider) lookup(userID string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	sid, ok := p.sbxs[userID]
	return sid, ok
}

func (p *Provider) forget(userID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.sbxs, userID)
}

// Create 从预热池领取一个沙箱。
func (p *Provider) Create(ctx context.Context, userID string) (*sandboxprovider.Info, error) {
	metadata := map[string]string{
		"user-id":                          userID,
		"e2b.agents.kruise.io/never-timeout": "true",
	}
	sbx, err := e2bsdk.Create(ctx, p.cfg.Template,
		e2bsdk.WithConfig(p.connOpts()...),
		e2bsdk.WithTimeout(p.cfg.TimeoutSec),
		e2bsdk.WithMetadata(metadata),
	)
	if err != nil {
		return nil, fmt.Errorf("e2b create: %w", err)
	}
	sandboxID := sbx.SandboxID()
	p.remember(userID, sandboxID)

	if err := p.waitReady(ctx, sbx); err != nil {
		return nil, fmt.Errorf("e2b wait ready: %w", err)
	}
	ep := p.endpoint(sbx)
	log.Printf("[e2b] create ok user=%s sandbox=%s endpoint=%s", userID, sandboxID, ep)
	return &sandboxprovider.Info{
		UserID:    userID,
		Workspace: fmt.Sprintf("users/%s", userID),
		Endpoint:  ep,
		Port:      p.cfg.AgentPort,
		SandboxID: sandboxID,
	}, nil
}

// Start 唤醒（connect 自动 resume paused 沙箱）。
func (p *Provider) Start(ctx context.Context, userID string) (*sandboxprovider.Info, error) {
	sandboxID, ok := p.lookup(userID)
	if !ok {
		return nil, fmt.Errorf("e2b: 未知 user=%s 的 sandboxID（请先 Create 或 Seed）", userID)
	}
	sbx, err := e2bsdk.Connect(ctx, sandboxID,
		e2bsdk.WithConfig(p.connOpts()...),
		e2bsdk.WithTimeout(p.cfg.TimeoutSec),
	)
	if err != nil {
		return nil, fmt.Errorf("e2b connect: %w", err)
	}
	if err := p.waitReady(ctx, sbx); err != nil {
		return nil, fmt.Errorf("e2b wait ready on wake: %w", err)
	}
	ep := p.endpoint(sbx)
	log.Printf("[e2b] wake ok user=%s sandbox=%s endpoint=%s", userID, sandboxID, ep)
	return &sandboxprovider.Info{
		UserID:    userID,
		Workspace: fmt.Sprintf("users/%s", userID),
		Endpoint:  ep,
		Port:      p.cfg.AgentPort,
		SandboxID: sandboxID,
	}, nil
}

// Stop 暂停沙箱。
func (p *Provider) Stop(ctx context.Context, userID string) error {
	sandboxID, ok := p.lookup(userID)
	if !ok {
		return nil // 幂等
	}
	sbx, err := e2bsdk.Connect(ctx, sandboxID, e2bsdk.WithConfig(p.connOpts()...))
	if err != nil {
		return fmt.Errorf("e2b connect for pause: %w", err)
	}
	if _, err := sbx.Pause(ctx); err != nil {
		return fmt.Errorf("e2b pause: %w", err)
	}
	log.Printf("[e2b] pause ok user=%s sandbox=%s", userID, sandboxID)
	return nil
}

// Delete 销毁沙箱。
func (p *Provider) Delete(ctx context.Context, userID string) error {
	sandboxID, ok := p.lookup(userID)
	if !ok {
		return nil
	}
	sbx, err := e2bsdk.Connect(ctx, sandboxID, e2bsdk.WithConfig(p.connOpts()...))
	if err != nil {
		log.Printf("[e2b] delete: connect 失败（视为已销毁）user=%s: %v", userID, err)
		p.forget(userID)
		return nil
	}
	if _, err := sbx.Kill(ctx); err != nil {
		return fmt.Errorf("e2b kill: %w", err)
	}
	p.forget(userID)
	log.Printf("[e2b] kill ok user=%s sandbox=%s", userID, sandboxID)
	return nil
}

// waitReady 轮询 GetInfo 等 sandbox 可查询即视为就绪。
func (p *Provider) waitReady(ctx context.Context, sbx *e2bsdk.Sandbox) error {
	deadline := time.Now().Add(p.cfg.WaitReady)
	for time.Now().Before(deadline) {
		if _, err := sbx.GetInfo(ctx); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(1 * time.Second):
		}
	}
	return fmt.Errorf("agent-runtime 未在 %s 内就绪（GetInfo 持续失败）", p.cfg.WaitReady)
}

// endpoint 由 sandbox-gateway 按 sandboxID 路由。
func (p *Provider) endpoint(sbx *e2bsdk.Sandbox) string {
	return fmt.Sprintf("%s/sandboxes/%s/port/%d",
		strings.TrimRight(p.cfg.SandboxBaseURL, "/"),
		sbx.SandboxID(), p.cfg.AgentPort)
}
