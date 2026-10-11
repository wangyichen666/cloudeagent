// Package backend 内的 e2b.go 是「全 E2B 路线」数据面实现：
// 对沙箱的一切操作（创建/唤醒/暂停/销毁/读文件/执行命令）都走
// OpenKruise Agents 的 E2B 兼容 SDK（github.com/openkruise/agents-api/e2b），
// control-plane 不直接操作 agents.kruise.io 任何 CRD。
//
// 生命周期映射：
//   - Create = e2b.Create(template=SandboxSet名)，从预热池领取一个沙箱；
//   - Start  = e2b.Connect(sandboxID)，自动 resume paused 沙箱并刷新 TTL；
//   - Stop   = e2b.Connect(sandboxID) 后 sbx.Pause()，冻结 Pod、释放 CPU/内存；
//   - Delete = e2b.Connect(sandboxID) 后 sbx.Kill()，销毁沙箱。
//
// 路由：沙箱内 agent-runtime 监听 18585，Create/Start 后用
// `hostname -i`（经 E2B Commands）拿到 Pod IP，endpoint = http://<podIP>:18585。
// control-plane 与 cloud-gateway 都在集群内，可直连 Pod IP。
package backend

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	e2bsdk "github.com/openkruise/agents-api/e2b"
)

// E2BConfig 是 E2B 后端的连接与行为配置。
type E2BConfig struct {
	APIURL          string // sandbox-manager API 地址（集群内：http://sandbox-manager.sandbox-system:8080；本地：http://127.0.0.1:8080）
	SandboxBaseURL  string // sandbox-gateway 数据面地址（集群内 http://sandbox-gateway.sandbox-system:7788；本地 http://127.0.0.1:7788）
	APIKey          string // E2B API Key（sandbox-manager 安装时配置）
	Scheme          string // http（内网）/ https（公网）
	Template        string // SandboxSet 预热池名（如 agent-pool）
	TimeoutSec      int32  // 沙箱空闲 TTL（秒），到期自动 pause/kill
	AgentPort       int    // 沙箱内 agent-runtime 端口
	WaitReady       time.Duration
}

// E2B 实现 InstanceBackend。
type E2B struct {
	cfg    E2BConfig
	mu     sync.Mutex
	sbxs   map[string]string // userID -> sandboxID（control-plane 重启后由 Seed 恢复）
}

// NewE2B 构造 E2B 后端。
func NewE2B(cfg E2BConfig) (*E2B, error) {
	if cfg.APIURL == "" {
		return nil, fmt.Errorf("e2b backend: APIURL 未配置")
	}
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("e2b backend: APIKey 未配置")
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
		cfg.TimeoutSec = 1800 // 默认 30 分钟空闲 TTL
	}
	if cfg.WaitReady == 0 {
		cfg.WaitReady = 60 * time.Second
	}
	return &E2B{cfg: cfg, sbxs: make(map[string]string)}, nil
}

// Seed 在 control-plane 启动时从 Store 恢复 userID->sandboxID 映射。
func (b *E2B) Seed(m map[string]string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for uid, sid := range m {
		if sid != "" {
			b.sbxs[uid] = sid
		}
	}
	log.Printf("[e2b] seed: 恢复 %d 个沙箱映射", len(m))
}

func (b *E2B) Name() string { return "e2b" }

func (b *E2B) connOpts() []e2bsdk.ConnectionConfigOption {
	opts := []e2bsdk.ConnectionConfigOption{
		e2bsdk.WithAPIKey(b.cfg.APIKey),
		e2bsdk.WithScheme(b.cfg.Scheme),
		e2bsdk.WithAPIURL(b.cfg.APIURL),
	}
	if b.cfg.SandboxBaseURL != "" {
		opts = append(opts, e2bsdk.WithSandboxBaseURL(b.cfg.SandboxBaseURL))
	}
	return opts
}

func (b *E2B) remember(userID, sandboxID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sbxs[userID] = sandboxID
}

func (b *E2B) lookup(userID string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	sid, ok := b.sbxs[userID]
	return sid, ok
}

func (b *E2B) forget(userID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.sbxs, userID)
}

// Create 从预热池领取一个沙箱，等 agent-runtime 就绪，返回 Pod IP endpoint。
func (b *E2B) Create(ctx context.Context, userID string) (*Info, error) {
	metadata := map[string]string{
		"user-id":                        userID,
		"e2b.agents.kruise.io/never-timeout": "true", // TTL 由 Reaper 控制，不依赖 E2B 自动销毁
	}
	sbx, err := e2bsdk.Create(ctx, b.cfg.Template,
		e2bsdk.WithConfig(b.connOpts()...),
		e2bsdk.WithTimeout(b.cfg.TimeoutSec),
		e2bsdk.WithMetadata(metadata),
	)
	if err != nil {
		return nil, fmt.Errorf("e2b create: %w", err)
	}
	sandboxID := sbx.SandboxID()
	b.remember(userID, sandboxID)

	if err := b.waitReady(ctx, sbx); err != nil {
		// 创建失败不自动 kill（保留现场），由 Manager 决定清理
		return nil, fmt.Errorf("e2b wait ready: %w", err)
	}
	ep := b.endpoint(sbx)
	log.Printf("[e2b] create ok user=%s sandbox=%s endpoint=%s", userID, sandboxID, ep)
	return &Info{
		UserID:    userID,
		Workspace: fmt.Sprintf("users/%s", userID),
		Endpoint:  ep,
		Port:      b.cfg.AgentPort,
		SandboxID: sandboxID,
	}, nil
}

// Start 唤醒（connect 自动 resume paused 沙箱）。
func (b *E2B) Start(ctx context.Context, userID string) (*Info, error) {
	sandboxID, ok := b.lookup(userID)
	if !ok {
		return nil, fmt.Errorf("e2b: 未知 user=%s 的 sandboxID（请先 Create 或 Seed）", userID)
	}
	sbx, err := e2bsdk.Connect(ctx, sandboxID,
		e2bsdk.WithConfig(b.connOpts()...),
		e2bsdk.WithTimeout(b.cfg.TimeoutSec),
	)
	if err != nil {
		return nil, fmt.Errorf("e2b connect: %w", err)
	}
	if err := b.waitReady(ctx, sbx); err != nil {
		return nil, fmt.Errorf("e2b wait ready on wake: %w", err)
	}
	ep := b.endpoint(sbx)
	log.Printf("[e2b] wake ok user=%s sandbox=%s endpoint=%s", userID, sandboxID, ep)
	return &Info{
		UserID:    userID,
		Workspace: fmt.Sprintf("users/%s", userID),
		Endpoint:  ep,
		Port:      b.cfg.AgentPort,
		SandboxID: sandboxID,
	}, nil
}

// Stop 暂停沙箱（冻结 Pod、释放 CPU/内存，sandboxID 不变）。
func (b *E2B) Stop(ctx context.Context, userID string) error {
	sandboxID, ok := b.lookup(userID)
	if !ok {
		return nil // 幂等：没有记录视为已停
	}
	sbx, err := e2bsdk.Connect(ctx, sandboxID, e2bsdk.WithConfig(b.connOpts()...))
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
func (b *E2B) Delete(ctx context.Context, userID string) error {
	sandboxID, ok := b.lookup(userID)
	if !ok {
		return nil
	}
	sbx, err := e2bsdk.Connect(ctx, sandboxID, e2bsdk.WithConfig(b.connOpts()...))
	if err != nil {
		// connect 失败说明沙箱可能已不存在，视为删除成功
		log.Printf("[e2b] delete: connect 失败（视为已销毁）user=%s: %v", userID, err)
		b.forget(userID)
		return nil
	}
	if _, err := sbx.Kill(ctx); err != nil {
		return fmt.Errorf("e2b kill: %w", err)
	}
	b.forget(userID)
	log.Printf("[e2b] kill ok user=%s sandbox=%s", userID, sandboxID)
	return nil
}

// waitReady 轮询 GetInfo 等 sandbox 可查询即视为就绪。
// 不依赖沙箱内 Commands（镜像精简、无 curl/wget）。
func (b *E2B) waitReady(ctx context.Context, sbx *e2bsdk.Sandbox) error {
	deadline := time.Now().Add(b.cfg.WaitReady)
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
	return fmt.Errorf("agent-runtime 未在 %s 内就绪（GetInfo 持续失败）", b.cfg.WaitReady)
}

// endpoint 由 sandbox-gateway 按 sandboxID 路由，control-plane 不直连 Pod IP。
func (b *E2B) endpoint(sbx *e2bsdk.Sandbox) string {
	return fmt.Sprintf("%s/sandboxes/%s/port/%d",
		strings.TrimRight(b.cfg.SandboxBaseURL, "/"),
		sbx.SandboxID(), b.cfg.AgentPort)
}

// Delete 销毁沙箱。
