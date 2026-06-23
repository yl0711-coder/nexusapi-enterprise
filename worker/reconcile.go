package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/nexusapi-platform/enterprise/service"
)

// ReconcileWorker 周期跑折扣对账(G):比对平台折扣镜像与 new-api 实际特殊倍率,
// 发现漂移即告警(日志 + 审计 + 通知),只读、绝不自动改价。安全常驻,默认开。
type ReconcileWorker struct {
	svc      *service.Service
	log      *slog.Logger
	interval time.Duration
}

// NewReconcileWorker 构造。interval<=0 默认 10 分钟(对账非实时,低频即可)。
func NewReconcileWorker(svc *service.Service, log *slog.Logger, interval time.Duration) *ReconcileWorker {
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	if log == nil {
		log = slog.Default()
	}
	return &ReconcileWorker{svc: svc, log: log, interval: interval}
}

// Run 阻塞运行直到 ctx 取消。
func (w *ReconcileWorker) Run(ctx context.Context) {
	w.log.Info("reconcile-worker 启动(折扣对账,只读告警)", "interval", w.interval.String())
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			w.log.Info("reconcile-worker 退出")
			return
		case <-t.C:
			safeTick(w.log, "reconcile", func() {
				rc, cancel := context.WithTimeout(ctx, 2*time.Minute)
				drifts, err := w.svc.ReconcileDiscounts(rc)
				if err != nil {
					w.log.Error("折扣对账失败(下轮重试)", "err", err)
				} else if len(drifts) == 0 {
					w.log.Debug("折扣对账:无漂移")
				}
				// 计费对账(守恒真账版):对上个完整小时比对 new-api.logs vs usage_ledger,少收即告警。
				if berr := w.svc.ReconcileBilling(rc); berr != nil {
					w.log.Error("计费对账失败(下轮重试)", "err", berr)
				}
				// 余额-台账对账(GZ-01 D4):校验 company_balance.total_consumed == Σusage_ledger,兜住少收盲区。
				if lerr := w.svc.ReconcileBalanceLedger(rc); lerr != nil {
					w.log.Error("余额-台账对账失败(下轮重试)", "err", lerr)
				}
				cancel()
			})
		}
	}
}
