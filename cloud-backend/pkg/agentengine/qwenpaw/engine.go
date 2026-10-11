package qwenpaw

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"cloude-agent/pkg/agentengine"
)

func init() {
	agentengine.Register("qwenpaw", New)
}

// Engine 是 QwenPaw ACP 内核的 agentengine.Engine 实现。
type Engine struct {
	acp       *ACPServer
	cfg       *agentengine.ConfigManager
	bin       string
	workspace string

	probeOnce sync.Once
	probeVer  string
	probeErr  error

	mu         sync.Mutex
	activeSess string
}

// New 创建 qwenpaw 引擎。bin 为空则自动查找 qwenpaw。
func New(name, workspace string, cfg *agentengine.Config) (agentengine.Engine, error) {
	bin := name
	if bin == "" || bin == "qwenpaw" {
		bin = "qwenpaw"
	}
	cfgMgr := agentengine.NewConfigManager("", nil)
	cfgMgr.Apply(cfg)
	return &Engine{
		acp:        NewACPServer(bin, workspace, cfgMgr),
		cfg:        cfgMgr,
		bin:        bin,
		workspace:  workspace,
		activeSess: "default",
	}, nil
}

// NewWithConfigManager 用外部 ConfigManager 创建（agent-runtime 热加载路径）。
func NewWithConfigManager(bin, workspace string, cfg *agentengine.ConfigManager) *Engine {
	if bin == "" {
		bin = "qwenpaw"
	}
	return &Engine{
		acp:        NewACPServer(bin, workspace, cfg),
		cfg:        cfg,
		bin:        bin,
		workspace:  workspace,
		activeSess: "default",
	}
}

func (e *Engine) Name() string { return "qwenpaw-acp" }

func (e *Engine) Ready() (bool, string) {
	if e.bin == "" {
		return false, "未配置 qwenpaw 可执行文件"
	}
	if _, err := exec.LookPath(e.bin); err != nil {
		if _, statErr := os.Stat(e.bin); statErr != nil {
			return false, fmt.Sprintf("找不到 qwenpaw 可执行文件 %s", e.bin)
		}
	}
	ver, verErr := e.version()
	if verErr != nil {
		return false, fmt.Sprintf("qwenpaw 版本探测失败: %v", verErr)
	}
	if major, _ := versionMajorMinor(ver); major > 0 && major < 2 {
		return false, fmt.Sprintf("qwenpaw 版本过低（当前 %s，需要 >=2.0）", ver)
	}
	cfg := e.cfg.Get()
	if cfg.BaseURL == "" || strings.HasPrefix(cfg.BaseURL, "mock://") {
		return false, "模型配置为 mock（设置真实 base_url 后启用 ACP 内核）"
	}
	return true, ""
}

func (e *Engine) version() (string, error) {
	e.probeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, e.bin, "--version").CombinedOutput()
		if err != nil {
			e.probeErr = err
			e.probeVer = strings.TrimSpace(string(out))
			if e.probeVer == "" {
				e.probeErr = fmt.Errorf("qwenpaw --version 失败: %w", err)
				return
			}
			e.probeErr = nil
			return
		}
		e.probeVer = strings.TrimSpace(string(out))
	})
	if e.probeErr != nil {
		return e.probeVer, e.probeErr
	}
	return e.probeVer, nil
}

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

func (e *Engine) Chat(ctx context.Context, sessionKey, message string, cb *agentengine.Callbacks) (string, error) {
	key := e.sessionKey(sessionKey)
	var sb strings.Builder

	// qwenpaw 专属：流式输出过滤里程碑注释行
	var filter *headlineLineFilter
	if cb != nil && cb.OnDelta != nil {
		filter = newHeadlineLineFilter(cb.OnDelta)
	}

	handler := updateHandlerFunc(func(u map[string]any) {
		if cb == nil {
			return
		}
		kind, _ := u["sessionUpdate"].(string)
		switch kind {
		case acpUpdateKindMessage:
			if c, ok := u["content"].(map[string]any); ok {
				if text, ok := c["text"].(string); ok && text != "" {
					sb.WriteString(text)
					if filter != nil {
						filter.Write(text)
					}
				}
			}
		case acpUpdateKindThought:
			if c, ok := u["content"].(map[string]any); ok {
				if text, ok := c["text"].(string); ok && cb.OnThought != nil {
					cb.OnThought(text)
				}
			}
		case acpUpdateKindToolS, acpUpdateKindToolP:
			if cb.OnTool != nil {
				cb.OnTool(u)
			}
		case "usage_update":
			if cb.OnUsage != nil {
				cb.OnUsage(u)
			}
		}
	})
	_, err := e.acp.Prompt(ctx, key, message, handler)
	if filter != nil {
		filter.Flush()
	}
	if err != nil {
		return "", err
	}
	reply := stripHeadlineText(sb.String())
	if reply == "" {
		reply = "（QwenPaw 未产生文本回复，请查看工具调用/工作区状态）"
	}
	return reply, nil
}

func (e *Engine) NewSession(ctx context.Context) (string, error) {
	key := fmt.Sprintf("s-%d", time.Now().UnixNano())
	e.mu.Lock()
	old := e.activeSess
	e.activeSess = key
	e.mu.Unlock()

	if old != "" && old != key {
		e.acp.CloseSessionLocal(old)
	}
	if _, err := e.acp.SessionID(ctx, key); err != nil {
		return "", err
	}
	return key, nil
}

func (e *Engine) CloseSession(sessionKey string) {
	e.acp.CloseSessionLocal(sessionKey)
}

func (e *Engine) SyncConfig(ctx context.Context) {
	e.acp.SyncConfig(ctx)
}

func (e *Engine) Close() {
	e.acp.Stop()
}

func (e *Engine) Info() (name, version string) {
	return e.acp.AgentInfo()
}

func (e *Engine) Connected() bool {
	return e.acp.Connected()
}

// ActiveSession 返回当前活动会话 key。
func (e *Engine) ActiveSession() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.activeSess
}

func (e *Engine) sessionKey(hint string) string {
	if hint != "" {
		return hint
	}
	return e.ActiveSession()
}
