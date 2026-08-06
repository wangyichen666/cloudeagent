package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"cloude-agent/internal/agent"
)

func main() {
	var (
		listen      = flag.String("listen", "127.0.0.1:18585", "监听地址")
		workspace   = flag.String("workspace", "./data/workspaces/default", "持久化工作区目录")
		configFile  = flag.String("config-file", "", "运行时配置文件（可选，watch 热重载；默认仅内存）")
	)
	flag.Parse()

	if err := os.MkdirAll(*workspace, 0o755); err != nil {
		log.Fatalf("mkdir workspace: %v", err)
	}

	daemon, err := agent.NewDaemon(*workspace, *configFile)
	if err != nil {
		log.Fatalf("init agent: %v", err)
	}

	server := &http.Server{
		Addr:              *listen,
		Handler:           daemon.Handler(),
		ReadHeaderTimeout: 10 * 1e9,
	}
	go func() {
		log.Printf("[agent-runtime] listening on %s workspace=%s", *listen, *workspace)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("serve: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	_ = server.Close()
	log.Println("[agent-runtime] stopped (工作区数据已保留)")
}
