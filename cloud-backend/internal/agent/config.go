package agent

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// RuntimeConfig 是每个实例运行时持有的模型配置。
// 设计要点（文档 6.2）：
//   - 镜像/工作区内只有「骨架」配置（占位符），真实值由控制面运行时注入；
//   - 默认只驻留内存；--config-file 场景（K8s pod-exec 挂载临时文件）走文件热加载。
type RuntimeConfig struct {
	BaseURL  string   `json:"base_url"`
	APIKey   string   `json:"api_key,omitempty"`
	Model    string   `json:"model"`
	Models   []string `json:"models,omitempty"`
	Provider string   `json:"provider,omitempty"`
	// ToolGuardMode 控制 QwenPaw Tool Guard：
	//   "" / "default"        权限请求走策略（默认拒绝，可挂接审批流）
	//   "bypass"              信任沙箱内完全放行工具调用（慎用）
	ToolGuardMode string `json:"tool_guard_mode,omitempty"`
}

// DefaultRuntimeConfig 返回本地骨架配置（占位符 + mock 兜底），
// 保证没有外部依赖时实例依然可用。
func DefaultRuntimeConfig() *RuntimeConfig {
	return &RuntimeConfig{
		BaseURL:  "mock://",
		APIKey:   "",
		Model:    "mock-gpt-4o",
		Models:   []string{"mock-gpt-4o"},
		Provider: "mock",
	}
}

// Masked 返回不含 api_key 的副本，用于可观测/审计输出。
func (c *RuntimeConfig) Masked() *RuntimeConfig {
	cp := *c
	if cp.APIKey != "" {
		cp.APIKey = "sk-****" + cp.APIKey[len(cp.APIKey)-4:]
	}
	return &cp
}

// ConfigManager 管理运行时可热切换的模型配置。
type ConfigManager struct {
	mu         sync.RWMutex
	cfg        *RuntimeConfig
	file       string
	reloadCh   chan struct{}
	closeCh    chan struct{}
	pollEvery  time.Duration
	onReloaded func(*RuntimeConfig)
}

func NewConfigManager(file string, onReloaded func(*RuntimeConfig)) *ConfigManager {
	m := &ConfigManager{
		cfg:        DefaultRuntimeConfig(),
		file:       file,
		reloadCh:   make(chan struct{}, 1),
		closeCh:    make(chan struct{}),
		pollEvery:  2 * time.Second,
		onReloaded: onReloaded,
	}
	if file != "" {
		if loaded, err := m.loadFile(); err == nil {
			m.cfg = loaded
		}
		go m.watchFile()
	}
	return m
}

func (m *ConfigManager) Get() *RuntimeConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	cp := *m.cfg
	return &cp
}

// Apply 通过 HTTP 热加载新配置（控制面注入路径），默认仅驻留内存。
func (m *ConfigManager) Apply(cfg *RuntimeConfig) *RuntimeConfig {
	m.mu.Lock()
	if cfg.BaseURL == "" {
		cfg.BaseURL = "mock://"
	}
	if cfg.Model == "" {
		cfg.Model = m.cfg.Model
	}
	if len(cfg.Models) == 0 {
		cfg.Models = m.cfg.Models
	}
	m.cfg = cfg
	m.mu.Unlock()
	if m.onReloaded != nil {
		m.onReloaded(m.Get())
	}
	return m.Get()
}

// PersistToFile 将当前配置写入临时文件（不写入持久工作区）。
func (m *ConfigManager) PersistToFile() error {
	if m.file == "" {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	data, err := json.MarshalIndent(m.cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.file, data, 0o600)
}

func (m *ConfigManager) loadFile() (*RuntimeConfig, error) {
	data, err := os.ReadFile(m.file)
	if err != nil {
		return nil, err
	}
	var cfg RuntimeConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// watchFile 模拟文档中的「watch 文件变化热重载」：轮询 --config-file 所指文件。
// 用于 K8s pod-exec 注入临时配置文件的路径。
func (m *ConfigManager) watchFile() {
	var lastMod time.Time
	if fi, err := os.Stat(m.file); err == nil {
		lastMod = fi.ModTime()
	}
	ticker := time.NewTicker(m.pollEvery)
	defer ticker.Stop()
	for {
		select {
		case <-m.closeCh:
			return
		case <-ticker.C:
			fi, err := os.Stat(m.file)
			if err != nil {
				continue
			}
			if !fi.ModTime().After(lastMod) {
				continue
			}
			lastMod = fi.ModTime()
			cfg, err := m.loadFile()
			if err != nil {
				continue
			}
			m.Apply(cfg)
		}
	}
}

func (m *ConfigManager) Close() {
	select {
	case <-m.closeCh:
	default:
		close(m.closeCh)
	}
}
