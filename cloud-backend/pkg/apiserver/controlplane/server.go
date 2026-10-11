package controlplane

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"cloude-agent/pkg/api"
	"cloude-agent/pkg/db"
	"cloude-agent/pkg/sandboxruntime"
	"cloude-agent/pkg/services"
)

// Config 汇聚控制面 HTTP 层启动参数。
type Config struct {
	Listen  string
	Manager *sandboxruntime.Manager
	Reaper  *sandboxruntime.Reaper
	Review  *services.ReviewWorker
	Auth    *Authenticator
}

// Server 是控制面 HTTP 入口：REST 管理面 + WS 会话代理。
// 前端只连控制面，Pod/容器完全不暴露。
type Server struct {
	cfg       Config
	upgrader  websocket.Upgrader
	wsManager map[string]int
	wsMu      sync.Mutex
}

func NewServer(cfg Config) *Server {
	return &Server{
		cfg: cfg,
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
		wsManager: make(map[string]int),
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("POST /v1/users", s.requireAdmin(s.handleCreateUser))
	mux.HandleFunc("GET /v1/users", s.requireAdmin(s.handleListUsers))
	mux.HandleFunc("GET /v1/users/{id}", s.requireAdmin(s.handleGetUser))
	mux.HandleFunc("DELETE /v1/users/{id}", s.requireAdmin(s.handleDeleteUser))
	mux.HandleFunc("POST /v1/users/{id}/suspend", s.requireAdmin(s.handleSuspend))
	mux.HandleFunc("POST /v1/users/{id}/wake", s.requireAdmin(s.handleWake))
	mux.HandleFunc("POST /v1/users/{id}/models", s.requireAdmin(s.handleSetModels))
	mux.HandleFunc("GET /v1/users/{id}/models", s.requireAdmin(s.handleGetModels))
	mux.HandleFunc("POST /v1/users/{id}/chat", s.authorizeInstance(s.handleChat))
	mux.HandleFunc("GET /v1/users/{id}/session", s.handleSessionWS)
	mux.HandleFunc("GET /v1/users/{id}/workspace", s.requireAdmin(s.handleWorkspace))
	mux.HandleFunc("GET /v1/users/{id}/connect", s.requireAdmin(s.handleConnect))
	mux.HandleFunc("POST /v1/users/{id}/sessions", s.authorizeInstance(s.handleNewSession))
	mux.HandleFunc("GET /v1/users/{id}/history", s.authorizeInstance(s.handleHistory))
	mux.HandleFunc("POST /v1/users/{id}/reviews", s.requireAdmin(s.handleCreateReview))
	mux.HandleFunc("GET /v1/users/{id}/reviews", s.requireAdmin(s.handleListReviews))
	mux.HandleFunc("GET /v1/users/{id}/reviews/{review_id}", s.requireAdmin(s.handleGetReview))
	return api.CORS(mux)
}

func (s *Server) Run(ctx context.Context) error {
	go s.cfg.Reaper.Run(ctx)
	server := &http.Server{
		Addr:              s.cfg.Listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("[control-plane] listening on %s (namespace=%s)", s.cfg.Listen, s.cfg.Auth.Namespace())
	go func() {
		<-ctx.Done()
		_ = server.Close()
	}()
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// --- middleware ---

func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.cfg.Auth.AuthorizeAdmin(r.Header.Get("Authorization")) {
			api.Error(w, http.StatusUnauthorized, "unauthorized", "需要管理面 Bearer Token")
			return
		}
		next(w, r)
	}
}

func (s *Server) authorizeInstance(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := r.PathValue("id")
		token := api.BearerToken(r.Header.Get("Authorization"))
		if token == "" {
			token = r.URL.Query().Get("token")
		}
		if !s.cfg.Auth.AuthorizeInstance(userID, token) {
			api.Error(w, http.StatusUnauthorized, "unauthorized", "实例 token 无效（可由控制面派生）")
			return
		}
		next(w, r)
	}
}

// --- handlers ---

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	api.JSON(w, http.StatusOK, map[string]any{
		"status":    "ok",
		"component": "control-plane",
		"time":      time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" {
		api.Error(w, http.StatusBadRequest, "bad_request", "请求体需要 {\"id\": \"<userID>\"}")
		return
	}
	inst, err := s.cfg.Manager.Create(r.Context(), req.ID)
	if err != nil {
		api.Error(w, http.StatusConflict, "create_failed", err.Error())
		return
	}
	api.JSON(w, http.StatusOK, inst)
}

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	instances, err := s.cfg.Manager.List(r.Context())
	if err != nil {
		api.Error(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	api.JSON(w, http.StatusOK, map[string]any{"instances": instances, "total": len(instances)})
}

func (s *Server) handleGetUser(w http.ResponseWriter, r *http.Request) {
	inst, err := s.cfg.Manager.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		api.Error(w, http.StatusNotFound, "not_found", "实例不存在")
		return
	}
	api.JSON(w, http.StatusOK, inst)
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("id")
	if err := s.cfg.Manager.Delete(r.Context(), userID); err != nil {
		api.Error(w, http.StatusInternalServerError, "delete_failed", err.Error())
		return
	}
	api.JSON(w, http.StatusOK, map[string]any{"ok": true, "user_id": userID})
}

func (s *Server) handleSuspend(w http.ResponseWriter, r *http.Request) {
	inst, err := s.cfg.Manager.Suspend(r.Context(), r.PathValue("id"))
	if err != nil {
		api.Error(w, http.StatusConflict, "suspend_failed", err.Error())
		return
	}
	api.JSON(w, http.StatusOK, inst)
}

func (s *Server) handleWake(w http.ResponseWriter, r *http.Request) {
	inst, err := s.cfg.Manager.Wake(r.Context(), r.PathValue("id"))
	if err != nil {
		api.Error(w, http.StatusConflict, "wake_failed", err.Error())
		return
	}
	api.JSON(w, http.StatusOK, inst)
}

func (s *Server) handleSetModels(w http.ResponseWriter, r *http.Request) {
	var cfg db.ModelConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil || cfg.Model == "" {
		api.Error(w, http.StatusBadRequest, "bad_request", "需要 {\"base_url\",\"api_key\",\"model\",\"provider\"}")
		return
	}
	inst, err := s.cfg.Manager.SetModelConfig(r.Context(), r.PathValue("id"), &cfg)
	if err != nil {
		api.Error(w, http.StatusConflict, "model_switch_failed", err.Error())
		return
	}
	api.JSON(w, http.StatusOK, map[string]any{"ok": true, "instance": inst, "model": cfg.Model})
}

func (s *Server) handleGetModels(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.cfg.Manager.GetModelConfig(r.Context(), r.PathValue("id"))
	if err != nil {
		api.Error(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	api.JSON(w, http.StatusOK, cfg)
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Message == "" {
		api.Error(w, http.StatusBadRequest, "bad_request", "需要 {\"message\": \"...\"}")
		return
	}
	resp, err := s.cfg.Manager.Chat(r.Context(), r.PathValue("id"), req.Message)
	if err != nil {
		api.Error(w, http.StatusConflict, "chat_failed", err.Error())
		return
	}
	api.JSON(w, http.StatusOK, resp)
}

func (s *Server) handleWorkspace(w http.ResponseWriter, r *http.Request) {
	out, err := s.cfg.Manager.Workspace(r.Context(), r.PathValue("id"))
	if err != nil {
		api.Error(w, http.StatusConflict, "workspace_failed", err.Error())
		return
	}
	api.JSON(w, http.StatusOK, out)
}

func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("id")
	info, err := s.cfg.Manager.Connect(r.Context(), userID)
	if err != nil {
		api.Error(w, http.StatusConflict, "connect_failed", err.Error())
		return
	}
	token := s.cfg.Auth.DerivedToken(userID)
	scheme := "ws"
	if r.TLS != nil {
		scheme = "wss"
	}
	wsURL := scheme + "://" + r.Host + "/v1/users/" + userID + "/session?token=" + token
	info["token"] = token
	info["ws_url"] = wsURL
	api.JSON(w, http.StatusOK, info)
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	out, err := s.cfg.Manager.History(r.Context(), r.PathValue("id"), limit, r.URL.Query().Get("session_id"))
	if err != nil {
		api.Error(w, http.StatusConflict, "history_failed", err.Error())
		return
	}
	api.JSON(w, http.StatusOK, out)
}

func (s *Server) handleNewSession(w http.ResponseWriter, r *http.Request) {
	out, err := s.cfg.Manager.NewSession(r.Context(), r.PathValue("id"))
	if err != nil {
		api.Error(w, http.StatusConflict, "new_session_failed", err.Error())
		return
	}
	out["user_id"] = r.PathValue("id")
	api.JSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateReview(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Repo     string `json:"repo"`
		PRNumber int    `json:"pr_number"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Repo == "" {
		api.Error(w, http.StatusBadRequest, "bad_request", "需要 {\"repo\": \"...\"}")
		return
	}
	review, err := s.cfg.Review.Submit(r.Context(), r.PathValue("id"), req.Repo, req.PRNumber)
	if err != nil {
		api.Error(w, http.StatusInternalServerError, "review_failed", err.Error())
		return
	}
	api.JSON(w, http.StatusAccepted, review)
}

func (s *Server) handleListReviews(w http.ResponseWriter, r *http.Request) {
	reviews, err := s.cfg.Review.ListReviews(r.Context(), r.PathValue("id"))
	if err != nil {
		api.Error(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	api.JSON(w, http.StatusOK, map[string]any{"reviews": reviews})
}

func (s *Server) handleGetReview(w http.ResponseWriter, r *http.Request) {
	review, err := s.cfg.Review.GetReview(r.Context(), r.PathValue("id"), r.PathValue("review_id"))
	if err != nil {
		api.Error(w, http.StatusNotFound, "not_found", "评审记录不存在")
		return
	}
	api.JSON(w, http.StatusOK, review)
}

// handleSessionWS 是 WebSocket 转发：前端连控制面，控制面按 userID 路由到实例，双向透传。
func (s *Server) handleSessionWS(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("id")
	token := r.URL.Query().Get("token")
	if !s.cfg.Auth.AuthorizeInstance(userID, token) {
		api.Error(w, http.StatusUnauthorized, "unauthorized", "实例 token 无效")
		return
	}
	inst, err := s.cfg.Manager.Get(r.Context(), userID)
	if err != nil || inst.Status != db.StatusRunning {
		api.Error(w, http.StatusConflict, "not_running", "实例未运行，请先唤醒")
		return
	}

	clientConn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer clientConn.Close()

	agentURL := s.cfg.Manager.GatewayWSURL(userID)
	if agentURL == "" {
		_ = clientConn.WriteJSON(map[string]any{"type": "error", "message": "未配置数据面网关（--gateway）"})
		return
	}
	agentConn, _, err := websocket.DefaultDialer.Dial(agentURL, nil)
	if err != nil {
		_ = clientConn.WriteJSON(map[string]any{"type": "error", "message": "无法连接实例: " + err.Error()})
		return
	}
	defer agentConn.Close()

	s.addWS(userID, 1)
	defer s.addWS(userID, -1)
	_ = s.cfg.Manager.TouchActivity(r.Context(), userID)

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
		_ = s.cfg.Manager.TouchActivity(r.Context(), userID)
		if err := agentConn.WriteMessage(websocket.TextMessage, data); err != nil {
			break
		}
	}
	<-done
}

func (s *Server) addWS(userID string, delta int) {
	s.wsMu.Lock()
	defer s.wsMu.Unlock()
	s.wsManager[userID] += delta
	if s.wsManager[userID] <= 0 {
		delete(s.wsManager, userID)
	}
	_ = s.cfg.Manager.SetWSConnections(context.Background(), userID, s.wsManager[userID])
}
