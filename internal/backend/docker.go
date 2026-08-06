package backend

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"
)

// DockerConfig 配置 Docker 容器后端。
type DockerConfig struct {
	Image                 string // 如 cloude-agent:local
	RemoveVolumeOnDelete  bool   // 删除实例时是否同时删除命名卷（对应 PVC）
}

// Docker 后端：每用户一个容器 + 一个命名卷。
// 这是本地开发中最接近「StatefulSet + PVC」的语义：
//   - 容器名/卷名稳定，路由端口在容器重启后不变；
//   - 休眠 = docker stop（容器删除但卷保留），唤醒 = docker start。
// 注意：凭证不写进卷，仅通过控制面 HTTP 注入内存（文档 6.2）。
type Docker struct {
	cfg DockerConfig
}

func NewDocker(cfg DockerConfig) (*Docker, error) {
	if cfg.Image == "" {
		return nil, fmt.Errorf("docker backend: Image 未配置")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		return nil, fmt.Errorf("docker backend: 未找到 docker: %w", err)
	}
	return &Docker{cfg: cfg}, nil
}

func (d *Docker) Name() string { return "docker" }

func (d *Docker) containerName(userID string) string { return "cloude-agent-" + userID }
func (d *Docker) volumeName(userID string) string     { return "cloude-agent-vol-" + userID }

func (d *Docker) Create(ctx context.Context, userID string) (*Info, error) {
	// 幂等：容器已存在则直接 Start。
	args := []string{"run", "-d", "--name", d.containerName(userID),
		"-v", d.volumeName(userID) + ":/workspace",
		"-e", "CLOUDE_WORKSPACE=/workspace",
		"-p", "127.0.0.1::18585",
		"--label", "managed-by=cloude-agent",
		d.cfg.Image,
	}
	if _, err := runDocker(ctx, args...); err != nil {
		// 容器名冲突说明已存在 → 唤醒路径
		if strings.Contains(err.Error(), "Conflict") || strings.Contains(err.Error(), "already in use") {
			return d.Start(ctx, userID)
		}
		return nil, err
	}
	return d.startInfo(ctx, userID)
}

func (d *Docker) Start(ctx context.Context, userID string) (*Info, error) {
	_, _ = runDocker(ctx, "start", d.containerName(userID))
	return d.startInfo(ctx, userID)
}

func (d *Docker) Stop(ctx context.Context, userID string) error {
	_, err := runDocker(ctx, "stop", "--time", "5", d.containerName(userID))
	return err
}

func (d *Docker) Delete(ctx context.Context, userID string) error {
	args := []string{"rm", "-f", d.containerName(userID)}
	if d.cfg.RemoveVolumeOnDelete {
		args = append(args, "-v")
	}
	_, err := runDocker(ctx, args...)
	return err
}

func (d *Docker) startInfo(ctx context.Context, userID string) (*Info, error) {
	out, err := runDocker(ctx, "port", d.containerName(userID), "18585")
	if err != nil {
		return nil, err
	}
	// 输出形如 "127.0.0.1:54321"
	addr := strings.TrimSpace(out)
	host, portStr, ok := strings.Cut(addr, ":")
	if !ok {
		return nil, fmt.Errorf("docker port 输出异常: %q", addr)
	}
	port := 0
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		return nil, fmt.Errorf("解析端口失败: %q", portStr)
	}
	return &Info{
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
