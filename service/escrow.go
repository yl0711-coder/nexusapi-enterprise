package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

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

// DerivedBalance 模型2 读穿余额(14 §3.4):不持第二本权威余额,展示=桶1读穿(newapi user.quota 实际剩余)+ 托管桶之和。
type DerivedBalance struct {
	WindowQuota    int64 `json:"window_quota"`    // 桶1 实际剩余(读穿 new-api org user.quota)
	HoldingQuota   int64 `json:"holding_quota"`   // 平台库托管桶之和(未进窗口)
	AvailableQuota int64 `json:"available_quota"` // = 窗口 + 托管(组织当前可用总额)
}

// GetDerivedBalance 读穿组织**实时**窗口/托管拆分(**仅运营方**——window/holding 是内部概念不漏给客户)。
// 窗口读 new-api(桶1 实际剩余,含消费递减)、托管读平台库。客户余额走 GetBalance(v1 M5:同为读求和,
// 但只回合计 available + billing_kind,20-§3;旧"派生稳定值"口径已被 v1 裁定取代——对门B 关联组织恒 0 失真)。
// 组织未开通池子 → 全 0。
func (s *Service) GetDerivedBalance(ctx context.Context, c session.Claims, orgID int64) (*DerivedBalance, error) {
	if err := assertOrgScope(c, orgID); err != nil {
		return nil, err
	}
	if err := assertRole(c, session.RoleOperator); err != nil {
		return nil, err // 窗口/托管拆分仅运营方(客户走 GetBalance 的诚实稳定余额)
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

// refillToWindow 续充核心(自动 worker 与手工应急**共用同一把锁 + 同一原子路径**):窗口 < trigger 才补,
// 从托管 FIFO 并入桶1、补到窗口上限 WINDOW_CAP。返回实并入额。涉钱安全(R5 F1/F6):per-org 锁 + 单事务
// FOR UPDATE 锁桶 → 串行消除并发重复释放=超拨;newapi add 失败不返错(DB 原子提交是真相,对账/下轮续充兜)。
func (s *Service) refillToWindow(ctx context.Context, orgID, trigger int64) (int64, error) {
	cred, err := s.orgCred(ctx, orgID)
	if err != nil {
		return 0, err
	}
	release, lerr := s.quotaLocker.Acquire(ctx, escrowLockKey(orgID))
	if lerr != nil {
		return 0, apperr.Internal("").WithCause(lerr)
	}
	defer release()

	window, gerr := s.upstream.GetUserQuota(ctx, cred.NewapiUserID) // 锁内重读(触发判定+补入空间都用它)
	if gerr != nil {
		return 0, mapUpstream(gerr)
	}
	if window >= trigger {
		return 0, nil // 未触发/窗口已够
	}
	space := escrowWindowCap - window
	if space <= 0 {
		return 0, nil // 窗口已满
	}
	var merged int64
	if err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
		m, e := s.store.MergeHoldingIntoActiveTx(ctx, tx, orgID, space) // FIFO 并入,合计最多 space
		merged = m
		return e
	}); err != nil {
		return 0, apperr.Internal("").WithCause(err)
	}
	if merged > 0 {
		if err := s.upstream.ManageUserQuota(ctx, cred.NewapiUserID, newapi.QuotaAdd, merged); err != nil {
			s.log.Error("续充 add 进 newapi 失败(DB 已原子提交,对账/下轮续充兜)", "org_id", orgID, "merged", merged, "err", err)
		}
	}
	return merged, nil
}

// RefillWindow 手工应急续充(运营方):把托管补进窗口到上限(窗口 < 上限即补)。与自动 worker 同锁同原子路径。
func (s *Service) RefillWindow(ctx context.Context, c session.Claims, orgID int64) (*DerivedBalance, error) {
	if !s.fundingEnabled {
		return nil, apperr.NotFound("") // v1 escrow 休眠(20-§9):平台不经手钱,端点下线(v2 开 NEXUS_PLATFORM_FUNDING_ENABLED 恢复)
	}
	if err := assertOrgScope(c, orgID); err != nil {
		return nil, err
	}
	if err := assertRole(c, session.RoleOperator); err != nil {
		return nil, err // 动钱:仅运营方
	}
	merged, err := s.refillToWindow(ctx, orgID, escrowWindowCap) // trigger=上限:窗口<上限即补满
	if err != nil {
		return nil, err
	}
	if merged > 0 {
		s.audit(ctx, c, orgID, "escrow_refill_manual", "balance", &orgID, map[string]any{"merged": merged})
	}
	return s.GetDerivedBalance(ctx, c, orgID)
}

// escrowThreshold 组织生效续充阈值 = COALESCE(手动覆盖, 自动值);无配置行 → DEFAULT_NEW。
func (s *Service) escrowThreshold(ctx context.Context, orgID int64) (int64, error) {
	cfg, err := s.store.GetEscrowConfig(ctx, orgID)
	if errors.Is(err, repo.ErrNotFound) {
		return escrowDefaultNew, nil
	}
	if err != nil {
		return 0, err
	}
	return cfg.EffectiveThreshold(), nil
}

// recomputeThresholdAuto 按近7天日志重算自动阈值(补货点)并写库。历史<7天/无数据 → DEFAULT_NEW。
// 阈值 = clamp(peakHourly×LEAD×MARGIN + maxSingle, FLOOR, CEIL);raw 超 CEIL = 超重度组织,告警。
func (s *Service) recomputeThresholdAuto(ctx context.Context, orgID int64) error {
	windowStart := s.now().Add(-7 * 24 * time.Hour)
	peakHourly, maxSingle, earliest, err := s.store.EscrowUsageStats(ctx, orgID, windowStart)
	if err != nil {
		return err
	}
	hasHistory := earliest.Valid && !earliest.Time.After(windowStart) // 有 ≥7 天历史
	auto, overCeil := computeAutoThreshold(peakHourly, maxSingle, hasHistory)
	if overCeil {
		s.log.Warn("escrow 超重度组织:补货点超上限被 clamp,满窗口可能撑不过一续充周期(建议缩短 worker 间隔/升级容量)",
			"org_id", orgID, "raw", int64(float64(peakHourly)*escrowLead*escrowMargin)+maxSingle, "ceil", escrowCeil,
			"peak_hourly", peakHourly, "max_single", maxSingle)
	}
	return s.store.UpsertThresholdAuto(ctx, orgID, auto)
}

// computeAutoThreshold 纯函数(涉钱·补货点,抽出供单测):由近7天用量算自动补货点。
// hasHistory=false(历史<7天)→ DEFAULT_NEW;否则 auto = clamp(peakHourly×LEAD×MARGIN + maxSingle, FLOOR, CEIL),
// overCeil=raw 超上限(超重度组织,调用方告警但仍 clamp 到 CEIL)。
func computeAutoThreshold(peakHourly, maxSingle int64, hasHistory bool) (auto int64, overCeil bool) {
	if !hasHistory {
		return escrowDefaultNew, false
	}
	raw := int64(float64(peakHourly)*escrowLead*escrowMargin) + maxSingle
	overCeil = raw > escrowCeil
	auto = raw
	if auto < escrowFloor {
		auto = escrowFloor
	}
	if auto > escrowCeil {
		auto = escrowCeil
	}
	return auto, overCeil
}

// AutoRefill 自动续充 worker tick(leader 单写者;observe 也跑——续充是池子 funding,不停员工)。
// 逐有桶组织:懒重算阈值(配置陈旧>20h/无行)→ 窗口<阈值则补满。失败逐组织隔离、下轮重试。
//
// 【架构B 退役停调】(33 §5,组长裁定 33-§12-17):escrow 分桶下发整体退役(内含绕 Transfer 的
// quota 直写)。worker 已停调,函数保留待删,勿新增调用。
func (s *Service) AutoRefill(ctx context.Context) error {
	if !s.fundingEnabled {
		return nil // v1 escrow 休眠(20-§9):worker 静默短路(v2 开 flag 恢复)
	}
	orgIDs, err := s.store.ListEscrowOrgIDs(ctx)
	if err != nil {
		return err
	}
	for _, orgID := range orgIDs {
		if rerr := s.autoRefillOrg(ctx, orgID); rerr != nil {
			s.log.Error("自动续充失败(下轮重试)", "org_id", orgID, "err", rerr)
		}
	}
	return nil
}

func (s *Service) autoRefillOrg(ctx context.Context, orgID int64) error {
	// 阈值每天重算(懒:无配置行或 >20h 陈旧则重算)。
	cfg, cerr := s.store.GetEscrowConfig(ctx, orgID)
	if cerr != nil && !errors.Is(cerr, repo.ErrNotFound) {
		return cerr
	}
	if errors.Is(cerr, repo.ErrNotFound) || s.now().Sub(cfg.UpdatedAt) > 20*time.Hour {
		if rerr := s.recomputeThresholdAuto(ctx, orgID); rerr != nil {
			s.log.Error("续充阈值重算失败(用旧值继续)", "org_id", orgID, "err", rerr)
		}
	}
	threshold, terr := s.escrowThreshold(ctx, orgID)
	if terr != nil {
		return terr
	}
	merged, rerr := s.refillToWindow(ctx, orgID, threshold)
	if rerr != nil {
		return rerr
	}
	if merged > 0 {
		s.log.Info("自动续充(托管→窗口)", "org_id", orgID, "merged", merged, "threshold", threshold)
	}
	return nil
}

// ReconcileEscrow 是 escrow 对账 worker(R5 后裁定,leader 单写者周期跑)。observe 也跑:observe 只挡"给员工写
// 停人额度",对账纠的是**池子窗口**(过多才减、绝不加、有地板),不停员工。涉钱终极安全口径(14 §15):
//   目标窗口 = 已释放(桶1) − 已消费(**我方 usage_ledger,bigint,彻底不碰 new-api used_quota**,int32 会溢出+将被清零);
//   ① 实际窗口 > 目标 = 超拨 → **自动 SUBTRACT** 到目标(地板≥0,安全方向,绝不减到客户合法拥有之下);
//   ② 实际窗口 < 目标 = 欠拨 → **绝不自动 ADD**(正 delta 可能是日志滞后=瞬时超拨/垫钱方向)→ 只告警,
//      由续充 worker(读真实窗口自愈)或人工按 SLA 补;
//   ③ 守恒断言:已释放+托管 == 充值−退款(纯平台侧账,无消费项,不碰 used_quota)。
// 开跑前先 drain 一次结算(把日志水位追平到 now),使"已消费"最新——防欠拨告警被结算滞后刷假(§15 前提②)。
//
// 【架构B 退役停调】(33 §5,组长裁定 33-§12-17):escrow 整体退役。worker 已停调,函数保留待删,
// 勿新增调用。注:handler 仍挂 escrow-balance/escrow/refill 两端点(RefillWindow),由组长阶段2
// 随 handler 层清理下线(现受 fundingEnabled=false 休眠闸兜底)。
func (s *Service) ReconcileEscrow(ctx context.Context) error {
	if !s.fundingEnabled {
		return nil // v1 escrow 休眠(20-§9):worker 静默短路(v2 开 flag 恢复)
	}
	// B5:leader-only 准入(escrow-drain·对账入口)。非 leader 不做。
	if ok, _, err := s.leadership.CanRunTick(ctx); err != nil {
		return err
	} else if !ok {
		return nil
	}
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

// escrowMaxCorrectFraction B2:单轮 escrow 窗口超拨自动纠偏的最大比例(占当前窗口);超此只告警不自动减,
// 防"ledger 多算导致的假超拨"被一次减成真扣客户钱。0.2 = 一次最多减掉窗口的 20%。
const escrowMaxCorrectFraction = 0.2

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
	// ① 窗口纠偏:目标窗口 = 已释放(桶1) − 已消费(funding 激活后的增量,B1)。地板 0。
	consumed, cerr := s.store.SumOrgConsumed(ctx, orgID)
	if cerr != nil {
		return cerr
	}
	// B1:排除 funding 激活前(观测期)的历史消费——首次对账快照当前 SUM(ledger) 为基线,此后只算增量;
	// 否则 v2 首充窗口被整段观测期消费冲成 0(客户真亏)。未快照时基线=当前(增量 0),优惠客户方向。
	baseline := consumed
	cfg, cfgErr := s.store.GetEscrowConfig(ctx, orgID)
	if cfgErr != nil && !errors.Is(cfgErr, repo.ErrNotFound) {
		return cfgErr
	}
	if cfg != nil && cfg.ConsumedBaseline != nil {
		baseline = *cfg.ConsumedBaseline
	} else if serr := s.store.SetEscrowConsumedBaseline(ctx, orgID, consumed); serr != nil {
		return serr
	}
	target := released - (consumed - baseline)
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
		delta := actual - target
		// B2:大额纠偏告警(不阻断)——正确性押"ledger 不多算";万一 ledger 多算导致的假超拨,一次减掉过大比例
		// 就是真扣客户钱。但合法退款/续充也会产生大额纠偏,**不能只凭幅度一律不减**(否则超拨窗口留存也是错、且破退款)。
		// 故:超过窗口 escrowMaxCorrectFraction 的纠偏**照常执行 + 同时告警**,供人工复核是否 ledger 多算;
		// ledger 多算本身另由 ReconcileBackfillLedger / ReconcileBilling 对账告警兜住。
		if actual > 0 && float64(delta) > float64(actual)*escrowMaxCorrectFraction {
			s.log.Error("escrow 窗口大额纠偏(疑 ledger 多算?人工复核)", "org_id", orgID, "actual", actual, "target", target, "delta", delta, "frac", escrowMaxCorrectFraction)
			s.auditSystem(ctx, orgID, "escrow_large_correction", "balance", &orgID,
				map[string]any{"actual": actual, "target": target, "delta": delta}, "alert")
		}
		if err := s.upstream.ManageUserQuota(ctx, int(uid), newapi.QuotaSubtract, delta); err != nil {
			return mapUpstream(err)
		}
		s.log.Warn("escrow 窗口超拨自动纠偏(减到目标)", "org_id", orgID, "actual", actual, "target", target, "subtract", delta)
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
