package service

import (
	"context"
	"errors"

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
	if _, err := s.store.GetOrganization(ctx, orgID); errors.Is(err, repo.ErrNotFound) {
		return nil, apperr.NotFound("组织不存在")
	} else if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}

	var note *string
	if in.Note != "" {
		note = &in.Note
	}
	bal, err := s.store.AddRecharge(ctx, &model.Recharge{
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
		return nil, apperr.Internal("").WithCause(err)
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
// 动钱红线:仅运营方;金额≤余额;乐观锁;重算组织状态;强制留痕(actor+原因)。
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
	bal, err := s.store.DebitBalance(ctx, orgID, in.AmountQuota, in.Reason, actorOf(c))
	if errors.Is(err, repo.ErrInsufficientBalance) {
		return nil, apperr.InvalidParam("冲正金额超过当前余额")
	}
	if errors.Is(err, repo.ErrNotFound) {
		return nil, apperr.NotFound("组织无余额记录")
	}
	if errors.Is(err, repo.ErrOptimisticLock) {
		return nil, apperr.OptimisticLock("余额并发冲突,请重试")
	}
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
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
	return s.store.ListRecharges(ctx, orgID, limit, offset)
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

	// 硬停:逐组织开关,默认关(用户拍板 2026-06-17)。开了才在 stopped 进/出时 override 成员 quota。
	flags, ferr := s.store.GetOrgBillingFlags(ctx, orgID)
	hardStop := ferr == nil && flags.HardStopEnabled
	if hardStop {
		switch {
		case target == model.OrgStatusStopped:
			if err := s.hardStopOrg(ctx, orgID); err != nil {
				s.log.Error("硬停失败", "org_id", orgID, "err", err)
			}
		case org.Status == model.OrgStatusStopped: // 从 stopped 恢复
			if err := s.restoreOrgQuotas(ctx, orgID); err != nil {
				s.log.Error("解硬停恢复 quota 失败", "org_id", orgID, "err", err)
			}
		}
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
