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

func NewDaemon(workspace string, configFile string) (*Daemon, error) {
	cfg := NewConfigManager(configFile, func(c *RuntimeConfig) {
		log.Printf("[agent] 模型配置热加载: model=%s provider=%s", c.Model, c.Provider)
	})
	session, err := NewSession(workspace, cfg)
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
	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "ok",
		"workspace": d.session.Workspace(),
		"messages":  idx,
		"model":     cfg.Model,
		"provider":  cfg.Provider,
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
	})
}

// handleSessionWS 是流式会话：客户端发送 {"type":"chat","message":...}，
// 服务端回 {"type":"delta","text":...}（可多条），最后 {"type":"done",...}。
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
		resp, err := d.session.Chat(context.Background(), &ChatRequest{Message: text, SessionID: sessionID})
		if err != nil {
			_ = conn.WriteJSON(map[string]any{"type": "error", "message": err.Error()})
			continue
		}
		// mock 模式分片推送，演示流式；真实模式单条 delta。
		chunks := chunkText(resp.Reply, 24)
		if len(chunks) <= 1 {
			chunks = []string{resp.Reply}
		}
		for _, chunk := range chunks {
			_ = conn.WriteJSON(map[string]any{"type": "delta", "text": chunk})
			time.Sleep(15 * time.Millisecond)
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
