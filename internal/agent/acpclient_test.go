package agent

// ACP 客户端网关测试：通过「测试二进制自举」启动一个 fake ACP server
// （真实的 exec 子进程 + stdio 管道），覆盖：
//   - initialize → session/new → session/prompt 全流程与流式增量
//   - session/request_permission 权限请求（默认拒绝 / 策略放行）
//   - 子进程崩溃后的自动重启（自愈）

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestACPServerHelper 是 fake ACP server 进程入口（由测试通过 exec 自举启动）。
func TestACPServerHelper(t *testing.T) {
	if os.Getenv("GO_WANT_ACP_HELPER") != "1" {
		return
	}
	mode := os.Getenv("GO_FAKE_ACP_MODE")
	sc := bufio.NewScanner(os.Stdin)
	enc := json.NewEncoder(os.Stdout)
	sessionID := "sess-1"
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var req rpcMessage
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			continue
		}
		if req.Method == "" {
			continue // 客户端响应，忽略
		}

		switch req.Method {
		case "initialize":
			_ = enc.Encode(&rpcMessage{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
				"protocolVersion": 1,
				"agentCapabilities": map[string]any{"loadSession": true},
				"agentInfo":        map[string]any{"name": "qwenpaw-fake", "version": "2.0.0-test"},
			}})
		case "session/new":
			_ = enc.Encode(&rpcMessage{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
				"sessionId": sessionID,
			}})
			if mode == "crash" {
				os.Exit(0) // 模拟「会话创建后、prompt 前」崩溃
			}
		case "session/prompt":
			if mode == "permission" {
				// 发送权限请求并等待客户端响应
				_ = enc.Encode(&rpcMessage{
					JSONRPC: "2.0",
					ID:      "perm-1",
					Method:  acpRequestPermission,
					Params: map[string]any{
						"sessionId": sessionID,
						"toolCall": map[string]any{
							"toolCallId": "tc-1",
							"title":      "shell: ls",
							"kind":       "execute",
						},
						"options": []any{
							map[string]any{"optionId": "allow_once", "name": "Allow", "kind": "allow_once"},
							map[string]any{"optionId": "deny", "name": "Deny", "kind": "reject_once"},
						},
					},
				})
				// 读取客户端对权限请求的响应
				if sc.Scan() {
					var resp rpcMessage
					_ = json.Unmarshal(sc.Bytes(), &resp)
					outcome := "none"
					if r, ok := resp.Result["outcome"].(string); ok {
						outcome = r
					}
					_ = enc.Encode(&rpcMessage{
						JSONRPC: "2.0",
						Method:  acpNotifyUpdate,
						Params: map[string]any{
							"sessionId": sessionID,
							"update": map[string]any{
								"sessionUpdate": "agent_message_chunk",
								"content":       map[string]any{"type": "text", "text": "permission=" + outcome},
							},
						},
					})
				}
			}
			_ = enc.Encode(&rpcMessage{
				JSONRPC: "2.0",
				Method:  acpNotifyUpdate,
				Params: map[string]any{
					"sessionId": sessionID,
					"update": map[string]any{
						"sessionUpdate": "agent_message_chunk",
						"content":       map[string]any{"type": "text", "text": "你好，"},
					},
				},
			})
			_ = enc.Encode(&rpcMessage{
				JSONRPC: "2.0",
				Method:  acpNotifyUpdate,
				Params: map[string]any{
					"sessionId": sessionID,
					"update": map[string]any{
						"sessionUpdate": "agent_thought_chunk",
						"content":       map[string]any{"type": "text", "text": "思考中..."},
					},
				},
			})
			_ = enc.Encode(&rpcMessage{
				JSONRPC: "2.0",
				Method:  acpNotifyUpdate,
				Params: map[string]any{
					"sessionId": sessionID,
					"update": map[string]any{
						"sessionUpdate": "agent_message_chunk",
						"content":       map[string]any{"type": "text", "text": "世界"},
					},
				},
			})
			_ = enc.Encode(&rpcMessage{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
				"stopReason": "end_turn",
			}})
		case "session/close":
			_ = enc.Encode(&rpcMessage{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{}})
		case "session/cancel":
			// 通知：无响应
		}
	}
	os.Exit(0)
}

func fakeSpawn(modeFn func(spawnNum int) string, spawns *int) func(string, string, []string) (*acpProcess, error) {
	return func(bin, workspace string, env []string) (*acpProcess, error) {
		mode := modeFn(*spawns)
		*spawns++
		cmd := exec.Command(bin, "-test.run=TestACPServerHelper")
		cmd.Env = append(os.Environ(),
			"GO_WANT_ACP_HELPER=1",
			"GO_FAKE_ACP_MODE="+mode,
		)
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return nil, err
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return nil, err
		}
		cmd.Stderr = io.Discard
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		return &acpProcess{cmd: cmd, stdin: stdin, stdout: stdout, exited: make(chan struct{})}, nil
	}
}

func TestWriteQwenPawConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := &RuntimeConfig{
		BaseURL: "https://api.example.com/v1",
		APIKey:  "sk-secret",
		Model:   "qwen-max",
	}
	if err := writeQwenPawConfig(dir, dir+".secret", cfg); err != nil {
		t.Fatalf("writeQwenPawConfig: %v", err)
	}

	files := map[string]string{
		"config.json":                   "agents",
		"workspaces/default/agent.json": "runtime-openai",
	}
	for rel, want := range files {
		data, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			t.Fatalf("读取 %s: %v", rel, err)
		}
		if !strings.Contains(string(data), want) {
			t.Errorf("%s 应包含 %q，实际: %s", rel, want, data)
		}
	}
	// SECRET_DIR 是 <cfgDir>.secret（后缀形式，非子目录），providers 在其下。
	for rel, want := range map[string]string{
		"providers/custom/runtime-openai.json": "api_key",
		"providers/active_model.json":          "qwen-max",
	} {
		data, err := os.ReadFile(filepath.Join(dir+".secret", rel))
		if err != nil {
			t.Fatalf("读取 %s: %v", rel, err)
		}
		if !strings.Contains(string(data), want) {
			t.Errorf("%s 应包含 %q，实际: %s", rel, want, data)
		}
	}

	// 凭证只出现在临时目录，且文件权限收紧。
	providerFile := filepath.Join(dir+".secret", "providers/custom/runtime-openai.json")
	fi, _ := os.Stat(providerFile)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("provider 文件权限应为 0600，实际 %o", fi.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(dir, "api_key")); !os.IsNotExist(err) {
		t.Fatal("凭证不应出现在配置根目录其他位置")
	}
}

func newTestACPServer(t *testing.T, mode string) (*ACPServer, *int) {
	t.Helper()
	cfg := NewConfigManager("", nil)
	cfg.Apply(&RuntimeConfig{
		BaseURL: "https://api.openai.example.com/v1",
		APIKey:  "sk-test",
		Model:   "qwen-max",
	})
	ws := t.TempDir()
	srv := NewACPServer(os.Args[0], ws, cfg)
	spawns := 0
	srv.spawnFn = fakeSpawn(func(int) string { return mode }, &spawns)
	t.Cleanup(srv.Stop)
	return srv, &spawns
}

func TestACPServerFullFlow(t *testing.T) {
	srv, _ := newTestACPServer(t, "echo")
	ctx := context.Background()

	var text strings.Builder
	var thoughts []string
	handler := UpdateHandlerFunc(func(u map[string]any) {
		kind, _ := u["sessionUpdate"].(string)
		c, _ := u["content"].(map[string]any)
		chunk, _ := c["text"].(string)
		switch kind {
		case "agent_message_chunk":
			text.WriteString(chunk)
		case "agent_thought_chunk":
			thoughts = append(thoughts, chunk)
		}
	})

	if _, err := srv.Prompt(ctx, "u-1", "你好", handler); err != nil {
		t.Fatalf("Prompt 失败: %v", err)
	}
	if got := text.String(); got != "你好，世界" {
		t.Fatalf("回复文本 = %q, want %q", got, "你好，世界")
	}
	if len(thoughts) != 1 || thoughts[0] != "思考中..." {
		t.Fatalf("thoughts = %v", thoughts)
	}

	name, version := srv.AgentInfo()
	if name != "qwenpaw-fake" || version != "2.0.0-test" {
		t.Fatalf("AgentInfo = %s %s", name, version)
	}
	if !srv.Connected() {
		t.Fatal("期望已连接")
	}

	// 同一 localKey 复用会话，不重复 session/new
	sid1, _ := srv.SessionID(ctx, "u-1")
	sid2, _ := srv.SessionID(ctx, "u-1")
	if sid1 != sid2 {
		t.Fatalf("会话复用失败: %s != %s", sid1, sid2)
	}
}

func TestACPServerPermissionDeniedByDefault(t *testing.T) {
	srv, _ := newTestACPServer(t, "permission")
	var text strings.Builder
	handler := UpdateHandlerFunc(func(u map[string]any) {
		if kind, _ := u["sessionUpdate"].(string); kind == "agent_message_chunk" {
			if c, ok := u["content"].(map[string]any); ok {
				text.WriteString(c["text"].(string))
			}
		}
	})
	if _, err := srv.Prompt(context.Background(), "u-1", "运行 ls", handler); err != nil {
		t.Fatalf("Prompt 失败: %v", err)
	}
	if !strings.Contains(text.String(), "permission=cancelled") {
		t.Fatalf("默认策略应拒绝权限请求，got %q", text.String())
	}
}

func TestACPServerPermissionPolicyAllow(t *testing.T) {
	srv, _ := newTestACPServer(t, "permission")
	srv.PermissionPolicy = func(req PermissionRequest) string {
		if len(req.Options) == 0 {
			t.Fatal("权限请求应携带 options")
		}
		return "allow_once"
	}
	var text strings.Builder
	handler := UpdateHandlerFunc(func(u map[string]any) {
		if kind, _ := u["sessionUpdate"].(string); kind == "agent_message_chunk" {
			if c, ok := u["content"].(map[string]any); ok {
				text.WriteString(c["text"].(string))
			}
		}
	})
	if _, err := srv.Prompt(context.Background(), "u-1", "运行 ls", handler); err != nil {
		t.Fatalf("Prompt 失败: %v", err)
	}
	if !strings.Contains(text.String(), "permission=selected") {
		t.Fatalf("策略应放行权限请求，got %q", text.String())
	}
}

// TestACPServerSelfHeal 验证子进程崩溃后下一次 Prompt 自动重启并成功。
func TestACPServerSelfHeal(t *testing.T) {
	cfg := NewConfigManager("", nil)
	cfg.Apply(&RuntimeConfig{
		BaseURL: "https://api.openai.example.com/v1",
		APIKey:  "sk-test",
		Model:   "qwen-max",
	})
	srv := NewACPServer(os.Args[0], t.TempDir(), cfg)
	spawns := 0
	srv.spawnFn = fakeSpawn(func(n int) string {
		if n == 0 {
			return "crash"
		}
		return "echo"
	}, &spawns)
	t.Cleanup(srv.Stop)

	// 第一次：fake 在 initialize 后立即退出（crash 模式）。
	sid, err := srv.SessionID(context.Background(), "u-1")
	if err != nil {
		t.Fatalf("首次 SessionID 失败: %v", err)
	}
	if sid != "sess-1" {
		t.Fatalf("sid = %q", sid)
	}
	time.Sleep(300 * time.Millisecond) // 等 watchProc 清理死进程

	var text strings.Builder
	handler := UpdateHandlerFunc(func(u map[string]any) {
		if kind, _ := u["sessionUpdate"].(string); kind == "agent_message_chunk" {
			if c, ok := u["content"].(map[string]any); ok {
				text.WriteString(c["text"].(string))
			}
		}
	})
	if _, err := srv.Prompt(context.Background(), "u-1", "你好", handler); err != nil {
		t.Fatalf("崩溃后 Prompt 未自愈: %v", err)
	}
	if got := text.String(); got != "你好，世界" {
		t.Fatalf("自愈后回复 = %q", got)
	}
	if spawns < 2 {
		t.Fatalf("期望至少重启 1 次，实际 spawn=%d", spawns)
	}
}

func TestACPServerCancel(t *testing.T) {
	srv, _ := newTestACPServer(t, "echo")
	sid, err := srv.SessionID(context.Background(), "u-1")
	if err != nil {
		t.Fatalf("SessionID 失败: %v", err)
	}
	if err := srv.Cancel(sid); err != nil {
		t.Fatalf("Cancel 失败: %v", err)
	}
}

func TestACPServerMockRejected(t *testing.T) {
	cfg := NewConfigManager("", nil) // 默认 mock://
	srv := NewACPServer(os.Args[0], t.TempDir(), cfg)
	_, err := srv.SessionID(context.Background(), "u-1")
	if err == nil {
		t.Fatal("mock 配置下不应启动 qwenpaw")
	}
	if !strings.Contains(fmt.Sprint(err), "mock") {
		t.Fatalf("错误应提示 mock 配置: %v", err)
	}
}
