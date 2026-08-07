package backend

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
)

// ProcessConfig 配置本地进程后端。
type ProcessConfig struct {
	AgentBin              string // agent-runtime 可执行文件路径
	DataDir               string // 工作区根目录（≈ 数据面本地根）
	KeepWorkspaceOnDelete bool   // 删除实例时是否保留工作区目录
}

// Process 后端：每个用户 = 一个 agent-runtime 进程 + 一个工作区目录。
// 休眠 = SIGTERM（目录保留），唤醒 = 重启进程（挂回同一目录，数据无缝恢复）。
// 稳定路由标识 = 记录在 .agent/port 的端口（对应 StatefulSet 的稳定 Pod DNS）。
type Process struct {
	cfg   ProcessConfig
	mu    sync.Mutex
	procs map[string]*exec.Cmd
}

func NewProcess(cfg ProcessConfig) (*Process, error) {
	if cfg.AgentBin == "" {
		return nil, fmt.Errorf("process backend: AgentBin 未配置")
	}
	if _, err := os.Stat(cfg.AgentBin); err != nil {
		return nil, fmt.Errorf("process backend: 找不到 agent-runtime 二进制 %s: %w", cfg.AgentBin, err)
	}
	for _, dir := range []string{cfg.DataDir, filepath.Join(cfg.DataDir, "workspaces"), filepath.Join(cfg.DataDir, "logs")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	return &Process{cfg: cfg, procs: make(map[string]*exec.Cmd)}, nil
}

func (p *Process) Name() string { return "process" }

func (p *Process) workspaceDir(userID string) string {
	return filepath.Join(p.cfg.DataDir, "workspaces", userID)
}

func (p *Process) agentDir(userID string) string {
	return filepath.Join(p.workspaceDir(userID), ".agent")
}

func (p *Process) portFile(userID string) string {
	return filepath.Join(p.agentDir(userID), "port")
}

func (p *Process) pidFile(userID string) string {
	return filepath.Join(p.agentDir(userID), "pid")
}

func (p *Process) logFile(userID string) string {
	return filepath.Join(p.cfg.DataDir, "logs", userID+".log")
}

func (p *Process) Create(ctx context.Context, userID string) (*Info, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ws := p.workspaceDir(userID)
	if err := os.MkdirAll(p.agentDir(userID), 0o755); err != nil {
		return nil, err
	}
	// 分配稳定端口并记录到工作区（非敏感元数据）。
	port, err := p.portFor(userID)
	if err != nil {
		return nil, err
	}
	if err := p.spawn(userID, port); err != nil {
		return nil, err
	}
	return &Info{UserID: userID, Workspace: ws, Endpoint: fmt.Sprintf("http://127.0.0.1:%d", port), Port: port}, nil
}

func (p *Process) Start(ctx context.Context, userID string) (*Info, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	port, err := p.portFor(userID)
	if err != nil {
		return nil, err
	}
	ws := p.workspaceDir(userID)
	// 幂等：若旧进程仍存活且健康，直接复用（控制面重启后友好）。
	if p.alive(userID) {
		endpoint := fmt.Sprintf("http://127.0.0.1:%d", port)
		if err := WaitHealth(ctx, endpoint, 3*time.Second); err == nil {
			return &Info{UserID: userID, Workspace: ws, Endpoint: endpoint, Port: port}, nil
		}
	}
	if err := p.spawn(userID, port); err != nil {
		return nil, err
	}
	return &Info{UserID: userID, Workspace: ws, Endpoint: fmt.Sprintf("http://127.0.0.1:%d", port), Port: port}, nil
}

func (p *Process) Stop(ctx context.Context, userID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	// 正常路径：持有 exec.Cmd 句柄，SIGTERM 后等待进程真正退出（顺带回收僵尸进程）。
	if cmd := p.procs[userID]; cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() {
			_ = cmd.Wait()
			close(done)
		}()
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
	// 恢复路径：控制面重启后不持有句柄，仅按 pid 文件回收孤儿进程。
	if pid, ok := p.readPID(userID); ok {
		_ = syscall.Kill(pid, syscall.SIGTERM)
		for i := 0; i < 20; i++ { // 最多等 5s
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

func (p *Process) Delete(ctx context.Context, userID string) error {
	if err := p.Stop(ctx, userID); err != nil {
		return err
	}
	ws := p.workspaceDir(userID)
	if p.cfg.KeepWorkspaceOnDelete {
		log.Printf("[backend] 保留工作区 %s（KeepWorkspaceOnDelete=true）", ws)
		return nil
	}
	return os.RemoveAll(ws)
}

func (p *Process) spawn(userID string, port int) error {
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
	// 立即释放文件句柄，交给子进程持有（Unix 下 stdout 可被继承后关闭父副本）。
	logFile.Close()
	p.procs[userID] = cmd
	_ = os.WriteFile(p.pidFile(userID), []byte(strconv.Itoa(cmd.Process.Pid)), 0o644)
	return nil
}

func (p *Process) portFor(userID string) (int, error) {
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

func (p *Process) readPID(userID string) (int, bool) {
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

// alive 检查进程是否存活（Unix kill(pid,0)）。
func (p *Process) alive(userID string) bool {
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
