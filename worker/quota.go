// Package worker 是 leader 单写者后台循环(10 §4.3):订阅补满 / 对账。
// 多实例水平扩展前必须先做选主(MySQL 租约 + term fencing,memory project_enterprise_platform_multinode_leader),
// 否则钱面写跨节点重复(双补=双倍发钱);单节点灰度由 NEXUS_WORKER_ENABLED + envLeadership 保证单写者。
package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/nexusapi-platform/enterprise/service"
)

// QuotaWorker 周期跑订阅档位补满(架构B 阶段1,31-ADR §4.3):
// 对 active+subscription 成员按组织时区自然边界补满到目标(D=目标−剩余,增量划账,幂等键=成员×周期桶)。
//
// 架构B 退役停调(33 §5 + 组长裁定 33-§12-4/17,函数保留待删、不再入口可达):
//   - ReverseExpiredGrants / ResetDuePolicies:A 版临时 grant / override 周期重置机器(额度落 token),
//     B 下成员额度=user.quota,周期语义由 RunSubscriptionTopup(经 Transfer)接管;
//   - AutoRefill:escrow 分桶下发续充(内含绕 Transfer 的 quota 直写),escrow 整体退役。
type QuotaWorker struct {
	svc      *service.Service
	log      *slog.Logger
	interval time.Duration
}

// NewQuotaWorker 构造 worker。interval<=0 用默认 5 分钟(周期桶边界后的首个 tick 即补满;
// 桶内已处理成员由进程内去重跳过,不空转打上游)。batch 参数保留签名兼容(调用方 main.go 不改),已不使用。
func NewQuotaWorker(svc *service.Service, log *slog.Logger, interval time.Duration, batch int) *QuotaWorker {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	if log == nil {
		log = slog.Default()
	}
	_ = batch
	return &QuotaWorker{svc: svc, log: log, interval: interval}
}

// Run 阻塞运行循环直到 ctx 取消。先立即跑一轮,再按 interval 周期跑。
func (w *QuotaWorker) Run(ctx context.Context) {
	w.log.Info("quota-worker 启动(订阅补满)", "interval", w.interval.String())
	safeTick(w.log, "quota", func() { w.tick(ctx) })
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			w.log.Info("quota-worker 退出")
			return
		case <-t.C:
			safeTick(w.log, "quota", func() { w.tick(ctx) })
		}
	}
}

func (w *QuotaWorker) tick(ctx context.Context) {
	// 订阅补满搬真钱:每候选成员一次 GetUserQuota + 可能一笔 Transfer(多次上游 HTTP),
	// 按数百成员给宽超时;leader 闸 / money_freeze / 幂等键防双补都在 service 内。
	c, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if err := w.svc.RunSubscriptionTopup(c); err != nil {
		w.log.Error("quota-worker 订阅补满失败(下 tick 重试)", "err", err)
		w.svc.RecordWorkerFailure(ctx, "quota", "subscription_topup", err)
	}
}
