package controlplane

import (
	"context"

	"cloude-agent/internal/models"
)

// stubGateway 是 AgentGateway 的测试桩（no-op）。
type stubGateway struct {
	registered map[string]string
}

func newStubGateway() *stubGateway {
	return &stubGateway{registered: make(map[string]string)}
}

func (g *stubGateway) Register(ctx context.Context, userID, endpoint string) error {
	g.registered[userID] = endpoint
	return nil
}
func (g *stubGateway) Unregister(ctx context.Context, userID string) error {
	delete(g.registered, userID)
	return nil
}
func (g *stubGateway) Chat(ctx context.Context, userID, message, sessionID string) (*models.ChatResponse, error) {
	return &models.ChatResponse{Reply: "echo:" + message, Mock: true, MessageIndex: 1}, nil
}
func (g *stubGateway) Workspace(ctx context.Context, userID string) (map[string]any, error) {
	return map[string]any{"workspace": "stub"}, nil
}
func (g *stubGateway) History(ctx context.Context, userID string, limit int, sessionID string) (map[string]any, error) {
	return map[string]any{"messages": []any{}, "total": 0}, nil
}
func (g *stubGateway) NewSession(ctx context.Context, userID string) (map[string]any, error) {
	return map[string]any{"session_id": "stub-session"}, nil
}
func (g *stubGateway) SessionInfo(ctx context.Context, userID string) (string, error) {
	return "stub-session", nil
}
func (g *stubGateway) Health(ctx context.Context, userID string) (map[string]any, error) {
	return map[string]any{"kernel": map[string]any{"connected": true}}, nil
}
func (g *stubGateway) SetConfig(ctx context.Context, userID string, cfg *models.ModelConfig) error {
	return nil
}
func (g *stubGateway) WSUpstreamURL(userID string) string {
	return "ws://stub-gateway/v1/agents/" + userID + "/session"
}
