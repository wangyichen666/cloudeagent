// Package process 是本地进程沙箱提供方：每用户一个 agent-runtime 进程 + 工作区目录。
// 休眠 = SIGTERM（目录保留），唤醒 = 重启进程。
package process

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"cloude-agent/pkg/sandboxprovider"
)

// Config 配置本地进程后端。
type Config struct {
	AgentBin              string // agent-runtime 可执行文件路径
	DataDir               string // 工作区根目录
	KeepWorkspaceOnDelete bool   // 删除时是否保留工作区
}

// Provider 实现 sandboxprovider.Provider。
type Provider struct {
	cfg   Config
	mu    sync.Mutex
	procs map[string]*exec.Cmd
}

func New(cfg Config) (*Provider, error) {
	if cfg.AgentBin == "" {
		return nil, fmt.Errorf("process provider: AgentBin 未配置")
	}
	if _, err := os.Stat(cfg.AgentBin); err != nil {
		return nil, fmt.Errorf("process provider: 找不到 agent-runtime 二进制 %s: %w", cfg.AgentBin, err)
	}
	for _, dir := range []string{cfg.DataDir, filepath.Join(cfg.DataDir, "workspaces"), filepath.Join(cfg.DataDir, "logs")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	return &Provider{cfg: cfg, procs: make(map[string]*exec.Cmd)}, nil
}

func (p *Provider) Name() string { return "process" }

func (p *Provider) workspaceDir(userID string) string {
	return filepath.Join(p.cfg.DataDir, "workspaces", userID)
}

func (p *Provider) agentDir(userID string) string {
	return filepath.Join(p.workspaceDir(userID), ".agent")
}

func (p *Provider) portFile(userID string) string {
	return filepath.Join(p.agentDir(userID), "port")
}

func (p *Provider) pidFile(userID string) string {
	return filepath.Join(p.agentDir(userID), "pid")
}

func (p *Provider) logFile(userID string) string {
	return filepath.Join(p.cfg.DataDir, "logs", userID+".log")
}

func (p *Provider) Create(ctx context.Context, userID string) (*sandboxprovider.Info, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ws := p.workspaceDir(userID)
	if err := os.MkdirAll(p.agentDir(userID), 0o755); err != nil {
		return nil, err
	}
	port, err := p.portFor(userID)
	if err != nil {
		return nil, err
	}
	if err := p.spawn(userID, port); err != nil {
		return nil, err
	}
	return &sandboxprovider.Info{UserID: userID, Workspace: ws, Endpoint: fmt.Sprintf("http://127.0.0.1:%d", port), Port: port}, nil
}

func (p *Provider) Start(ctx context.Context, userID string) (*sandboxprovider.Info, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	port, err := p.portFor(userID)
	if err != nil {
		return nil, err
	}
	ws := p.workspaceDir(userID)
	if p.alive(userID) {
		endpoint := fmt.Sprintf("http://127.0.0.1:%d", port)
		return &sandboxprovider.Info{UserID: userID, Workspace: ws, Endpoint: endpoint, Port: port}, nil
	}
	if err := p.spawn(userID, port); err != nil {
		return nil, err
	}
	return &sandboxprovider.Info{UserID: userID, Workspace: ws, Endpoint: fmt.Sprintf("http://127.0.0.1:%d", port), Port: port}, nil
}

func (p *Provider) Stop(ctx context.Context, userID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if cmd := p.procs[userID]; cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		delete(p.procs, userID)
		_ = os.Remove(p.pidFile(userID))
		return nil
	}
	if pid, ok := p.readPID(userID); ok {
		_ = syscall.Kill(pid, syscall.SIGTERM)
		for i := 0; i < 20; i++ {
			if !p.alive(userID) {
				break
			}
			time.Sleep(250 * time.Millisecond)
		}
		if p.alive(userID) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	delete(p.procs, userID)
	_ = os.Remove(p.pidFile(userID))
	return nil
}

func (p *Provider) Delete(ctx context.Context, userID string) error {
	if err := p.Stop(ctx, userID); err != nil {
		return err
	}
	ws := p.workspaceDir(userID)
	if p.cfg.KeepWorkspaceOnDelete {
		log.Printf("[process] 保留工作区 %s", ws)
		return nil
	}
	return os.RemoveAll(ws)
}

func (p *Provider) spawn(userID string, port int) error {
	ws := p.workspaceDir(userID)
	logFile, err := os.OpenFile(p.logFile(userID), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	cmd := exec.Command(p.cfg.AgentBin,
		"--listen", fmt.Sprintf("127.0.0.1:%d", port),
		"--workspace", ws,
	)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("start agent process: %w", err)
	}
	logFile.Close()
	p.procs[userID] = cmd
	_ = os.WriteFile(p.pidFile(userID), []byte(strconv.Itoa(cmd.Process.Pid)), 0o644)
	return nil
}

func (p *Provider) portFor(userID string) (int, error) {
	if data, err := os.ReadFile(p.portFile(userID)); err == nil {
		if port, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && port > 0 {
			return port, nil
		}
	}
	port, err := freePort()
	if err != nil {
		return 0, err
	}
	if err := os.WriteFile(p.portFile(userID), []byte(strconv.Itoa(port)), 0o644); err != nil {
		return 0, err
	}
	return port, nil
}

func (p *Provider) readPID(userID string) (int, bool) {
	data, err := os.ReadFile(p.pidFile(userID))
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

func (p *Provider) alive(userID string) bool {
	pid, ok := p.readPID(userID)
	if !ok {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}
