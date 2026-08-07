package controlplane

import (
	"context"

	"cloude-agent/internal/models"
)

// SeatService 抽象「身份/席位服务」（文档 2.1 外部依赖，可 mock）：
// 控制面透传用户身份，换取 {baseURL, apiKey, 可用模型列表}，运行时注入实例。
type SeatService interface {
	Resolve(ctx context.Context, userID string) (*models.ModelConfig, error)
}

// MockSeatService 是本地实现的席位服务：返回固定配置，
// 并支持通过控制面 POST /models 做按用户覆盖（覆盖值存热缓存，休眠后丢弃）。
type MockSeatService struct {
	Model    string
	Provider string
	BaseURL  string
	APIKey   string
	Models   []string
}

func NewMockSeatService() *MockSeatService {
	return &MockSeatService{
		Model:    "mock-gpt-4o",
		Provider: "seat-service",
		BaseURL:  "mock://",
		APIKey:   "sk-local-mock-seat",
		Models:   []string{"mock-gpt-4o", "mock-claude-sonnet"},
	}
}

func (m *MockSeatService) Resolve(ctx context.Context, userID string) (*models.ModelConfig, error) {
	return &models.ModelConfig{
		BaseURL:  m.BaseURL,
		APIKey:   m.APIKey,
		Model:    m.Model,
		Models:   append([]string(nil), m.Models...),
		Provider: m.Provider,
	}, nil
}

func (m *MockSeatService) Name() string { return "mock-seat" }
