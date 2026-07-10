package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/nexusapi-platform/enterprise/adapter/newapi"
	"github.com/nexusapi-platform/enterprise/model"
	"github.com/nexusapi-platform/enterprise/pkg/apperr"
	"github.com/nexusapi-platform/enterprise/repo"
)

// escrowLockKey 是 escrow 动钱操作(入账/续充/退款/对账)的 per-org 串行锁键。
// 同组织所有改窗口/桶的操作串行 → 消除并发丢失更新/双 add/seq 撞 uk/越 cap(R5 F1/F2)。
// 进程内锁(单节点);多节点需换分布式锁(同 applyMemberOverride,记入选主改造已知项)。
func escrowLockKey(orgID int64) string { return fmt.Sprintf("escrow:org:%d", orgID) }

// escrowWindowCap 是桶1(镜像进 org user.quota 的可花窗口)上限 ≈ $4000(= 4000 × QuotaPerUnit 500000)。
// < int32 上限 ~$4294(0020/14 §3.3),留头寸防溢出;续充合并后桶1 断言 ≤ 此值。
const escrowWindowCap int64 = 2_000_000_000

// escrowDefaultThreshold 建桶时写入 escrow_bucket.threshold(该列 R5后已不参与续充触发,触发看 org_escrow_config;留作历史值)。
const escrowDefaultThreshold int64 = 100_000_000

// 续充阈值(补货点/reorder point)常量(14 §15;org_escrow_config.threshold_auto 每天按此重算):
//   阈值 = clamp(peakHourly×LEAD×MARGIN + maxSingle, FLOOR, CEIL);历史<7天 → DEFAULT_NEW。
const (
	escrowFloor      int64   = 25_000_000    // ~$50
	escrowCeil       int64   = 1_000_000_000 // ~$2000 = WINDOW_CAP/2
	escrowDefaultNew int64   = 50_000_000    // ~$100(新组织/历史<7天)
	escrowLead       float64 = 0.5           // 续充延迟 SLA(小时):worker 宕机到人工修可容忍
	escrowMargin     float64 = 1.5           // 安全系数
)

// applyRecharge 模型2 入账(由 billing.Recharge 调,**取代旧 AddRecharge+allocateRecharge 两步**):
// 持 per-org 锁 → 锁内重读 window → **单事务原子**{记账(transfer_no 幂等)+ 影子余额 + escrow 分桶(FOR UPDATE)}
// → 提交后 add fit 进 newapi(绝不 override)。
// 涉钱安全(R5 修复 F1/F2/F5):锁串行消除并发丢失更新/双 add/seq 撞 uk/越 cap;记账与分桶同事务=原子;
//   newapi add 失败**不回滚不返错**——DB 已原子提交(释放已记)是真相,escrow 对账 worker 会把窗口补到
//   桶1−used(自愈),故入账方向恒安全。返回入账后影子余额;重复 transfer_no → repo.ErrConflict。
func (s *Service) applyRecharge(ctx context.Context, orgID int64, r *model.Recharge) (*model.Balance, error) {
	org, err := s.store.GetOrganization(ctx, orgID)
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	cred, err := s.EnsureOrgProvisioned(ctx, orgID, org.Name)
	if err != nil {
		return nil, err
	}
	release, lerr := s.quotaLocker.Acquire(ctx, escrowLockKey(orgID))
	if lerr != nil {
		return nil, apperr.Internal("").WithCause(lerr)
	}
	defer release()

	window, gerr := s.upstream.GetUserQuota(ctx, cred.NewapiUserID) // 锁内重读,防陈旧 window 越 cap
	if gerr != nil {
		return nil, mapUpstream(gerr)
	}
	fit := r.Amount
	if space := escrowWindowCap - window; fit > space {
		fit = space
	}
	if fit < 0 {
		fit = 0
	}
	remainder := r.Amount - fit

	var bal *model.Balance
	if err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
		b, aerr := s.store.AddRechargeTx(ctx, tx, r) // 记账 + 影子(transfer_no 幂等);ErrConflict 透传回滚
		if aerr != nil {
			return aerr
		}
		bal = b
		active, agerr := s.store.GetActiveEscrowBucketTx(ctx, tx, orgID) // FOR UPDATE 锁桶1
		firstTime := errors.Is(agerr, repo.ErrNotFound)
		if !firstTime && agerr != nil {
			return agerr
		}
		if firstTime {
			if _, e := s.store.CreateEscrowBucketTx(ctx, tx, &model.EscrowBucket{OrgID: orgID, Seq: 1, Amount: fit, Status: model.EscrowActive, Threshold: escrowDefaultThreshold}); e != nil {
				return e
			}
		} else if e := s.store.UpdateEscrowBucketTx(ctx, tx, active.ID, active.Amount+fit, model.EscrowActive); e != nil {
			return e
		}
		if remainder > 0 {
			maxSeq, e := s.store.MaxEscrowSeqTx(ctx, tx, orgID)
			if e != nil {
				return e
			}
			holdingSeq := maxSeq + 1
			if firstTime {
				holdingSeq = 2
			}
			if _, e := s.store.CreateEscrowBucketTx(ctx, tx, &model.EscrowBucket{OrgID: orgID, Seq: holdingSeq, Amount: remainder, Status: model.EscrowHolding, Threshold: escrowDefaultThreshold}); e != nil {
				return e
			}
		}
		return nil
	}); err != nil {
		if errors.Is(err, repo.ErrConflict) || errors.Is(err, repo.ErrOptimisticLock) {
			return nil, err // 幂等/并发冲突,原样透传给调用方
		}
		return nil, apperr.Internal("").WithCause(err)
	}
	// 提交后 add fit 进 newapi(绝不 override)。失败=欠拨,不返错——reconcile worker 自愈窗口到 桶1−used。
	if fit > 0 {
		if window+fit > escrowWindowCap {
			s.log.Error("入账后桶1 超窗口上限(已记账,reconcile 将兜)", "org_id", orgID, "window", window, "fit", fit)
		} else if err := s.upstream.ManageUserQuota(ctx, cred.NewapiUserID, newapi.QuotaAdd, fit); err != nil {
			s.log.Error("入账 add 进 newapi 失败(DB 已原子提交,reconcile 将自愈窗口)", "org_id", orgID, "fit", fit, "err", err)
		}
	}
	return bal, nil
}

// ───────────────────────────── 分桶下发/对账侧已退役(45号 14/15,执行 33 §5)─────────────────────────────
// 已删除:GetDerivedBalance/RefillWindow/refillToWindow/escrowThreshold/recomputeThresholdAuto/
// computeAutoThreshold/AutoRefill/autoRefillOrg/ReconcileEscrow/reconcileOrgEscrow 及 DerivedBalance 类型,
// 连同 /escrow-balance、/escrow/refill 两个端点——escrow 分桶下发含绕 Transfer 的 quota 直写,与架构B
// "钱只经 Transfer 搬动"冲突;架构B 金库补钱=运营方 new-api 侧代充 + Transfer 划拨,escrow 无角色。
// 保留:applyRecharge 记账侧(fundingEnabled 闸内,v1 恒 404 休眠,v2 决策后再启);防复活守卫见
// TestIntegration_EscrowRetired(端点 404 + escrow_bucket 恒零写入)。
