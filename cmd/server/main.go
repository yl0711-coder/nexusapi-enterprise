// Command server 是企业管理平台后端入口。
//
// 里程碑 0 阶段:业务路由尚未接入(adapter/newapi 已完成,见仓库 README)。
// 本进程当前只提供健康检查,用以打通"镜像构建 → 推 GHCR → 生产 pull + up"的
// 部署管道(四条铁律 §3:绝不在生产机 build)。里程碑 1 起在此挂载 handler/service。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// version 由构建时 -ldflags "-X main.version=..." 注入;默认标记里程碑 0。
var version = "0.0.0-m0"

func main() {
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	mux := http.NewServeMux()
	// 存活探针:进程在即 200。
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "version": version})
	})
	// 就绪探针:里程碑 0 无下游强依赖(平台不可用绝不拖垮 new-api,§铁律2),恒就绪。
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ready", "milestone": "0-adapter-only"})
	})

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("企业管理平台后端启动 version=%s addr=%s", version, addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("服务异常退出: %v", err)
		}
	}()

	// 优雅退出:收到信号先停接新请求,给在途请求 10s 收尾。
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Print("收到退出信号,优雅关闭中...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("关闭超时: %v", err)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
