package domain

// ModelConfig 是运行时动态注入的模型配置。
// 关键设计决策：base_url/api_key 只在内存/临时配置，
// 休眠后丢弃，唤醒时由控制面重新注入；绝不落入 PostgreSQL/PVC/代码仓库。
type ModelConfig struct {
	BaseURL  string   `json:"base_url"`
	APIKey   string   `json:"api_key,omitempty"`
	Model    string   `json:"model"`
	Models   []string `json:"models,omitempty"`
	Provider string   `json:"provider,omitempty"`
}

// Masked 返回不含完整 api_key 的副本，用于可观测/审计输出。
func (c *ModelConfig) Masked() *ModelConfig {
	cp := *c
	if cp.APIKey != "" {
		if len(cp.APIKey) >= 4 {
			cp.APIKey = "sk-****" + cp.APIKey[len(cp.APIKey)-4:]
		} else {
			cp.APIKey = "****"
		}
	}
	return &cp
}
