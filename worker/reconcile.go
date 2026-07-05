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
				defer cancel() // panic 时也释放(safeTick 会 recover),不泄漏到 2min 超时(P1-4)
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
				// 模型2 escrow 对账(R5 F4):窗口纠偏(桶1 vs newapi quota+used 自愈入账/续充/退款的写残窗)
				// + 守恒断言(已释放+托管==充值−退款)。observe 下 ReconcileEscrow 内部短路。
				if eerr := w.svc.ReconcileEscrow(rc); eerr != nil {
					w.log.Error("escrow 对账失败(下轮重试)", "err", eerr)
				}
				// 回填-台账对账安全网(24-§9):已回填组织 SUM(ledger) vs new-api stat 权威值,漂移即告警(只读)。
				if berr := w.svc.ReconcileBackfillLedger(rc); berr != nil {
					w.log.Error("回填-台账对账失败(下轮重试)", "err", berr)
				}
				// B6a:订阅旁路再断言(v2)——钱包组织周期重设 wallet_only + 告警 active 订阅。
				if werr := w.svc.ReassertWalletOnly(rc); werr != nil {
					w.log.Error("订阅旁路再断言失败(下轮重试)", "err", werr)
				}
				// 逐条明细保留清理(24-§6:默认 0=永久保留跳过;配天数才清)。只删本库,housekeeping,不涉钱。
				if perr := w.svc.PurgeOldUsageDetail(rc); perr != nil {
					w.log.Error("用量明细保留清理失败(下轮重试)", "err", perr)
				}
			})
		}
	}
}
