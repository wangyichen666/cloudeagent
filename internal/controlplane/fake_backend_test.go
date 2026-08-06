package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"

	"cloude-agent/internal/backend"
	"cloude-agent/internal/models"
)

// fakeBackend 是数据面的替身：不启动真实进程，记录调用序列供断言。
type fakeBackend struct {
	mu        sync.Mutex
	endpoints map[string]string
	calls     []string
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{endpoints: make(map[string]string)}
}

func (f *fakeBackend) Name() string { return "fake" }

func (f *fakeBackend) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeBackend) setEndpoint(userID, endpoint string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.endpoints[userID] = endpoint
}

func (f *fakeBackend) Create(ctx context.Context, userID string) (*backend.Info, error) {
	f.record("create:" + userID)
	return &backend.Info{Workspace: "/fake/" + userID, Endpoint: f.endpoints[userID], Port: 1}, nil
}

func (f *fakeBackend) Start(ctx context.Context, userID string) (*backend.Info, error) {
	f.record("start:" + userID)
	return &backend.Info{Workspace: "/fake/" + userID, Endpoint: f.endpoints[userID], Port: 1}, nil
}

func (f *fakeBackend) Stop(ctx context.Context, userID string) error {
	f.record("stop:" + userID)
	return nil
}

func (f *fakeBackend) Delete(ctx context.Context, userID string) error {
	f.record("delete:" + userID)
	return nil
}

func (f *fakeBackend) hasCall(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == prefix {
			return true
		}
	}
	return false
}

// fakeAgentServer 是实例运行时的替身：health/config/chat。
func fakeAgentServer(t interface{ Helper() }) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
	})
	mux.HandleFunc("POST /v1/config", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /v1/chat", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Message string `json:"message"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		_ = json.NewEncoder(w).Encode(models.ChatResponse{
			Reply:        "echo:" + req.Message,
			Model:        "fake-model",
			Provider:     "fake",
			Mock:         true,
			MessageIndex: 1,
			SessionID:    "fake-session",
		})
	})
	return httptest.NewServer(mux)
}
