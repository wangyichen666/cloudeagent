package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
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
	acp       *ACPServer
	acpBin    string
	mu        sync.Mutex
	promptMu  sync.Mutex // 单用户实例：串行化 prompt，避免同一会话交错

	probeOnce sync.Once
	probeVer  string
	probeErr  error
}

// NewSession 创建会话。acpBin 为 qwenpaw 可执行文件路径（空串 = 禁用 ACP 内核，
// 纯 mock 回退；非空但找不到文件同样回退 mock）。
func NewSession(workspace string, cfg *ConfigManager, acpBin string) (*Session, error) {
	dir := filepath.Join(workspace, ".agent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Session{
		workspace: workspace,
		cfg:       cfg,
		acpBin:    acpBin,
		acp:       NewACPServer(acpBin, workspace, cfg),
	}, nil
}

// AcpReady 报告 QwenPaw ACP 内核是否可用：
// 需要可执行文件存在 + 已配置真实模型端点（非 mock）。
func (s *Session) AcpReady() (bool, string) {
	if s.acpBin == "" {
		return false, "未配置 qwenpaw 可执行文件（--qwenpaw-bin）"
	}
	if _, err := exec.LookPath(s.acpBin); err != nil {
		if _, statErr := os.Stat(s.acpBin); statErr != nil {
			return false, fmt.Sprintf("找不到 qwenpaw 可执行文件 %s", s.acpBin)
		}
	}
	ver, verErr := s.qwenpawVersion()
	if verErr != nil {
		return false, fmt.Sprintf("qwenpaw 版本探测失败: %v", verErr)
	}
	// 仅在能明确解析出版本且主版本 <2 时拦截；未知格式不误伤
	// （wrapper/别名脚本可能不响应 --version，让 ACP 握手给出真实错误）。
	if major, _ := versionMajorMinor(ver); major > 0 && major < 2 {
		return false, fmt.Sprintf(
			"qwenpaw 版本过低（当前 %s，ACP --runtime-provider 需要 >=2.0），请升级：pip install -U qwenpaw",
			ver)
	}
	cfg := s.cfg.Get()
	if cfg.BaseURL == "" || strings.HasPrefix(cfg.BaseURL, "mock://") {
		return false, "模型配置为 mock（设置真实 base_url 后启用 ACP 内核）"
	}
	return true, ""
}

// qwenpawVersion 探测一次并缓存 `qwenpaw --version` 输出。
func (s *Session) qwenpawVersion() (string, error) {
	s.probeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, s.acpBin, "--version").CombinedOutput()
		if err != nil {
			// 某些版本把版本号打在 stderr 或非零退出；尝试宽松解析。
			s.probeErr = err
			s.probeVer = strings.TrimSpace(string(out))
			if s.probeVer == "" {
				s.probeErr = fmt.Errorf("qwenpaw --version 失败: %w", err)
				return
			}
			s.probeErr = nil
			return
		}
		s.probeVer = strings.TrimSpace(string(out))
	})
	if s.probeErr != nil {
		return s.probeVer, s.probeErr
	}
	return s.probeVer, nil
}

// versionMajorMinor 从 "QwenPaw, version 2.0.4" 这类输出解析主版本号。
func versionMajorMinor(ver string) (int, int) {
	major, minor := 0, 0
	for _, part := range strings.FieldsFunc(ver, func(r rune) bool {
		return r == '.' || r == ' ' || r == ',' || r == '-'
	}) {
		var n int
		if _, err := fmt.Sscanf(part, "%d", &n); err == nil {
			if major == 0 {
				major = n
				continue
			}
			minor = n
			return major, minor
		}
	}
	return major, minor
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

	var reply string
	var mock bool
	if ready, reason := s.AcpReady(); ready {
		var sb strings.Builder
		filter := newHeadlineLineFilter(func(text string) { _, _ = sb.WriteString(text) })
		handler := UpdateHandlerFunc(func(u map[string]any) {
			if kind, _ := u["sessionUpdate"].(string); kind == acpUpdateKindMessage {
				if c, ok := u["content"].(map[string]any); ok {
					if text, ok := c["text"].(string); ok {
						filter.Write(text)
					}
				}
			}
		})
		key := s.acpSessionKey(req.SessionID)
		s.promptMu.Lock()
		_, err = s.acp.Prompt(ctx, key, req.Message, handler)
		s.promptMu.Unlock()
		if err != nil {
			_, _ = s.appendLog("error", err.Error(), cfg.Model)
			return nil, err
		}
		filter.Flush()
		reply = stripHeadlineText(sb.String())
		if reply == "" {
			reply = "（QwenPaw 未产生文本回复，请查看工具调用/工作区状态）"
		}
	} else {
		// mock 回退：零外部依赖可用；同时保留诊断信息。
		if !strings.HasPrefix(cfg.BaseURL, "mock://") {
			log.Printf("[session] ACP 内核不可用，回退 mock: %s", reason)
		}
		llm := NewLLM(cfg)
		reply, err = llm.Complete(ctx, cfg, req.Message)
		if err != nil {
			_, _ = s.appendLog("error", err.Error(), cfg.Model)
			return nil, err
		}
		mock = llm.Name() == "mock"
	}
	_, _ = s.appendLog("assistant", reply, cfg.Model)
	return &models.ChatResponse{
		Reply:        reply,
		Model:        cfg.Model,
		Provider:     cfg.Provider,
		Mock:         mock,
		MessageIndex: idx,
		SessionID:    s.sessionID(req.SessionID),
	}, nil
}

// StreamEmitter 是流式会话的输出回调。
// kind: delta / thought / tool_call / usage / error
type StreamEmitter func(kind string, payload map[string]any) error

// ChatStream 流式执行一次会话请求（WebSocket 路径）。
// 真实内核：ACP session/update 增量转发；mock：分片模拟流式。
func (s *Session) ChatStream(ctx context.Context, req *ChatRequest, emit StreamEmitter) (*models.ChatResponse, error) {
	cfg := s.cfg.Get()
	idx, err := s.appendLog("user", req.Message, cfg.Model)
	if err != nil {
		return nil, fmt.Errorf("persist user message: %w", err)
	}

	var reply string
	var mock bool
	if ready, _ := s.AcpReady(); ready {
		var sb strings.Builder
		filter := newHeadlineLineFilter(func(text string) {
			sb.WriteString(text)
			if strings.TrimSpace(text) != "" {
				_ = emit("delta", map[string]any{"text": text})
			}
		})
		handler := UpdateHandlerFunc(func(u map[string]any) {
			kind, _ := u["sessionUpdate"].(string)
			switch kind {
			case acpUpdateKindMessage:
				if c, ok := u["content"].(map[string]any); ok {
					if text, ok := c["text"].(string); ok && text != "" {
						filter.Write(text)
					}
				}
			case acpUpdateKindThought:
				if c, ok := u["content"].(map[string]any); ok {
					if text, ok := c["text"].(string); ok {
						_ = emit("thought", map[string]any{"text": text})
					}
				}
			case acpUpdateKindToolS, acpUpdateKindToolP:
				_ = emit("tool_call", u)
			case "usage_update":
				_ = emit("usage", u)
			}
		})
		key := s.acpSessionKey(req.SessionID)
		s.promptMu.Lock()
		_, err = s.acp.Prompt(ctx, key, req.Message, handler)
		s.promptMu.Unlock()
		if err != nil {
			_ = emit("error", map[string]any{"message": err.Error()})
			_, _ = s.appendLog("error", err.Error(), cfg.Model)
			return nil, err
		}
		filter.Flush()
		reply = stripHeadlineText(sb.String())
	} else {
		llm := NewLLM(cfg)
		reply, err = llm.Complete(ctx, cfg, req.Message)
		if err != nil {
			_ = emit("error", map[string]any{"message": err.Error()})
			_, _ = s.appendLog("error", err.Error(), cfg.Model)
			return nil, err
		}
		mock = llm.Name() == "mock"
		for _, chunk := range chunkText(reply, 24) {
			if err := emit("delta", map[string]any{"text": chunk}); err != nil {
				return nil, err
			}
		}
	}

	_, _ = s.appendLog("assistant", reply, cfg.Model)
	return &models.ChatResponse{
		Reply:        reply,
		Model:        cfg.Model,
		Provider:     cfg.Provider,
		Mock:         mock,
		MessageIndex: idx,
		SessionID:    s.sessionID(req.SessionID),
	}, nil
}

// SyncConfig 模型热切换：让 ACP 内核感知配置变化（凭证变化 → 重启子进程）。
func (s *Session) SyncConfig(ctx context.Context) {
	s.acp.SyncConfig(ctx)
}

// Close 停止 ACP 子进程（实例休眠/退出时调用）。
func (s *Session) Close() {
	s.acp.Stop()
}

// KernelStatus 返回 ACP 内核状态，供 /health 与可观测使用。
func (s *Session) KernelStatus() map[string]any {
	ready, reason := s.AcpReady()
	name, version := s.acp.AgentInfo()
	if name == "" {
		name = "qwenpaw"
	}
	return map[string]any{
		"name":        "qwenpaw-acp",
		"enabled":     ready,
		"reason":      reason,
		"connected":   s.acp.Connected(),
		"agent":       name,
		"version":     version,
		"lastRestart": s.acp.LastRestart().Format(time.RFC3339),
	}
}

func (s *Session) acpSessionKey(hint string) string {
	if hint != "" {
		return hint
	}
	return "default"
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
