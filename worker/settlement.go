package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/nexusapi-platform/enterprise/service"
)

// SettlementWorker 周期读 new-api 消费 logs 扣公司余额(03 §3.1)。
// 只对开了 billing_enabled 的组织扣费(逐组织灰度,默认关),安全常驻。
type SettlementWorker struct {
	svc      *service.Service
	log      *slog.Logger
	interval time.Duration
}

// NewSettlementWorker 构造。interval<=0 默认 60s。
func NewSettlementWorker(svc *service.Service, log *slog.Logger, interval time.Duration) *SettlementWorker {
	if interval <= 0 {
		interval = time.Minute
	}
	if log == nil {
		log = slog.Default()
	}
	return &SettlementWorker{svc: svc, log: log, interval: interval}
}

// Run 阻塞运行直到 ctx 取消。
func (w *SettlementWorker) Run(ctx context.Context) {
	w.log.Info("settlement-worker 启动", "interval", w.interval.String())
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			w.log.Info("settlement-worker 退出")
			return
		case <-t.C:
			safeTick(w.log, "settlement", func() {
				c, cancel := context.WithTimeout(ctx, 45*time.Second)
				n, err := w.svc.RunSettlement(c)
				cancel()
				if err != nil {
					w.log.Error("settlement 扣费失败", "err", err)
				} else if n > 0 {
					w.log.Info("settlement 扣费", "deducted_quota", n)
				}
				// 历史回填(24):forward 之后**同 goroutine 串行**跑一片(tick 分片,不另起并发 goroutine)。
				// 与 forward 共用 settlementMu,回填与 forward-补漏绝不并发写 ledger(命根子)。独立超时,不占 forward 的。
				bc, bcancel := context.WithTimeout(ctx, 45*time.Second)
				if berr := w.svc.RunBackfillSlice(bc); berr != nil {
					w.log.Error("历史回填分片失败(保持 running,下轮从 cursor_ts 续)", "err", berr)
				}
				bcancel()
				// 完整日志镜像只服务排障,不进 settlement 账本。独立短超时,失败只告警下轮补。
				lc, lcancel := context.WithTimeout(ctx, 20*time.Second)
				if lerr := w.svc.RunLogMirrorSlice(lc); lerr != nil {
					w.log.Error("new-api 完整日志镜像失败(保持 running,下轮续)", "err", lerr)
				}
				lcancel()
			})
		}
	}
}
