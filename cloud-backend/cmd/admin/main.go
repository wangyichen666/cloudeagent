// control-plane：控制面统一入口。
// 用 --sandbox 选择沙箱提供方：e2b（OpenKruise Agents E2B SDK）/ process（本地进程）/ docker（容器）。
//
// E2B 用法（本地经 port-forward 连 sandbox-manager/gateway）：
//
//	kubectl port-forward -n sandbox-system svc/sandbox-manager 8080:8080 &
//	kubectl port-forward -n sandbox-system svc/sandbox-gateway  7788:7788 &
//	./bin/control-plane \
//	  --sandbox e2b \
//	  --e2b-api-url http://127.0.0.1:8080 \
//	  --e2b-sandbox-base-url http://127.0.0.1:7788 \
//	  --e2b-api-key e2b_xxx \
//	  --gateway http://127.0.0.1:18500
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

	apicp "cloude-agent/pkg/apiserver/controlplane"
	"cloude-agent/pkg/db"
	"cloude-agent/pkg/sandboxprovider"
	sandboxDocker "cloude-agent/pkg/sandboxprovider/docker"
	sandboxE2B "cloude-agent/pkg/sandboxprovider/e2b"
	sandboxProcess "cloude-agent/pkg/sandboxprovider/process"
	"cloude-agent/pkg/sandboxruntime"
	"cloude-agent/pkg/services"
)

func main() {
	var (
		listen     = flag.String("listen", ":8080", "控制面监听地址")
		sandboxTyp = flag.String("sandbox", "e2b", "沙箱提供方: e2b | process | docker")
		storeTyp   = flag.String("store", "memory", "状态面存储: memory | postgres")
		dsn        = flag.String("dsn", "", "PostgreSQL DSN（store=postgres 时必填）")
		redisAddr  = flag.String("redis-addr", "", "Redis 地址（可选，用于多副本热状态/分布式锁）")

		namespace  = flag.String("namespace", "cloude-agent", "命名空间（派生实例 token）")
		adminToken = flag.String("admin-token", "dev-admin-token", "管理面 Bearer Token")
		gatewayURL = flag.String("gateway", "http://127.0.0.1:18500", "cloud-gateway 数据面网关地址")

		// process / docker 沙箱参数
		agentBin     = flag.String("agent-bin", "./bin/agent-runtime", "agent-runtime 二进制路径（process 沙箱）")
		dataDir      = flag.String("data-dir", "./data", "工作区/日志根目录（process 沙箱）")
		agentImage   = flag.String("agent-image", "cloude-agent:local", "实例镜像（docker 沙箱）")
		removeVolume = flag.Bool("docker-remove-volume", true, "删除实例时同时删除 Docker 命名卷")

		// E2B 沙箱参数
		e2bAPIURL    = flag.String("e2b-api-url", "http://sandbox-manager.sandbox-system:8080", "sandbox-manager API URL")
		e2bSbxBase   = flag.String("e2b-sandbox-base-url", "http://sandbox-gateway.sandbox-system:7788", "sandbox-gateway 数据面 URL")
		e2bAPIKey    = flag.String("e2b-api-key", "", "E2B API Key")
		e2bTemplate  = flag.String("e2b-template", "agent-pool", "SandboxSet 预热池名")
		e2bTimeout   = flag.Int("e2b-timeout", 1800, "沙箱空闲 TTL（秒）")
		e2bWaitReady = flag.Duration("e2b-wait-ready", 90*time.Second, "沙箱内 agent-runtime 就绪超时")

		idleTimeout  = flag.Duration("idle-timeout", 0, "空闲自动暂停阈值，0=禁用")
		reapInterval = flag.Duration("reap-interval", 15*time.Second, "reaper 扫描间隔")

		model     = flag.String("model", "mock-gpt-4o", "席位默认模型")
		provider  = flag.String("provider", "seat-service", "席位默认 provider")
		baseURL   = flag.String("base-url", "mock://", "席位默认 baseURL")
		apiKey    = flag.String("api-key", "sk-local-mock-seat", "席位默认 apiKey（仅内存注入）")
		modelList = flag.String("models", "mock-gpt-4o,mock-claude-sonnet", "可用模型列表（逗号分隔）")
	)
	flag.Parse()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// ---- 状态面 ----
	var st db.Store
	var err error
	switch *storeTyp {
	case "memory":
		st = db.NewMemory()
	case "postgres":
		if *dsn == "" {
			log.Fatal("store=postgres 时需要 --dsn")
		}
		if st, err = db.NewPostgres(ctx, *dsn); err != nil {
			log.Fatalf("postgres: %v", err)
		}
	default:
		log.Fatalf("未知 store: %s", *storeTyp)
	}
	defer st.Close()

	var cache db.ModelConfigCache
	if *redisAddr != "" {
		if cache, err = db.NewRedisCache(*redisAddr, 24*time.Hour); err != nil {
			log.Fatalf("redis: %v", err)
		}
	} else {
		cache = db.NewMemoryCache(24 * time.Hour)
	}

	// ---- 沙箱提供方 ----
	var bk sandboxprovider.Provider
	switch *sandboxTyp {
	case "e2b":
		bk, err = sandboxE2B.New(sandboxE2B.Config{
			APIURL:         *e2bAPIURL,
			SandboxBaseURL: *e2bSbxBase,
			APIKey:         *e2bAPIKey,
			Scheme:         "http",
			Template:       *e2bTemplate,
			TimeoutSec:     int32(*e2bTimeout),
			AgentPort:      18585,
			WaitReady:      *e2bWaitReady,
		})
		if err != nil {
			log.Fatalf("e2b provider: %v", err)
		}
		// 启动恢复：从 Store 读已有实例，重建 userID->sandboxID 映射
		if insts, lerr := st.ListInstances(ctx); lerr == nil {
			if seeder, ok := bk.(sandboxprovider.Seeder); ok {
				m := map[string]string{}
				for _, inst := range insts {
					if inst.SandboxID != "" {
						m[inst.UserID] = inst.SandboxID
					}
				}
				seeder.Seed(m)
			}
		}
	case "process":
		bk, err = sandboxProcess.New(sandboxProcess.Config{
			AgentBin:              *agentBin,
			DataDir:               *dataDir,
			KeepWorkspaceOnDelete: false,
		})
	case "docker":
		bk, err = sandboxDocker.New(sandboxDocker.Config{
			Image:                *agentImage,
			RemoveVolumeOnDelete: *removeVolume,
		})
	default:
		log.Fatalf("未知 sandbox: %s（可选 e2b / process / docker）", *sandboxTyp)
	}
	if err != nil {
		log.Fatalf("init sandbox provider: %v", err)
	}
	log.Printf("[control-plane] sandbox provider: %s", bk.Name())

	// ---- 席位服务 ----
	seats := sandboxruntime.NewMockSeatService()
	seats.Model = *model
	seats.Provider = *provider
	seats.BaseURL = *baseURL
	seats.APIKey = *apiKey
	seats.Models = splitModels(*modelList)

	manager := sandboxruntime.NewManager(st, cache, bk, seats, sandboxruntime.NewGatewayClient(*gatewayURL))
	reaper := sandboxruntime.NewReaper(manager, *idleTimeout, *reapInterval)
	reviewWorker := services.NewReviewWorker(st, manager.GetModelConfig)
	auth := apicp.NewAuthenticator(*namespace, *adminToken)

	server := apicp.NewServer(apicp.Config{
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
