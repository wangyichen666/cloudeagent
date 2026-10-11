// Package docker 是本地容器沙箱提供方：每用户一个容器 + 命名卷。
package docker

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"

	"cloude-agent/internal/sandbox"
)

// Config 配置 Docker 容器后端。
type Config struct {
	Image                string // 如 cloude-agent:local
	RemoveVolumeOnDelete bool   // 删除时是否同时删除命名卷
}

// Provider 实现 sandbox.Provider。
type Provider struct {
	cfg Config
}

func New(cfg Config) (*Provider, error) {
	if cfg.Image == "" {
		return nil, fmt.Errorf("docker provider: Image 未配置")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		return nil, fmt.Errorf("docker provider: 未找到 docker: %w", err)
	}
	return &Provider{cfg: cfg}, nil
}

func (d *Provider) Name() string { return "docker" }

func (d *Provider) containerName(userID string) string { return "cloude-agent-" + userID }
func (d *Provider) volumeName(userID string) string    { return "cloude-agent-vol-" + userID }

func (d *Provider) Create(ctx context.Context, userID string) (*sandbox.Info, error) {
	args := []string{"run", "-d", "--name", d.containerName(userID),
		"-v", d.volumeName(userID) + ":/workspace",
		"-e", "CLOUDE_WORKSPACE=/workspace",
		"-p", "127.0.0.1::18585",
		"--label", "managed-by=cloude-agent",
		d.cfg.Image,
	}
	if _, err := runDocker(ctx, args...); err != nil {
		if strings.Contains(err.Error(), "Conflict") || strings.Contains(err.Error(), "already in use") {
			return d.Start(ctx, userID)
		}
		return nil, err
	}
	return d.startInfo(ctx, userID)
}

func (d *Provider) Start(ctx context.Context, userID string) (*sandbox.Info, error) {
	_, _ = runDocker(ctx, "start", d.containerName(userID))
	return d.startInfo(ctx, userID)
}

func (d *Provider) Stop(ctx context.Context, userID string) error {
	_, err := runDocker(ctx, "stop", "--time", "5", d.containerName(userID))
	return err
}

func (d *Provider) Delete(ctx context.Context, userID string) error {
	args := []string{"rm", "-f", d.containerName(userID)}
	if d.cfg.RemoveVolumeOnDelete {
		args = append(args, "-v")
	}
	_, err := runDocker(ctx, args...)
	return err
}

func (d *Provider) startInfo(ctx context.Context, userID string) (*sandbox.Info, error) {
	out, err := runDocker(ctx, "port", d.containerName(userID), "18585")
	if err != nil {
		return nil, err
	}
	addr := strings.TrimSpace(out)
	host, portStr, ok := strings.Cut(addr, ":")
	if !ok {
		return nil, fmt.Errorf("docker port 输出异常: %q", addr)
	}
	port := 0
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		return nil, fmt.Errorf("解析端口失败: %q", portStr)
	}
	return &sandbox.Info{
		UserID:    userID,
		Workspace: d.volumeName(userID),
		Endpoint:  fmt.Sprintf("http://%s:%d", host, port),
		Port:      port,
	}, nil
}

func runDocker(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("[docker] %v -> %s", args, strings.TrimSpace(string(out)))
		return "", fmt.Errorf("docker %v: %w (%s)", args, err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}
