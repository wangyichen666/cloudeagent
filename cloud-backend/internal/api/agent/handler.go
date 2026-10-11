// Package agent 是 Agent 实例运行时的 HTTP/WS 接口层。
// 只负责协议适配（路由、序列化、WebSocket），业务逻辑全部委托给
// internal/agentruntime 应用层。
package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/gorilla/websocket"

	"cloude-agent/internal/agentruntime"
	"cloude-agent/internal/domain"
	"cloude-agent/internal/engine"
)

// Handler 为实例运行时创建 HTTP handler。端口约定 18585。
func Handler(d *agentruntime.Daemon) http.Handler {
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handleHealth(d))
	mux.HandleFunc("GET /v1/config", handleGetConfig(d))
	mux.HandleFunc("POST /v1/config", handleSetConfig(d))
	mux.HandleFunc("POST /v1/chat", handleChat(d))
	mux.HandleFunc("GET /v1/workspace", handleWorkspace(d))
	mux.HandleFunc("GET /v1/history", handleHistory(d))
	mux.HandleFunc("POST /v1/session/new", handleNewSession(d))
	mux.HandleFunc("GET /v1/session/info", handleGetSession(d))
	mux.HandleFunc("GET /v1/session", handleSessionWS(d, upgrader))
	return mux
}

func handleHealth(d *agentruntime.Daemon) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		out, err := d.Health()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"status": "error", "error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func handleGetConfig(d *agentruntime.Daemon) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, d.GetConfig())
	}
}

func handleSetConfig(d *agentruntime.Daemon) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var cfg engine.Config
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		applied := d.SetConfig(&cfg)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":       true,
			"model":    applied.Model,
			"provider": applied.Provider,
			"note":     "配置已热加载，无需重启实例",
		})
	}
}

func handleChat(d *agentruntime.Daemon) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req domain.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		resp, err := d.Chat(r.Context(), &req)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

func handleWorkspace(d *agentruntime.Daemon) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		out, err := d.Workspace()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func handleHistory(d *agentruntime.Daemon) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit := 100
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				limit = n
			}
		}
		out, err := d.History(limit, r.URL.Query().Get("session_id"))
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func handleNewSession(d *agentruntime.Daemon) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionID, err := d.NewSession(r.Context())
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "session_id": sessionID})
	}
}

func handleGetSession(d *agentruntime.Daemon) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"session_id": d.ActiveSession()})
	}
}

func handleSessionWS(d *agentruntime.Daemon, upgrader websocket.Upgrader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
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
			resp, err := d.ChatStream(context.Background(), &domain.ChatRequest{Message: text, SessionID: sessionID},
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
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
