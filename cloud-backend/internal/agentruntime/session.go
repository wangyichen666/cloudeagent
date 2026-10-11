package agentruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"cloude-agent/internal/engine"
	"cloude-agent/internal/domain"
)

// Session 管理每个实例的工作区会话：
// 对话历史持久化在 workspace/.agent/conversation.jsonl，
// 休眠再唤醒后数据不丢。
//
// 引擎通过 engine.Engine 接口抽象：优先使用配置的真实引擎（如 qwenpaw），
// 不可用时自动回退到 mock 引擎。
type Session struct {
	workspace string
	cfg       *engine.ConfigManager
	eng       engine.Engine // 当前活跃引擎（qwenpaw 或 mock）
	engName   string        // 引擎名称（用于日志/可观测）
	mu        sync.Mutex
	promptMu  sync.Mutex // 单用户实例：串行化 prompt
}

// NewSession 创建会话。
// engineName 指定首选引擎（如 "qwenpaw"）；为空或不可用时自动回退 mock。
func NewSession(workspace, engineName string, cfg *engine.ConfigManager) (*Session, error) {
	dir := filepath.Join(workspace, ".agent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Session{
		workspace: workspace,
		cfg:       cfg,
	}
	s.eng, s.engName = s.buildEngine(engineName)
	return s, nil
}

// buildEngine 按名称创建引擎，失败则回退 mock。
func (s *Session) buildEngine(name string) (engine.Engine, string) {
	if name == "" {
		name = "qwenpaw"
	}
	eng, err := engine.New(name, s.workspace, s.cfg.Get())
	if err != nil {
		log.Printf("[session] 引擎 %s 创建失败，回退 mock: %v", name, err)
		mockEng, _ := engine.New("mock", s.workspace, s.cfg.Get())
		return mockEng, "mock"
	}
	return eng, name
}

// EngineReady 报告当前引擎是否可用。
func (s *Session) EngineReady() (bool, string) {
	return s.eng.Ready()
}

func (s *Session) LogPath() string {
	return filepath.Join(s.workspace, ".agent", "conversation.jsonl")
}

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

func (s *Session) appendLog(role, content, model, sessionID string) (int, error) {
	idx, err := s.MessageIndex()
	if err != nil {
		return 0, err
	}
	idx++
	entry := map[string]any{
		"ts":         time.Now().UTC().Format(time.RFC3339),
		"role":       role,
		"content":    content,
		"model":      model,
		"index":      idx,
		"session_id": sessionID,
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
func (s *Session) Chat(ctx context.Context, req *domain.ChatRequest) (*domain.ChatResponse, error) {
	cfg := s.cfg.Get()
	key := s.sessionKey(req.SessionID)
	idx, err := s.appendLog("user", req.Message, cfg.Model, key)
	if err != nil {
		return nil, fmt.Errorf("persist user message: %w", err)
	}

	var reply string
	var mock bool

	// 引擎不可用时自动切换到 mock（保持实例可用）
	if ready, reason := s.eng.Ready(); !ready {
		if s.engName != "mock" {
			log.Printf("[session] 引擎 %s 不可用，回退 mock: %s", s.engName, reason)
			mockEng, _ := engine.New("mock", s.workspace, cfg)
			s.eng = mockEng
			s.engName = "mock"
		}
	}

	s.promptMu.Lock()
	reply, err = s.eng.Chat(ctx, key, req.Message, nil)
	s.promptMu.Unlock()
	if err != nil {
		_, _ = s.appendLog("error", err.Error(), cfg.Model, key)
		return nil, err
	}
	if reply == "" && s.engName == "qwenpaw" {
		reply = "（引擎未产生文本回复，请查看工具调用/工作区状态）"
	}
	mock = s.engName == "mock"

	_, _ = s.appendLog("assistant", reply, cfg.Model, key)
	return &domain.ChatResponse{
		Reply:        reply,
		Model:        cfg.Model,
		Provider:     cfg.Provider,
		Mock:         mock,
		MessageIndex: idx,
		SessionID:    s.sessionID(req.SessionID),
	}, nil
}

// StreamEmitter 是流式会话的输出回调。
type StreamEmitter func(kind string, payload map[string]any) error

// ChatStream 流式执行一次会话请求（WebSocket 路径）。
func (s *Session) ChatStream(ctx context.Context, req *domain.ChatRequest, emit StreamEmitter) (*domain.ChatResponse, error) {
	cfg := s.cfg.Get()
	key := s.sessionKey(req.SessionID)
	idx, err := s.appendLog("user", req.Message, cfg.Model, key)
	if err != nil {
		return nil, fmt.Errorf("persist user message: %w", err)
	}

	if ready, reason := s.eng.Ready(); !ready && s.engName != "mock" {
		log.Printf("[session] 引擎 %s 不可用，回退 mock: %s", s.engName, reason)
		mockEng, _ := engine.New("mock", s.workspace, cfg)
		s.eng = mockEng
		s.engName = "mock"
	}

	var reply string
	var mock bool

	callbacks := &engine.Callbacks{
		OnDelta: func(text string) {
			if strings.TrimSpace(text) != "" {
				_ = emit("delta", map[string]any{"text": text})
			}
		},
		OnThought: func(text string) {
			_ = emit("thought", map[string]any{"text": text})
		},
		OnTool: func(update map[string]any) {
			_ = emit("tool_call", update)
		},
		OnUsage: func(update map[string]any) {
			_ = emit("usage", update)
		},
	}

	s.promptMu.Lock()
	reply, err = s.eng.Chat(ctx, key, req.Message, callbacks)
	s.promptMu.Unlock()
	if err != nil {
		_ = emit("error", map[string]any{"message": err.Error()})
		_, _ = s.appendLog("error", err.Error(), cfg.Model, key)
		return nil, err
	}
	mock = s.engName == "mock"

	_, _ = s.appendLog("assistant", reply, cfg.Model, key)
	return &domain.ChatResponse{
		Reply:        reply,
		Model:        cfg.Model,
		Provider:     cfg.Provider,
		Mock:         mock,
		MessageIndex: idx,
		SessionID:    s.sessionID(req.SessionID),
	}, nil
}

// SyncConfig 模型热切换：通知引擎配置变化。
func (s *Session) SyncConfig(ctx context.Context) {
	s.eng.SyncConfig(ctx)
}

// Close 停止引擎（沙箱休眠/退出时调用）。
func (s *Session) Close() {
	s.eng.Close()
	s.cfg.Close()
}

// KernelStatus 返回引擎状态，供 /health 与可观测使用。
func (s *Session) KernelStatus() map[string]any {
	ready, reason := s.eng.Ready()
	name, version := s.eng.Info()
	if name == "" {
		name = s.engName
	}
	return map[string]any{
		"name":        s.engName,
		"enabled":     ready,
		"reason":      reason,
		"connected":   s.eng.Connected(),
		"agent":       name,
		"version":     version,
	}
}

// ActiveSession 返回当前活动会话 id。
func (s *Session) ActiveSession() string {
	if q, ok := s.eng.(interface{ ActiveSession() string }); ok {
		return q.ActiveSession()
	}
	return "default"
}

// NewSession 让引擎开启一个新会话。
func (s *Session) NewSession(ctx context.Context) (string, error) {
	return s.eng.NewSession(ctx)
}

// History 返回最近 N 条会话记录。
func (s *Session) History(n int, sessionID string) ([]map[string]any, error) {
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
	if sessionID != "" {
		filtered := lines[:0]
		for _, line := range lines {
			var entry map[string]any
			if json.Unmarshal([]byte(line), &entry) == nil && entry["session_id"] == sessionID {
				filtered = append(filtered, line)
			}
		}
		lines = filtered
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

// WorkspaceSummary 返回工作区文件列表与最近对话记录。
func (s *Session) WorkspaceSummary() (map[string]any, error) {
	entries, err := os.ReadDir(s.workspace)
	if err != nil {
		return nil, err
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
	history, _ := s.History(3, "")
	return map[string]any{
		"workspace": s.workspace,
		"files":     files,
		"recent":    history,
		"kernel":    s.KernelStatus(),
	}, nil
}

func (s *Session) sessionKey(hint string) string {
	if hint != "" {
		return hint
	}
	return s.ActiveSession()
}

func (s *Session) sessionID(hint string) string {
	if hint != "" {
		return hint
	}
	return fmt.Sprintf("local-%d", time.Now().UnixNano())
}
