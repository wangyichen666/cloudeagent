package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"cloude-agent/internal/backend"
	"cloude-agent/internal/models"
	"cloude-agent/internal/store"
)

// Manager 是编排核心（文档 4.3 生命周期状态机）：
//   [None] -> [Running] <-> [Suspended] -> [None]
// Create/Suspend/Wake/Delete 全部收敛在此，状态写入 Store（唯一事实来源）。
type Manager struct {
	store         store.Store
	cache         store.ModelConfigCache
	backend       backend.InstanceBackend
	seats         SeatService
	injectTimeout time.Duration
	mu            sync.Mutex // 串行化单实例的状态迁移（本地等价于 Redis 分布式锁）
}

func NewManager(store store.Store, cache store.ModelConfigCache, bk backend.InstanceBackend, seats SeatService) *Manager {
	return &Manager{
		store:         store,
		cache:         cache,
		backend:       bk,
		seats:         seats,
		injectTimeout: 5 * time.Second,
	}
}

// Create 对应状态机 None -> Running：创建后端资源 -> 等 Ready -> 注入模型配置。
func (m *Manager) Create(ctx context.Context, userID string) (*models.Instance, error) {
	if err := backend.ValidateUserID(userID); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	if inst, err := m.store.GetInstance(ctx, userID); err == nil {
		switch inst.Status {
		case models.StatusRunning:
			return inst, nil // 幂等
		case models.StatusSuspended:
			return m.wakeLocked(ctx, userID, inst)
		case models.StatusFailed:
			_ = m.backend.Delete(ctx, userID)
		}
	}

	now := time.Now().UTC()
	inst := &models.Instance{
		UserID:       userID,
		InstanceName: "agent-" + userID,
		Status:       models.StatusRunning,
		CreatedAt:    now,
		UpdatedAt:    now,
		LastActiveAt: now,
	}
	info, err := m.backend.Create(ctx, userID)
	if err != nil {
		return m.failInstance(ctx, userID, inst, err)
	}
	inst.Workspace, inst.Endpoint, inst.Port = info.Workspace, info.Endpoint, info.Port
	if err := backend.WaitHealth(ctx, inst.Endpoint, 15*time.Second); err != nil {
		_ = m.backend.Delete(ctx, userID)
		return m.failInstance(ctx, userID, inst, err)
	}
	cfg, err := m.resolveConfig(ctx, userID)
	if err != nil {
		_ = m.backend.Delete(ctx, userID)
		return m.failInstance(ctx, userID, inst, err)
	}
	if err := m.injectConfig(ctx, inst.Endpoint, cfg); err != nil {
		_ = m.backend.Delete(ctx, userID)
		return m.failInstance(ctx, userID, inst, err)
	}
	if err := m.store.CreateInstance(ctx, inst); err != nil {
		_ = m.backend.Delete(ctx, userID)
		return m.failInstance(ctx, userID, inst, err)
	}
	log.Printf("[manager] create ok user=%s endpoint=%s model=%s", userID, inst.Endpoint, cfg.Model)
	return inst, nil
}

// Suspend 对应 Running -> Suspended：停掉运行时，保留工作区（PVC 语义）。
func (m *Manager) Suspend(ctx context.Context, userID string) (*models.Instance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	inst, err := m.store.GetInstance(ctx, userID)
	if err != nil {
		return nil, err
	}
	if inst.Status != models.StatusRunning {
		return inst, fmt.Errorf("实例当前状态为 %s，无法休眠", inst.Status)
	}
	if err := m.backend.Stop(ctx, userID); err != nil {
		return nil, fmt.Errorf("休眠失败: %w", err)
	}
	// 凭证不落盘：休眠即丢弃热缓存，唤醒时重新注入（文档 6.2）。
	_ = m.cache.Delete(ctx, userID)
	inst.Status = models.StatusSuspended
	inst.UpdatedAt = time.Now().UTC()
	if err := m.store.UpdateInstance(ctx, inst); err != nil {
		return nil, err
	}
	log.Printf("[manager] suspend ok user=%s", userID)
	return inst, nil
}

// Wake 对应 Suspended -> Running：重启运行时挂回同一工作区，重新注入模型配置。
func (m *Manager) Wake(ctx context.Context, userID string) (*models.Instance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	inst, err := m.store.GetInstance(ctx, userID)
	if err != nil {
		return nil, err
	}
	return m.wakeLocked(ctx, userID, inst)
}

func (m *Manager) wakeLocked(ctx context.Context, userID string, inst *models.Instance) (*models.Instance, error) {
	if inst.Status == models.StatusRunning {
		return inst, nil
	}
	info, err := m.backend.Start(ctx, userID)
	if err != nil {
		return m.failInstance(ctx, userID, inst, err)
	}
	inst.Workspace, inst.Endpoint, inst.Port = info.Workspace, info.Endpoint, info.Port
	if err := backend.WaitHealth(ctx, inst.Endpoint, 15*time.Second); err != nil {
		return m.failInstance(ctx, userID, inst, err)
	}
	cfg, err := m.resolveConfig(ctx, userID)
	if err != nil {
		return m.failInstance(ctx, userID, inst, err)
	}
	if err := m.injectConfig(ctx, inst.Endpoint, cfg); err != nil {
		return m.failInstance(ctx, userID, inst, err)
	}
	inst.Status = models.StatusRunning
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
	_ = m.cache.Delete(ctx, userID)
	if err := m.store.DeleteInstance(ctx, userID); err != nil {
		return err
	}
	log.Printf("[manager] delete ok user=%s", userID)
	return nil
}

func (m *Manager) Get(ctx context.Context, userID string) (*models.Instance, error) {
	return m.store.GetInstance(ctx, userID)
}

func (m *Manager) List(ctx context.Context) ([]*models.Instance, error) {
	return m.store.ListInstances(ctx)
}

// SetModelConfig 实现模型热切换：直接热加载到运行中的实例并更新热缓存。
func (m *Manager) SetModelConfig(ctx context.Context, userID string, cfg *models.ModelConfig) (*models.Instance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	inst, err := m.store.GetInstance(ctx, userID)
	if err != nil {
		return nil, err
	}
	if inst.Status != models.StatusRunning {
		return nil, fmt.Errorf("实例未运行，请先唤醒再切换模型")
	}
	if err := m.injectConfig(ctx, inst.Endpoint, cfg); err != nil {
		return nil, fmt.Errorf("热加载失败: %w", err)
	}
	_ = m.cache.Set(ctx, userID, cfg, 24*time.Hour)
	_ = m.store.TouchActivity(ctx, userID)
	log.Printf("[manager] model hot-switch user=%s model=%s provider=%s", userID, cfg.Model, cfg.Provider)
	return inst, nil
}

func (m *Manager) GetModelConfig(ctx context.Context, userID string) (*models.ModelConfig, error) {
	cfg, err := m.resolveConfig(ctx, userID)
	if err != nil {
		return nil, err
	}
	return cfg.Masked(), nil
}

// resolveConfig：用户覆盖（热缓存）优先，否则走席位服务。
func (m *Manager) resolveConfig(ctx context.Context, userID string) (*models.ModelConfig, error) {
	if cfg, err := m.cache.Get(ctx, userID); err == nil && cfg.BaseURL != "" {
		return cfg, nil
	}
	return m.seats.Resolve(ctx, userID)
}

// injectConfig 把运行时模型配置注入实例（HTTP 热加载，凭证仅驻留内存）。
func (m *Manager) injectConfig(ctx context.Context, endpoint string, cfg *models.ModelConfig) error {
	body, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/v1/config", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: m.injectTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("config inject status %d", resp.StatusCode)
	}
	return nil
}

// Chat 走控制面统一入口：鉴权/限流/审计都在这里做（文档 5.2 方案 A）。
func (m *Manager) Chat(ctx context.Context, userID, message string) (*models.ChatResponse, error) {
	inst, err := m.ensureRunning(ctx, userID)
	if err != nil {
		return nil, err
	}
	payload := map[string]string{"message": message}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, inst.Endpoint+"/v1/chat", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 90 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("agent chat status %d", resp.StatusCode)
	}
	var out models.ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	_ = m.store.TouchActivity(ctx, userID)
	return &out, nil
}

// ensureRunning 保证实例运行（文档：唤醒后再路由）。
func (m *Manager) ensureRunning(ctx context.Context, userID string) (*models.Instance, error) {
	inst, err := m.store.GetInstance(ctx, userID)
	if err != nil {
		return nil, err
	}
	if inst.Status != models.StatusRunning {
		return nil, fmt.Errorf("实例状态为 %s，请先 POST /v1/users/%s/wake 唤醒", inst.Status, userID)
	}
	return inst, nil
}

// Workspace 代理查询实例工作区（可观测）。
func (m *Manager) Workspace(ctx context.Context, userID string) (map[string]any, error) {
	inst, err := m.ensureRunning(ctx, userID)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Get(inst.Endpoint + "/v1/workspace")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// TouchActivity 更新活跃度（供 idle reaper 判定）。
func (m *Manager) TouchActivity(ctx context.Context, userID string) error {
	return m.store.TouchActivity(ctx, userID)
}

func (m *Manager) SetWSConnections(ctx context.Context, userID string, n int) error {
	return m.store.SetWSConnections(ctx, userID, n)
}

func (m *Manager) failInstance(ctx context.Context, userID string, inst *models.Instance, err error) (*models.Instance, error) {
	inst.Status = models.StatusFailed
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
