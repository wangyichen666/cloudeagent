// Package agentclient 是 gateway 到 agent-runtime 的内部客户端：
// 只负责按 endpoint 转发 REST/WS，不承载业务逻辑。
package agentclient

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

	"github.com/gorilla/websocket"
)

// Client 复用连接池转发到 agent-runtime 的 REST 接口。
type Client struct {
	http *http.Client
}

func New() *Client {
	return &Client{
		http: &http.Client{Timeout: 120 * time.Second},
	}
}

// Request 把 gateway 收到的请求原样转发给 agent-runtime 并回传响应体。
func (c *Client) Request(
	ctx context.Context,
	method, endpoint, path string,
	query url.Values,
	body any,
) (int, []byte, error) {
	u := strings.TrimSuffix(endpoint, "/") + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, reader)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("agent %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, data, nil
}

// DialWS 拨号 agent-runtime 的 WebSocket 会话接口。
func DialWS(ctx context.Context, endpoint, path string) (*websocket.Conn, *http.Response, error) {
	u := "ws" + strings.TrimPrefix(strings.TrimSuffix(endpoint, "/"), "http") + path
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	return dialer.DialContext(ctx, u, nil)
}
