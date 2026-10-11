// control-plane-e2b：全 E2B 路线的控制面入口。
// 数据面用 OpenKruise Agents 的 E2B SDK 创建/唤醒/暂停/销毁沙箱，
// 不直接操作 agents.kruise.io CRD。
//
// 用法（本地经 port-forward 连 sandbox-manager/gateway）：
//
//	kubectl port-forward -n sandbox-system svc/sandbox-manager 8080:8080 &
//	kubectl port-forward -n sandbox-system svc/sandbox-gateway  7788:7788 &
//	./bin/control-plane-e2b \
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

	"cloude-agent/internal/controlplane"
	"cloude-agent/internal/gateway"
	sandboxE2B "cloude-agent/internal/sandbox/e2b"
	"cloude-agent/internal/store"
)

func main() {
	var (
		listen     = flag.String("listen", ":8080", "控制面监听地址")
		storeTyp   = flag.String("store", "memory", "状态面存储: memory | postgres")
		dsn        = flag.String("dsn", "", "PostgreSQL DSN")
		redisAddr  = flag.String("redis-addr", "", "Redis 地址（可选）")
		namespace  = flag.String("namespace", "cloude-agent", "命名空间（派生实例 token）")
		adminToken = flag.String("admin-token", "dev-admin-token", "管理面 Bearer Token")
		gatewayURL = flag.String("gateway", "http://127.0.0.1:18500", "cloud-gateway 地址")

		e2bAPIURL     = flag.String("e2b-api-url", "http://sandbox-manager.sandbox-system:8080", "sandbox-manager API URL")
		e2bSbxBase    = flag.String("e2b-sandbox-base-url", "http://sandbox-gateway.sandbox-system:7788", "sandbox-gateway 数据面 URL")
		e2bAPIKey     = flag.String("e2b-api-key", "", "E2B API Key")
		e2bTemplate   = flag.String("e2b-template", "agent-pool", "SandboxSet 预热池名")
		e2bTimeout    = flag.Int("e2b-timeout", 1800, "沙箱空闲 TTL（秒）")
		e2bWaitReady  = flag.Duration("e2b-wait-ready", 90*time.Second, "沙箱内 agent-runtime 就绪超时")

		idleTimeout  = flag.Duration("idle-timeout", 0, "空闲自动暂停阈值，0=禁用")
		reapInterval = flag.Duration("reap-interval", 15*time.Second, "reaper 扫描间隔")

		model     = flag.String("model", "mock-gpt-4o", "席位默认模型")
		provider  = flag.String("provider", "seat-service", "席位默认 provider")
		baseURL   = flag.String("base-url", "mock://", "席位默认 baseURL")
		apiKey    = flag.String("api-key", "sk-local-mock-seat", "席位默认 apiKey（仅内存注入）")
		modelList = flag.String("models", "mock-gpt-4o,mock-claude-sonnet", "可用模型列表")
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
		if st, err = store.NewPostgres(ctx, *dsn); err != nil {
			log.Fatalf("postgres: %v", err)
		}
	default:
		log.Fatalf("未知 store: %s", *storeTyp)
	}
	defer st.Close()

	var cache store.ModelConfigCache
	if *redisAddr != "" {
		if cache, err = store.NewRedisCache(*redisAddr, 24*time.Hour); err != nil {
			log.Fatalf("redis: %v", err)
		}
	} else {
		cache = store.NewMemoryCache(24 * time.Hour)
	}

	// ---- E2B 沙箱提供方 ----
	bk, err := sandboxE2B.New(sandboxE2B.Config{
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
		m := map[string]string{}
		for _, inst := range insts {
			if inst.SandboxID != "" {
				m[inst.UserID] = inst.SandboxID
			}
		}
		bk.Seed(m)
	}

	seats := controlplane.NewMockSeatService()
	seats.Model = *model
	seats.Provider = *provider
	seats.BaseURL = *baseURL
	seats.APIKey = *apiKey
	seats.Models = splitModels(*modelList)

	manager := controlplane.NewManager(st, cache, bk, seats, gateway.New(*gatewayURL))
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
		log.Fatalf("control-plane-e2b: %v", err)
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
