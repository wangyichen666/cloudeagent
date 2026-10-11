package engine

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// ConfigManager 管理运行时可热切换的模型配置。
// 默认仅驻留内存；--config-file 场景走文件轮询热加载。
type ConfigManager struct {
	mu         sync.RWMutex
	cfg        *Config
	file       string
	reloadCh   chan struct{}
	closeCh    chan struct{}
	pollEvery  time.Duration
	onReloaded func(*Config)
}

// NewConfigManager 创建配置管理器。file 为空则纯内存。
func NewConfigManager(file string, onReloaded func(*Config)) *ConfigManager {
	m := &ConfigManager{
		cfg:        DefaultConfig(),
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

// DefaultConfig 返回本地骨架配置（mock 兜底）。
func DefaultConfig() *Config {
	return &Config{
		BaseURL:  "mock://",
		APIKey:   "",
		Model:    "mock-gpt-4o",
		Models:   []string{"mock-gpt-4o"},
		Provider: "mock",
	}
}

// Get 返回当前配置的副本。
func (m *ConfigManager) Get() *Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	cp := *m.cfg
	return &cp
}

// Apply 热加载新配置（控制面注入路径）。
func (m *ConfigManager) Apply(cfg *Config) *Config {
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

// Masked 返回不含 api_key 的副本。
func (c *Config) Masked() *Config {
	cp := *c
	if cp.APIKey != "" {
		cp.APIKey = "sk-****" + cp.APIKey[len(cp.APIKey)-4:]
	}
	return &cp
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

func (m *ConfigManager) loadFile() (*Config, error) {
	data, err := os.ReadFile(m.file)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

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
