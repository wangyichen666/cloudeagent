package agent

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/gorilla/websocket"

	"cloude-agent/internal/engine"
)

// Daemon 是 Agent 实例运行时：提供健康检查、配置热加载、
// REST 聊天与 WebSocket 流式会话。端口约定 18585。
type Daemon struct {
	cfg      *engine.ConfigManager
	session  *Session
	upgrader websocket.Upgrader
	started  time.Time
}

// NewDaemon 创建实例运行时。
// engineName 指定首选引擎（如 "qwenpaw"）；为空则默认 qwenpaw，不可用回退 mock。
func NewDaemon(workspace, configFile, engineName string) (*Daemon, error) {
	cfg := engine.NewConfigManager(configFile, func(c *engine.Config) {
		log.Printf("[agent] 模型配置热加载: model=%s provider=%s", c.Model, c.Provider)
	})
	session, err := NewSession(workspace, engineName, cfg)
	if err != nil {
		return nil, err
	}
	return &Daemon{
		cfg:     cfg,
		session: session,
		started: time.Now(),
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
	mux.HandleFunc("GET /v1/history", d.handleHistory)
	mux.HandleFunc("POST /v1/session/new", d.handleNewSession)
	mux.HandleFunc("GET /v1/session/info", d.handleGetSession)
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
	var cfg engine.Config
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	applied := d.cfg.Apply(&cfg)
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
	history, _ := d.session.History(3, "")
	writeJSON(w, http.StatusOK, map[string]any{
		"workspace": d.session.Workspace(),
		"files":     files,
		"recent":    history,
		"kernel":    d.session.KernelStatus(),
	})
}

func (d *Daemon) handleHistory(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			limit = n
		}
	}
	entries, err := d.session.History(limit, r.URL.Query().Get("session_id"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"messages": entries,
		"total":    len(entries),
		"path":     d.session.LogPath(),
	})
}

func (d *Daemon) handleNewSession(w http.ResponseWriter, r *http.Request) {
	sessionID, err := d.session.NewSession(r.Context())
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"session_id": sessionID,
	})
}

func (d *Daemon) handleGetSession(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": d.session.ActiveSession(),
	})
}

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

// Close 停止引擎（实例退出时调用）。
func (d *Daemon) Close() {
	d.session.Close()
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
