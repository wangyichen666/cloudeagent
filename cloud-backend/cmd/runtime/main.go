package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	apiagent "cloude-agent/pkg/apiserver/agent"
	"cloude-agent/pkg/agentruntime"
)

func main() {
	var (
		listen     = flag.String("listen", "127.0.0.1:18585", "监听地址")
		workspace  = flag.String("workspace", "./data/workspaces/default", "持久化工作区目录")
		configFile = flag.String("config-file", "", "运行时配置文件（可选，watch 热重载；默认仅内存）")
		engineName = flag.String("engine", "", "Agent 引擎名称（qwenpaw / mock；默认 qwenpaw，不可用回退 mock）")
		qwenpawBin = flag.String("qwenpaw-bin", "", "【已废弃，用 --engine】QwenPaw ACP 内核可执行文件路径")
	)
	flag.Parse()

	if err := os.MkdirAll(*workspace, 0o755); err != nil {
		log.Fatalf("mkdir workspace: %v", err)
	}

	// 兼容旧参数：--qwenpaw-bin 非空时引擎名取其路径
	eng := *engineName
	if eng == "" && *qwenpawBin != "" {
		eng = *qwenpawBin
	}
	if eng == "" {
		if v := os.Getenv("AGENT_ENGINE"); v != "" {
			eng = v
		} else if v := os.Getenv("QWENPAW_BIN"); v != "" {
			eng = v
		} else {
			eng = "qwenpaw"
		}
	}

	daemon, err := agentruntime.NewDaemon(*workspace, *configFile, eng)
	if err != nil {
		log.Fatalf("init agent: %v", err)
	}

	server := &http.Server{
		Addr:              *listen,
		Handler:           apiagent.Handler(daemon),
		ReadHeaderTimeout: 10 * 1e9,
	}
	go func() {
		log.Printf("[agent-runtime] listening on %s workspace=%s engine=%s", *listen, *workspace, eng)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("serve: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	_ = server.Close()
	daemon.Close()
	log.Println("[agent-runtime] stopped (工作区数据已保留)")
}
