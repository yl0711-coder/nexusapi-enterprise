// Package worker 是 leader 单写者后台循环(10 §4.3):定时重置 / grant 到期反向 / 异步补偿。
// MVP 单实例直接跑;多实例水平扩展时再加分布式选主(与 bootstrap 锁同栈),本期不做。
package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/nexusapi-platform/enterprise/service"
)

// QuotaWorker 周期扫到期 grant 并反向应用(03 §3.4)。
type QuotaWorker struct {
	svc      *service.Service
	log      *slog.Logger
	interval time.Duration
	batch    int
}

// NewQuotaWorker 构造 worker。interval<=0 用默认 1 分钟;batch<=0 用 100。
func NewQuotaWorker(svc *service.Service, log *slog.Logger, interval time.Duration, batch int) *QuotaWorker {
	if interval <= 0 {
		interval = time.Minute
	}
	if batch <= 0 {
		batch = 100
	}
	if log == nil {
		log = slog.Default()
	}
	return &QuotaWorker{svc: svc, log: log, interval: interval, batch: batch}
}

// Run 阻塞运行循环直到 ctx 取消。先立即跑一轮,再按 interval 周期跑。
func (w *QuotaWorker) Run(ctx context.Context) {
	w.log.Info("quota-worker 启动", "interval", w.interval.String(), "batch", w.batch)
	w.tick(ctx)
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			w.log.Info("quota-worker 退出")
			return
		case <-t.C:
			w.tick(ctx)
		}
	}
}

func (w *QuotaWorker) tick(ctx context.Context) {
	c, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	n, err := w.svc.ReverseExpiredGrants(c, w.batch)
	if err != nil {
		w.log.Error("quota-worker 扫到期失败", "err", err)
	} else if n > 0 {
		w.log.Info("quota-worker 反向到期 grant", "count", n)
	}
	// 周期重置(03 §3.3):按 quota_policy 周期边界重算 override 下发。
	if rn, rerr := w.svc.ResetDuePolicies(c); rerr != nil {
		w.log.Error("quota-worker 周期重置失败", "err", rerr)
	} else if rn > 0 {
		w.log.Info("quota-worker 周期重置", "members", rn)
	}
}
