// control-plane-k8s：生产路径入口。
// 与本地进程/Docker 后端共用同一控制面逻辑，仅把数据面换成 StatefulSet 后端。
// 用法（在集群内运行或提供 kubeconfig）：
//   cd k8sbackend && go build -o ../bin/control-plane-k8s ./cmd/control-plane
//   ./bin/control-plane-k8s --kubeconfig ~/.kube/config --agent-image cloude-agent:local
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"cloude-agent/internal/controlplane"
	"cloude-agent/internal/store"
	"cloude-agent/k8sbackend"
)

func main() {
	var (
		listen      = flag.String("listen", ":8080", "控制面监听地址")
		kubeconfig  = flag.String("kubeconfig", "", "kubeconfig 路径；空则使用集群内配置")
		namespace   = flag.String("namespace", "cloude-agent", "数据面命名空间")
		image       = flag.String("agent-image", "cloude-agent:local", "实例镜像")
		storageClass = flag.String("storage-class", "", "PVC StorageClass（空=默认，生产建议 longhorn）")
		removePVC   = flag.Bool("remove-pvc-on-delete", true, "删除实例时同时删除 PVC")
		adminToken  = flag.String("admin-token", "dev-admin-token", "管理面 Bearer Token")
		redisAddr   = flag.String("redis-addr", "", "Redis 地址（多副本共享热状态/锁）")
		dsn         = flag.String("dsn", "", "PostgreSQL DSN（建议生产必填）")
		idleTimeout = flag.Duration("idle-timeout", 0, "空闲自动休眠阈值，0=禁用")
	)
	flag.Parse()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 状态面：PostgreSQL 优先，未配置则用内存（仅供本地体验）。
	var st store.Store
	if *dsn != "" {
		pg, err := store.NewPostgres(ctx, *dsn)
		if err != nil {
			log.Fatalf("postgres: %v", err)
		}
		defer pg.Close()
		st = pg
	} else {
		st = store.NewMemory()
	}

	var cache store.ModelConfigCache
	if *redisAddr != "" {
		rc, err := store.NewRedisCache(*redisAddr, 24*time.Hour)
		if err != nil {
			log.Fatalf("redis: %v", err)
		}
		defer rc.Close()
		cache = rc
	} else {
		cache = store.NewMemoryCache(24 * time.Hour)
	}

	// 数据面：K8s StatefulSet 后端（文档 4.1 首选方案）
	restConfig, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		log.Fatalf("kubeconfig: %v", err)
	}
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		log.Fatalf("clientset: %v", err)
	}
	bk, err := k8sbackend.New(clientset, k8sbackend.Config{
		Namespace:         *namespace,
		Image:             *image,
		StorageClassName:  *storageClass,
		RemovePVCOnDelete: *removePVC,
	})
	if err != nil {
		log.Fatalf("k8s backend: %v", err)
	}

	seats := controlplane.NewMockSeatService()
	manager := controlplane.NewManager(st, cache, bk, seats)
	reaper := controlplane.NewReaper(manager, *idleTimeout, 15*time.Second)
	review := controlplane.NewReviewWorker(st, manager.GetModelConfig)
	auth := controlplane.NewAuthenticator(*namespace, *adminToken)
	server := controlplane.NewServer(controlplane.Config{
		Listen:  *listen,
		Manager: manager,
		Reaper:  reaper,
		Review:  review,
		Auth:    auth,
	})

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		cancel()
	}()
	if err := server.Run(ctx); err != nil {
		log.Fatalf("control-plane-k8s: %v", err)
	}
}
