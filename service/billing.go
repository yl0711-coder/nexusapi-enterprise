package service

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"

	"github.com/nexusapi-platform/enterprise/adapter/newapi"
	"github.com/nexusapi-platform/enterprise/model"
	"github.com/nexusapi-platform/enterprise/pkg/apperr"
	"github.com/nexusapi-platform/enterprise/pkg/session"
	"github.com/nexusapi-platform/enterprise/repo"
)

// RechargeInput 入账入参(US-08)。transfer_no 为入账幂等键(对公转账唯一号)。
type RechargeInput struct {
	AmountQuota int64
	TransferNo  string
	AmountCNY   *int64
	Note        string
}

// Recharge 运营方人工入账(US-08,E02,动钱红线:仅运营方)。
// transfer_no 幂等 + 乐观锁;入账后若组织原 low/stopped 且余额回正 → 恢复 active。写审计。
func (s *Service) Recharge(ctx context.Context, c session.Claims, orgID int64, in RechargeInput) (*model.Balance, error) {
	// 动钱红线:仅运营方可入账(E02);组织管理员/团队负责人/成员一律 403。
	if err := assertRole(c, session.RoleOperator); err != nil {
		return nil, err
	}
	if in.AmountQuota <= 0 {
		return nil, apperr.InvalidParam("入账金额须为正")
	}
	if in.TransferNo == "" {
		return nil, apperr.InvalidParam("缺少转账唯一号(入账幂等键)")
	}
	if err := firstErr(checkLen("转账唯一号", in.TransferNo, maxTransferNoLen), checkText("备注", in.Note, maxNoteLen)); err != nil {
		return nil, err // T3:超长返 422,不落库不 500
	}
	if _, err := s.store.GetOrganization(ctx, orgID); errors.Is(err, repo.ErrNotFound) {
		return nil, apperr.NotFound("组织不存在")
	} else if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}

	var note *string
	if in.Note != "" {
		note = &in.Note
	}
	// 模型2:入账=持 per-org 锁的原子操作{记账+影子+escrow 分桶}+ 提交后 add 进 newapi(applyRecharge)。
	// 取代旧"AddRecharge 后再 allocateRecharge"两步非原子(R5 F5 修复)。
	bal, err := s.applyRecharge(ctx, orgID, &model.Recharge{
		OrgID: orgID, Amount: in.AmountQuota, AmountCNY: in.AmountCNY,
		TransferNo: in.TransferNo, Operator: actorOf(c), Note: note,
	})
	if errors.Is(err, repo.ErrConflict) {
		return nil, apperr.Conflict("该转账唯一号已入账,勿重复提交")
	}
	if errors.Is(err, repo.ErrOptimisticLock) {
		return nil, apperr.OptimisticLock("余额并发更新冲突,请重试")
	}
	if err != nil {
		return nil, err // applyRecharge 已包 apperr.Internal
	}
	if err := s.recomputeOrgStatus(ctx, orgID, bal); err != nil {
		s.log.Error("入账后重算组织状态失败", "org_id", orgID, "err", err)
	}
	s.audit(ctx, c, orgID, "recharge", "balance", &orgID, map[string]any{
		"amount": in.AmountQuota, "transfer_no": in.TransferNo, "balance_after": bal.Balance,
	})
	return bal, nil
}

// DebitInput 减余额冲正入参(US-12 执行半段)。
type DebitInput struct {
	AmountQuota int64
	Reason      string
}

// DebitBalance 运营方执行退款冲正(US-12 / D1:平台是数字台账,线下退款后在平台录入减余额)。
// 模型2(R5 F3 修复):退款必须**真减花钱能力**——持 per-org 锁,单事务{影子余额−refund流水 + escrow 减额
// (先减托管后减桶1,ReduceEscrowTx)}+ 桶1 减的部分对应 newapi 窗口 subtract。绝不只减死账(那样退款后客户照花=双付)。
// 金额≤可用(影子 balance==escrow available,二者守恒一致);乐观锁;newapi subtract 失败由 reconcile 自愈。
func (s *Service) DebitBalance(ctx context.Context, c session.Claims, orgID int64, in DebitInput) (*model.Balance, error) {
	if err := assertRole(c, session.RoleOperator); err != nil {
		return nil, err
	}
	if in.AmountQuota <= 0 {
		return nil, apperr.InvalidParam("冲正金额须为正")
	}
	if in.Reason == "" {
		return nil, apperr.InvalidParam("冲正须填原因(留痕)")
	}
	if err := checkText("原因", in.Reason, maxNoteLen); err != nil { // LOW-2 二道闸
		return nil, err
	}
	uid, _, ok, err := s.store.GetOrgNewapiCred(ctx, orgID)
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	if !ok {
		return nil, apperr.InvalidParam("组织未开通池子,无可冲正余额")
	}
	release, lerr := s.quotaLocker.Acquire(ctx, escrowLockKey(orgID))
	if lerr != nil {
		return nil, apperr.Internal("").WithCause(lerr)
	}
	defer release()

	var bal *model.Balance
	var windowDec int64
	if err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
		b, derr := s.store.DebitBalanceTx(ctx, tx, orgID, in.AmountQuota, in.Reason, actorOf(c)) // 影子+refund流水+amount≤balance
		if derr != nil {
			return derr
		}
		bal = b
		wd, eerr := s.store.ReduceEscrowTx(ctx, tx, orgID, in.AmountQuota) // 先减托管后减桶1
		if eerr != nil {
			return eerr
		}
		windowDec = wd
		return nil
	}); err != nil {
		if errors.Is(err, repo.ErrInsufficientBalance) {
			return nil, apperr.InvalidParam("冲正金额超过当前可用余额")
		}
		if errors.Is(err, repo.ErrNotFound) {
			return nil, apperr.NotFound("组织无余额记录")
		}
		if errors.Is(err, repo.ErrOptimisticLock) {
			return nil, apperr.OptimisticLock("余额并发冲突,请重试")
		}
		return nil, apperr.Internal("").WithCause(err)
	}
	// 桶1 减的部分对应 newapi 窗口 subtract(托管减不动 newapi)。失败→reconcile 自愈窗口。
	if windowDec > 0 {
		if err := s.upstream.ManageUserQuota(ctx, int(uid), newapi.QuotaSubtract, windowDec); err != nil {
			s.log.Error("退款 subtract newapi 窗口失败(DB 已原子提交,reconcile 将自愈)", "org_id", orgID, "dec", windowDec, "err", err)
		}
	}
	if err := s.recomputeOrgStatus(ctx, orgID, bal); err != nil {
		s.log.Error("冲正后重算组织状态失败", "org_id", orgID, "err", err)
	}
	s.audit(ctx, c, orgID, "refund_debit", "balance", &orgID, map[string]any{"amount": in.AmountQuota, "reason": in.Reason, "balance_after": bal.Balance})
	return bal, nil
}

// GetBalance 查公司余额(O/A)。
func (s *Service) GetBalance(ctx context.Context, c session.Claims, orgID int64) (*model.Balance, error) {
	if err := assertOrgScope(c, orgID); err != nil {
		return nil, err
	}
	if err := assertRole(c, session.RoleOperator, session.RoleOrgAdmin); err != nil {
		return nil, err
	}
	// R5后裁定:客户可看自己"可用余额"(诚实余额,balance=充值−消费−退款),观测期不藏(藏的是价:倍率/折扣/计费设置)。
	b, err := s.store.GetOrCreateBalance(ctx, orgID)
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	return b, nil
}

// ListRecharges 列入账记录(O/A)。
func (s *Service) ListRecharges(ctx context.Context, c session.Claims, orgID int64, limit, offset int) ([]*model.Recharge, int, error) {
	if err := assertOrgScope(c, orgID); err != nil {
		return nil, 0, err
	}
	if err := assertRole(c, session.RoleOperator, session.RoleOrgAdmin); err != nil {
		return nil, 0, err
	}
	// R5后裁定:客户可看自己的入账记录(其钱),观测期不藏。
	recs, total, err := s.store.ListRecharges(ctx, orgID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	// T14:把 operator:<id> 解析成可读姓名(与审批"显申请人姓名"同口径),小缓存避免重复查。
	nameCache := map[int64]string{}
	for _, r := range recs {
		if id, ok := parseActorID(r.Operator); ok {
			n, cached := nameCache[id]
			if !cached {
				n, _ = s.store.GetMemberNameByID(ctx, id)
				nameCache[id] = n
			}
			r.OperatorName = n
		}
	}
	return recs, total, nil
}

// parseActorID 从 "operator:7" / "member:7" 等 actor 串解析出成员 id。
func parseActorID(actor string) (int64, bool) {
	i := strings.LastIndexByte(actor, ':')
	if i < 0 || i == len(actor)-1 {
		return 0, false
	}
	id, err := strconv.ParseInt(actor[i+1:], 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

// RequestRechargeInput 申请充值/退款入参(US-09/US-12)。
type RequestRechargeInput struct {
	Type   string // topup / refund
	Amount int64
	Note   string
}

// RequestRecharge 组织管理员发起申请充值(US-09)或退款申请(US-12),**只记录、绝不改余额**。
// 计费子集:仅组织管理员。退款金额不得超过当前余额。
func (s *Service) RequestRecharge(ctx context.Context, c session.Claims, orgID int64, in RequestRechargeInput) (*model.RechargeRequest, error) {
	if err := assertOrgScope(c, orgID); err != nil {
		return nil, err
	}
	if err := assertRole(c, session.RoleOrgAdmin); err != nil {
		return nil, err
	}
	if in.Type != model.RechargeReqTopup && in.Type != model.RechargeReqRefund {
		return nil, apperr.InvalidParam("申请类型须为 topup 或 refund")
	}
	if in.Amount <= 0 {
		return nil, apperr.InvalidParam("申请金额须为正")
	}
	if in.Type == model.RechargeReqRefund {
		b, err := s.store.GetOrCreateBalance(ctx, orgID)
		if err != nil {
			return nil, apperr.Internal("").WithCause(err)
		}
		if in.Amount > b.Balance {
			return nil, apperr.InvalidParam("退款金额不得超过当前余额")
		}
	}
	if err := checkText("备注", in.Note, maxNoteLen); err != nil { // LOW-2 二道闸
		return nil, err
	}
	var note *string
	if in.Note != "" {
		note = &in.Note
	}
	id, err := s.store.CreateRechargeRequest(ctx, &model.RechargeRequest{
		OrgID: orgID, RequestType: in.Type, Amount: in.Amount, Note: note, Applicant: actorOf(c),
	})
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	action := "request_topup"
	if in.Type == model.RechargeReqRefund {
		action = "request_refund"
	}
	s.audit(ctx, c, orgID, action, "balance", &orgID, map[string]any{"amount": in.Amount})
	// 返回刚建的申请。
	reqs, _, err := s.store.ListRechargeRequests(ctx, orgID, 1, 0)
	if err != nil || len(reqs) == 0 {
		return &model.RechargeRequest{ID: id, OrgID: orgID, RequestType: in.Type, Amount: in.Amount, Status: model.RechargeReqPending}, nil
	}
	return reqs[0], nil
}

// ListRechargeRequests 列申请(O/A)。
func (s *Service) ListRechargeRequests(ctx context.Context, c session.Claims, orgID int64, limit, offset int) ([]*model.RechargeRequest, int, error) {
	if err := assertOrgScope(c, orgID); err != nil {
		return nil, 0, err
	}
	if err := assertRole(c, session.RoleOperator, session.RoleOrgAdmin); err != nil {
		return nil, 0, err
	}
	// R5后裁定:客户可看自己发起的充值申请(其钱),观测期不藏。
	return s.store.ListRechargeRequests(ctx, orgID, limit, offset)
}

// BillingSettingsInput 计费灰度开关入参(nil=不改)。
type BillingSettingsInput struct {
	BillingEnabled  *bool
	HardStopEnabled *bool
	LowWatermark    *int64
}

// BillingSettings 当前计费开关 + 低位阈值。
type BillingSettings struct {
	BillingEnabled  bool
	HardStopEnabled bool
	LowWatermark    int64
}

// SetBillingSettings 设组织计费灰度开关 + 低位阈值(仅运营方,逐组织灰度;扣费/硬停默认关)。
func (s *Service) SetBillingSettings(ctx context.Context, c session.Claims, orgID int64, in BillingSettingsInput) (*BillingSettings, error) {
	if err := assertRole(c, session.RoleOperator); err != nil {
		return nil, err
	}
	if _, err := s.store.GetOrganization(ctx, orgID); errors.Is(err, repo.ErrNotFound) {
		return nil, apperr.NotFound("组织不存在")
	} else if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	if err := s.store.SetOrgBillingFlags(ctx, orgID, in.BillingEnabled, in.HardStopEnabled); err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	if in.LowWatermark != nil {
		if *in.LowWatermark < 0 { // R2-轻微:低位阈值不得为负(负值无意义且会让状态判定失灵)
			return nil, apperr.InvalidParam("低位告警阈值不得为负")
		}
		if err := s.store.SetLowWatermark(ctx, orgID, *in.LowWatermark); err != nil {
			return nil, apperr.Internal("").WithCause(err)
		}
	}
	s.audit(ctx, c, orgID, "set_billing_settings", "organization", &orgID, map[string]any{
		"billing_enabled": in.BillingEnabled, "hard_stop_enabled": in.HardStopEnabled, "low_watermark": in.LowWatermark,
	})
	return s.GetBillingSettings(ctx, c, orgID)
}

// GetBillingSettings 读计费开关 + 低位阈值(O/A)。
func (s *Service) GetBillingSettings(ctx context.Context, c session.Claims, orgID int64) (*BillingSettings, error) {
	if err := assertOrgScope(c, orgID); err != nil {
		return nil, err
	}
	if err := assertRole(c, session.RoleOperator, session.RoleOrgAdmin); err != nil {
		return nil, err
	}
	if err := s.mvpHidePrice(c); err != nil { // MVP(观测)藏价:客户直连不可读计费开关/阈值;运营方/支持态正常
		return nil, err
	}
	f, err := s.store.GetOrgBillingFlags(ctx, orgID)
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	b, err := s.store.GetOrCreateBalance(ctx, orgID)
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	return &BillingSettings{BillingEnabled: f.BillingEnabled, HardStopEnabled: f.HardStopEnabled, LowWatermark: b.LowWatermark}, nil
}

// recomputeOrgStatus 按余额水位重算组织服务状态并(若变化)落库 + 审计(US-11)。
//
// 余额 <= 0 → stopped;0 < 余额 <= low_watermark(且阈值>0)→ low;否则 active。
// **硬停(stopped 时把成员 quota override 为 0)是逐组织开关、默认关(用户拍板 2026-06-17),
// 3a 不执行**——这里只翻组织状态旗标 + 告警留痕,绝不切断客户服务;真正硬停在 3b 逐组织灰度。
func (s *Service) recomputeOrgStatus(ctx context.Context, orgID int64, b *model.Balance) error {
	target := model.OrgStatusActive
	switch {
	case b.Balance <= 0:
		target = model.OrgStatusStopped
	case b.LowWatermark > 0 && b.Balance <= b.LowWatermark:
		target = model.OrgStatusLow
	}
	org, err := s.store.GetOrganization(ctx, orgID)
	if err != nil {
		return err
	}
	if org.Status == target {
		return nil
	}
	if err := s.store.UpdateOrgStatus(ctx, orgID, target); err != nil {
		return err
	}

	// GZ-04 方案②:settlement 不再直接下发 override(消除双写者),这里只翻组织状态旗标(上面已落库)。
	// 硬停仍是逐组织开关、默认关(用户拍板 2026-06-17)。开了 hard_stop 且状态涉及 stopped 进/出时,
	// 触发一次"即时收敛"——逐成员经唯一下发出口 applyMemberOverride 按新状态重算下发(应硬停→0,恢复→正常额),
	// 硬停/恢复当拍生效;其余状态变化(active↔low)不改下发值,无需收敛。
	flags, ferr := s.store.GetOrgBillingFlags(ctx, orgID)
	hardStop := ferr == nil && flags.HardStopEnabled
	if hardStop && (target == model.OrgStatusStopped || org.Status == model.OrgStatusStopped) {
		s.convergeOrgQuotas(ctx, orgID)
	}
	hsState := "disabled"
	if hardStop {
		hsState = "enabled"
	}
	s.auditSystem(ctx, orgID, "org_status_change", "organization", &orgID, map[string]any{
		"from": org.Status, "to": target, "balance": b.Balance, "hard_stop": hsState,
	}, "ok")
	s.log.Info("组织状态随余额变化", "org_id", orgID, "from", org.Status, "to", target, "balance", b.Balance, "hard_stop", hsState)
	return nil
}
