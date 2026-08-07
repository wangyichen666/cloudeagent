// cloud-gateway：backend 与 Pod 内 agent 之间的数据面网关。
// 只负责「连接」：路由注册、REST/WS 转发、连接管理，不含业务逻辑。
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"cloud-gateway/internal/agentclient"
	"cloud-gateway/internal/registry"
	"cloud-gateway/internal/server"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:18500", "gateway 监听地址（backend 调用入口）")
	flag.Parse()

	reg := registry.New()
	agent := agentclient.New()
	srv := server.New(reg, agent)

	httpServer := &http.Server{
		Addr:              *listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Printf("[cloud-gateway] listening on %s", *listen)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("serve: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(ctx)
	log.Println("[cloud-gateway] stopped")
}
