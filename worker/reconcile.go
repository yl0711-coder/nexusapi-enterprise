package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/nexusapi-platform/enterprise/service"
)

// ReconcileWorker 周期跑对账(只读检测 + 受控修复):
//   - ReconcileTransfers(架构B 阶段1,BE② 核心):划账账本读-核-补(滞留行精确收敛,leader-gated,
//     修复写受 money_freeze 管、检测告警恒开)+ 交叉恒等式扫描(内部限频,只告警不自动修);
//   - ReconcileDiscounts:折扣倍率漂移告警(只读,observe 短路已拆);
//   - ReconcileBilling:logs↔usage_ledger 真账对账(少收告警);
//   - ReconcileBackfillLedger / ReassertWalletOnly / PurgeOldUsageDetail:安全网与 housekeeping。
//
// 架构B 退役停调(33 §5 + 组长裁定 33-§12-9/17,函数保留待删、不再入口可达):
//   - ReconcileBalanceLedger:对账对象 company_balance 第二账已砍(余额=读求和,单一真相=账本+new-api);
//   - ReconcileEscrow:escrow 分桶下发整体退役(内含绕 Transfer 的窗口纠偏直写)。
type ReconcileWorker struct {
	svc      *service.Service
	log      *slog.Logger
	interval time.Duration
}

// NewReconcileWorker 构造。interval<=0 默认 10 分钟(滞留划账 2 分钟起判,10 分钟收敛节拍够用)。
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
	w.log.Info("reconcile-worker 启动(划账对账环 + 折扣/计费对账)", "interval", w.interval.String())
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			w.log.Info("reconcile-worker 退出")
			return
		case <-t.C:
			safeTick(w.log, "reconcile", func() {
				rc, cancel := context.WithTimeout(ctx, 5*time.Minute)
				defer cancel() // panic 时也释放(safeTick 会 recover),不泄漏超时(P1-4)
				// 划账对账环(钱核心命门):滞留行收敛 + 恒等式(31-ADR §4.2 读-核-补)。
				if drifts, err := w.svc.ReconcileTransfers(rc); err != nil {
					w.log.Error("划账对账环失败(下轮重试)", "err", err)
					w.svc.RecordWorkerFailure(ctx, "reconcile", "transfers", err)
				} else if len(drifts) > 0 {
					w.log.Warn("划账对账环发现漂移(详见 ledger_* 告警/审计)", "count", len(drifts))
				}
				// 折扣对账(只读告警,绝不自动改价)。
				drifts, err := w.svc.ReconcileDiscounts(rc)
				if err != nil {
					w.log.Error("折扣对账失败(下轮重试)", "err", err)
					w.svc.RecordWorkerFailure(ctx, "reconcile", "discounts", err)
				} else if len(drifts) == 0 {
					w.log.Debug("折扣对账:无漂移")
				}
				// 计费对账(守恒真账版):对上个完整小时比对 new-api.logs vs usage_ledger,少收即告警。
				if berr := w.svc.ReconcileBilling(rc); berr != nil {
					w.log.Error("计费对账失败(下轮重试)", "err", berr)
					w.svc.RecordWorkerFailure(ctx, "reconcile", "billing", berr)
				}
				// 回填-台账对账安全网(24-§9):已回填组织 SUM(ledger) vs new-api stat 权威值,漂移即告警(只读)。
				if berr := w.svc.ReconcileBackfillLedger(rc); berr != nil {
					w.log.Error("回填-台账对账失败(下轮重试)", "err", berr)
					w.svc.RecordWorkerFailure(ctx, "reconcile", "backfill_ledger", berr)
				}
				// 订阅旁路再断言(31-ADR §7 纵深):周期重设 wallet_only + 告警 active 订阅。
				if werr := w.svc.ReassertWalletOnly(rc); werr != nil {
					w.log.Error("订阅旁路再断言失败(下轮重试)", "err", werr)
					w.svc.RecordWorkerFailure(ctx, "reconcile", "wallet_only", werr)
				}
				// 逐条明细保留清理(24-§6:默认 0=永久保留跳过;配天数才清)。只删本库,housekeeping,不涉钱。
				if perr := w.svc.PurgeOldUsageDetail(rc); perr != nil {
					w.log.Error("用量明细保留清理失败(下轮重试)", "err", perr)
					w.svc.RecordWorkerFailure(ctx, "reconcile", "purge_usage_detail", perr)
				}
				// 金库低预警巡检(29-PRD §4.9,BE③;阶段2 接线,与 VerifyQuotaPerUnit 同批,33 §11 带入项):
				// 只读只报绝不写 quota/停服(fail-open);内部 leader-gated + 阈值未配置(<=0)静默跳过。
				if terr := w.svc.CheckTreasuryLowWatermarks(rc); terr != nil {
					w.log.Error("金库低预警巡检失败(下轮重试)", "err", terr)
					w.svc.RecordWorkerFailure(ctx, "reconcile", "treasury_low_watermark", terr)
				}
			})
		}
	}
}
