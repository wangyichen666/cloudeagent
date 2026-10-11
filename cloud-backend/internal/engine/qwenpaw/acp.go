// Package qwenpaw 是 QwenPaw ACP 内核的引擎实现。
//
// 通过 stdio JSON-RPC 与 qwenpaw acp 子进程通信：
//
//	agent-runtime ──(JSON-RPC 2.0 over stdin/stdout)──> qwenpaw acp
//	    initialize → session/new → session/prompt → session/cancel/close
//
// 模型凭证不落盘：子进程启动前把 base_url/api_key/model 生成到临时配置目录，
// 进程退出即销毁；配置热切换 = 重启 ACP 子进程。
package qwenpaw

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"cloude-agent/internal/engine"
)

const (
	acpProtocolVersion = 1

	acpMethodInitialize      = "initialize"
	acpMethodNewSession      = "session/new"
	acpMethodPrompt          = "session/prompt"
	acpMethodCancel          = "session/cancel"
	acpMethodCloseSession    = "session/close"
	acpMethodSetConfigOption = "session/set_config_option"

	acpNotifyUpdate      = "session/update"
	acpRequestPermission = "session/request_permission"
	acpUpdateKindMessage = "agent_message_chunk"
	acpUpdateKindThought = "agent_thought_chunk"
	acpUpdateKindToolS   = "tool_call"
	acpUpdateKindToolP   = "tool_call_update"

	errACPTransport     = acpTransportError("acp transport failure")
	acpErrCodeProcessGone = -32001
)

type acpTransportError string

func (e acpTransportError) Error() string { return string(e) }

// ACPServer 管理一个 qwenpaw acp 子进程及其 JSON-RPC 会话。
type ACPServer struct {
	bin       string
	workspace string
	cfg       *engine.ConfigManager

	spawnFn func(bin, workspace string, env []string) (*acpProcess, error)

	mu          sync.Mutex
	proc        *acpProcess
	nextID      int64
	pending     map[string]chan *rpcMessage
	handlers    map[string]updateHandler
	sessions    map[string]string
	spawnedCfg  *engine.Config
	cfgDir      string
	connected   bool
	agentName   string
	agentVersion string
	lastRestart time.Time
	stopping    bool

	PermissionPolicy func(req PermissionRequest) string
	OnUpdate         func(acpSessionID string, update map[string]any)
}

type acpProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	exited chan struct{}
	cfgDir string
}

// PermissionRequest 是 ACP 权限请求的最小视图。
type PermissionRequest struct {
	SessionID string
	ToolCall  map[string]any
	Options   []PermissionOption
}

type PermissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

type updateHandler interface {
	HandleUpdate(update map[string]any)
}

type updateHandlerFunc func(update map[string]any)

func (f updateHandlerFunc) HandleUpdate(u map[string]any) { f(u) }

type rpcMessage struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      any            `json:"id,omitempty"`
	Method  string         `json:"method,omitempty"`
	Params  map[string]any `json:"params,omitempty"`
	Result  map[string]any `json:"result,omitempty"`
	Error   *rpcError      `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// NewACPServer 创建 ACP 客户端网关。
func NewACPServer(bin, workspace string, cfg *engine.ConfigManager) *ACPServer {
	return &ACPServer{
		bin:       bin,
		workspace: workspace,
		cfg:       cfg,
		pending:   make(map[string]chan *rpcMessage),
		handlers:  make(map[string]updateHandler),
		sessions:  make(map[string]string),
		spawnFn:   spawnACPProcess,
	}
}

func (a *ACPServer) Connected() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.connected
}

func (a *ACPServer) AgentInfo() (name, version string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.agentName, a.agentVersion
}

func (a *ACPServer) LastRestart() time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastRestart
}

func (a *ACPServer) ensureStarted(ctx context.Context) error {
	a.mu.Lock()
	if a.proc != nil && a.connected {
		a.mu.Unlock()
		return nil
	}
	a.mu.Unlock()
	return a.start(ctx)
}

func (a *ACPServer) start(ctx context.Context) error {
	cfg := a.cfg.Get()
	if cfg.BaseURL == "" || strings.HasPrefix(cfg.BaseURL, "mock://") {
		return fmt.Errorf("qwenpaw ACP 需要真实模型配置（base_url 不能为 mock）")
	}

	env := append(os.Environ(),
		"OPENAI_BASE_URL="+strings.TrimSuffix(cfg.BaseURL, "/"),
		"OPENAI_API_KEY="+cfg.APIKey,
		"OPENAI_MODEL="+cfg.Model,
		"PYTHONUNBUFFERED=1",
	)
	proc, err := a.spawnFn(a.bin, a.workspace, env)
	if err != nil {
		return fmt.Errorf("启动 qwenpaw acp: %w", err)
	}

	a.mu.Lock()
	a.proc = proc
	a.cfgDir = proc.cfgDir
	a.connected = false
	a.lastRestart = time.Now()
	a.mu.Unlock()

	go a.readLoop(proc.stdout)
	go a.watchProc(proc)

	initCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := a.request(initCtx, acpMethodInitialize, map[string]any{
		"protocolVersion": acpProtocolVersion,
		"clientCapabilities": map[string]any{},
		"clientInfo": map[string]any{
			"name":    "cloudeagent",
			"title":   "CloudeAgent",
			"version": "0.1.0",
		},
	})
	if err != nil {
		_ = a.killLocked()
		return fmt.Errorf("qwenpaw ACP initialize 失败: %w", err)
	}

	if info, ok := resp["agentInfo"].(map[string]any); ok {
		a.mu.Lock()
		a.agentName, _ = info["name"].(string)
		a.agentVersion, _ = info["version"].(string)
		a.mu.Unlock()
	}

	a.mu.Lock()
	a.connected = true
	a.spawnedCfg = cfg
	a.mu.Unlock()
	log.Printf("[acp] qwenpaw ACP 内核已连接: agent=%s version=%s workspace=%s",
		a.agentName, a.agentVersion, a.workspace)
	return nil
}

func spawnACPProcess(bin, workspace string, env []string) (*acpProcess, error) {
	cmd := exec.Command(bin, "acp",
		"--workspace", workspace,
		"--local-diagnostics",
	)
	tmpDir := filepath.Join(workspace, ".tmp")
	_ = os.MkdirAll(tmpDir, 0o755)
	cfg := envRuntimeConfig(env)
	cfgDir, err := qwenpawConfigDir(env)
	if err != nil {
		return nil, err
	}
	secretDir := qwenpawSecretDir(env, cfgDir)
	if err := writeQwenPawConfig(cfgDir, secretDir, cfg); err != nil {
		return nil, err
	}
	cmd.Env = append(env,
		"TMPDIR="+tmpDir,
		"QWENPAW_WORKING_DIR="+cfgDir,
		"QWENPAW_SECRET_DIR="+secretDir,
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &acpProcess{
		cmd:    cmd,
		stdin:  stdin,
		stdout: stdout,
		exited: make(chan struct{}),
		cfgDir: cfgDir,
	}, nil
}

func envRuntimeConfig(env []string) *engine.Config {
	cfg := &engine.Config{}
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		switch k {
		case "OPENAI_BASE_URL":
			cfg.BaseURL = v
		case "OPENAI_API_KEY":
			cfg.APIKey = v
		case "OPENAI_MODEL":
			cfg.Model = v
		}
	}
	return cfg
}

func qwenpawConfigDir(env []string) (string, error) {
	for _, kv := range env {
		if strings.HasPrefix(kv, "QWENPAW_CONFIG_DIR=") {
			dir := strings.TrimPrefix(kv, "QWENPAW_CONFIG_DIR=")
			if dir != "" {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					return "", err
				}
				return dir, nil
			}
		}
	}
	return os.MkdirTemp("", "qwenpaw-cfg-")
}

func qwenpawSecretDir(env []string, cfgDir string) string {
	for _, kv := range env {
		if strings.HasPrefix(kv, "QWENPAW_SECRET_DIR=") {
			if dir := strings.TrimPrefix(kv, "QWENPAW_SECRET_DIR="); dir != "" {
				return dir
			}
		}
	}
	return cfgDir + ".secret"
}

func writeQwenPawConfig(cfgDir, secretDir string, cfg *engine.Config) error {
	providerID := "runtime-openai"
	agentDir := filepath.Join(cfgDir, "workspaces", "default")
	providersDir := filepath.Join(secretDir, "providers", "custom")
	for _, dir := range []string{agentDir, providersDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	write := func(path string, v any) error {
		data, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return err
		}
		return os.WriteFile(path, data, 0o600)
	}
	if err := write(filepath.Join(cfgDir, "config.json"), map[string]any{
		"agents": map[string]any{
			"active_agent": "default",
			"profiles": map[string]any{
				"default": map[string]any{
					"id":            "default",
					"workspace_dir": agentDir,
					"enabled":       true,
				},
			},
		},
	}); err != nil {
		return err
	}
	if err := write(filepath.Join(agentDir, "agent.json"), map[string]any{
		"id":           "default",
		"name":         "default",
		"active_model": map[string]any{"provider_id": providerID, "model": cfg.Model},
	}); err != nil {
		return err
	}
	if err := write(filepath.Join(providersDir, providerID+".json"), map[string]any{
		"id":                       providerID,
		"name":                     "ACP Runtime OpenAI",
		"base_url":                 cfg.BaseURL,
		"api_key":                  cfg.APIKey,
		"chat_model":               "OpenAIChatModel",
		"models":                   []map[string]any{{"id": cfg.Model, "name": cfg.Model}},
		"extra_models":             []any{},
		"is_custom":                true,
		"support_connection_check": false,
		"support_model_discovery":  false,
	}); err != nil {
		return err
	}
	return write(filepath.Join(secretDir, "providers", "active_model.json"),
		map[string]any{"provider_id": providerID, "model": cfg.Model})
}

func (a *ACPServer) watchProc(proc *acpProcess) {
	_ = proc.cmd.Wait()
	close(proc.exited)
	a.mu.Lock()
	if a.proc == proc {
		a.proc = nil
		a.connected = false
		a.sessions = make(map[string]string)
		a.handlers = make(map[string]updateHandler)
		for id, ch := range a.pending {
			select {
			case ch <- &rpcMessage{JSONRPC: "2.0", Error: &rpcError{
				Code:    acpErrCodeProcessGone,
				Message: "qwenpaw acp process exited",
			}}:
			default:
			}
			delete(a.pending, id)
		}
		wasStopping := a.stopping
		a.mu.Unlock()
		if !wasStopping {
			log.Printf("[acp] qwenpaw acp 子进程退出（PID=%d），下一次请求将自动重启", proc.cmd.Process.Pid)
		}
	} else {
		a.mu.Unlock()
	}
}

func (a *ACPServer) readLoop(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var msg rpcMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			log.Printf("[acp] 忽略非 JSON-RPC 行: %s", truncateForLog(line))
			continue
		}
		a.dispatch(&msg)
	}
	if err := sc.Err(); err != nil {
		log.Printf("[acp] 读取 stdout 结束: %v", err)
	}
}

func (a *ACPServer) dispatch(msg *rpcMessage) {
	if msg.Method == acpRequestPermission {
		a.handlePermissionRequest(msg)
		return
	}
	if msg.ID != nil {
		a.mu.Lock()
		ch := a.pending[fmt.Sprint(msg.ID)]
		delete(a.pending, fmt.Sprint(msg.ID))
		a.mu.Unlock()
		if ch != nil {
			ch <- msg
		}
		return
	}
	switch msg.Method {
	case acpNotifyUpdate:
		params := msg.Params
		sessionID, _ := params["sessionId"].(string)
		update, _ := params["update"].(map[string]any)
		if update == nil {
			return
		}
		a.mu.Lock()
		h := a.handlers[sessionID]
		global := a.OnUpdate
		a.mu.Unlock()
		if h != nil {
			h.HandleUpdate(update)
		} else if global != nil {
			global(sessionID, update)
		}
	}
}

func (a *ACPServer) handlePermissionRequest(msg *rpcMessage) {
	params := msg.Params
	req := PermissionRequest{
		SessionID: paramString(params, "sessionId"),
	}
	if tc, ok := params["toolCall"].(map[string]any); ok {
		req.ToolCall = tc
	}
	if opts, ok := params["options"].([]any); ok {
		for _, o := range opts {
			if om, ok := o.(map[string]any); ok {
				req.Options = append(req.Options, PermissionOption{
					OptionID: paramString(om, "optionId"),
					Name:     paramString(om, "name"),
					Kind:     paramString(om, "kind"),
				})
			}
		}
	}
	var result map[string]any
	policy := a.PermissionPolicy
	if policy != nil {
		if opt := policy(req); opt != "" && opt != "deny" && opt != "reject_once" {
			result = map[string]any{"outcome": "selected", "optionId": opt}
		}
	}
	if result == nil {
		result = map[string]any{"outcome": "cancelled"}
		title := ""
		if tc, ok := req.ToolCall["title"].(string); ok {
			title = tc
		}
		log.Printf("[acp] 权限请求已拒绝（headless 默认策略）: tool=%q session=%s", title, req.SessionID)
	}
	a.mu.Lock()
	_ = a.writeLocked(&rpcMessage{JSONRPC: "2.0", ID: msg.ID, Result: result})
	a.mu.Unlock()
}

func (a *ACPServer) request(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	a.mu.Lock()
	if a.proc == nil {
		a.mu.Unlock()
		return nil, fmt.Errorf("%w: 子进程未启动", errACPTransport)
	}
	a.nextID++
	id := a.nextID
	ch := make(chan *rpcMessage, 1)
	a.pending[fmt.Sprint(id)] = ch
	msg := &rpcMessage{JSONRPC: "2.0", ID: id, Method: method, Params: params}
	if err := a.writeLocked(msg); err != nil {
		delete(a.pending, fmt.Sprint(id))
		a.mu.Unlock()
		return nil, fmt.Errorf("写入 %s 请求: %w", method, err)
	}
	a.mu.Unlock()

	select {
	case <-ctx.Done():
		a.mu.Lock()
		delete(a.pending, fmt.Sprint(id))
		a.mu.Unlock()
		return nil, ctx.Err()
	case resp := <-ch:
		if resp.Error != nil {
			if resp.Error.Code == acpErrCodeProcessGone {
				return nil, fmt.Errorf("%w: 子进程已退出", errACPTransport)
			}
			return nil, fmt.Errorf("%s 错误(%d): %s", method, resp.Error.Code, resp.Error.Message)
		}
		return resp.Result, nil
	}
}

func (a *ACPServer) notify(method string, params map[string]any) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.proc == nil {
		return fmt.Errorf("%w: 子进程未启动", errACPTransport)
	}
	return a.writeLocked(&rpcMessage{JSONRPC: "2.0", Method: method, Params: params})
}

func (a *ACPServer) writeLocked(msg *rpcMessage) error {
	if a.proc == nil || a.proc.stdin == nil {
		return fmt.Errorf("%w: 子进程未启动", errACPTransport)
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if _, err := a.proc.stdin.Write(data); err != nil {
		a.connected = false
		if a.proc != nil && a.proc.cmd.Process != nil {
			_ = a.proc.cmd.Process.Kill()
		}
		return err
	}
	return nil
}

func (a *ACPServer) SessionID(ctx context.Context, localKey string) (string, error) {
	if err := a.ensureStarted(ctx); err != nil {
		return "", err
	}
	a.mu.Lock()
	if sid, ok := a.sessions[localKey]; ok {
		a.mu.Unlock()
		return sid, nil
	}
	a.mu.Unlock()

	newCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := a.request(newCtx, acpMethodNewSession, map[string]any{
		"cwd":        a.workspace,
		"mcpServers": []any{},
	})
	if err != nil {
		return "", fmt.Errorf("session/new 失败: %w", err)
	}
	sid := paramString(resp, "sessionId")
	if sid == "" {
		return "", errors.New("session/new 未返回 sessionId")
	}
	a.mu.Lock()
	a.sessions[localKey] = sid
	a.mu.Unlock()

	cfg := a.cfg.Get()
	if cfg.ToolGuardMode == "bypass" {
		_, _ = a.request(ctx, acpMethodSetConfigOption, map[string]any{
			"sessionId": sid,
			"configId":  "mode",
			"value":     "bypassPermissions",
		})
	}
	return sid, nil
}

func (a *ACPServer) Prompt(ctx context.Context, localKey, message string, handler updateHandler) (string, error) {
	reply, err := a.promptOnce(ctx, localKey, message, handler)
	if err != nil && errors.Is(err, errACPTransport) {
		log.Printf("[acp] 传输故障，重启内核并重试: %v", err)
		restartCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if rerr := a.Restart(restartCtx); rerr != nil {
			return "", fmt.Errorf("ACP 内核重启失败: %w（原错误: %v）", rerr, err)
		}
		reply, err = a.promptOnce(ctx, localKey, message, handler)
	}
	return reply, err
}

func (a *ACPServer) promptOnce(ctx context.Context, localKey, message string, handler updateHandler) (string, error) {
	sid, err := a.SessionID(ctx, localKey)
	if err != nil {
		return "", err
	}
	if handler == nil {
		handler = updateHandlerFunc(func(map[string]any) {})
	}
	a.mu.Lock()
	a.handlers[sid] = handler
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.handlers, sid)
		a.mu.Unlock()
	}()

	resp, err := a.request(ctx, acpMethodPrompt, map[string]any{
		"sessionId": sid,
		"prompt": []any{
			map[string]any{"type": "text", "text": message},
		},
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			_ = a.Cancel(sid)
		}
		return "", fmt.Errorf("session/prompt 失败: %w", err)
	}
	stopReason := paramString(resp, "stopReason")
	log.Printf("[acp] prompt 完成 session=%s stop=%s", sid, stopReason)
	return "", nil
}

func (a *ACPServer) Cancel(sid string) error {
	return a.notify(acpMethodCancel, map[string]any{"sessionId": sid})
}

func (a *ACPServer) CloseSession(sid string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _ = a.request(ctx, acpMethodCloseSession, map[string]any{"sessionId": sid})
	a.mu.Lock()
	for k, v := range a.sessions {
		if v == sid {
			delete(a.sessions, k)
		}
	}
	delete(a.handlers, sid)
	a.mu.Unlock()
}

func (a *ACPServer) CloseSessionLocal(localKey string) {
	a.mu.Lock()
	sid := a.sessions[localKey]
	a.mu.Unlock()
	if sid == "" {
		return
	}
	a.CloseSession(sid)
}

func (a *ACPServer) SyncConfig(ctx context.Context) {
	cfg := a.cfg.Get()
	a.mu.Lock()
	needRestart := a.proc != nil && a.spawnedCfg != nil &&
		(cfg.BaseURL != a.spawnedCfg.BaseURL ||
			cfg.APIKey != a.spawnedCfg.APIKey ||
			cfg.Model != a.spawnedCfg.Model)
	a.mu.Unlock()
	if !needRestart {
		return
	}
	log.Printf("[acp] 模型配置变化，重启 qwenpaw ACP 子进程注入新凭证")
	if err := a.Restart(ctx); err != nil {
		log.Printf("[acp] 重启失败: %v（保持旧状态，下次请求重试）", err)
	}
}

func (a *ACPServer) Restart(ctx context.Context) error {
	a.mu.Lock()
	a.stopping = true
	_ = a.killLocked()
	a.stopping = false
	a.mu.Unlock()

	select {
	case <-time.After(300 * time.Millisecond):
	case <-ctx.Done():
		return ctx.Err()
	}
	return a.start(ctx)
}

func (a *ACPServer) Stop() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stopping = true
	_ = a.killLocked()
	a.stopping = false
	a.sessions = make(map[string]string)
	a.handlers = make(map[string]updateHandler)
}

func (a *ACPServer) killLocked() error {
	if a.proc == nil {
		return nil
	}
	proc := a.proc
	a.proc = nil
	a.connected = false
	if a.cfgDir != "" {
		_ = os.RemoveAll(a.cfgDir)
		a.cfgDir = ""
	}
	a.sessions = make(map[string]string)
	a.handlers = make(map[string]updateHandler)
	if proc.cmd.Process != nil {
		_ = proc.cmd.Process.Signal(os.Interrupt)
		select {
		case <-proc.exited:
		case <-time.After(5 * time.Second):
			_ = proc.cmd.Process.Kill()
			select {
			case <-proc.exited:
			case <-time.After(3 * time.Second):
			}
		}
	}
	_ = proc.stdin.Close()
	_ = proc.stdout.Close()
	return nil
}

func paramString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func truncateForLog(s string) string {
	const max = 200
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
