package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"

	"cloude-agent/internal/domain"
	"cloude-agent/internal/sandbox"
)

// FakeSandbox 是沙箱提供方的测试替身：不启动真实进程，记录调用序列供断言。
type FakeSandbox struct {
	mu        sync.Mutex
	endpoints map[string]string
	calls     []string
}

func NewFakeSandbox() *FakeSandbox {
	return &FakeSandbox{endpoints: make(map[string]string)}
}

func (f *FakeSandbox) Name() string { return "fake" }

func (f *FakeSandbox) SetEndpoint(userID, endpoint string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.endpoints[userID] = endpoint
}

func (f *FakeSandbox) Create(ctx context.Context, userID string) (*sandbox.Info, error) {
	f.record("create:" + userID)
	return &sandbox.Info{Workspace: "/fake/" + userID, Endpoint: f.endpoints[userID], Port: 1}, nil
}

func (f *FakeSandbox) Start(ctx context.Context, userID string) (*sandbox.Info, error) {
	f.record("start:" + userID)
	return &sandbox.Info{Workspace: "/fake/" + userID, Endpoint: f.endpoints[userID], Port: 1}, nil
}

func (f *FakeSandbox) Stop(ctx context.Context, userID string) error {
	f.record("stop:" + userID)
	return nil
}

func (f *FakeSandbox) Delete(ctx context.Context, userID string) error {
	f.record("delete:" + userID)
	return nil
}

func (f *FakeSandbox) HasCall(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == prefix {
			return true
		}
	}
	return false
}

func (f *FakeSandbox) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

// FakeAgentServer 是实例运行时的 HTTP 替身：health/config/chat。
func FakeAgentServer(t interface{ Helper() }) *httptest.Server {
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
		_ = json.NewEncoder(w).Encode(domain.ChatResponse{
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

// StubGateway 是 AgentGateway 的测试桩（no-op）。
type StubGateway struct {
	registered map[string]string
}

func NewStubGateway() *StubGateway {
	return &StubGateway{registered: make(map[string]string)}
}

func (g *StubGateway) Register(ctx context.Context, userID, endpoint string) error {
	g.registered[userID] = endpoint
	return nil
}
func (g *StubGateway) Unregister(ctx context.Context, userID string) error {
	delete(g.registered, userID)
	return nil
}
func (g *StubGateway) Chat(ctx context.Context, userID, message, sessionID string) (*domain.ChatResponse, error) {
	return &domain.ChatResponse{Reply: "echo:" + message, Mock: true, MessageIndex: 1}, nil
}
func (g *StubGateway) Workspace(ctx context.Context, userID string) (map[string]any, error) {
	return map[string]any{"workspace": "stub"}, nil
}
func (g *StubGateway) History(ctx context.Context, userID string, limit int, sessionID string) (map[string]any, error) {
	return map[string]any{"messages": []any{}, "total": 0}, nil
}
func (g *StubGateway) NewSession(ctx context.Context, userID string) (map[string]any, error) {
	return map[string]any{"session_id": "stub-session"}, nil
}
func (g *StubGateway) SessionInfo(ctx context.Context, userID string) (string, error) {
	return "stub-session", nil
}
func (g *StubGateway) Health(ctx context.Context, userID string) (map[string]any, error) {
	return map[string]any{"kernel": map[string]any{"connected": true}}, nil
}
func (g *StubGateway) SetConfig(ctx context.Context, userID string, cfg *domain.ModelConfig) error {
	return nil
}
func (g *StubGateway) WSUpstreamURL(userID string) string {
	return "ws://stub-gateway/v1/agents/" + userID + "/session"
}
