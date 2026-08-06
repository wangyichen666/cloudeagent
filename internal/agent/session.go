package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"cloude-agent/internal/models"
)

// Session 管理每个实例的工作区会话：
// 对话历史持久化在 workspace/.agent/conversation.jsonl，
// 休眠再唤醒后数据不丢（对应文档「有状态」诉求，PVC 语义的本地实现）。
type Session struct {
	workspace string
	cfg       *ConfigManager
	mu        sync.Mutex
}

func NewSession(workspace string, cfg *ConfigManager) (*Session, error) {
	dir := filepath.Join(workspace, ".agent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Session{workspace: workspace, cfg: cfg}, nil
}

func (s *Session) LogPath() string {
	return filepath.Join(s.workspace, ".agent", "conversation.jsonl")
}

// MessageIndex 返回已持久化的消息条数（从 1 开始计数）。
func (s *Session) MessageIndex() (int, error) {
	data, err := os.ReadFile(s.LogPath())
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	return strings.Count(string(data), "\n"), nil
}

func (s *Session) appendLog(role, content, model string) (int, error) {
	idx, err := s.MessageIndex()
	if err != nil {
		return 0, err
	}
	idx++
	entry := map[string]any{
		"ts":      time.Now().UTC().Format(time.RFC3339),
		"role":    role,
		"content": content,
		"model":   model,
		"index":   idx,
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return 0, err
	}
	f, err := os.OpenFile(s.LogPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return 0, err
	}
	return idx, nil
}

// Chat 同步执行一次会话请求（控制面 REST 路径）。
func (s *Session) Chat(ctx context.Context, req *ChatRequest) (*models.ChatResponse, error) {
	cfg := s.cfg.Get()
	idx, err := s.appendLog("user", req.Message, cfg.Model)
	if err != nil {
		return nil, fmt.Errorf("persist user message: %w", err)
	}
	// 每次请求时按当前配置创建 LLM，保证模型热切换即时生效。
	llm := NewLLM(cfg)
	reply, err := llm.Complete(ctx, cfg, req.Message)
	if err != nil {
		// 失败也记录，便于观察与自愈排查。
		_, _ = s.appendLog("error", err.Error(), cfg.Model)
		return nil, err
	}
	_, _ = s.appendLog("assistant", reply, cfg.Model)
	return &models.ChatResponse{
		Reply:        reply,
		Model:        cfg.Model,
		Provider:     cfg.Provider,
		Mock:         llm.Name() == "mock",
		MessageIndex: idx,
		SessionID:    s.sessionID(req.SessionID),
	}, nil
}

func (s *Session) sessionID(hint string) string {
	if hint != "" {
		return hint
	}
	return fmt.Sprintf("local-%d", time.Now().UnixNano())
}

// History 返回最近 N 条会话记录（供 GET /v1/workspace 与可观测使用）。
func (s *Session) History(n int) ([]map[string]any, error) {
	data, err := os.ReadFile(s.LogPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 0 || (len(lines) == 1 && lines[0] == "") {
		return nil, nil
	}
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	out := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err == nil {
			out = append(out, entry)
		}
	}
	return out, nil
}

func (s *Session) Workspace() string { return s.workspace }
