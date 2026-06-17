// Command server 是企业管理平台后端入口。
//
// 里程碑 1:identity + org + RBAC + 开通成员(US-01)。在此接 MySQL + 代发 key adapter
// + REST 路由。配置全经环境变量注入(主密钥/会话密钥/DSN/上游凭证绝不入镜像,10 §3.2)。
//
// 四条铁律:对 new-api 零数据侵入(只走官方 HTTP API,adapter 收口);平台不可用绝不拖垮
// new-api(独立进程,出事 docker stop 即摘);绝不在生产机 build(镜像 pull+up);涉钱先讲风险。
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
	_ "time/tzdata" // 嵌入时区库(distroless 无 tzdata,周期重置按组织时区需要,B3)

	"github.com/nexusapi-platform/enterprise/adapter/newapi"
	"github.com/nexusapi-platform/enterprise/handler"
	"github.com/nexusapi-platform/enterprise/pkg/crypto"
	"github.com/nexusapi-platform/enterprise/pkg/session"
	"github.com/nexusapi-platform/enterprise/repo"
	"github.com/nexusapi-platform/enterprise/service"
	"github.com/nexusapi-platform/enterprise/worker"
)

// version 由构建时 -ldflags "-X main.version=..." 注入。
var version = "6.2.0-g"

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	if err := run(log); err != nil {
		log.Error("启动失败", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	addr := envOr("LISTEN_ADDR", ":8080")

	// —— 必需配置(缺失即拒启动,绝不裸奔)——
	dsn := os.Getenv("NEXUS_DB_DSN")
	if dsn == "" {
		return errors.New("缺少 NEXUS_DB_DSN(MySQL 连接串)")
	}
	masterKey := os.Getenv("NEXUS_MASTER_KEY")
	if masterKey == "" {
		return errors.New("缺少 NEXUS_MASTER_KEY(base64 的 32 字节主密钥,10 §3.2)")
	}
	sessionKey := os.Getenv("NEXUS_SESSION_KEY")
	if len(sessionKey) < 16 {
		return errors.New("缺少 NEXUS_SESSION_KEY(会话签名密钥,>=16 字节)")
	}

	keyring, err := crypto.NewKeyringFromBase64(envOr("NEXUS_MASTER_KEY_ID", "v1"), masterKey)
	if err != nil {
		return err
	}
	ttl := time.Duration(atoiOr("NEXUS_SESSION_TTL_HOURS", 12)) * time.Hour
	signer, err := session.NewSigner([]byte(sessionKey), ttl)
	if err != nil {
		return err
	}

	bootCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	store, err := repo.Open(bootCtx, dsn)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.Migrate(bootCtx); err != nil {
		return err
	}
	log.Info("数据库已连接并完成迁移")

	// 代发 key adapter:指向 new-api 官方管理 API(零数据侵入)。
	upstream := newapi.New(newapi.Config{
		BaseURL:     os.Getenv("NEWAPI_BASE_URL"),
		AdminToken:  os.Getenv("NEWAPI_ADMIN_TOKEN"),
		AdminUserID: atoiOr("NEWAPI_ADMIN_USER_ID", 1),
		Logger: func(level, event string, kv map[string]any) {
			log.Info("upstream", append([]any{"event", event, "level", level}, flatten(kv)...)...)
		},
	}, nil)

	svc := service.New(service.Deps{
		Store: store, Upstream: upstream, Keyring: keyring, Signer: signer, Logger: log,
	})

	// 运营方引导账号(首启种子,幂等)。
	if email := os.Getenv("NEXUS_BOOTSTRAP_OPERATOR_EMAIL"); email != "" {
		if err := svc.SeedOperator(bootCtx, email, os.Getenv("NEXUS_BOOTSTRAP_OPERATOR_PASSWORD")); err != nil {
			return err
		}
	}

	// quota-worker(leader 单写者:扫 grant 到期反向,03 §3.4)。MVP 单实例默认开。
	workerCtx, workerCancel := context.WithCancel(context.Background())
	defer workerCancel()
	if os.Getenv("NEXUS_WORKER_ENABLED") != "false" {
		iv := time.Duration(atoiOr("NEXUS_WORKER_INTERVAL_SEC", 60)) * time.Second
		go worker.NewQuotaWorker(svc, log, iv, 100).Run(workerCtx)
		// 结算 worker:只对开了 billing_enabled 的组织扣费(逐组织灰度,默认关)。
		go worker.NewSettlementWorker(svc, log, iv).Run(workerCtx)
		// 对账 worker(G):折扣镜像 vs new-api 实际特殊倍率,只读告警不改价。低频(默认 10min)。
		rv := time.Duration(atoiOr("NEXUS_RECONCILE_INTERVAL_SEC", 600)) * time.Second
		go worker.NewReconcileWorker(svc, log, rv).Run(workerCtx)
	}

	h := handler.New(svc, signer, log, version)
	srv := &http.Server{
		Addr:              addr,
		Handler:           h.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Info("企业管理平台后端启动", "version", version, "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("服务异常退出", "err", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Info("收到退出信号,优雅关闭中...")
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	return srv.Shutdown(shutCtx)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func atoiOr(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func flatten(kv map[string]any) []any {
	out := make([]any, 0, len(kv)*2)
	for k, v := range kv {
		out = append(out, k, v)
	}
	return out
}
