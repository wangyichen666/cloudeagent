package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"cloude-agent/internal/backend"
	"cloude-agent/internal/controlplane"
	"cloude-agent/internal/store"
)

func main() {
	var (
		listen     = flag.String("listen", ":8080", "控制面监听地址")
		backendTyp = flag.String("backend", "process", "数据面后端: process(本地进程) | docker(每用户容器)")
		storeTyp   = flag.String("store", "memory", "状态面存储: memory | postgres")
		dsn        = flag.String("dsn", "", "PostgreSQL DSN（store=postgres 时必填）")
		redisAddr  = flag.String("redis-addr", "", "Redis 地址（可选，用于多副本热状态/分布式锁）")

		agentBin     = flag.String("agent-bin", "./bin/agent-runtime", "agent-runtime 二进制路径（process 后端）")
		dataDir      = flag.String("data-dir", "./data", "工作区/日志根目录（process 后端）")
		agentImage   = flag.String("agent-image", "cloude-agent:local", "实例镜像（docker 后端）")
		removeVolume = flag.Bool("docker-remove-volume", true, "删除实例时同时删除 Docker 命名卷（对应 PVC）")

		namespace  = flag.String("namespace", "cloude-agent", "命名空间（用于派生实例 token 与 K8s 命名）")
		adminToken = flag.String("admin-token", "dev-admin-token", "管理面 Bearer Token")

		idleTimeout  = flag.Duration("idle-timeout", 0, "空闲自动休眠阈值，0=禁用")
		reapInterval = flag.Duration("reap-interval", 15*time.Second, "idle reaper 扫描间隔")

		model     = flag.String("model", "mock-gpt-4o", "席位服务默认模型")
		provider  = flag.String("provider", "seat-service", "席位服务默认 provider")
		baseURL   = flag.String("base-url", "mock://", "席位服务默认 baseURL（mock:// 表示本地 mock LLM）")
		apiKey    = flag.String("api-key", "sk-local-mock-seat", "席位服务默认 apiKey（仅内存注入，不落盘）")
		modelList = flag.String("models", "mock-gpt-4o,mock-claude-sonnet", "可用模型列表（逗号分隔）")
	)
	flag.Parse()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// ---- 状态面 ----
	var st store.Store
	var err error
	switch *storeTyp {
	case "memory":
		st = store.NewMemory()
	case "postgres":
		if *dsn == "" {
			log.Fatal("store=postgres 时需要 --dsn")
		}
		st, err = store.NewPostgres(ctx, *dsn)
		if err != nil {
			log.Fatalf("postgres: %v", err)
		}
	default:
		log.Fatalf("未知 store: %s", *storeTyp)
	}
	defer st.Close()

	// ---- 热状态（模型配置缓存/锁） ----
	var cache store.ModelConfigCache
	if *redisAddr != "" {
		cache, err = store.NewRedisCache(*redisAddr, 24*time.Hour)
		if err != nil {
			log.Fatalf("redis: %v", err)
		}
	} else {
		cache = store.NewMemoryCache(24 * time.Hour)
	}

	// ---- 数据面 ----
	var bk backend.InstanceBackend
	switch *backendTyp {
	case "process":
		bk, err = backend.NewProcess(backend.ProcessConfig{
			AgentBin:              *agentBin,
			DataDir:               *dataDir,
			KeepWorkspaceOnDelete: false,
		})
	case "docker":
		bk, err = backend.NewDocker(backend.DockerConfig{
			Image:                *agentImage,
			RemoveVolumeOnDelete: *removeVolume,
		})
	default:
		log.Fatalf("未知 backend: %s", *backendTyp)
	}
	if err != nil {
		log.Fatalf("init backend: %v", err)
	}

	// ---- 席位服务（身份 -> 模型配置） ----
	seats := controlplane.NewMockSeatService()
	seats.Model = *model
	seats.Provider = *provider
	seats.BaseURL = *baseURL
	seats.APIKey = *apiKey
	seats.Models = splitModels(*modelList)

	manager := controlplane.NewManager(st, cache, bk, seats)
	reaper := controlplane.NewReaper(manager, *idleTimeout, *reapInterval)
	reviewWorker := controlplane.NewReviewWorker(st, manager.GetModelConfig)
	auth := controlplane.NewAuthenticator(*namespace, *adminToken)

	server := controlplane.NewServer(controlplane.Config{
		Listen:  *listen,
		Manager: manager,
		Reaper:  reaper,
		Review:  reviewWorker,
		Auth:    auth,
	})

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		log.Println("收到退出信号，优雅关闭...")
		cancel()
	}()

	if err := server.Run(ctx); err != nil {
		log.Fatalf("control-plane: %v", err)
	}
}

func splitModels(s string) []string {
	var out []string
	for _, m := range strings.Split(s, ",") {
		if m != "" {
			out = append(out, m)
		}
	}
	return out
}
