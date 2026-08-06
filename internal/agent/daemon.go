package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/gorilla/websocket"
)

// Daemon 是 Agent 实例运行时：提供健康检查、配置热加载、
// REST 聊天与 WebSocket 流式会话。端口约定 18585（与文档一致）。
type Daemon struct {
	cfg      *ConfigManager
	session  *Session
	upgrader websocket.Upgrader
	started  time.Time
}

// NewDaemon 创建实例运行时。acpBin 为 QwenPaw ACP 内核可执行文件
// （qwenpaw）；留空或不可用则自动回退 mock LLM。
func NewDaemon(workspace string, configFile string, acpBin string) (*Daemon, error) {
	cfg := NewConfigManager(configFile, func(c *RuntimeConfig) {
		log.Printf("[agent] 模型配置热加载: model=%s provider=%s", c.Model, c.Provider)
	})
	session, err := NewSession(workspace, cfg, acpBin)
	if err != nil {
		return nil, fmt.Errorf("init session: %w", err)
	}
	return &Daemon{
		cfg:      cfg,
		session:  session,
		started:  time.Now(),
		upgrader: websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }},
	}, nil
}

func (d *Daemon) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", d.handleHealth)
	mux.HandleFunc("GET /v1/config", d.handleGetConfig)
	mux.HandleFunc("POST /v1/config", d.handleSetConfig)
	mux.HandleFunc("POST /v1/chat", d.handleChat)
	mux.HandleFunc("GET /v1/workspace", d.handleWorkspace)
	mux.HandleFunc("GET /v1/session", d.handleSessionWS)
	return mux
}

func (d *Daemon) handleHealth(w http.ResponseWriter, r *http.Request) {
	idx, err := d.session.MessageIndex()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"status": "error", "error": err.Error()})
		return
	}
	cfg := d.cfg.Get()
	kernel := d.session.KernelStatus()
	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "ok",
		"workspace": d.session.Workspace(),
		"messages":  idx,
		"model":     cfg.Model,
		"provider":  cfg.Provider,
		"kernel":    kernel,
		"uptime_s":  int(time.Since(d.started).Seconds()),
	})
}

func (d *Daemon) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, d.cfg.Get().Masked())
}

func (d *Daemon) handleSetConfig(w http.ResponseWriter, r *http.Request) {
	var cfg RuntimeConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	applied := d.cfg.Apply(&cfg)
	// 模型/凭证变化立即同步到 ACP 内核（凭证差异 → 重启子进程注入新环境）。
	// 异步执行避免阻塞 HTTP 响应；重启期间旧会话保持，新请求自动等待。
	go d.session.SyncConfig(context.Background())
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"model":    applied.Model,
		"provider": applied.Provider,
		"note":     "配置已热加载，无需重启实例",
	})
}

func (d *Daemon) handleChat(w http.ResponseWriter, r *http.Request) {
	var req ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	resp, err := d.session.Chat(r.Context(), &req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (d *Daemon) handleWorkspace(w http.ResponseWriter, r *http.Request) {
	entries, err := os.ReadDir(d.session.Workspace())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	files := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		info, _ := e.Info()
		files = append(files, map[string]any{
			"name":  e.Name(),
			"dir":   e.IsDir(),
			"bytes": info.Size(),
		})
	}
	history, _ := d.session.History(3)
	writeJSON(w, http.StatusOK, map[string]any{
		"workspace": d.session.Workspace(),
		"files":     files,
		"recent":    history,
		"kernel":    d.session.KernelStatus(),
	})
}

// handleSessionWS 是流式会话：客户端发送 {"type":"chat","message":...}，
// 服务端回 {"type":"delta"/"thought"/"tool_call"/"usage"/"error",...}，
// 最后 {"type":"done",...}。真实 ACP 内核逐增量转发，mock 分片模拟。
func (d *Daemon) handleSessionWS(w http.ResponseWriter, r *http.Request) {
	conn, err := d.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	for {
		var msg map[string]any
		if err := conn.ReadJSON(&msg); err != nil {
			return
		}
		if msg["type"] != "chat" {
			_ = conn.WriteJSON(map[string]any{"type": "error", "message": "unknown message type"})
			continue
		}
		text, _ := msg["message"].(string)
		sessionID, _ := msg["session_id"].(string)
		resp, err := d.session.ChatStream(context.Background(), &ChatRequest{Message: text, SessionID: sessionID},
			func(kind string, payload map[string]any) error {
				payload["type"] = kind
				return conn.WriteJSON(payload)
			})
		if err != nil {
			_ = conn.WriteJSON(map[string]any{"type": "error", "message": err.Error()})
			continue
		}
		_ = conn.WriteJSON(map[string]any{
			"type":          "done",
			"session_id":    resp.SessionID,
			"message_index": resp.MessageIndex,
			"model":         resp.Model,
			"mock":          resp.Mock,
		})
	}
}

// Close 停止 ACP 子进程（实例退出时调用，保证子进程不残留）。
func (d *Daemon) Close() {
	d.session.Close()
	d.cfg.Close()
}

func chunkText(s string, n int) []string {
	runes := []rune(s)
	var out []string
	for len(runes) > 0 {
		if len(runes) < n {
			n = len(runes)
		}
		out = append(out, string(runes[:n]))
		runes = runes[n:]
	}
	return out
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
