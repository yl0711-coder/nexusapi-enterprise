// 架构B 阶段1(33 §3.1,BE②):划账引擎 = 守恒命根子。BE② 对外唯一写钱面——
// BE①(首笔/退额)、worker(订阅补满)、reconcile(修复)、追加划账 全部经 Transfer 家族,谁都不许绕过直接改 quota。
//
// 【护栏全景】(31-ADR §4/§12):
//   · 用 Increase/DecreaseUserQuota(quota_guard,保 Redis 缓存新鲜),禁 override;
//   · 幂等不靠单次调用,靠账本(idempotency_key 唯一)+ 对账环"读-核-补";
//   · subtract 先于 add(fail 朝少钱,宁可少发绝不多发);
//   · 同一金库并发划账串行化(org 级进程锁;多节点前随选主升分布式锁,33 §10-2);
//   · money_freeze 急停管辖三入口(Transfer/Topup/Reconcile);reconcile 检测/告警恒开、
//     一切修复写(含账本状态写)受 freeze 管(组长裁定 33-§12-10);
//   · worker/reconcile = 第 4/5 个 leader-gated 写钱入口,多节点前必过选主。
//
// 【阶段1 精确补齐】saga 步骤日志(0035)+ 双证据判定:
//   主证据 = new-api manage 日志(rc.4 已核:ManageUser add/subtract 均记 LogTypeManage,
//   controller/user.go:937/948,组长裁定 33-§12-8);兜底 = from_balance_before 相对取证。
//   裁决纪律:自动写钱只发生在账目封闭性数学上证明缺口存在的那一侧;一切对不上的情形停手、告警、留给人。
package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nexusapi-platform/enterprise/adapter/newapi"
	"github.com/nexusapi-platform/enterprise/model"
	"github.com/nexusapi-platform/enterprise/pkg/apperr"
	"github.com/nexusapi-platform/enterprise/pkg/session"
	"github.com/nexusapi-platform/enterprise/repo"
)

// 账本 reason 常量(枚举收敛,报表/审计可依赖)。
const (
	ReasonInitialGrant   = "initial_grant"      // 开通首笔
	ReasonTopup          = "topup"              // 管理员追加
	ReasonOffboardRefund = "offboard_refund"    // 离职退额(成员→金库)
	ReasonSubscription   = "subscription_topup" // worker 周期补满
	ReasonReconcileFix   = "reconcile_fix"      // 对账环修复
)

// ErrMoneyFrozen 全局钱动作急停中(事故止血;与业务无关,平时恒 false)。
var ErrMoneyFrozen = apperr.New(apperr.CodeInvalidParam, 503, "平台钱动作已临时冻结(事故处置中),请稍后再试")

// 划账错误哨兵(errors.Is 判别;经 apperr.WithCause 携带,worker/调用方程序化分支,不匹配字符串)。
var (
	errCauseInsufficient  = errors.New("ledger: 出账方余额不足")
	errCauseReplayPending = errors.New("ledger: 幂等重放命中 pending(对账环收敛中)")
	errCauseReplayFailed  = errors.New("ledger: 幂等重放命中 failed(需换新幂等键)")
)

// 对账环参数。
const (
	reconcilePendingAge  = 2 * time.Minute // pending 滞留判定阈值(避开在飞主路径)
	reconcileEscalateAge = 24 * time.Hour  // 滞留看门狗:超此时长未收敛升级告警
	identityScanInterval = time.Hour       // 交叉恒等式全量扫描节拍(N 次上游读,低频)
	evidenceWindowSlack  = 5 * time.Minute // 日志取证窗口前置裕量(平台 DB 与 new-api 时钟偏差吸收)
	evidenceMaxPages     = 10              // 取证窗口日志翻页上限(超出=窗口内操作异常多,判 unknown)
)

// ledgerRuntime 钱核心的进程内运行态(单 leader 进程;多节点前随选主重造)。零值可用,懒初始化。
type ledgerRuntime struct {
	mu           sync.Mutex
	topupSeen    map[int64]string // memberID → 本进程已处理的周期桶(重启丢失由账本幂等键兜底)
	negDrift     map[int64]int64  // newapi uid → 上轮负漂移值(连续两轮同额才告警,滤结算/日志暂态)
	lastIdentity time.Time        // 上次恒等式全量扫描时刻
}

func (l *ledgerRuntime) seenBucket(memberID int64) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.topupSeen[memberID]
}

func (l *ledgerRuntime) markBucket(memberID int64, bucket string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.topupSeen == nil {
		l.topupSeen = map[int64]string{}
	}
	l.topupSeen[memberID] = bucket
}

// moneyFrozen 读急停开关。**fail-closed(39号体检阻断-3,涉钱红线)**:读失败视为已冻结——
// 急停的语义就是"出事时拦住钱",DB 抖动/异常正是最该冻结的时刻,绝不能恰好放行。
// (平台其它读失败处 fail-open 是对的:观测/展示不该因读挂阻断业务;急停开关是唯一例外。)
// 影响面=划账/订阅补满/reconcile 修复写被暂拒(可重试);检测与告警不经此函数、恒开。
func (s *Service) moneyFrozen(ctx context.Context) bool {
	frozen, err := s.store.GetSettingBool(ctx, "money_freeze")
	if err != nil {
		s.log.Error("🔴读 money_freeze 失败,按已冻结处理拦住钱动作(fail-closed;读取恢复后自动解除)", "err", err)
		return true
	}
	return frozen
}

// ───────────────────────────── Transfer 主路径 ─────────────────────────────

// transferOpts 内部选项(不改冻结的导出签名,组长裁定 33-§12-3)。
type transferOpts struct {
	// sweep 扫余模式(离职退额专用):金额=锁内实时读到的出账方余额;实扣不足(在途消费后补)
	// 按实扣 roll-forward 入账,不判 partial 失败(裁定 33-§12-12)。
	sweep bool
}

// Transfer 划账守恒唯一入口(33 §3.1 五步序 + 阶段1 saga 日志)。
// 每笔 = 账本一行(pending→applied);失败/超时不盲目重试,留 pending 给对账环收敛。
// actor 记入账本 created_by(审计);amountRaw 恒正,方向由 from/to 表达。
func (s *Service) Transfer(ctx context.Context, orgID int64, fromUID, toUID int, memberID int64, amountRaw int64, reason, idemKey, actor string) error {
	_, err := s.transferExec(ctx, orgID, fromUID, toUID, memberID, amountRaw, reason, idemKey, actor, transferOpts{})
	return err
}

// transferExec 五步序执行体。返回实际划转 raw(sweep 调用方需要)。
//
//	⓪ 急停闸 → ① org 锁内校验(余额/int32 预检) → ② 账本 pending(recorded)
//	→ ③ 出账 subtract + J1(debited) → ④ 入账 add + J2(credited) → ⑤ applied
func (s *Service) transferExec(ctx context.Context, orgID int64, fromUID, toUID int, memberID int64, amountRaw int64, reason, idemKey, actor string, opt transferOpts) (int64, error) {
	// ⓪ 急停闸(34 §3-⑤)。
	if s.moneyFrozen(ctx) {
		return 0, ErrMoneyFrozen
	}
	if fromUID <= 0 || toUID <= 0 || fromUID == toUID {
		return 0, apperr.InvalidParam("划账双方 user id 非法")
	}
	if idemKey == "" {
		return 0, apperr.InvalidParam("缺少幂等键")
	}
	if amountRaw <= 0 && !opt.sweep {
		return 0, apperr.InvalidParam("划账金额必须为正")
	}
	// 同一金库串行化(org 级;单节点=进程内锁,多节点前随选主升分布式锁,33 §10-2)。
	release, lerr := s.quotaLocker.Acquire(ctx, fmt.Sprintf("transfer:org:%d", orgID))
	if lerr != nil {
		return 0, apperr.Internal("").WithCause(lerr)
	}
	defer release()

	// ① 校验:出账方实时余额(quota_guard 内含 clamp,但划账语义要求"足额划转",不足额=业务拒,不静默半划;
	//    sweep 例外:金额=当下余额,扫多少是多少)。入账方 int32 预检(guard 在 ④ 再兜;此处提前拒,
	//    避免可预见的"已扣未加"滞留行)。
	fromBal, gerr := s.upstream.GetUserQuota(ctx, fromUID)
	if gerr != nil {
		return 0, mapUpstream(gerr)
	}
	if opt.sweep {
		amountRaw = fromBal
		if amountRaw <= 0 {
			return 0, nil // 无可扫(已被在途消费清空/本就为 0):幂等安全,无账本行
		}
	} else if fromBal < amountRaw {
		return 0, apperr.New(apperr.CodeInvalidParam, 409,
			fmt.Sprintf("出账方余额不足(余 %d,需 %d),请先充值", fromBal, amountRaw)).WithCause(errCauseInsufficient)
	}
	toBal, gerr := s.upstream.GetUserQuota(ctx, toUID)
	if gerr != nil {
		return 0, mapUpstream(gerr)
	}
	if toBal+amountRaw > newapi.Int32QuotaMax {
		return 0, apperr.New(apperr.CodeInvalidParam, 409,
			fmt.Sprintf("入账方写后额度将超 int32 上限(当前 %d + %d),拒绝划账", toBal, amountRaw))
	}

	// ② 账本落 pending(idemKey 唯一;冲突=重放,按已存在行状态处置)。from_balance_before 是
	//    recorded 行"相对取证法"的锚点(锁内读;同 org 划账被本锁串行,锚点可信)。
	row, created, ierr := s.store.InsertTransferPending(ctx, &repo.LedgerTransfer{
		OrgID: orgID, FromUserID: int64(fromUID), ToUserID: int64(toUID), MemberID: memberID,
		AmountRaw: amountRaw, IdempotencyKey: idemKey, Reason: reason, CreatedBy: actor,
		FromBalanceBefore: &fromBal,
	})
	if ierr != nil {
		return 0, apperr.Internal("").WithCause(ierr)
	}
	if !created {
		// 39号体检 P2-1(涉钱):幂等重放必须先验参数一致——同键但金额/方向/成员不一致时,
		// 绝不能按"已完成"回幂等成功(接口回 ok 实际一分没划=掉单假到账)。不一致一律 409 如实报。
		if row.AmountRaw != amountRaw || row.FromUserID != int64(fromUID) || row.ToUserID != int64(toUID) || row.MemberID != memberID {
			return 0, apperr.New(apperr.CodeInvalidParam, 409, fmt.Sprintf(
				"幂等键冲突:该键已存在一笔参数不同的划账(已有金额 %d,本次 %d)——本次未执行,请换新幂等键发起", row.AmountRaw, amountRaw))
		}
		switch row.Status {
		case repo.LedgerApplied:
			return row.DebitedRaw, nil // 重放已完成的同参划账:幂等成功
		case repo.LedgerFailed:
			return 0, apperr.New(apperr.CodeInvalidParam, 409,
				"该笔划账此前已判定失败,请换新幂等键重新发起").WithCause(errCauseReplayFailed)
		default:
			// pending 滞留:上一次执行中断,交对账环收敛,不在请求路径里抢修(避免双写竞态)。
			return 0, apperr.New(apperr.CodeInvalidParam, 409,
				"该笔划账仍在处理中(对账环将收敛),请勿重复提交").WithCause(errCauseReplayPending)
		}
	}

	// ③ 出账先扣(subtract 先于 add:中断时钱只会少不会多,fail 朝少钱)。
	deducted, derr := s.upstream.DecreaseUserQuota(ctx, fromUID, amountRaw)
	if derr != nil {
		// 出账失败(大概率未扣到钱,但"已发出未确认"不能排除):账本行留 recorded,
		// 对账环以 manage 日志主证据判定:落地→roll-forward / 未落地→failed 关单。
		s.log.Error("划账:出账扣减失败(留 recorded 给对账环)", "ledger_id", row.ID, "err", derr)
		return 0, mapUpstream(derr)
	}
	// J1:出账已确认,立即落进度日志。本地 DB 写失败=钱已扣、日志未记(极罕见):停手报错,
	// 行留 recorded,对账环日志证据可还原,绝不带伤继续入账。
	if jerr := s.store.MarkTransferDebited(ctx, row.ID, deducted); jerr != nil {
		s.log.Error("划账:J1 进度日志写失败(钱已扣!对账环将按日志证据收敛)", "ledger_id", row.ID, "deducted", deducted, "err", jerr)
		return 0, apperr.Internal("").WithCause(jerr)
	}
	if deducted == 0 {
		// 实扣 0(①校验后余额被并发消费清空):无净效果,直接关单。
		if merr := s.store.MarkTransferFailedWithReason(ctx, row.ID, repo.FailSweepEmpty); merr != nil {
			s.log.Error("划账:实扣 0 关单失败(留对账环)", "ledger_id", row.ID, "err", merr)
		}
		if opt.sweep {
			return 0, nil
		}
		return 0, apperr.New(apperr.CodeInvalidParam, 409, "出账方余额发生变动,本笔划账未完成")
	}
	if deducted < amountRaw && !opt.sweep {
		// 普通模式部分扣(并发消费抖动;金库为出账方时理论不可能——金库无 token 无消费、同 org 划账被锁串行):
		// 标记 partial_debit,对账环两阶段退回出账方后关单(绝不带部分金额入账)。
		s.log.Error("划账:出账实扣少于期望(标 partial_debit 待对账环退回)", "ledger_id", row.ID, "expect", amountRaw, "actual", deducted)
		if _, serr := s.store.SetTransferFailReason(ctx, row.ID, "", repo.FailPartialDebit); serr != nil {
			s.log.Error("划账:partial_debit 标记失败(对账环仍能按 debited_raw<amount_raw 识别)", "ledger_id", row.ID, "err", serr)
		}
		return 0, apperr.New(apperr.CodeInvalidParam, 409, "出账方余额发生变动,本笔划账未完成(对账环将退回)")
	}
	// sweep 模式:实扣即目标(roll-forward),入账按 deducted。
	moved := deducted

	// ④ 入账(int32 越界由 quota_guard 再兜;失败=守恒缺口窗口,靠对账环补齐,绝不吞错)。
	if aerr := s.upstream.IncreaseUserQuota(ctx, toUID, moved); aerr != nil {
		s.log.Error("划账:入账失败(出账已扣!守恒缺口,对账环必须补齐)", "ledger_id", row.ID, "err", aerr)
		return 0, mapUpstream(aerr)
	}
	// J2:入账已确认。写失败只告警不报错(双边钱面已正确,状态由对账环日志证据补齐)。
	if jerr := s.store.MarkTransferCredited(ctx, row.ID); jerr != nil {
		s.log.Error("划账:J2 进度日志写失败(双边已完成,对账环将补状态)", "ledger_id", row.ID, "err", jerr)
		return moved, nil
	}

	// ⑤ 账本置 applied。
	if merr := s.store.MarkTransferApplied(ctx, row.ID); merr != nil {
		s.log.Error("划账:置 applied 失败(双边已完成,对账环将补状态)", "ledger_id", row.ID, "err", merr)
	}
	return moved, nil
}

// ───────────────────────────── 追加划账 / 离职退额 ─────────────────────────────

// GrantMemberQuota 组织管理员追加划账(33 §3.5 POST /members/:id/quota:grant 背后;契约增补,裁定 33-§12-3)。
// 金库→成员,走 Transfer(急停/守恒/int32 由其内部管辖);成员额度帽在此校验(31-ADR §4.3)。
func (s *Service) GrantMemberQuota(ctx context.Context, c session.Claims, orgID, memberID int64, amountRaw int64, note, idemKey string) error {
	if err := assertOrgScope(c, orgID); err != nil {
		return err
	}
	if err := assertRole(c, session.RoleOperator, session.RoleOrgAdmin); err != nil {
		return err
	}
	if c.SupportSessionID != 0 {
		return apperr.MoneyRedline("支持态下禁止动钱(划账红线)")
	}
	if amountRaw <= 0 {
		return apperr.InvalidParam("划账金额必须为正")
	}
	m, err := s.store.GetMember(ctx, orgID, memberID)
	if errors.Is(err, repo.ErrNotFound) {
		return apperr.NotFound("成员不存在")
	}
	if err != nil {
		return apperr.Internal("").WithCause(err)
	}
	if m.Status != model.MemberStatusActive || m.BootstrapState != model.BootstrapDone || m.NewapiUserID == nil {
		return apperr.Conflict("成员未就绪或已停用,不能追加额度")
	}
	org, err := s.store.GetOrganization(ctx, orgID)
	if err != nil {
		return apperr.Internal("").WithCause(err)
	}
	if org.NewapiUserID == nil {
		return apperr.Conflict("组织金库未开通")
	}
	// 成员额度帽(平台默认 $1000,超管可配;int32 上限由 quota_guard 二道兜)。
	capRaw, serr := s.store.GetSettingInt64(ctx, "member_quota_cap_raw", 500_000_000)
	if serr != nil {
		s.log.Error("读成员额度帽失败(用默认值,fail-open)", "err", serr)
	}
	cur, gerr := s.upstream.GetUserQuota(ctx, int(*m.NewapiUserID))
	if gerr != nil {
		return mapUpstream(gerr)
	}
	if cur+amountRaw > capRaw {
		return apperr.New(apperr.CodeInvalidParam, 409,
			fmt.Sprintf("超出成员额度帽(当前 %d + %d > 帽 %d),请调低金额或联系超管调帽", cur, amountRaw, capRaw))
	}
	// 幂等键:客户端键限定作用域;无键则服务端生成(HTTP 层重试幂等由 platform_idempotency 兜)。
	if idemKey == "" {
		idemKey = fmt.Sprintf("grant:%d:%d:%s", orgID, memberID, randomHex(12))
	} else {
		idemKey = fmt.Sprintf("grant:%d:%d:%s", orgID, memberID, idemKey)
	}
	if err := s.Transfer(ctx, orgID, int(*org.NewapiUserID), int(*m.NewapiUserID), memberID, amountRaw, ReasonTopup, idemKey, actorOf(c)); err != nil {
		return err
	}
	s.audit(ctx, c, orgID, "grant_member_quota", "member", &memberID, map[string]any{
		"amount_raw": amountRaw, "note": note, "idem_key": idemKey,
	})
	return nil
}

// RefundMemberBalance 离职退额原语(契约增补,裁定 33-§12-3):读实时余额反向 Transfer(成员→金库)。
// disable-first / 静默确认由 BE① 调度;本原语坚守钱面红线:成员仍 active 一律拒(先停后退)。
// sweep 语义:实扣不足(disable→静默 ~60s TTL 窗内在途消费后补)按实扣 roll-forward;返回实退总额。
// 幂等安全:金额=实时余额,重复/并发调用第二笔扫到的必然是 0(锁内读),不会双退。
func (s *Service) RefundMemberBalance(ctx context.Context, orgID, memberID int64, actor string) (int64, error) {
	m, err := s.store.GetMemberAnyState(ctx, orgID, memberID)
	if errors.Is(err, repo.ErrNotFound) {
		return 0, apperr.NotFound("成员不存在")
	}
	if err != nil {
		return 0, apperr.Internal("").WithCause(err)
	}
	if m.Status == model.MemberStatusActive {
		return 0, apperr.Conflict("成员仍处于启用状态,须先停用(disable-first)再退额")
	}
	if m.NewapiUserID == nil {
		return 0, nil // 未开通 new-api user:无额可退
	}
	org, err := s.store.GetOrganization(ctx, orgID)
	if err != nil {
		return 0, apperr.Internal("").WithCause(err)
	}
	if org.NewapiUserID == nil {
		return 0, apperr.Conflict("组织金库未开通,无法退回")
	}
	anchor := s.now().UnixMilli()
	var total int64
	for attempt := 1; attempt <= 2; attempt++ {
		bal, gerr := s.upstream.GetUserQuota(ctx, int(*m.NewapiUserID))
		if gerr != nil {
			return total, mapUpstream(gerr)
		}
		if bal <= 0 {
			break
		}
		idem := fmt.Sprintf("offboard:%d:%d", memberID, anchor)
		if attempt > 1 {
			idem = fmt.Sprintf("offboard:%d:%d:%d", memberID, anchor, attempt)
		}
		moved, terr := s.transferExec(ctx, orgID, int(*m.NewapiUserID), int(*org.NewapiUserID), memberID,
			bal, ReasonOffboardRefund, idem, actor, transferOpts{sweep: true})
		if terr != nil {
			return total, terr
		}
		total += moved
	}
	// 收尾复核:两轮扫余后仍有余额(极窄窗:停用前恰好落了订阅补满等)→ 告警人工。
	if bal, gerr := s.upstream.GetUserQuota(ctx, int(*m.NewapiUserID)); gerr == nil && bal > 0 {
		s.log.Error("离职退额:两轮扫余后成员仍有余额(需人工核对)", "member_id", memberID, "residual", bal)
		s.auditLedger(ctx, orgID, "offboard_refund_residual", &memberID, map[string]any{"residual_raw": bal}, "alert")
	}
	if total > 0 {
		s.auditLedger(ctx, orgID, "offboard_refund", &memberID, map[string]any{"refunded_raw": total, "actor": actor}, "ok")
	}
	return total, nil
}

// ───────────────────────────── 对账环(读-核-补) ─────────────────────────────

// Drift 对账环发现的漂移(账本期望 vs 实际 quota 不符 / 恒等式破)。
type Drift struct {
	LedgerID int64  `json:"ledger_id"`
	Kind     string `json:"kind"` // pending_stuck / identity_mismatch / treasury_alert
	Detail   string `json:"detail"`
}

// ReconcileTransfers 对账环:读-核-补(33 §3.1)。
// 🔒 leader-gated(第 5 个写钱入口):补齐/标 failed 在写 quota/账本,多节点前必过选主。
// 【33 §10-1 + 裁定 33-§12-10】读/检测/告警恒开;一切修复"写"(quota 写与账本状态写)受 money_freeze 管
// ——冻结期间仍能看到漂移在哪,只是不自动修。
// 附带交叉恒等式全量扫描(内部限频 identityScanInterval;读+告警,永不自动修)。
func (s *Service) ReconcileTransfers(ctx context.Context) ([]Drift, error) {
	if ok, _, lerr := s.leadership.CanRunTick(ctx); lerr != nil {
		return nil, lerr
	} else if !ok {
		return nil, nil // 非 leader:检测也让给 leader(避免双份告警;多节点升级点已注)
	}
	writeAllowed := !s.moneyFrozen(ctx)
	pend, err := s.store.ListPendingTransfers(ctx, reconcilePendingAge, 100)
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	var drifts []Drift
	for _, t := range pend {
		if d := s.convergePending(ctx, t, writeAllowed); d != nil {
			drifts = append(drifts, *d)
		}
	}
	// 交叉恒等式「成员 quota + Σ消费 == Σ划账净入账」全量扫描(读+告警恒开,含 freeze 期间)。
	drifts = append(drifts, s.scanMoneyIdentity(ctx)...)
	return drifts, nil
}

// convergePending 收敛单条滞留行。返回非 nil = 本轮未能收敛(漂移上报);nil = 已收敛/已被主路径处理。
func (s *Service) convergePending(ctx context.Context, stale *repo.LedgerTransfer, writeAllowed bool) *Drift {
	d := &Drift{LedgerID: stale.ID, Kind: "pending_stuck",
		Detail: fmt.Sprintf("划账滞留 pending: org=%d %d→%d amount=%d phase=%s fail_reason=%q reason=%s",
			stale.OrgID, stale.FromUserID, stale.ToUserID, stale.AmountRaw, stale.Phase, stale.FailReason, stale.Reason)}
	s.log.Error("对账环:发现滞留划账(需收敛)", "ledger_id", stale.ID, "phase", stale.Phase, "write_allowed", writeAllowed)
	if s.now().Sub(stale.CreatedAt) > reconcileEscalateAge {
		s.auditLedger(ctx, stale.OrgID, "ledger_pending_stuck_24h", &stale.MemberID,
			map[string]any{"ledger_id": stale.ID, "phase": stale.Phase}, "alert")
	}
	if !writeAllowed {
		return d // 冻结:只报不修(含账本状态写,裁定 33-§12-10)
	}
	// 与主路径同一把 org 锁:排除与在飞 Transfer/重放的竞态;锁内重读最新态再判。
	release, lerr := s.quotaLocker.Acquire(ctx, fmt.Sprintf("transfer:org:%d", stale.OrgID))
	if lerr != nil {
		return d
	}
	defer release()
	t, gerr := s.store.GetTransfer(ctx, stale.ID)
	if gerr != nil {
		return d
	}
	if t.Status != repo.LedgerPending {
		return nil // 已被收敛
	}
	var resolved bool
	switch {
	case t.Phase == repo.PhaseCredited:
		resolved = s.finishCredited(ctx, t)
	case t.Phase == repo.PhaseDebited:
		resolved = s.convergeDebited(ctx, t)
	default: // recorded
		resolved = s.convergeRecorded(ctx, t)
	}
	if resolved {
		return nil
	}
	return d
}

// finishCredited 双边上游写都已确认(J2 在)、只差 status:零歧义,补 applied。
func (s *Service) finishCredited(ctx context.Context, t *repo.LedgerTransfer) bool {
	if err := s.store.MarkTransferApplied(ctx, t.ID); err != nil {
		s.log.Error("对账环:补 applied 失败(下轮重试)", "ledger_id", t.ID, "err", err)
		return false
	}
	s.log.Warn("对账环:credited 行补状态 applied(双边此前已完成)", "ledger_id", t.ID)
	return true
}

// convergeRecorded recorded 行:出账是否落地?主证据 = 出账方 manage 日志(减少);兜底 = 相对取证。
// 落地 → J1 补记 + 转 debited 收敛(roll-forward);未落地 → failed 关单(业务换新键重发);
// 对不上 → 告警停手(绝不猜)。
func (s *Service) convergeRecorded(ctx context.Context, t *repo.LedgerTransfer) bool {
	verdict := s.judgeUpstreamOp(ctx, t, t.FromUserID, repo.OpDebit, t.AmountRaw)
	if verdict == opUnknown {
		verdict = s.judgeRecordedByBalance(ctx, t)
	}
	switch verdict {
	case opNotLanded:
		if err := s.store.MarkTransferFailedWithReason(ctx, t.ID, repo.FailDebitNotLanded); err != nil {
			s.log.Error("对账环:recorded 关单失败(下轮重试)", "ledger_id", t.ID, "err", err)
			return false
		}
		s.log.Warn("对账环:滞留划账判定出账未落地,已标 failed 关单(业务可换新幂等键重发)",
			"ledger_id", t.ID, "amount", t.AmountRaw)
		s.auditLedger(ctx, t.OrgID, "ledger_reconcile_close", &t.MemberID,
			map[string]any{"ledger_id": t.ID, "verdict": "debit_not_landed"}, "ok")
		return true
	case opLanded:
		// 出账已落地未记(J1 崩溃窗):补进度日志,随即按 debited 规则继续收敛入账侧。
		if err := s.store.MarkTransferDebited(ctx, t.ID, t.AmountRaw); err != nil {
			s.log.Error("对账环:recorded 补 J1 失败(下轮重试)", "ledger_id", t.ID, "err", err)
			return false
		}
		s.log.Warn("对账环:recorded 行日志证据判出账已落地,roll-forward 至 debited", "ledger_id", t.ID)
		t2, gerr := s.store.GetTransfer(ctx, t.ID)
		if gerr != nil {
			return false
		}
		return s.convergeDebited(ctx, t2)
	default:
		s.log.Error("对账环:recorded 行证据不足/对不上,停手待人工(取证已附)",
			"ledger_id", t.ID, "from_balance_before", t.FromBalanceBefore, "amount", t.AmountRaw)
		s.auditLedger(ctx, t.OrgID, "ledger_reconcile_ambiguous", &t.MemberID,
			map[string]any{"ledger_id": t.ID, "phase": "recorded"}, "alert")
		return false
	}
}

// judgeRecordedByBalance recorded 行的兜底相对取证(日志证据不可用时):
// 锚点 = from_balance_before(锁内读),叠加本行之后同出账方的已确认账本净变动;
// 金库无消费(I7 铁律)且减少只来自平台划账 → 差值 0=未扣 / −amount=已扣;其余(代充噪声等)=unknown。
func (s *Service) judgeRecordedByBalance(ctx context.Context, t *repo.LedgerTransfer) opVerdict {
	if t.FromBalanceBefore == nil {
		return opUnknown
	}
	delta, amb, err := s.store.SumJournaledDeltaAfter(ctx, t.FromUserID, t.ID)
	if err != nil || amb > 0 {
		return opUnknown // 有进度不确定的兄弟行:锚点算不准,停手
	}
	b1, gerr := s.upstream.GetUserQuota(ctx, int(t.FromUserID))
	if gerr != nil {
		return opUnknown
	}
	// 双读三明治:计算期间余额/账本稳定才可判。
	delta2, amb2, err2 := s.store.SumJournaledDeltaAfter(ctx, t.FromUserID, t.ID)
	b2, gerr2 := s.upstream.GetUserQuota(ctx, int(t.FromUserID))
	if err2 != nil || gerr2 != nil || amb2 > 0 || b1 != b2 || delta != delta2 {
		return opUnknown
	}
	diff := b2 - (*t.FromBalanceBefore + delta)
	switch {
	case diff == 0:
		return opNotLanded
	case diff == -t.AmountRaw:
		return opLanded
	default:
		return opUnknown // 窗口内有代充等账外扰动,无法排除巧合,留人工
	}
}

// convergeDebited debited 行:出账已确认。两分支:
//   - fail_reason ∈ {partial_debit, refunding} → 退回出账方后关单(两阶段防重复退);
//   - 正常缺口 → 入账是否落地?主证据 = 入账方 manage 日志(增加);
//     已落地 → 只补状态(绝不再发);未落地 → 恒等式静默三明治**共同确认**后才补入账(唯一自动补发点,
//     双保险:双发需 new-api 崩溃窗 × 恒等式巧合同时成立);入账 int32 不可达 → 退回出账方。
func (s *Service) convergeDebited(ctx context.Context, t *repo.LedgerTransfer) bool {
	if t.FailReason == repo.FailPartialDebit || t.FailReason == repo.FailRefunding {
		return s.refundDebited(ctx, t)
	}
	moved := t.DebitedRaw
	if moved <= 0 {
		// debited 但实扣 0(主路径已关单的防御分支):无净效果,直接关单。
		if err := s.store.MarkTransferFailedWithReason(ctx, t.ID, repo.FailSweepEmpty); err != nil {
			return false
		}
		return true
	}
	// 普通模式实扣不足但未标 partial(标记写失败的兜底):按 partial 退回,绝不带部分金额入账。
	// sweep(离职退额)行例外:实扣<期望是合法 roll-forward,按实扣入账。
	if t.Reason != ReasonOffboardRefund && moved < t.AmountRaw {
		return s.refundDebited(ctx, t)
	}
	verdict := s.judgeUpstreamOp(ctx, t, t.ToUserID, repo.OpCredit, moved)
	switch verdict {
	case opLanded:
		// 入账已落地未记(J2 崩溃窗):只补状态,绝不二次入账。
		if err := s.store.MarkTransferCredited(ctx, t.ID); err != nil {
			s.log.Error("对账环:debited 补 J2 失败(下轮重试)", "ledger_id", t.ID, "err", err)
			return false
		}
		if err := s.store.MarkTransferApplied(ctx, t.ID); err != nil {
			s.log.Error("对账环:补 applied 失败(下轮重试)", "ledger_id", t.ID, "err", err)
			return false
		}
		s.log.Warn("对账环:debited 行日志证据判入账已落地,补状态完成(未二次入账)", "ledger_id", t.ID)
		return true
	case opNotLanded:
		// 入账未落地。先看 int32 可达性:不可达则钱永远进不去 → 退回出账方(方向安全,钱回金库)。
		toBal, gerr := s.upstream.GetUserQuota(ctx, int(t.ToUserID))
		if gerr != nil {
			return false
		}
		if toBal+moved > newapi.Int32QuotaMax {
			s.log.Warn("对账环:debited 行入账 int32 不可达,转退回出账方", "ledger_id", t.ID, "to_balance", toBal, "amount", moved)
			return s.refundDebited(ctx, t)
		}
		// 双保险第二道:成员侧账目封闭恒等式(静默三明治)确认"确实没收到"才补发。
		confirmed := s.identitySandwichConfirmsNotCredited(ctx, t, moved)
		if confirmed != opNotLanded {
			s.log.Error("对账环:debited 行日志判未入账但恒等式未能确认,停手待人工(绝不冒险补发)",
				"ledger_id", t.ID, "identity_verdict", int(confirmed))
			s.auditLedger(ctx, t.OrgID, "ledger_reconcile_ambiguous", &t.MemberID,
				map[string]any{"ledger_id": t.ID, "phase": "debited"}, "alert")
			return false
		}
		// 【唯一自动补发点】双证据一致:入账确实未发生 → 补齐(精确补齐,不多发不少发)。
		if aerr := s.upstream.IncreaseUserQuota(ctx, int(t.ToUserID), moved); aerr != nil {
			s.log.Error("对账环:补入账失败(下轮重试)", "ledger_id", t.ID, "err", aerr)
			return false
		}
		if err := s.store.MarkTransferCredited(ctx, t.ID); err != nil {
			s.log.Error("对账环:补入账成功但 J2 写失败(下轮按日志证据补状态)", "ledger_id", t.ID, "err", err)
			return false
		}
		if err := s.store.MarkTransferApplied(ctx, t.ID); err != nil {
			return false
		}
		s.log.Warn("对账环:debited 行已精确补齐入账(双证据确认缺口)", "ledger_id", t.ID, "amount", moved)
		s.auditLedger(ctx, t.OrgID, "ledger_reconcile_fix", &t.MemberID,
			map[string]any{"ledger_id": t.ID, "credited_raw": moved}, "ok")
		return true
	default:
		s.log.Error("对账环:debited 行入账证据不足/对不上,停手待人工", "ledger_id", t.ID)
		s.auditLedger(ctx, t.OrgID, "ledger_reconcile_ambiguous", &t.MemberID,
			map[string]any{"ledger_id": t.ID, "phase": "debited"}, "alert")
		return false
	}
}

// refundDebited 退回出账方(partial_debit / 入账不可达):两阶段(refunding→refunded)防重复退。
// refunding 中断重入时,先以日志证据判"退没退到"(出账方的增加日志),绝不盲目再退。
func (s *Service) refundDebited(ctx context.Context, t *repo.LedgerTransfer) bool {
	refund := t.DebitedRaw
	if refund <= 0 {
		if err := s.store.MarkTransferFailedWithReason(ctx, t.ID, repo.FailRefunded); err != nil {
			return false
		}
		return true
	}
	if t.FailReason == repo.FailRefunding {
		// 上次退回中断:判出账方是否已收到退款(增加日志)。
		switch s.judgeUpstreamOp(ctx, t, t.FromUserID, repo.OpCredit, refund) {
		case opLanded:
			if err := s.store.MarkTransferFailedWithReason(ctx, t.ID, repo.FailRefunded); err != nil {
				return false
			}
			s.log.Warn("对账环:退回此前已落地,补关单(未二次退款)", "ledger_id", t.ID)
			return true
		case opNotLanded:
			// 继续退回(下方公共路径)。
		default:
			s.log.Error("对账环:退回进度证据不足,停手待人工", "ledger_id", t.ID)
			return false
		}
	} else {
		// partial_debit/'' → refunding(单向标记;失败即让位下轮)。
		if ok, err := s.store.SetTransferFailReason(ctx, t.ID, t.FailReason, repo.FailRefunding); err != nil || !ok {
			return false
		}
	}
	if err := s.upstream.IncreaseUserQuota(ctx, int(t.FromUserID), refund); err != nil {
		s.log.Error("对账环:退回出账方失败(留 refunding 下轮续)", "ledger_id", t.ID, "err", err)
		return false
	}
	if err := s.store.MarkTransferFailedWithReason(ctx, t.ID, repo.FailRefunded); err != nil {
		s.log.Error("对账环:退回成功但关单失败(下轮按日志证据补)", "ledger_id", t.ID, "err", err)
		return false
	}
	s.log.Warn("对账环:已退回出账方并关单(净效果 0)", "ledger_id", t.ID, "refunded_raw", refund)
	s.auditLedger(ctx, t.OrgID, "ledger_reconcile_refund", &t.MemberID,
		map[string]any{"ledger_id": t.ID, "refunded_raw": refund}, "ok")
	return true
}

// identitySandwichConfirmsNotCredited 成员侧账目封闭恒等式 + 静默三明治:
// 成员 user.quota 变动来源封闭(只有平台划账[全在账本]与消费[全在 new-api 日志,/api/log/stat 权威]),
// 故 quota == Σ账本净入(不含本行) − Σ消费 ⇔ 本行入账未发生。判定期间余额必须静止(两读一致),否则 unknown。
func (s *Service) identitySandwichConfirmsNotCredited(ctx context.Context, t *repo.LedgerTransfer, moved int64) opVerdict {
	username, found, err := s.store.GetUsernameByNewapiUserID(ctx, t.ToUserID)
	if err != nil || !found {
		return opUnknown
	}
	q1, gerr := s.upstream.GetUserQuota(ctx, int(t.ToUserID))
	if gerr != nil {
		return opUnknown
	}
	consumed, cerr := s.upstream.SumConsumedQuotaByUsername(ctx, username, s.now().Unix()+60)
	if cerr != nil {
		return opUnknown
	}
	netIn, amb, nerr := s.store.SumJournaledNetByUser(ctx, t.ToUserID, t.ID)
	if nerr != nil || amb > 0 {
		return opUnknown
	}
	q2, gerr2 := s.upstream.GetUserQuota(ctx, int(t.ToUserID))
	if gerr2 != nil || q1 != q2 {
		return opUnknown // 判定期间在消费:下轮再判(消费终会静默)
	}
	expectedNot := netIn - consumed
	switch q2 {
	case expectedNot:
		return opNotLanded
	case expectedNot + moved:
		return opLanded
	default:
		return opUnknown
	}
}

// ─────────────────── 日志主证据(组长裁定 33-§12-8) ───────────────────

type opVerdict int

const (
	opUnknown   opVerdict = iota // 证据不可用/对不上:停手告警,绝不写
	opLanded                     // 该笔上游写已落地
	opNotLanded                  // 该笔上游写从未发生
)

// judgeUpstreamOp 以 new-api manage 日志为主证据,判"本行对 userID 的某方向钱面操作是否落地":
// 窗口内该 user 的 manage 增/减日志(金额可无损还原:USD ＄%.6f=raw 精确可逆 / Tokens %d 直读)
// 多重集扣除账本已确认的同方向操作 → 残差含本笔金额=落地;残差空=未落地;其余=unknown。
// 日志写在 new-api 侧 quota 写成功之后(RecordLogWithAdminInfo,同请求同步),故"日志在=写落地"成立;
// "日志缺=未落地"存在 new-api 崩溃窗残余,由调用方按方向配第二道保险(补发前恒等式共同确认)。
func (s *Service) judgeUpstreamOp(ctx context.Context, t *repo.LedgerTransfer, userID int64, kind int, amount int64) opVerdict {
	username, found, err := s.store.GetUsernameByNewapiUserID(ctx, userID)
	if err != nil || !found || username == "" {
		return opUnknown
	}
	since := t.CreatedAt.Add(-evidenceWindowSlack)
	observed, sawUnparseable, ferr := s.fetchManageOps(ctx, username, since.Unix(), kind, t)
	if ferr != nil {
		s.log.Warn("对账环:读 manage 日志失败(转兜底取证)", "ledger_id", t.ID, "err", ferr)
		return opUnknown
	}
	journaled, jerr := s.store.ListJournaledOpsSince(ctx, userID, since, t.ID, kind)
	if jerr != nil {
		return opUnknown
	}
	return judgeManageOps(observed, journaled, amount, sawUnparseable)
}

// fetchManageOps 拉取窗口内该 username 的 manage(type=3)额度日志并解析为金额多重集(kind 方向)。
// 覆盖(override)日志出现在平台管辖 user 上 = 违规(禁 override 红线),红色告警并判证据不完整。
func (s *Service) fetchManageOps(ctx context.Context, username string, sinceUnix int64, kind int, t *repo.LedgerTransfer) ([]int64, bool, error) {
	qpu, qerr := s.store.GetSettingInt64(ctx, "quota_per_unit", 500000)
	if qerr != nil {
		return nil, true, nil // 配置读挂:证据不可用(fail 向 unknown,绝不误判)
	}
	until := s.now().Unix() + 120
	var out []int64
	sawUnparseable := false
	for page := 1; page <= evidenceMaxPages; page++ {
		entries, total, err := s.upstream.ReadAllLogsByUsername(ctx, username, sinceUnix, until, page, 100)
		if err != nil {
			return nil, false, err
		}
		for _, e := range entries {
			if e.Type != 3 { // 3 = new-api LogTypeManage
				continue
			}
			amountRaw, opKind, class := parseManageLog(e.Content, qpu)
			switch class {
			case manageLogQuotaOp:
				if opKind == kind {
					out = append(out, amountRaw)
				}
			case manageLogOverride:
				s.log.Error("对账环:平台管辖 user 出现 override 覆盖额度日志(违规!禁 override 红线)",
					"username", username, "content", e.Content, "ledger_id", t.ID)
				s.auditLedger(ctx, t.OrgID, "ledger_override_detected", &t.MemberID,
					map[string]any{"username": username, "content": e.Content}, "alert")
				sawUnparseable = true
			case manageLogUnparseable:
				sawUnparseable = true
			}
		}
		if len(entries) == 0 || page*100 >= total {
			if total > evidenceMaxPages*100 {
				sawUnparseable = true // 窗口内日志异常多,证据不完整
			}
			break
		}
		if page == evidenceMaxPages {
			sawUnparseable = true // 翻页上限截断:证据不完整
		}
	}
	return out, sawUnparseable, nil
}

// manage 日志分类。
const (
	manageLogIrrelevant  = iota // 非额度操作(禁用/启用等):忽略
	manageLogQuotaOp            // 增/减额度,金额已无损还原
	manageLogOverride           // 覆盖额度(违规信号)
	manageLogUnparseable        // 额度操作但金额无法无损还原(CNY/自定义币显示制式)
)

// parseManageLog 解析 rc.4 manage 日志内容(controller/user.go:937/948 格式,对 rc.4 源码核过):
//
//	"管理员增加用户额度 ＄4.000000 额度" / "管理员减少用户额度 2000000 点额度" / "管理员覆盖用户额度从 X 为 Y"
//
// USD(logger.LogQuota 默认分支,全角＄+%.6f):raw/QPU 对 QPU≤1e6 的整数无损(raw×(1e6/QPU) 为整),
// round 还原后强校验;Tokens(%d 点额度):raw 直读。CNY/自定义币依赖汇率,不可无损还原 → unparseable。
func parseManageLog(content string, qpu int64) (amountRaw int64, kind int, class int) {
	const (
		addPrefix      = "管理员增加用户额度 "
		subPrefix      = "管理员减少用户额度 "
		overridePrefix = "管理员覆盖用户额度从 "
	)
	var rest string
	switch {
	case strings.HasPrefix(content, addPrefix):
		kind, rest = repo.OpCredit, strings.TrimPrefix(content, addPrefix)
	case strings.HasPrefix(content, subPrefix):
		kind, rest = repo.OpDebit, strings.TrimPrefix(content, subPrefix)
	case strings.HasPrefix(content, overridePrefix):
		return 0, 0, manageLogOverride
	default:
		return 0, 0, manageLogIrrelevant
	}
	if v, ok := strings.CutSuffix(rest, " 点额度"); ok { // Tokens 显示制式:raw 直读
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil || n < 0 {
			return 0, 0, manageLogUnparseable
		}
		return n, kind, manageLogQuotaOp
	}
	v, ok := strings.CutSuffix(rest, " 额度")
	if !ok {
		return 0, 0, manageLogUnparseable
	}
	if usd, ok2 := strings.CutPrefix(v, "＄"); ok2 { // USD 显示制式(全角＄,%.6f)
		f, err := strconv.ParseFloat(strings.TrimSpace(usd), 64)
		if err != nil || f < 0 {
			return 0, 0, manageLogUnparseable
		}
		raw := math.Round(f * float64(qpu))
		if math.Abs(f*float64(qpu)-raw) > 0.01 { // 非无损:拒判
			return 0, 0, manageLogUnparseable
		}
		return int64(raw), kind, manageLogQuotaOp
	}
	return 0, 0, manageLogUnparseable // ¥/自定义币:汇率不可逆,不可无损还原
}

// judgeManageOps 多重集判定(纯函数,单测覆盖):
// observed(日志出现的同方向操作) − journaled(账本已确认的同方向操作) = 残差。
// 残差含 amount → 落地;残差空 → 未落地;负残差(账本有而日志无=窗口裁边/清日志)或
// 有对不上的残差或证据不完整 → unknown(停手)。
func judgeManageOps(observed, journaled []int64, amount int64, sawUnparseable bool) opVerdict {
	if sawUnparseable {
		return opUnknown
	}
	res := map[int64]int{}
	for _, v := range observed {
		res[v]++
	}
	for _, v := range journaled {
		res[v]--
	}
	extras := 0
	for _, c := range res {
		if c < 0 {
			return opUnknown
		}
		extras += c
	}
	if res[amount] > 0 {
		return opLanded
	}
	if extras == 0 {
		return opNotLanded
	}
	return opUnknown
}

// ─────────────── 交叉恒等式全量扫描(读+告警,永不自动修) ───────────────

// scanMoneyIdentity 恒等式:成员 quota + Σ消费(权威 /api/log/stat) == Σ账本净入(31-ADR §4.1,
// 专抓违规直充/回绕)。正漂移 = 有人绕平台给成员加钱 → 立即告警;负漂移 = 日志清理/账外减额 →
// 连续两轮同额才告警(滤暂态)。金库侧:出现消费 = 违反"金库不建 token"铁律红色告警;低水位预警。
// 内部限频(identityScanInterval);只读+告警,永不自动修(直充是纪律违规不是机械故障,修=人工 runbook)。
// 前提(上线检查单):new-api 实例 QuotaForNewUser(新用户初始额度)必须为 0,否则全员恒等偏移该值
// (症状=所有成员同额正漂移,告警文案可据此定位)。
func (s *Service) scanMoneyIdentity(ctx context.Context) []Drift {
	now := s.now()
	s.ledger.mu.Lock()
	due := s.ledger.lastIdentity.IsZero() || now.Sub(s.ledger.lastIdentity) >= identityScanInterval
	if due {
		s.ledger.lastIdentity = now
	}
	s.ledger.mu.Unlock()
	if !due {
		return nil
	}
	var drifts []Drift
	after := int64(0)
	for {
		refs, err := s.store.ListMembersWithNewapiUser(ctx, after, 500)
		if err != nil {
			s.log.Error("恒等式扫描:列成员失败(本轮中止)", "err", err)
			return drifts
		}
		if len(refs) == 0 {
			break
		}
		for _, r := range refs {
			after = r.MemberID
			if d := s.checkMemberIdentity(ctx, r); d != nil {
				drifts = append(drifts, *d)
			}
		}
	}
	drifts = append(drifts, s.checkTreasuries(ctx)...)
	return drifts
}

func (s *Service) checkMemberIdentity(ctx context.Context, r repo.MemberUserRef) *Drift {
	if r.NewapiUsername == "" {
		return nil // 旧 A 版残留/未完整开通:无法查权威消费,跳过
	}
	q1, err := s.upstream.GetUserQuota(ctx, int(r.NewapiUserID))
	if err != nil {
		return nil
	}
	consumed, cerr := s.upstream.SumConsumedQuotaByUsername(ctx, r.NewapiUsername, s.now().Unix()+60)
	if cerr != nil {
		return nil
	}
	netIn, amb, nerr := s.store.SumJournaledNetByUser(ctx, r.NewapiUserID, 0)
	if nerr != nil || amb > 0 {
		return nil // 有进度不确定行:本轮不判
	}
	q2, gerr := s.upstream.GetUserQuota(ctx, int(r.NewapiUserID))
	if gerr != nil || q1 != q2 {
		return nil // 正在消费:静默三明治不成立,下轮再判
	}
	drift := q2 + consumed - netIn
	s.ledger.mu.Lock()
	prev, hadPrev := s.ledger.negDrift[r.NewapiUserID]
	if drift < 0 {
		if s.ledger.negDrift == nil {
			s.ledger.negDrift = map[int64]int64{}
		}
		s.ledger.negDrift[r.NewapiUserID] = drift
	} else {
		delete(s.ledger.negDrift, r.NewapiUserID)
	}
	s.ledger.mu.Unlock()
	if drift == 0 {
		return nil
	}
	ledgerConsumed, _ := s.store.SumConsumedByNewapiUser(ctx, r.NewapiUserID) // 镜像口径诊断值
	detail := fmt.Sprintf("成员恒等式漂移: member=%d uid=%d drift=%+d (quota=%d consumed=%d net_in=%d 镜像consumed=%d)",
		r.MemberID, r.NewapiUserID, drift, q2, consumed, netIn, ledgerConsumed)
	if drift > 0 { // 违规直充/双发方向:立即告警
		s.log.Error("恒等式扫描:成员正漂移(疑违规直充/双发)!若全员同额漂移=QuotaForNewUser 未清零", "member_id", r.MemberID, "drift", drift)
		s.auditLedger(ctx, r.OrgID, "ledger_identity_mismatch", &r.MemberID, map[string]any{"detail": detail}, "alert")
		return &Drift{Kind: "identity_mismatch", Detail: detail}
	}
	if hadPrev && prev == drift { // 连续两轮同额负漂移:真漂移(日志被清/账外减额)
		s.log.Error("恒等式扫描:成员负漂移持续(日志清理或账外减额)", "member_id", r.MemberID, "drift", drift)
		s.auditLedger(ctx, r.OrgID, "ledger_identity_mismatch", &r.MemberID, map[string]any{"detail": detail}, "alert")
		return &Drift{Kind: "identity_mismatch", Detail: detail}
	}
	return nil
}

func (s *Service) checkTreasuries(ctx context.Context) []Drift {
	lowWM, err := s.store.GetSettingInt64(ctx, "treasury_low_watermark_raw", 50_000_000)
	if err != nil {
		lowWM = 50_000_000
	}
	var drifts []Drift
	after := int64(0)
	for {
		orgs, lerr := s.store.ListTreasuryOrgs(ctx, after, 500)
		if lerr != nil || len(orgs) == 0 {
			break
		}
		for _, o := range orgs {
			after = o.OrgID
			if o.TreasuryUsername != "" {
				if consumed, cerr := s.upstream.SumConsumedQuotaByUsername(ctx, o.TreasuryUsername, s.now().Unix()); cerr == nil && consumed > 0 {
					detail := fmt.Sprintf("金库出现消费日志(违反金库不建 token 铁律): org=%d consumed=%d", o.OrgID, consumed)
					s.log.Error("恒等式扫描:金库被消费!", "org_id", o.OrgID, "consumed", consumed)
					s.auditLedger(ctx, o.OrgID, "treasury_consumption_detected", nil, map[string]any{"detail": detail}, "alert")
					drifts = append(drifts, Drift{Kind: "treasury_alert", Detail: detail})
				}
			}
			if bal, gerr := s.upstream.GetUserQuota(ctx, int(o.TreasuryUID)); gerr == nil && bal < lowWM {
				s.log.Warn("金库低水位预警(请运营方增量代充,单次≤$4294)", "org_id", o.OrgID, "balance", bal, "watermark", lowWM)
				drifts = append(drifts, Drift{Kind: "treasury_alert",
					Detail: fmt.Sprintf("金库低水位: org=%d balance=%d watermark=%d", o.OrgID, bal, lowWM)})
			}
		}
	}
	return drifts
}

// ───────────────────────────── 订阅补满 worker ─────────────────────────────

// RunSubscriptionTopup 订阅档位周期补满 worker(33 §3.1)。
// 🔒 leader-gated(第 4 个写钱入口):多节点前必过选主(单节点灰度不触发)。
// 双补防线:leader 闸(一)+ 账本幂等键唯一约束(二,结构性——未选主双跑也只入账一次)。
// 跳过 disabled/offboarded/expired/quarantined/软删/未开通/停用档位/硬停组织(候选 SQL 谓词即护栏);
// 幂等键 = subtopup:{memberID}:{周期桶},桶 = 组织时区自然边界标签(格式冻结,裁定 33-§12-11);
// D = 目标 − 剩余,D≤0 跳过(当期补满语义,不累积不清零);fail-open:单成员失败只告警不中断不停服。
func (s *Service) RunSubscriptionTopup(ctx context.Context) error {
	if ok, _, err := s.leadership.CanRunTick(ctx); err != nil {
		return err
	} else if !ok {
		return nil
	}
	if s.moneyFrozen(ctx) {
		s.log.Warn("订阅补满 worker:money_freeze 生效,本轮跳过")
		return nil
	}
	capRaw, err := s.store.GetSettingInt64(ctx, "member_quota_cap_raw", 500_000_000)
	if err != nil {
		s.log.Error("读成员额度帽失败(用默认值,fail-open)", "err", err)
	}
	locs := map[string]*time.Location{}
	insufficientAlerted := map[int64]bool{} // 金库不足告警:每 org 每轮一次
	after := int64(0)
	for {
		cands, lerr := s.store.ListSubscriptionTopupCandidates(ctx, after, 200)
		if lerr != nil {
			return apperr.Internal("").WithCause(lerr)
		}
		if len(cands) == 0 {
			return nil
		}
		for _, c := range cands {
			after = c.MemberID
			s.topupOne(ctx, c, capRaw, locs, insufficientAlerted)
		}
	}
}

// topupOne 单成员补满(错误逐成员隔离,fail-open 绝不停服)。
func (s *Service) topupOne(ctx context.Context, c repo.TopupCandidate, capRaw int64, locs map[string]*time.Location, insufficientAlerted map[int64]bool) {
	loc, ok := locs[c.OrgTimezone]
	if !ok {
		if l, lerr := time.LoadLocation(c.OrgTimezone); lerr == nil {
			loc = l
		} else {
			loc = time.UTC
			s.log.Warn("订阅补满:组织时区无效,按 UTC 边界", "org_id", c.OrgID, "tz", c.OrgTimezone)
		}
		locs[c.OrgTimezone] = loc
	}
	bucket, bok := subscriptionBucket(c.ResetPeriod, s.now(), loc)
	if !bok {
		s.log.Warn("订阅补满:非法周期,跳过", "member_id", c.MemberID, "period", c.ResetPeriod)
		return
	}
	// 进程内去重:本桶已处理(补过/判满/关单)不再评估——避免每 tick 对全员打 GetUserQuota。
	// 重启后内存丢失重评估一次:双发由账本幂等键兜底;"已判满后重启 + 期内又消费"会触发一次
	// 期中补满(方向=成员多得,上限=目标额;已知边角,记档接受)。
	if s.ledger.seenBucket(c.MemberID) == bucket {
		return
	}
	// 竞态再核(离职撞车窗口收窄,31-ADR §4.3):Transfer 前重读成员状态;非 active/done 不 mark
	// (状态若恢复,本桶还能补)。
	m, merr := s.store.GetMember(ctx, c.OrgID, c.MemberID)
	if merr != nil || m.Status != model.MemberStatusActive || m.BootstrapState != model.BootstrapDone || m.NewapiUserID == nil {
		return
	}
	target := c.TargetRaw
	if target > capRaw {
		target = capRaw // 帽兜底(主校验在档位保存;超管调帽后自动生效)
	}
	cur, gerr := s.upstream.GetUserQuota(ctx, c.MemberUID)
	if gerr != nil {
		s.log.Error("订阅补满:读成员余额失败(下 tick 重试)", "member_id", c.MemberID, "err", gerr)
		return
	}
	d := target - cur
	if d <= 0 {
		s.ledger.markBucket(c.MemberID, bucket) // 已满:本桶完结
		return
	}
	idem := fmt.Sprintf("subtopup:%d:%s", c.MemberID, bucket)
	err := s.Transfer(ctx, c.OrgID, c.TreasuryUID, c.MemberUID, c.MemberID, d, ReasonSubscription, idem, "system:subscription-worker")
	switch {
	case err == nil:
		s.ledger.markBucket(c.MemberID, bucket)
		s.log.Info("订阅补满:已补至目标", "member_id", c.MemberID, "bucket", bucket, "topup_raw", d, "target_raw", target)
	case errors.Is(err, errCauseInsufficient):
		// 金库不足:不 mark(代充到位后同桶自动补上,幂等键天然支持迟到补满);每 org 每轮告警一次。
		if !insufficientAlerted[c.OrgID] {
			insufficientAlerted[c.OrgID] = true
			s.log.Error("订阅补满:金库余额不足(请运营方增量代充;到账后本周期自动补上)", "org_id", c.OrgID, "need_raw", d)
			s.auditLedger(ctx, c.OrgID, "subscription_treasury_insufficient", &c.MemberID,
				map[string]any{"bucket": bucket, "need_raw": d}, "alert")
		}
	case errors.Is(err, errCauseReplayPending):
		s.ledger.markBucket(c.MemberID, bucket) // 本桶已有行在收敛(对账环),不重发
		s.log.Warn("订阅补满:本桶划账收敛中(对账环),跳过", "member_id", c.MemberID, "bucket", bucket)
	case errors.Is(err, errCauseReplayFailed):
		s.ledger.markBucket(c.MemberID, bucket) // 本桶键已判死:本周期放弃(告警),下周期新桶
		s.log.Error("订阅补满:本桶划账此前判死,本周期放弃(可人工 quota:grant 补偿)", "member_id", c.MemberID, "bucket", bucket)
	default:
		s.log.Error("订阅补满:划账失败(下 tick 重试)", "member_id", c.MemberID, "err", err)
	}
}

// subscriptionBucket 周期桶标签(组织时区自然边界;标签法天然规避 DST/偏移坑,入账本即永久审计锚)。
// daily=2006-01-02 / weekly=ISO 周(周一 00:00 边界)/ monthly=2006-01。格式冻结(裁定 33-§12-11)。
func subscriptionBucket(period string, now time.Time, loc *time.Location) (string, bool) {
	lt := now.In(loc)
	switch period {
	case "daily":
		return lt.Format("2006-01-02"), true
	case "weekly":
		y, w := lt.ISOWeek()
		return fmt.Sprintf("%d-W%02d", y, w), true
	case "monthly":
		return lt.Format("2006-01"), true
	}
	return "", false
}

// ───────────────────────────── 杂项 ─────────────────────────────

// VerifyQuotaPerUnit 启动自检(33 §3.6/34 §3-④):平台配置的 quota_per_unit 必须与所连 new-api
// 实例实际 QuotaPerUnit 一致——不一致拒启动(fail-fast),防两边漂移金额错 50 万倍。
// dev 可 env NEXUS_SKIP_QPU_CHECK=true 跳过(生产禁设);调用方在 cmd/server 启动链装配(阶段2 硬项)。
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

// auditLedger 写一条钱核心系统审计(actor=system:ledger;运维盯 ledger_* / *_alert 事件)。
func (s *Service) auditLedger(ctx context.Context, orgID int64, action string, targetID *int64, detail map[string]any, result string) {
	tt := "ledger"
	e := &model.AuditEntry{OrgID: orgID, Actor: "system:ledger", Action: action, TargetType: &tt, TargetID: targetID, Result: result}
	if detail != nil {
		if b, err := json.Marshal(detail); err == nil {
			e.Detail = b
		}
	}
	if err := s.store.WriteAudit(ctx, e); err != nil {
		s.log.Error("钱核心写审计失败", "action", action, "err", err)
	}
}

// randomHex 生成 n 字节随机 hex(服务端幂等键缺省)。
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b)
}
