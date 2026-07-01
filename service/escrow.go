package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/nexusapi-platform/enterprise/adapter/newapi"
	"github.com/nexusapi-platform/enterprise/model"
	"github.com/nexusapi-platform/enterprise/pkg/apperr"
	"github.com/nexusapi-platform/enterprise/pkg/session"
	"github.com/nexusapi-platform/enterprise/repo"
)

// escrowLockKey 是 escrow 动钱操作(入账/续充/退款/对账)的 per-org 串行锁键。
// 同组织所有改窗口/桶的操作串行 → 消除并发丢失更新/双 add/seq 撞 uk/越 cap(R5 F1/F2)。
// 进程内锁(单节点);多节点需换分布式锁(同 applyMemberOverride,记入选主改造已知项)。
func escrowLockKey(orgID int64) string { return fmt.Sprintf("escrow:org:%d", orgID) }

// escrowWindowCap 是桶1(镜像进 org user.quota 的可花窗口)上限 ≈ $4000(= 4000 × QuotaPerUnit 500000)。
// < int32 上限 ~$4294(0020/14 §3.3),留头寸防溢出;续充合并后桶1 断言 ≤ 此值。
const escrowWindowCap int64 = 2_000_000_000

// escrowDefaultThreshold 续充触发阈值默认 ≈ $200(必须 > 单笔最大请求成本;运维按消费速率×续充延迟调,14 §3.7)。
const escrowDefaultThreshold int64 = 100_000_000

// DerivedBalance 模型2 读穿余额(14 §3.4):不持第二本权威余额,展示=桶1读穿(newapi user.quota 实际剩余)+ 托管桶之和。
type DerivedBalance struct {
	WindowQuota    int64 `json:"window_quota"`    // 桶1 实际剩余(读穿 new-api org user.quota)
	HoldingQuota   int64 `json:"holding_quota"`   // 平台库托管桶之和(未进窗口)
	AvailableQuota int64 `json:"available_quota"` // = 窗口 + 托管(组织当前可用总额)
}

// GetDerivedBalance 读穿组织余额(O/A)。窗口读 new-api(桶1 实际剩余,含已消费递减);托管读平台库。
// 绝不持第二本权威余额——余额永远派生自 new-api 桶1 + 平台库托管。组织未开通池子 → 全 0。
func (s *Service) GetDerivedBalance(ctx context.Context, c session.Claims, orgID int64) (*DerivedBalance, error) {
	if err := assertOrgScope(c, orgID); err != nil {
		return nil, err
	}
	if err := assertRole(c, session.RoleOperator, session.RoleOrgAdmin); err != nil {
		return nil, err
	}
	if err := s.mvpHidePrice(c); err != nil { // R5 RBAC:与 GetBalance 一致,observe 下客户不可读余额(藏价一致性)
		return nil, err
	}
	uid, _, ok, err := s.store.GetOrgNewapiCred(ctx, orgID)
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	var window int64
	if ok {
		w, gerr := s.upstream.GetUserQuota(ctx, int(uid))
		if gerr != nil {
			return nil, mapUpstream(gerr)
		}
		window = w
	}
	holding, err := s.store.SumHoldingEscrow(ctx, orgID)
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	return &DerivedBalance{WindowQuota: window, HoldingQuota: holding, AvailableQuota: window + holding}, nil
}

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

// RefillWindow 手工续充(运营方,v1):把一个托管桶并入桶1 可花窗口(window<threshold 时调;v1 无自动 worker,ADR §9)。
// 可并入额 = min(该桶额, 窗口剩余空间);全并→该桶 merged、部分→减额仍 holding。add 不 override。
// 涉钱安全(R5 修复 F1/F6):持 per-org 锁 + 单事务 FOR UPDATE 锁桶1/托管桶 → 串行消除并发重复释放=超拨;
//   newapi add 失败**不返错**——DB 已原子提交是真相,reconcile worker 把窗口补到 桶1−used 自愈(漏钱方向也由对账兜)。
func (s *Service) RefillWindow(ctx context.Context, c session.Claims, orgID int64) (*DerivedBalance, error) {
	if err := assertOrgScope(c, orgID); err != nil {
		return nil, err
	}
	if err := assertRole(c, session.RoleOperator); err != nil {
		return nil, err // 动钱:仅运营方
	}
	cred, err := s.orgCred(ctx, orgID)
	if err != nil {
		return nil, err
	}
	release, lerr := s.quotaLocker.Acquire(ctx, escrowLockKey(orgID))
	if lerr != nil {
		return nil, apperr.Internal("").WithCause(lerr)
	}
	defer release()

	window, gerr := s.upstream.GetUserQuota(ctx, cred.NewapiUserID) // 锁内重读
	if gerr != nil {
		return nil, mapUpstream(gerr)
	}
	var merge int64
	if err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
		h, herr := s.store.NextHoldingBucketTx(ctx, tx, orgID) // FOR UPDATE
		if errors.Is(herr, repo.ErrNotFound) {
			return nil // 无托管可续充,merge=0
		}
		if herr != nil {
			return herr
		}
		merge = h.Amount
		if space := escrowWindowCap - window; merge > space {
			merge = space
		}
		if merge <= 0 {
			merge = 0
			return nil // 窗口已满,无可并入空间
		}
		active, aerr := s.store.GetActiveEscrowBucketTx(ctx, tx, orgID) // FOR UPDATE 锁桶1
		if aerr != nil {
			return aerr
		}
		if e := s.store.UpdateEscrowBucketTx(ctx, tx, active.ID, active.Amount+merge, model.EscrowActive); e != nil {
			return e
		}
		if merge >= h.Amount {
			return s.store.UpdateEscrowBucketTx(ctx, tx, h.ID, 0, model.EscrowMerged)
		}
		return s.store.UpdateEscrowBucketTx(ctx, tx, h.ID, h.Amount-merge, model.EscrowHolding)
	}); err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	if merge > 0 {
		if err := s.upstream.ManageUserQuota(ctx, cred.NewapiUserID, newapi.QuotaAdd, merge); err != nil {
			s.log.Error("续充 add 进 newapi 失败(DB 已原子提交,reconcile 将自愈窗口)", "org_id", orgID, "merge", merge, "err", err)
		}
		s.audit(ctx, c, orgID, "escrow_refill", "balance", &orgID, map[string]any{"merged": merge, "window_before": window})
	}
	return s.GetDerivedBalance(ctx, c, orgID)
}

// ReconcileEscrow 是 escrow 对账 worker(R5 后裁定,leader 单写者周期跑)。observe 也跑:observe 只挡"给员工写
// 停人额度",对账纠的是**池子窗口**(过多才减、绝不加、有地板),不停员工。涉钱终极安全口径(14 §15):
//   目标窗口 = 已释放(桶1) − 已消费(**我方 usage_ledger,bigint,彻底不碰 new-api used_quota**,int32 会溢出+将被清零);
//   ① 实际窗口 > 目标 = 超拨 → **自动 SUBTRACT** 到目标(地板≥0,安全方向,绝不减到客户合法拥有之下);
//   ② 实际窗口 < 目标 = 欠拨 → **绝不自动 ADD**(正 delta 可能是日志滞后=瞬时超拨/垫钱方向)→ 只告警,
//      由续充 worker(读真实窗口自愈)或人工按 SLA 补;
//   ③ 守恒断言:已释放+托管 == 充值−退款(纯平台侧账,无消费项,不碰 used_quota)。
// 开跑前先 drain 一次结算(把日志水位追平到 now),使"已消费"最新——防欠拨告警被结算滞后刷假(§15 前提②)。
func (s *Service) ReconcileEscrow(ctx context.Context) error {
	if _, err := s.RunSettlement(ctx); err != nil { // drain-to-boundary:落账不扣钱(observe/非observe 都只落 ledger)
		s.log.Warn("escrow 对账前 drain 结算失败(用当前账本继续)", "err", err)
	}
	orgIDs, err := s.store.ListEscrowOrgIDs(ctx)
	if err != nil {
		return err
	}
	for _, orgID := range orgIDs {
		if rerr := s.reconcileOrgEscrow(ctx, orgID); rerr != nil {
			s.log.Error("escrow 对账失败(下轮重试)", "org_id", orgID, "err", rerr)
		}
	}
	return nil
}

func (s *Service) reconcileOrgEscrow(ctx context.Context, orgID int64) error {
	release, lerr := s.quotaLocker.Acquire(ctx, escrowLockKey(orgID)) // 与入账/续充/退款互斥
	if lerr != nil {
		return lerr
	}
	defer release()

	uid, _, ok, err := s.store.GetOrgNewapiCred(ctx, orgID)
	if err != nil {
		return err
	}
	if !ok {
		return nil // 未开通池子
	}
	var released int64
	active, agerr := s.store.GetActiveEscrowBucket(ctx, orgID)
	if agerr == nil {
		released = active.Amount
	} else if !errors.Is(agerr, repo.ErrNotFound) {
		return agerr
	}
	// ① 窗口纠偏:目标窗口 = 已释放(桶1) − 已消费(我方账本 bigint)。地板 0。
	consumed, cerr := s.store.SumOrgConsumed(ctx, orgID)
	if cerr != nil {
		return cerr
	}
	target := released - consumed
	if target < 0 {
		target = 0 // 地板:消费超已释放(异常)也不把窗口算成负
	}
	actual, qerr := s.upstream.GetUserQuota(ctx, int(uid))
	if qerr != nil {
		return mapUpstream(qerr)
	}
	switch {
	case actual > target:
		// 超拨:实际窗口高于应有 → 自动 SUBTRACT 到目标(安全方向;地板 target≥0,绝不减到客户合法拥有之下)。
		if err := s.upstream.ManageUserQuota(ctx, int(uid), newapi.QuotaSubtract, actual-target); err != nil {
			return mapUpstream(err)
		}
		s.log.Warn("escrow 窗口超拨自动纠偏(减到目标)", "org_id", orgID, "actual", actual, "target", target, "subtract", actual-target)
	case actual < target:
		// 欠拨:实际窗口低于应有 → **绝不自动 ADD**(已 drain 仍低=真欠拨,非滞后)。续充 worker(读真实窗口自愈)
		// 或人工按 SLA 补。只告警,不动 newapi(§15 终极安全:对账永不经自动加垫钱)。
		s.log.Error("🔴escrow 窗口欠拨(不自动补,待续充 worker 自愈/人工按 SLA 修)", "org_id", orgID, "actual", actual, "target", target, "shortfall", target-actual)
	}
	// ③ 守恒断言:已释放 + 托管 == 充值 − 退款(纯平台侧,无消费项,不碰 used_quota)。
	holding, herr := s.store.SumHoldingEscrow(ctx, orgID)
	if herr != nil {
		return herr
	}
	bal, berr := s.store.GetOrCreateBalance(ctx, orgID)
	if berr != nil {
		return berr
	}
	if expected := bal.TotalRecharged - bal.TotalRefunded; released+holding != expected {
		s.log.Error("🔴escrow 守恒破(已释放+托管 != 充值−退款,疑双账分叉/丢单)", "org_id", orgID,
			"released", released, "holding", holding, "recharged", bal.TotalRecharged, "refunded", bal.TotalRefunded)
	}
	return nil
}
