package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ChatRequest 是会话输入。
type ChatRequest struct {
	Message   string `json:"message"`
	SessionID string `json:"session_id,omitempty"`
}

// LLM 是模型访问抽象：本地 mock 或任意 OpenAI 兼容端点。
type LLM interface {
	Complete(ctx context.Context, cfg *RuntimeConfig, userMessage string) (string, error)
	Name() string
}

// MockLLM 无需任何网络/外部依赖，用于本地演示。
type MockLLM struct{}

func (MockLLM) Name() string { return "mock" }

func (MockLLM) Complete(ctx context.Context, cfg *RuntimeConfig, userMessage string) (string, error) {
	lines := []string{
		fmt.Sprintf("【mock 回复 · 模型 %s · provider %s】", cfg.Model, cfg.Provider),
		fmt.Sprintf("你刚才说：%s", userMessage),
		"（这是本地 mock LLM 的输出。设置真实 base_url/apiKey 后即可切换到真实模型，配置可热切换，无需重启实例。）",
	}
	return strings.Join(lines, "\n"), nil
}

// OpenAIClient 兼容任何 OpenAI /v1/chat/completions 端点。
type OpenAIClient struct {
	http    *http.Client
	timeout time.Duration
}

func NewOpenAIClient(timeout time.Duration) *OpenAIClient {
	return &OpenAIClient{http: &http.Client{Timeout: timeout}, timeout: timeout}
}

func (o *OpenAIClient) Name() string { return "openai-compatible" }

func (o *OpenAIClient) Complete(ctx context.Context, cfg *RuntimeConfig, userMessage string) (string, error) {
	url := strings.TrimSuffix(cfg.BaseURL, "/") + "/v1/chat/completions"
	payload := map[string]any{
		"model": cfg.Model,
		"messages": []map[string]string{
			{"role": "user", "content": userMessage},
		},
		"stream": false,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}
	resp, err := o.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("llm request: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("llm status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return "", fmt.Errorf("llm decode: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("llm returned no choices")
	}
	return parsed.Choices[0].Message.Content, nil
}

// NewLLM 按配置选择实现。
func NewLLM(cfg *RuntimeConfig) LLM {
	if cfg.BaseURL == "" || cfg.BaseURL == "mock://" || strings.HasPrefix(cfg.BaseURL, "mock://") {
		return MockLLM{}
	}
	return NewOpenAIClient(60 * time.Second)
}
