package agent

// ACP（Agent Client Protocol）客户端网关。
//
// 负责把 QwenPaw 的 ACP server（`qwenpaw acp`，stdio JSON-RPC）接入
// agent-runtime：
//
//	agent-runtime ──(JSON-RPC 2.0 over stdin/stdout)──> qwenpaw acp
//	    initialize → session/new → session/prompt → session/cancel/close
//	    session/update 通知：流式消息 / 思考 / 工具调用
//	    session/request_permission：权限请求（策略默认拒绝，可挂接回调）
//
// 模型凭证不落盘：子进程启动前把 base_url/api_key/model 生成到临时配置目录
// （进程/容器用系统临时目录，K8s Pod 用 emptyDir 挂载的 QWENPAW_CONFIG_DIR），
// 并通过 QWENPAW_WORKING_DIR / QWENPAW_SECRET_DIR 指给 qwenpaw；
// 进程退出即销毁；配置热切换 = 重启 ACP 子进程，工作区数据不丢。
//
// 自愈：子进程异常退出后，下一次请求自动重启并重建会话。

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
)

const (
	acpProtocolVersion = 1

	// wire 方法名（ACP v1）
	acpMethodInitialize      = "initialize"
	acpMethodNewSession      = "session/new"
	acpMethodPrompt          = "session/prompt"
	acpMethodCancel          = "session/cancel"
	acpMethodCloseSession    = "session/close"
	acpMethodSetConfigOption = "session/set_config_option"

	// server -> client
	acpNotifyUpdate      = "session/update"
	acpRequestPermission = "session/request_permission"
	acpUpdateKindMessage = "agent_message_chunk"
	acpUpdateKindThought = "agent_thought_chunk"
	acpUpdateKindToolS   = "tool_call"
	acpUpdateKindToolP   = "tool_call_update"

	// errACPTransport 标记传输层故障（子进程退出/管道断裂），用于触发自愈重启。
	errACPTransport = acpTransportError("acp transport failure")

	// acpErrCodeProcessGone：进程退出时注入 pending 请求的合成错误码。
	acpErrCodeProcessGone = -32001
)

type acpTransportError string

func (e acpTransportError) Error() string { return string(e) }

// ACPServer 管理一个 qwenpaw acp 子进程及其 JSON-RPC 会话。
type ACPServer struct {
	bin       string
	workspace string
	cfg       *ConfigManager

	spawnFn func(bin, workspace string, env []string) (*acpProcess, error)

	mu           sync.Mutex
	proc         *acpProcess
	nextID       int64
	pending      map[string]chan *rpcMessage
	handlers     map[string]UpdateHandler // acp session id -> 本次 prompt 的流式订阅者
	sessions     map[string]string        // localKey -> acp session id
	spawnedCfg   *RuntimeConfig           // 本次启动子进程时使用的模型配置
	cfgDir       string                   // 本次启动时为 qwenpaw 生成的临时配置目录
	connected    bool
	agentName    string
	agentVersion string
	lastRestart  time.Time
	stopping     bool

	// PermissionPolicy 返回用户选择的 option id；nil 或返回空串 = 拒绝。
	PermissionPolicy func(req PermissionRequest) string
	// OnUpdate 是未被 handlers 消费的 session/update 的兜底回调（可观测用）。
	OnUpdate func(acpSessionID string, update map[string]any)
}

// acpProcess 封装子进程的三件套：命令句柄 + stdin/stdout。
// stdout 只承载 JSON-RPC；QwenPaw 日志走 stderr。
type acpProcess struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	exited  chan struct{} // watchProc 独占调用 Wait 后关闭；killLocked 据此等待退出
	cfgDir  string        // 本次进程的 qwenpaw 临时配置目录（Stop 时清理）
}

// PermissionRequest 是 ACP 权限请求的最小视图（来自 session/request_permission）。
type PermissionRequest struct {
	SessionID string
	ToolCall  map[string]any
	Options   []PermissionOption
}

// PermissionOption 是权限请求中可选择的选项。
type PermissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

// UpdateHandler 接收一次 prompt 期间的流式更新（session/update 的 update 字段）。
type UpdateHandler interface {
	HandleUpdate(update map[string]any)
}

// UpdateHandlerFunc 便于以函数形式注册。
type UpdateHandlerFunc func(update map[string]any)

func (f UpdateHandlerFunc) HandleUpdate(u map[string]any) { f(u) }

// rpcMessage 是 JSON-RPC 2.0 信封（请求/响应/通知统一）。
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

// NewACPServer 创建 ACP 客户端网关。workspace 必须是绝对路径。
func NewACPServer(bin, workspace string, cfg *ConfigManager) *ACPServer {
	return &ACPServer{
		bin:       bin,
		workspace: workspace,
		cfg:       cfg,
		pending:   make(map[string]chan *rpcMessage),
		handlers:  make(map[string]UpdateHandler),
		sessions:  make(map[string]string),
		spawnFn:   spawnACPProcess,
	}
}

// Name 返回内核名称（用于健康检查/可观测）。
func (a *ACPServer) Name() string { return "qwenpaw-acp" }

// Connected 报告 ACP 子进程是否已 initialize 成功。
func (a *ACPServer) Connected() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.connected
}

// AgentInfo 返回 initialize 阶段声明的内核信息。
func (a *ACPServer) AgentInfo() (name, version string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.agentName, a.agentVersion
}

// LastRestart 返回最近一次子进程重启时间（自愈可观测）。
func (a *ACPServer) LastRestart() time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastRestart
}

// ensureStarted 保证子进程已启动并完成 initialize；未启动/已退出则（重新）拉起。
func (a *ACPServer) ensureStarted(ctx context.Context) error {
	a.mu.Lock()
	if a.proc != nil && a.connected {
		a.mu.Unlock()
		return nil
	}
	a.mu.Unlock()

	return a.start(ctx)
}

// start 以当前模型配置启动 qwenpaw acp 子进程并完成握手。
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

	// 握手：initialize。30 秒超时（首次启动 QwenPaw 需要加载 workspace）。
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

// spawnACPProcess 是默认的 qwenpaw 拉起实现（可测试替换）。
func spawnACPProcess(bin, workspace string, env []string) (*acpProcess, error) {
	// 注意：不能用 CommandContext 绑定请求 ctx —— ACP 内核的生命周期属于
	// agent-runtime（跨多个 HTTP/WS 请求存活），只在 Stop/Restart 时终止。
	cmd := exec.Command(bin, "acp",
		"--workspace", workspace,
		"--local-diagnostics",
	)
	// 容器内根文件系统可能是只读（k8s 加固配置）：
	// 把 Python/临时文件目录指到用户工作区，保证 QwenPaw 可写。
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
	cmd.Stderr = os.Stderr // 日志与 JSON-RPC 分离：stdout 仅协议
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

// envRuntimeConfig 从环境变量中取回模型配置（start 注入的 OPENAI_*）。
func envRuntimeConfig(env []string) *RuntimeConfig {
	cfg := &RuntimeConfig{}
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

// qwenpawConfigDir 返回 QwenPaw 配置根目录：
//   - QWENPAW_CONFIG_DIR（容器内 emptyDir 挂载，凭证不落盘）优先；
//   - 否则在系统临时目录下创建（进程/Docker 后端，退出即清理由调用方负责）。
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
	dir, err := os.MkdirTemp("", "qwenpaw-cfg-")
	if err != nil {
		return "", err
	}
	return dir, nil
}

// qwenpawSecretDir 返回凭证目录（providers/active_model 所在）。
// 显式设置 QWENPAW_SECRET_DIR（如 emptyDir 内部）优先，避免只读根文件系统上
// 默认的 "<WORKING_DIR>.secret" 兄弟路径不可写。
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

// writeQwenPawConfig 为 qwenpaw acp（2.x）生成最小配置：
// 凭证只写进临时目录（emptyDir/系统 tmp），进程退出即销毁，绝不写入持久工作区。
func writeQwenPawConfig(cfgDir, secretDir string, cfg *RuntimeConfig) error {
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

	// 根配置：仅声明 agent 档案引用。
	if err := write(filepath.Join(cfgDir, "config.json"), map[string]any{
		"agents": map[string]any{
			"active_agent": "default",
			"profiles": map[string]any{
				"default": map[string]any{
					"id":             "default",
					"workspace_dir":  agentDir,
					"enabled":        true,
				},
			},
		},
	}); err != nil {
		return err
	}
	// agent 档案：活动模型。
	if err := write(filepath.Join(agentDir, "agent.json"), map[string]any{
		"id":           "default",
		"name":         "default",
		"active_model": map[string]any{"provider_id": providerID, "model": cfg.Model},
	}); err != nil {
		return err
	}
	// 自定义 OpenAI 兼容 provider（含凭证，仅存临时目录）。
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
	// ProviderManager 的活动模型。
	if err := write(filepath.Join(secretDir, "providers", "active_model.json"),
		map[string]any{"provider_id": providerID, "model": cfg.Model}); err != nil {
		return err
	}
	return nil
}

// watchProc 在子进程退出时清理状态（自愈：下一次请求自动重启）。
// 唯一的 cmd.Wait() 调用方：killLocked 通过 exited 通道等待，避免并发 Wait。
func (a *ACPServer) watchProc(proc *acpProcess) {
	_ = proc.cmd.Wait()
	close(proc.exited)
	a.mu.Lock()
	if a.proc == proc {
		a.proc = nil
		a.connected = false
		a.sessions = make(map[string]string)
		a.handlers = make(map[string]UpdateHandler)
		// 冲掉所有等待中的请求，避免调用方永久阻塞。
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

// readLoop 逐行解析 stdout 上的 JSON-RPC 消息并分发。
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
			// 宽容处理：忽略非 JSON 行（例如 banner），但计数可观测。
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
	// server -> client 请求：权限请求（带 id，但不是对客户端请求的响应）。
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

// handlePermissionRequest 处理 server -> client 的权限请求。
// 默认 headless 安全策略：拒绝（不自动放行任何工具调用）；
// 可通过 PermissionPolicy 挂接控制面审批流。
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
		log.Printf("[acp] 权限请求已拒绝（headless 默认策略）: tool=%q session=%s",
			title, req.SessionID)
	}
	a.mu.Lock()
	_ = a.writeLocked(&rpcMessage{JSONRPC: "2.0", ID: msg.ID, Result: result})
	a.mu.Unlock()
}

// request 发送一个 JSON-RPC 请求并等待响应。
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

// notify 发送 JSON-RPC 通知（无响应）。
func (a *ACPServer) notify(method string, params map[string]any) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.proc == nil {
		return fmt.Errorf("%w: 子进程未启动", errACPTransport)
	}
	return a.writeLocked(&rpcMessage{JSONRPC: "2.0", Method: method, Params: params})
}

// writeLocked 需要持有 a.mu。
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
		// 子进程已退出：清理引用，下一次 ensureStarted 自动重启（自愈）。
		a.connected = false
		if a.proc != nil && a.proc.cmd.Process != nil {
			_ = a.proc.cmd.Process.Kill() // 确保被 watchProc 回收
		}
		return err
	}
	return nil
}

// SessionID 返回本地会话键对应的 ACP 会话 id；不存在则创建。
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

	// session/new 也带超时：qwenpaw 冷启动建会话可能较慢，但绝不允许永久挂起。
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

	// 可选：绕过 Tool Guard（仅当配置显式要求，信任沙箱场景）。
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

// Prompt 同步执行一次会话请求，流式更新交给 handler（agent_message_chunk 等）。
// 返回最终回复文本（由 handler 聚合时一并完成；此处仅做错误/stopReason 处理）。
func (a *ACPServer) Prompt(ctx context.Context, localKey, message string, handler UpdateHandler) (string, error) {
	reply, err := a.promptOnce(ctx, localKey, message, handler)
	if err != nil && errors.Is(err, errACPTransport) {
		// 自愈：子进程异常退出 → 重启并重试一次。
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

func (a *ACPServer) promptOnce(ctx context.Context, localKey, message string, handler UpdateHandler) (string, error) {
	sid, err := a.SessionID(ctx, localKey)
	if err != nil {
		return "", err
	}

	if handler == nil {
		handler = UpdateHandlerFunc(func(map[string]any) {})
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

// Cancel 发送 session/cancel 通知（异步，无响应）。
func (a *ACPServer) Cancel(sid string) error {
	return a.notify(acpMethodCancel, map[string]any{"sessionId": sid})
}

// CloseSession 关闭一个 ACP 会话（best-effort）。
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

// SyncConfig 在模型配置变化后调用：若 ACP 已运行且配置与启动时不同，
// 则重启子进程注入新凭证（凭证不落盘的体现）。
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

// Restart 停止并重启子进程。会话在 QwenPaw 侧为内存态，重启后自动重建；
// 对话历史由 agent-runtime 的 .agent/conversation.jsonl 持久化。
func (a *ACPServer) Restart(ctx context.Context) error {
	a.mu.Lock()
	a.stopping = true
	_ = a.killLocked()
	a.stopping = false
	a.mu.Unlock()

	// 给子进程一点退出时间，避免立即重启端口/资源冲突。
	select {
	case <-time.After(300 * time.Millisecond):
	case <-ctx.Done():
		return ctx.Err()
	}
	return a.start(ctx)
}

// Stop 停止 ACP 子进程（实例休眠/删除时调用）。会话数据保留在工作区。
func (a *ACPServer) Stop() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stopping = true
	_ = a.killLocked()
	a.stopping = false
	a.sessions = make(map[string]string)
	a.handlers = make(map[string]UpdateHandler)
}

// killLocked 需要持有 a.mu。SIGTERM 优雅退出，5 秒后 SIGKILL。
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
	// 子进程的内存态会话随之失效：清空映射，重启后重新 session/new。
	a.sessions = make(map[string]string)
	a.handlers = make(map[string]UpdateHandler)
	if proc.cmd.Process != nil {
		_ = proc.cmd.Process.Signal(os.Interrupt) // SIGTERM 语义（Unix）
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
