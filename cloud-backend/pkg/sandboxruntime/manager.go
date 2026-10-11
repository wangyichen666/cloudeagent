package sandboxruntime

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"cloude-agent/pkg/db"
	"cloude-agent/pkg/sandboxprovider"
)

// AgentGateway 是 backend 依赖的数据面网关抽象：
// 业务层只按 userID 调用，不感知 Pod 地址与内部协议。
type AgentGateway interface {
	Register(ctx context.Context, userID, endpoint string) error
	Unregister(ctx context.Context, userID string) error
	Chat(ctx context.Context, userID, message, sessionID string) (*db.ChatResponse, error)
	Workspace(ctx context.Context, userID string) (map[string]any, error)
	History(ctx context.Context, userID string, limit int, sessionID string) (map[string]any, error)
	NewSession(ctx context.Context, userID string) (map[string]any, error)
	SessionInfo(ctx context.Context, userID string) (string, error)
	Health(ctx context.Context, userID string) (map[string]any, error)
	SetConfig(ctx context.Context, userID string, cfg *db.ModelConfig) error
	WSUpstreamURL(userID string) string
}

// Manager 是编排核心（文档 4.3 生命周期状态机）：
//   [None] -> [Running] <-> [Suspended] -> [None]
// Create/Suspend/Wake/Delete 全部收敛在此，状态写入 Store（唯一事实来源）。
type Manager struct {
	store         db.Store
	cache         db.ModelConfigCache
	backend       sandboxprovider.Provider
	seats         SeatService
	gateway       AgentGateway
	injectTimeout time.Duration
	mu            sync.Mutex
}

func NewManager(store db.Store, cache db.ModelConfigCache, bk sandboxprovider.Provider, seats SeatService, gw AgentGateway) *Manager {
	return &Manager{
		store:         store,
		cache:         cache,
		backend:       bk,
		seats:         seats,
		gateway:       gw,
		injectTimeout: 5 * time.Second,
	}
}

// registerGateway 把实例地址登记到 gateway（失败不阻断业务，仅告警；
// 后续转发会在 gateway 侧给出明确未注册错误）。
func (m *Manager) registerGateway(ctx context.Context, userID, endpoint string) {
	if m.gateway == nil {
		return
	}
	if err := m.gateway.Register(ctx, userID, endpoint); err != nil {
		log.Printf("[manager] gateway 注册失败 user=%s: %v", userID, err)
	}
}

// Create 对应状态机 None -> Running：创建后端资源 -> 等 Ready -> 注入模型配置。
func (m *Manager) Create(ctx context.Context, userID string) (*db.Instance, error) {
	if err := sandboxprovider.ValidateUserID(userID); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	if inst, err := m.store.GetInstance(ctx, userID); err == nil {
		switch inst.Status {
		case db.StatusRunning:
			return inst, nil // 幂等
		case db.StatusSuspended:
			return m.wakeLocked(ctx, userID, inst)
		case db.StatusFailed:
			_ = m.backend.Delete(ctx, userID)
		}
	}

	now := time.Now().UTC()
	inst := &db.Instance{
		UserID:       userID,
		InstanceName: "agent-" + userID,
		Status:       db.StatusRunning,
		CreatedAt:    now,
		UpdatedAt:    now,
		LastActiveAt: now,
	}
	info, err := m.backend.Create(ctx, userID)
	if err != nil {
		return m.failInstance(ctx, userID, inst, err)
	}
	inst.Workspace, inst.Endpoint, inst.Port = info.Workspace, info.Endpoint, info.Port
	if err := sandboxprovider.WaitHealth(ctx, inst.Endpoint, 60*time.Second); err != nil {
		_ = m.backend.Delete(ctx, userID)
		return m.failInstance(ctx, userID, inst, err)
	}
	m.registerGateway(ctx, userID, inst.Endpoint)
	cfg, err := m.resolveConfig(ctx, userID)
	if err != nil {
		_ = m.backend.Delete(ctx, userID)
		return m.failInstance(ctx, userID, inst, err)
	}
	if err := m.setConfigViaGateway(ctx, userID, cfg); err != nil {
		// E2B gateway 在 kind 本地环境注册延迟，setConfig 失败不阻塞 create（沙箱已运行，后续 chat 时再注入）
		log.Printf("[manager] WARN setConfig 失败（忽略）user=%s: %v", userID, err)
	}
	if err := m.store.CreateInstance(ctx, inst); err != nil {
		_ = m.backend.Delete(ctx, userID)
		return m.failInstance(ctx, userID, inst, err)
	}
	log.Printf("[manager] create ok user=%s endpoint=%s model=%s", userID, inst.Endpoint, cfg.Model)
	return inst, nil
}

// Suspend 对应 Running -> Suspended：停掉运行时，保留工作区（PVC 语义）。
func (m *Manager) Suspend(ctx context.Context, userID string) (*db.Instance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	inst, err := m.store.GetInstance(ctx, userID)
	if err != nil {
		return nil, err
	}
	if inst.Status != db.StatusRunning {
		return inst, fmt.Errorf("实例当前状态为 %s，无法休眠", inst.Status)
	}
	if err := m.backend.Stop(ctx, userID); err != nil {
		return nil, fmt.Errorf("休眠失败: %w", err)
	}
	// 凭证不落盘：休眠即丢弃热缓存，唤醒时重新注入（文档 6.2）。
	_ = m.cache.Delete(ctx, userID)
	inst.Status = db.StatusSuspended
	inst.UpdatedAt = time.Now().UTC()
	if err := m.store.UpdateInstance(ctx, inst); err != nil {
		return nil, err
	}
	log.Printf("[manager] suspend ok user=%s", userID)
	return inst, nil
}

// Wake 对应 Suspended -> Running：重启运行时挂回同一工作区，重新注入模型配置。
func (m *Manager) Wake(ctx context.Context, userID string) (*db.Instance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	inst, err := m.store.GetInstance(ctx, userID)
	if err != nil {
		return nil, err
	}
	return m.wakeLocked(ctx, userID, inst)
}

func (m *Manager) wakeLocked(ctx context.Context, userID string, inst *db.Instance) (*db.Instance, error) {
	if inst.Status == db.StatusRunning {
		return inst, nil
	}
	info, err := m.backend.Start(ctx, userID)
	if err != nil {
		return m.failInstance(ctx, userID, inst, err)
	}
	inst.Workspace, inst.Endpoint, inst.Port = info.Workspace, info.Endpoint, info.Port
	if err := sandboxprovider.WaitHealth(ctx, inst.Endpoint, 60*time.Second); err != nil {
		return m.failInstance(ctx, userID, inst, err)
	}
	m.registerGateway(ctx, userID, inst.Endpoint)
	cfg, err := m.resolveConfig(ctx, userID)
	if err != nil {
		return m.failInstance(ctx, userID, inst, err)
	}
	if err := m.setConfigViaGateway(ctx, userID, cfg); err != nil {
		// E2B gateway 在 kind 本地环境注册延迟，setConfig 失败不阻塞 wake
		log.Printf("[manager] WARN setConfig 失败（忽略）user=%s: %v", userID, err)
	}
	inst.Status = db.StatusRunning
	inst.Error = ""
	inst.UpdatedAt = time.Now().UTC()
	inst.LastActiveAt = time.Now().UTC()
	if err := m.store.UpdateInstance(ctx, inst); err != nil {
		return nil, err
	}
	log.Printf("[manager] wake ok user=%s endpoint=%s model=%s", userID, inst.Endpoint, cfg.Model)
	return inst, nil
}

// Delete 对应 [Running/Suspended] -> None：销毁后端资源与映射记录。
func (m *Manager) Delete(ctx context.Context, userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.backend.Delete(ctx, userID); err != nil {
		return fmt.Errorf("销毁实例失败: %w", err)
	}
	if m.gateway != nil {
		_ = m.gateway.Unregister(ctx, userID)
	}
	_ = m.cache.Delete(ctx, userID)
	if err := m.store.DeleteInstance(ctx, userID); err != nil {
		return err
	}
	log.Printf("[manager] delete ok user=%s", userID)
	return nil
}

func (m *Manager) Get(ctx context.Context, userID string) (*db.Instance, error) {
	return m.store.GetInstance(ctx, userID)
}

// Seed 把启动时发现的后端实例登记进状态存储（幂等）：
// 控制面重启后（内存模式）自动恢复已有实例，避免 Pod 在跑但列表为空。
// 模型凭证仍遵守「不落盘」设计：不在此恢复，唤醒时由席位/前端重新注入。
func (m *Manager) Seed(ctx context.Context, infos []sandboxprovider.Info) error {
	for _, info := range infos {
		if info.UserID == "" {
			continue
		}
		if _, err := m.store.GetInstance(ctx, info.UserID); err == nil {
			continue
		}
		now := time.Now()
		inst := &db.Instance{
			UserID:       info.UserID,
			InstanceName: "agent-" + info.UserID,
			Status:       db.StatusRunning,
			Workspace:    info.Workspace,
			Endpoint:     info.Endpoint,
			Port:         info.Port,
			LastActiveAt: now,
			CreatedAt:    now,
			UpdatedAt:    now,
		}
		if err := m.store.CreateInstance(ctx, inst); err != nil {
			return err
		}
		m.registerGateway(ctx, info.UserID, info.Endpoint)
		log.Printf("[manager] reconcile: 恢复实例 %s endpoint=%s", info.UserID, info.Endpoint)
	}
	return nil
}

func (m *Manager) List(ctx context.Context) ([]*db.Instance, error) {
	return m.store.ListInstances(ctx)
}

// SetModelConfig 实现模型热切换：直接热加载到运行中的实例并更新热缓存。
func (m *Manager) SetModelConfig(ctx context.Context, userID string, cfg *db.ModelConfig) (*db.Instance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	inst, err := m.store.GetInstance(ctx, userID)
	if err != nil {
		return nil, err
	}
	if inst.Status != db.StatusRunning {
		return nil, fmt.Errorf("实例未运行，请先唤醒再切换模型")
	}
	if err := m.setConfigViaGateway(ctx, userID, cfg); err != nil {
		return nil, fmt.Errorf("热加载失败: %w", err)
	}
	_ = m.cache.Set(ctx, userID, cfg, 24*time.Hour)
	_ = m.store.TouchActivity(ctx, userID)
	log.Printf("[manager] model hot-switch user=%s model=%s provider=%s", userID, cfg.Model, cfg.Provider)
	return inst, nil
}

func (m *Manager) GetModelConfig(ctx context.Context, userID string) (*db.ModelConfig, error) {
	cfg, err := m.resolveConfig(ctx, userID)
	if err != nil {
		return nil, err
	}
	return cfg.Masked(), nil
}

// resolveConfig：用户覆盖（热缓存）优先，否则走席位服务。
func (m *Manager) resolveConfig(ctx context.Context, userID string) (*db.ModelConfig, error) {
	if cfg, err := m.cache.Get(ctx, userID); err == nil && cfg.BaseURL != "" {
		return cfg, nil
	}
	return m.seats.Resolve(ctx, userID)
}

// setConfigViaGateway 经 gateway 向实例注入运行时模型配置（凭证仅驻留内存）。
func (m *Manager) setConfigViaGateway(ctx context.Context, userID string, cfg *db.ModelConfig) error {
	if m.gateway == nil {
		return fmt.Errorf("未配置数据面网关（--gateway）")
	}
	return m.gateway.SetConfig(ctx, userID, cfg)
}

// Chat 走控制面统一入口：鉴权/限流/审计都在这里做（文档 5.2 方案 A）。
func (m *Manager) Chat(ctx context.Context, userID, message string) (*db.ChatResponse, error) {
	if _, err := m.ensureRunning(ctx, userID); err != nil {
		return nil, err
	}
	out, err := m.gateway.Chat(ctx, userID, message, "")
	if err != nil {
		return nil, err
	}
	_ = m.store.TouchActivity(ctx, userID)
	return out, nil
}

// ensureRunning 保证实例运行（文档：唤醒后再路由）。
func (m *Manager) ensureRunning(ctx context.Context, userID string) (*db.Instance, error) {
	inst, err := m.store.GetInstance(ctx, userID)
	if err != nil {
		return nil, err
	}
	if inst.Status != db.StatusRunning {
		return nil, fmt.Errorf("实例状态为 %s，请先 POST /v1/users/%s/wake 唤醒", inst.Status, userID)
	}
	return inst, nil
}

// Workspace 代理查询实例工作区（可观测）。
func (m *Manager) Workspace(ctx context.Context, userID string) (map[string]any, error) {
	if _, err := m.ensureRunning(ctx, userID); err != nil {
		return nil, err
	}
	return m.gateway.Workspace(ctx, userID)
}

// History 代理读取实例的持久化对话历史（.agent/conversation.jsonl）。
// sessionID 非空时只返回该会话的记录。
func (m *Manager) History(ctx context.Context, userID string, limit int, sessionID string) (map[string]any, error) {
	if _, err := m.ensureRunning(ctx, userID); err != nil {
		return nil, err
	}
	return m.gateway.History(ctx, userID, limit, sessionID)
}

// NewSession 让 Pod 内的 QwenPaw 真实开启一个新会话。
func (m *Manager) NewSession(ctx context.Context, userID string) (map[string]any, error) {
	if _, err := m.ensureRunning(ctx, userID); err != nil {
		return nil, err
	}
	return m.gateway.NewSession(ctx, userID)
}

// Connect 返回前端「连接 Agent」所需信息：确保实例运行、拉取内核状态。
// 实例 token 与 WebSocket 地址由 Server 层签发（Authenticator 持有命名空间）。
func (m *Manager) Connect(ctx context.Context, userID string) (map[string]any, error) {
	inst, err := m.ensureRunning(ctx, userID)
	if err != nil {
		return nil, err
	}
	kernel := map[string]any{}
	if health, err := m.gateway.Health(ctx, userID); err == nil {
		if k, ok := health["kernel"].(map[string]any); ok {
			kernel = k
		}
	}
	sessionID := ""
	if sid, err := m.gateway.SessionInfo(ctx, userID); err == nil {
		sessionID = sid
	}
	return map[string]any{
		"user_id":   userID,
		"status":    inst.Status,
		"endpoint":  inst.Endpoint,
		"workspace": inst.Workspace,
		"kernel":    kernel,
		"session_id": sessionID,
	}, nil
}

// TouchActivity 更新活跃度（供 idle reaper 判定）。
func (m *Manager) TouchActivity(ctx context.Context, userID string) error {
	return m.store.TouchActivity(ctx, userID)
}

func (m *Manager) SetWSConnections(ctx context.Context, userID string, n int) error {
	return m.store.SetWSConnections(ctx, userID, n)
}

// GatewayWSURL 返回 backend 流式转发时连接 gateway 的 WS 地址。
func (m *Manager) GatewayWSURL(userID string) string {
	if m.gateway == nil {
		return ""
	}
	return m.gateway.WSUpstreamURL(userID)
}

func (m *Manager) failInstance(ctx context.Context, userID string, inst *db.Instance, err error) (*db.Instance, error) {
	inst.Status = db.StatusFailed
	inst.Error = err.Error()
	inst.UpdatedAt = time.Now().UTC()
	if _, getErr := m.store.GetInstance(ctx, userID); getErr == nil {
		_ = m.store.UpdateInstance(ctx, inst)
	} else {
		_ = m.store.CreateInstance(ctx, inst)
	}
	log.Printf("[manager] FAIL user=%s: %v", userID, err)
	return inst, err
}
