// 架构B 阶段0(33 §3.1):划账引擎 = 守恒命根子。BE② 对外唯一写钱面——
// BE①(首笔/退额)、worker(订阅补满)、reconcile(修复) 全部经 Transfer 家族,谁都不许绕过直接改 quota。
//
// 【护栏全景】(31-ADR §4/§12):
//   · 用 Increase/DecreaseUserQuota(quota_guard,保 Redis 缓存新鲜),禁 override;
//   · 幂等不靠单次调用,靠账本(idempotency_key 唯一)+ 对账环"读-核-补";
//   · subtract 先于 add(fail 朝少钱,宁可少发绝不多发);
//   · 同一金库并发划账串行化(org 级进程锁;多节点前随选主升分布式锁,33 §10-2);
//   · money_freeze 急停管辖三入口(Transfer/Topup/Reconcile 的写);reconcile 读/告警恒开(33 §10-1);
//   · worker/reconcile-写 = 第 4/5 个 leader-gated 写钱入口,多节点前必过选主。
package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nexusapi-platform/enterprise/pkg/apperr"
	"github.com/nexusapi-platform/enterprise/repo"
)

// 账本 reason 常量(枚举收敛,报表/审计可依赖)。
const (
	ReasonInitialGrant  = "initial_grant"      // 开通首笔
	ReasonTopup         = "topup"              // 管理员追加
	ReasonOffboardRefund = "offboard_refund"   // 离职退额(成员→金库)
	ReasonSubscription  = "subscription_topup" // worker 周期补满
	ReasonReconcileFix  = "reconcile_fix"      // 对账环修复
)

// ErrMoneyFrozen 全局钱动作急停中(事故止血;与业务无关,平时恒 false)。
var ErrMoneyFrozen = apperr.New(apperr.CodeInvalidParam, 503, "平台钱动作已临时冻结(事故处置中),请稍后再试")

// moneyFrozen 读急停开关(读失败视为未冻结——fail-open,配置读挂不应停服;冻结是显式人工动作)。
func (s *Service) moneyFrozen(ctx context.Context) bool {
	frozen, err := s.store.GetSettingBool(ctx, "money_freeze")
	if err != nil {
		s.log.Error("读 money_freeze 失败(按未冻结处理,fail-open)", "err", err)
		return false
	}
	return frozen
}

// Transfer 划账守恒唯一入口(33 §3.1 五步序)。
// 每笔 = 账本一行(pending→applied);失败/超时不盲目重试,留 pending 给对账环收敛。
// actor 记入账本 created_by(审计);amountRaw 恒正,方向由 from/to 表达。
func (s *Service) Transfer(ctx context.Context, orgID int64, fromUID, toUID int, memberID int64, amountRaw int64, reason, idemKey, actor string) error {
	// ⓪ 急停闸(34 §3-⑤)。
	if s.moneyFrozen(ctx) {
		return ErrMoneyFrozen
	}
	if fromUID <= 0 || toUID <= 0 || fromUID == toUID {
		return apperr.InvalidParam("划账双方 user id 非法")
	}
	if idemKey == "" {
		return apperr.InvalidParam("缺少幂等键")
	}
	// 同一金库串行化(org 级;单节点=进程内锁,多节点前随选主升分布式锁,33 §10-2)。
	release, lerr := s.quotaLocker.Acquire(ctx, fmt.Sprintf("transfer:org:%d", orgID))
	if lerr != nil {
		return apperr.Internal("").WithCause(lerr)
	}
	defer release()

	// ① 校验:金额恒正 + 入账方写后 ≤int32 + 出账方实时余额充足(quota_guard 内含 clamp,
	//    但划账语义要求"足额划转",不足额=业务拒,不静默半划)。
	if amountRaw <= 0 {
		return apperr.InvalidParam("划账金额必须为正")
	}
	fromBal, gerr := s.upstream.GetUserQuota(ctx, fromUID)
	if gerr != nil {
		return mapUpstream(gerr)
	}
	if fromBal < amountRaw {
		return apperr.New(apperr.CodeInvalidParam, 409, fmt.Sprintf("出账方余额不足(余 %d,需 %d),请先充值", fromBal, amountRaw))
	}

	// ② 账本落 pending(idemKey 唯一;冲突=重放,按已存在行状态处置)。
	row, created, ierr := s.store.InsertTransferPending(ctx, &repo.LedgerTransfer{
		OrgID: orgID, FromUserID: int64(fromUID), ToUserID: int64(toUID), MemberID: memberID,
		AmountRaw: amountRaw, IdempotencyKey: idemKey, Reason: reason, CreatedBy: actor,
	})
	if ierr != nil {
		return apperr.Internal("").WithCause(ierr)
	}
	if !created {
		switch row.Status {
		case repo.LedgerApplied:
			return nil // 重放已完成的划账:幂等成功
		case repo.LedgerFailed:
			return apperr.New(apperr.CodeInvalidParam, 409, "该笔划账此前已判定失败,请换新幂等键重新发起")
		default:
			// pending 滞留:上一次执行中断,交对账环收敛,不在请求路径里抢修(避免双写竞态)。
			return apperr.New(apperr.CodeInvalidParam, 409, "该笔划账仍在处理中(对账环将收敛),请勿重复提交")
		}
	}

	// ③ 出账先扣(subtract 先于 add:中断时钱只会少不会多,fail 朝少钱)。
	deducted, derr := s.upstream.DecreaseUserQuota(ctx, fromUID, amountRaw)
	if derr != nil {
		// 出账失败(未扣到钱):账本行留 pending,对账环将核实双边实际值后标 failed。
		s.log.Error("划账:出账扣减失败(留 pending 给对账环)", "ledger_id", row.ID, "err", derr)
		return mapUpstream(derr)
	}
	if deducted < amountRaw {
		// clamp 生效=①校验后余额被并发消费掏空(时间抖动)。已扣部分留账本 pending,对账环按实际值收敛。
		s.log.Error("划账:出账实扣少于期望(并发消费抖动,留 pending 对账)", "ledger_id", row.ID, "expect", amountRaw, "actual", deducted)
		return apperr.New(apperr.CodeInvalidParam, 409, "出账方余额发生变动,本笔划账未完成(对账环将收敛)")
	}

	// ④ 入账(int32 越界由 quota_guard 预检;失败=守恒破的窗口,靠对账环补齐,绝不吞错)。
	if aerr := s.upstream.IncreaseUserQuota(ctx, toUID, amountRaw); aerr != nil {
		s.log.Error("划账:入账失败(出账已扣!守恒缺口,对账环必须补齐)", "ledger_id", row.ID, "err", aerr)
		return mapUpstream(aerr)
	}

	// ⑤ 账本置 applied。
	if merr := s.store.MarkTransferApplied(ctx, row.ID); merr != nil {
		// 双边都已完成、只差账本落状态:留 pending,对账环读双边实际值后会标 applied(读-核-补)。
		s.log.Error("划账:置 applied 失败(双边已完成,对账环将补状态)", "ledger_id", row.ID, "err", merr)
	}
	return nil
}

// Drift 对账环发现的漂移(账本期望 vs 实际 quota 不符)。
type Drift struct {
	LedgerID int64  `json:"ledger_id"`
	Kind     string `json:"kind"` // pending_stuck / identity_mismatch
	Detail   string `json:"detail"`
}

// ReconcileTransfers 对账环:读-核-补(33 §3.1)。
// 🔒 leader-gated(第 5 个写钱入口):补齐/标 failed 在写 quota/账本,多节点前必过选主。
// 【33 §10-1】读/检测/告警恒开;仅"写"(修复动作)受 money_freeze 管——冻结期间仍能看到漂移在哪,只是不自动修。
//
// 阶段0 交付骨架:pending 滞留行的核对与收敛闭环;交叉恒等式(Σ成员净增==Σ划账净入账)的全量扫描
// 由 BE② 在阶段1 按本骨架扩展(契约已定:SumAppliedNetByUser + GetUserQuota 对比)。
func (s *Service) ReconcileTransfers(ctx context.Context) ([]Drift, error) {
	writeAllowed := !s.moneyFrozen(ctx) // 冻结只掐写,检测照跑
	pend, err := s.store.ListPendingTransfers(ctx, 2*time.Minute, 100)
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	var drifts []Drift
	for _, t := range pend {
		d := Drift{LedgerID: t.ID, Kind: "pending_stuck",
			Detail: fmt.Sprintf("划账滞留 pending: org=%d %d→%d amount=%d reason=%s", t.OrgID, t.FromUserID, t.ToUserID, t.AmountRaw, t.Reason)}
		drifts = append(drifts, d)
		s.log.Error("对账环:发现滞留划账(需收敛)", "ledger_id", t.ID, "write_allowed", writeAllowed)
		if !writeAllowed {
			continue // 冻结:只报不修
		}
		// 读-核-补(阶段0 最小收敛,保守朝少钱):
		// 无法从双边当前值唯一还原"是否已扣/已加"(消费在并发发生),阶段0 策略 = 保守判定:
		// 仅当出账方余额 ≥ 金额(等价于"大概率没扣成")时标 failed 关单(让业务重发新幂等键);
		// 其余(可能已扣未加/已加未记)留 pending 并持续告警,由 BE② 阶段1 用"账本前后快照"扩展精确补齐。
		fromBal, gerr := s.upstream.GetUserQuota(ctx, int(t.FromUserID))
		if gerr != nil {
			continue
		}
		if fromBal >= t.AmountRaw {
			if merr := s.store.MarkTransferFailed(ctx, t.ID); merr == nil {
				s.log.Warn("对账环:滞留划账判未扣成,已标 failed 关单", "ledger_id", t.ID)
			}
		}
	}
	return drifts, nil
}

// RunSubscriptionTopup 订阅档位周期补满 worker。
// 🔒 leader-gated(第 4 个写钱入口):多节点前必过选主(单节点灰度不触发)。
// 跳过 disabled/offboarded/quarantined;幂等键=(成员,周期桶,组织时区自然边界);D=目标-剩余,增量划账。
//
// 阶段0 交付骨架(编译+契约成立);遍历/周期桶/时区边界由 BE② 阶段1 按契约实现。
func (s *Service) RunSubscriptionTopup(ctx context.Context) error {
	if s.moneyFrozen(ctx) {
		s.log.Warn("订阅补满 worker:money_freeze 生效,本轮跳过")
		return nil
	}
	// 阶段1 BE② 实现:ListActiveSubscriptionMembers → 按 (member, 周期桶) 幂等键调 s.Transfer(金库→成员, D)
	return nil
}

// VerifyQuotaPerUnit 启动自检(33 §3.6/34 §3-④):平台配置的 quota_per_unit 必须与所连 new-api
// 实例实际 QuotaPerUnit 一致——不一致拒启动(fail-fast),防两边漂移金额错 50 万倍。
// dev 可 env NEXUS_SKIP_QPU_CHECK=true 跳过(生产禁设);调用方在 cmd/server 启动链装配。
func (s *Service) VerifyQuotaPerUnit(ctx context.Context) error {
	want, err := s.store.GetSettingInt64(ctx, "quota_per_unit", 500000)
	if err != nil {
		return fmt.Errorf("读平台 quota_per_unit 配置失败: %w", err)
	}
	got, uerr := s.upstream.GetQuotaPerUnit(ctx)
	if uerr != nil {
		return fmt.Errorf("读 new-api 实际 QuotaPerUnit 失败: %w", uerr)
	}
	if int64(got) != want {
		return fmt.Errorf("QuotaPerUnit 不一致:平台配置=%d, new-api 实际=%v —— 拒绝启动(金额换算将错乱),请对齐 platform_setting.quota_per_unit", want, got)
	}
	return nil
}

// mapUpstreamLedger 占位:沿用现有 mapUpstream;此处仅保证 errors 包引用面稳定。
var _ = errors.Is
