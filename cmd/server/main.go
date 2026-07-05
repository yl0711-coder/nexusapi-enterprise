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
	"sync"
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
var version = "7.1.0-billingfix"

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
	// 安全:拒绝生产误用 dev 默认密钥(会话密钥泄露=可伪造任意角色会话提权)。
	// dev/测试显式 NEXUS_ALLOW_DEV_KEYS=true 放行;生产绝不设此开关。
	if os.Getenv("NEXUS_ALLOW_DEV_KEYS") != "true" {
		devSessionKeys := map[string]bool{"dev-only-session-signing-key-32bytes!!": true}
		devMasterKeys := map[string]bool{"MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=": true}
		if devSessionKeys[sessionKey] {
			return errors.New("NEXUS_SESSION_KEY 是 dev 默认值,生产禁用(会话密钥泄露可伪造提权);请注入真随机密钥")
		}
		if devMasterKeys[masterKey] {
			return errors.New("NEXUS_MASTER_KEY 是 dev 默认值,生产禁用;请注入真随机主密钥")
		}
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

	// MVP 灰度模式(改动⑥):NEXUS_MVP_MODE=true → 观测模式(结算只落账不扣钱/不停服)+ 路由白名单封锁。
	// v1 裁定A(20-§2.1):observe 唯一职责=额度执行机器休眠;建令牌已剥离(开通一律真建)。
	mvpMode := os.Getenv("NEXUS_MVP_MODE") == "true"
	if mvpMode {
		log.Info("MVP 灰度模式已开启:观测模式(额度执行机器休眠) + 路由白名单封锁(非白名单写操作 404)")
	}
	// v1 escrow 休眠总闸(20-§9):默认 false=平台不经手钱(充值/退款/续充/escrow对账/计费对账全禁);v2 才开。
	fundingEnabled := os.Getenv("NEXUS_PLATFORM_FUNDING_ENABLED") == "true"
	if fundingEnabled {
		log.Info("平台经手钱已开启(v2 escrow):入账/续充/退款/对账生效")
	} else {
		log.Info("v1 观测管理版:escrow 休眠(钱在 new-api,平台只看不碰)")
	}

	svc := service.New(service.Deps{
		Store: store, Upstream: upstream, Keyring: keyring, Signer: signer, Logger: log,
		ObserveMode: mvpMode, FundingEnabled: fundingEnabled,
		// 历史回填限速旋钮(24-§4.5):默认 8 窗口/tick、5 页/秒,量小够用;大回填靠分片多 tick 排空。
		BackfillWindowsPerTick: atoiOr("NEXUS_BACKFILL_WINDOWS_PER_TICK", 8),
		BackfillQPS:            atoiOr("NEXUS_BACKFILL_QPS", 5),
		// 逐条明细保留期(24-§6):默认 0=永久保留(不清理);量涨后设天数启用定期清理。
		UsageDetailRetentionDays: atoiOr("NEXUS_USAGE_DETAIL_RETENTION_DAYS", 0),
	})

	// 运营方引导账号(首启种子,幂等)。
	if email := os.Getenv("NEXUS_BOOTSTRAP_OPERATOR_EMAIL"); email != "" {
		if err := svc.SeedOperator(bootCtx, email, os.Getenv("NEXUS_BOOTSTRAP_OPERATOR_PASSWORD")); err != nil {
			return err
		}
	}

	// quota-worker(leader 单写者:扫 grant 到期反向,03 §3.4)。MVP 单实例默认开。
	// GZ-02 修复2:worker 在 WaitGroup 下启动,关闭时先 cancel + 等当前 tick 收尾、再 drain HTTP。
	// defer workerCancel() 仅作早退路径(worker 启动后到信号等待之间若异常 return)的兜底;
	// 正常关闭由下方信号处理段显式 workerCancel() 保证「先于 srv.Shutdown」的正确顺序。
	workerCtx, workerCancel := context.WithCancel(context.Background())
	defer workerCancel()
	var workerWG sync.WaitGroup
	if os.Getenv("NEXUS_WORKER_ENABLED") != "false" {
		iv := time.Duration(atoiOr("NEXUS_WORKER_INTERVAL_SEC", 60)) * time.Second
		startWorker(&workerWG, func() { worker.NewQuotaWorker(svc, log, iv, 100).Run(workerCtx) })
		// 结算 worker:只对开了 billing_enabled 的组织扣费(逐组织灰度,默认关)。
		startWorker(&workerWG, func() { worker.NewSettlementWorker(svc, log, iv).Run(workerCtx) })
		// 对账 worker(G):折扣镜像 vs new-api 实际特殊倍率,只读告警不改价。低频(默认 10min)。
		rv := time.Duration(atoiOr("NEXUS_RECONCILE_INTERVAL_SEC", 600)) * time.Second
		startWorker(&workerWG, func() { worker.NewReconcileWorker(svc, log, rv).Run(workerCtx) })
	}

	h := handler.New(svc, signer, log, version, mvpMode)
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
	// GZ-02 修复2:先停 worker 并等当前 tick 收尾(避免结算被拦腰砍断 → 下轮对账误报),再 drain HTTP。
	// 1) 取消 workerCtx,让各 worker 的 select 看到 Done 后退出循环;正在跑的 tick 因派生 ctx 取消而提前结束。
	workerCancel()
	// 2) 等当前正在跑的 tick 收尾。给一个总收尾上限,避免某个 tick 卡死导致永不退出。
	//    上限须 >= 最长 tick 超时(settlement 45s),并与生产 compose stop_grace_period 对齐(GZ-02 修复3)。
	waitWorkers(&workerWG, log, 50*time.Second)
	// 3) 再 drain HTTP。
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	return srv.Shutdown(shutCtx)
}

// startWorker 在 WaitGroup 下启动一个 worker goroutine,使关闭路径能等它收尾(GZ-02 修复2)。
func startWorker(wg *sync.WaitGroup, run func()) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		run()
	}()
}

// waitWorkers 等所有 worker goroutine 收尾,最长等 timeout;超时则记录并放行,
// 避免单个卡死 tick 导致进程永不退出(GZ-02 修复2)。
func waitWorkers(wg *sync.WaitGroup, log *slog.Logger, timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		log.Info("worker 已全部收尾")
	case <-time.After(timeout):
		log.Warn("等待 worker 收尾超时,继续关闭", "timeout", timeout.String())
	}
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
