// Package server 实现 gateway 对 backend 暴露的稳定 API：
// 路由注册 + 到 agent-runtime 的 REST/WS 转发。
package server

import (
	"encoding/json"
	"log"
	"net/http"
	"net/url"

	"github.com/gorilla/websocket"

	"cloud-gateway/internal/agentclient"
	"cloud-gateway/internal/registry"
)

// Server 持有路由表与 agent 客户端。
type Server struct {
	reg    *registry.Registry
	agent  *agentclient.Client
	upgrader websocket.Upgrader
}

func New(reg *registry.Registry, agent *agentclient.Client) *Server {
	return &Server{
		reg:   reg,
		agent: agent,
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
	}
}

// Handler 返回 gateway 的 HTTP 路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// 路由注册（backend 调用）
	mux.HandleFunc("POST /v1/registry/{userID}", s.handleRegister)
	mux.HandleFunc("DELETE /v1/registry/{userID}", s.handleUnregister)
	mux.HandleFunc("GET /v1/registry", s.handleListRegistry)
	// 业务转发（backend 调用）
	mux.HandleFunc("GET /v1/agents/{userID}/health", s.proxyGET("/health", false))
	mux.HandleFunc("POST /v1/agents/{userID}/chat", s.proxyPOST("/v1/chat"))
	mux.HandleFunc("GET /v1/agents/{userID}/workspace", s.proxyGET("/v1/workspace", false))
	mux.HandleFunc("GET /v1/agents/{userID}/history", s.proxyGET("/v1/history", true))
	mux.HandleFunc("POST /v1/agents/{userID}/sessions", s.proxyPOST("/v1/session/new"))
	mux.HandleFunc("GET /v1/agents/{userID}/session/info", s.proxyGET("/v1/session/info", false))
	mux.HandleFunc("POST /v1/agents/{userID}/config", s.proxyPOST("/v1/config"))
	mux.HandleFunc("GET /v1/agents/{userID}/session", s.handleWSRelay)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "component": "cloud-gateway"})
	})
	return s.cors(mux)
}

func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- 路由注册 ---

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("userID")
	var req struct {
		Endpoint string `json:"endpoint"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Endpoint == "" {
		writeError(w, http.StatusBadRequest, "需要 {\"endpoint\": \"http://...\"}")
		return
	}
	s.reg.Register(userID, req.Endpoint)
	log.Printf("[gateway] 注册 %s -> %s", userID, req.Endpoint)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "user_id": userID})
}

func (s *Server) handleUnregister(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("userID")
	s.reg.Unregister(userID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "user_id": userID})
}

func (s *Server) handleListRegistry(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"agents": s.reg.Snapshot()})
}

// --- REST 转发 ---

func (s *Server) proxyGET(path string, withQuery bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := r.PathValue("userID")
		endpoint, ok := s.reg.Resolve(userID)
		if !ok {
			writeError(w, http.StatusNotFound, "agent 未注册（backend 尚未创建/唤醒该实例）")
			return
		}
		var q url.Values
		if withQuery {
			q = r.URL.Query()
		}
		status, data, err := s.agent.Request(r.Context(), http.MethodGet, endpoint, path, q, nil)
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(data)
	}
}

func (s *Server) proxyPOST(path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := r.PathValue("userID")
		endpoint, ok := s.reg.Resolve(userID)
		if !ok {
			writeError(w, http.StatusNotFound, "agent 未注册（backend 尚未创建/唤醒该实例）")
			return
		}
		var body any
		if r.ContentLength > 0 {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		status, data, err := s.agent.Request(r.Context(), http.MethodPost, endpoint, path, nil, body)
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(data)
	}
}

// --- WS 隧道 ---

func (s *Server) handleWSRelay(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("userID")
	endpoint, ok := s.reg.Resolve(userID)
	if !ok {
		writeError(w, http.StatusNotFound, "agent 未注册（backend 尚未创建/唤醒该实例）")
		return
	}
	clientConn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer clientConn.Close()

	agentConn, _, err := agentclient.DialWS(r.Context(), endpoint, "/v1/session")
	if err != nil {
		_ = clientConn.WriteJSON(map[string]any{"type": "error", "message": "无法连接实例: " + err.Error()})
		return
	}
	defer agentConn.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			_, data, err := agentConn.ReadMessage()
			if err != nil {
				return
			}
			if err := clientConn.WriteMessage(websocket.TextMessage, data); err != nil {
				return
			}
		}
	}()
	for {
		_, data, err := clientConn.ReadMessage()
		if err != nil {
			_ = agentConn.WriteMessage(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, "client closed"))
			break
		}
		if err := agentConn.WriteMessage(websocket.TextMessage, data); err != nil {
			break
		}
	}
	<-done
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, message string) {
	writeJSON(w, code, map[string]any{"error": map[string]any{"message": message}})
}
