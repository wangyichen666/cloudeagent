// Package gateway 是 cloud-backend 调用 cloud-gateway（数据面网关）的 thin client。
// backend 的业务代码不再直连 Pod：创建/唤醒后注册路由，对话/会话/历史/配置
// 全部经 gateway 转发；gateway 才是唯一知道「agent 在哪」的系统。
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"cloude-agent/internal/models"
)

// Client 封装 gateway 的稳定 REST API。
type Client struct {
	base string
	http *http.Client
}

func New(base string) *Client {
	return &Client{
		base: strings.TrimSuffix(base, "/"),
		http: &http.Client{Timeout: 120 * time.Second},
	}
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any) ([]byte, error) {
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gateway %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("gateway %s %s 状态 %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return data, nil
}

// Register 把实例 endpoint 注册到 gateway（创建/唤醒后调用）。
func (c *Client) Register(ctx context.Context, userID, endpoint string) error {
	_, err := c.do(ctx, http.MethodPost, "/v1/registry/"+url.PathEscape(userID), nil,
		map[string]any{"endpoint": endpoint})
	return err
}

// Unregister 注销实例路由（删除实例时调用）。
func (c *Client) Unregister(ctx context.Context, userID string) error {
	_, err := c.do(ctx, http.MethodDelete, "/v1/registry/"+url.PathEscape(userID), nil, nil)
	return err
}

// Chat 经 gateway 转发同步对话。
func (c *Client) Chat(ctx context.Context, userID, message, sessionID string) (*models.ChatResponse, error) {
	data, err := c.do(ctx, http.MethodPost, "/v1/agents/"+url.PathEscape(userID)+"/chat", nil,
		map[string]any{"message": message, "session_id": sessionID})
	if err != nil {
		return nil, err
	}
	var out models.ChatResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Workspace 经 gateway 读取实例工作区摘要。
func (c *Client) Workspace(ctx context.Context, userID string) (map[string]any, error) {
	data, err := c.do(ctx, http.MethodGet, "/v1/agents/"+url.PathEscape(userID)+"/workspace", nil, nil)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// History 经 gateway 读取对话历史（可按 session_id 过滤）。
func (c *Client) History(ctx context.Context, userID string, limit int, sessionID string) (map[string]any, error) {
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", fmt.Sprint(limit))
	}
	if sessionID != "" {
		q.Set("session_id", sessionID)
	}
	data, err := c.do(ctx, http.MethodGet, "/v1/agents/"+url.PathEscape(userID)+"/history", q, nil)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// NewSession 经 gateway 让 Pod 内 agent 真实开启新会话。
func (c *Client) NewSession(ctx context.Context, userID string) (map[string]any, error) {
	data, err := c.do(ctx, http.MethodPost, "/v1/agents/"+url.PathEscape(userID)+"/sessions", nil, map[string]any{})
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// SessionInfo 经 gateway 读取当前活动会话 id。
func (c *Client) SessionInfo(ctx context.Context, userID string) (string, error) {
	data, err := c.do(ctx, http.MethodGet, "/v1/agents/"+url.PathEscape(userID)+"/session/info", nil, nil)
	if err != nil {
		return "", err
	}
	var out struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", err
	}
	return out.SessionID, nil
}

// Health 经 gateway 读取 agent 健康/内核状态。
func (c *Client) Health(ctx context.Context, userID string) (map[string]any, error) {
	data, err := c.do(ctx, http.MethodGet, "/v1/agents/"+url.PathEscape(userID)+"/health", nil, nil)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// SetConfig 经 gateway 向实例注入模型配置（凭证仅内存，不落盘）。
func (c *Client) SetConfig(ctx context.Context, userID string, cfg *models.ModelConfig) error {
	_, err := c.do(ctx, http.MethodPost, "/v1/agents/"+url.PathEscape(userID)+"/config", nil, cfg)
	return err
}

// WSUpstreamURL 返回 backend 转发流式会话时连接 gateway 的 WS 地址。
func (c *Client) WSUpstreamURL(userID string) string {
	u := "ws" + strings.TrimPrefix(c.base, "http") + "/v1/agents/" + url.PathEscape(userID) + "/session"
	return u
}
