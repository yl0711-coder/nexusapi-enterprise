package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nexusapi-platform/enterprise/model"
	"github.com/nexusapi-platform/enterprise/pkg/apperr"
	"github.com/nexusapi-platform/enterprise/pkg/session"
	"github.com/nexusapi-platform/enterprise/repo"
)

// 审批三档阈值(08 §3.1;原型默认值,可配)。本期硬编默认,后续接 approval-rules 端点。
const (
	approvalAutoMaxQuota int64 = 50_000_000  // ① 自动通过额度上限
	approvalAutoMaxDays        = 1           // ① 自动通过时长上限(天)
	approvalL1MaxQuota   int64 = 150_000_000 // ② 一审上限(超此或开新模型 → 二审)
)

// SubmitApprovalInput 提交申请入参(US-06;成员发起)。
type SubmitApprovalInput struct {
	Model    string // 申请的模型(开新模型→二审);留空=纯增额
	Amount   int64  // 申请额度(quota)
	Duration string // today/3d/week
	Reason   string
}

// SubmitApproval 成员提交增额/开模型申请,后端算档(08 §3.1):
// ① ≤autoMax 且 ≤1天 且非新模型 → 自动通过 + 即时下发;② ≤l1Max → 一审(团队负责人);
// ③ >l1Max 或 开新模型 → 二审(团队负责人→组织管理员)。任何登录用户可为本人提交。
func (s *Service) SubmitApproval(ctx context.Context, c session.Claims, in SubmitApprovalInput) (*model.Approval, error) {
	if in.Amount <= 0 {
		return nil, apperr.InvalidParam("申请额度须为正")
	}
	applicant, err := s.store.GetMember(ctx, c.OrgID, c.MemberID)
	if errors.Is(err, repo.ErrNotFound) {
		return nil, apperr.Unauthenticated("账号不存在")
	}
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}

	// 判定是否开新模型(模型不在当前层级模型集 → 新模型)。
	newModel := false
	reqType := model.ReqQuotaRaise
	if in.Model != "" {
		newModel = !s.memberHasModel(ctx, applicant, in.Model)
		if newModel {
			reqType = model.ReqModelOpen
		}
	}
	days := durationDays(in.Duration)

	// 可配审批阈值(E13);取库,失败则用默认常量兜底。
	autoMax, autoDays, l1Max := approvalAutoMaxQuota, approvalAutoMaxDays, approvalL1MaxQuota
	if r, rerr := s.store.GetApprovalRules(ctx, c.OrgID); rerr == nil {
		autoMax, autoDays, l1Max = r.AutoMaxQuota, r.AutoMaxDays, r.L1MaxQuota
	}

	a := &model.Approval{
		OrgID: c.OrgID, ApplicantID: c.MemberID, TeamID: applicant.TeamID, RequestType: reqType,
		Payload: model.ApprovalPayload{Model: in.Model, Amount: in.Amount, Duration: in.Duration, Reason: in.Reason},
	}

	switch {
	case !newModel && in.Amount <= autoMax && days <= autoDays:
		a.State = model.ApprovalAutoApprove
	case !newModel && in.Amount <= l1Max:
		a.State = model.ApprovalPending
		a.IsLevel2 = false
	default: // >l1Max 或 开新模型
		a.State = model.ApprovalPending
		a.IsLevel2 = true
	}

	id, err := s.store.CreateApproval(ctx, a)
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	a.ID = id

	if a.State == model.ApprovalAutoApprove {
		if err := s.dispatchApproval(ctx, a, applicant); err != nil {
			s.log.Error("自动通过下发失败(已记 auto_approved,进重试/告警)", "approval_id", id, "err", err)
		}
		s.notify(ctx, c.OrgID, c.MemberID, "approval_result", "申请已自动通过", fmt.Sprintf("增额 %d 已即时下发", in.Amount))
	}
	s.audit(ctx, c, c.OrgID, "submit_approval", "approval", &id, map[string]any{
		"state": a.State, "amount": in.Amount, "model": in.Model, "level2": a.IsLevel2,
	})
	return a, nil
}

// DecideApproval 审批裁决(US-06;有权审批者批准/驳回)。乐观锁防并发重复裁决(20902)。
func (s *Service) DecideApproval(ctx context.Context, c session.Claims, approvalID int64, approved bool, comment string) (*model.Approval, error) {
	a, err := s.store.GetApproval(ctx, c.OrgID, approvalID)
	if errors.Is(err, repo.ErrNotFound) {
		return nil, apperr.NotFound("申请不存在")
	}
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}

	// RBAC:一审(pending)团队负责人(本团队)/组织管理员;二审(l1_approved)仅组织管理员(E12)。
	switch a.State {
	case model.ApprovalPending:
		if err := assertRole(c, session.RoleOrgAdmin, session.RoleTeamLeader); err != nil {
			return nil, err
		}
		if c.Role == session.RoleTeamLeader {
			if err := assertTeamScope(c, a.TeamID); err != nil {
				return nil, err
			}
		}
	case model.ApprovalL1Approved:
		if err := assertRole(c, session.RoleOrgAdmin); err != nil {
			return nil, err
		}
	default:
		return nil, apperr.New(apperr.CodeApprovalHandled, 409, "该申请已处理")
	}

	if !approved {
		var reason *string
		if comment != "" {
			reason = &comment
		}
		col := "l1"
		if a.State == model.ApprovalL1Approved {
			col = "l2"
		}
		if ok, err := s.store.AdvanceApproval(ctx, approvalID, a.State, model.ApprovalRejected, col, c.MemberID, reason); err != nil {
			return nil, apperr.Internal("").WithCause(err)
		} else if !ok {
			return nil, apperr.New(apperr.CodeApprovalHandled, 409, "该申请已被处理")
		}
		s.notify(ctx, a.OrgID, a.ApplicantID, "approval_result", "申请被驳回", comment)
		s.audit(ctx, c, a.OrgID, "decide_approval", "approval", &approvalID, map[string]any{"approved": false})
		return s.store.GetApproval(ctx, c.OrgID, approvalID)
	}

	// 批准:一审且二审档 → l1_approved(待二审);否则 → approved + 下发。
	if a.State == model.ApprovalPending && a.IsLevel2 {
		if ok, err := s.store.AdvanceApproval(ctx, approvalID, model.ApprovalPending, model.ApprovalL1Approved, "l1", c.MemberID, nil); err != nil {
			return nil, apperr.Internal("").WithCause(err)
		} else if !ok {
			return nil, apperr.New(apperr.CodeApprovalHandled, 409, "该申请已被处理")
		}
		s.notify(ctx, a.OrgID, a.ApplicantID, "approval_result", "申请一审通过,待二审", "")
		s.audit(ctx, c, a.OrgID, "decide_approval", "approval", &approvalID, map[string]any{"approved": true, "stage": "l1"})
		return s.store.GetApproval(ctx, c.OrgID, approvalID)
	}

	col := "l1"
	if a.State == model.ApprovalL1Approved {
		col = "l2"
	}
	if ok, err := s.store.AdvanceApproval(ctx, approvalID, a.State, model.ApprovalApproved, col, c.MemberID, nil); err != nil {
		return nil, apperr.Internal("").WithCause(err)
	} else if !ok {
		return nil, apperr.New(apperr.CodeApprovalHandled, 409, "该申请已被处理")
	}
	applicant, err := s.store.GetMember(ctx, a.OrgID, a.ApplicantID)
	if err == nil {
		if derr := s.dispatchApproval(ctx, a, applicant); derr != nil {
			s.log.Error("审批通过下发失败(状态保持 approved,进重试/告警)", "approval_id", approvalID, "err", derr)
		}
	}
	s.notify(ctx, a.OrgID, a.ApplicantID, "approval_result", "申请已通过并下发", "")
	s.audit(ctx, c, a.OrgID, "decide_approval", "approval", &approvalID, map[string]any{"approved": true, "stage": "final"})
	return s.store.GetApproval(ctx, c.OrgID, approvalID)
}

// ListApprovals 列申请队列(组织管理员看本 org;团队负责人看本团队;成员看本人)。
func (s *Service) ListApprovals(ctx context.Context, c session.Claims, state string, limit, offset int) ([]*model.Approval, int, error) {
	var teamID, applicantID *int64
	switch c.Role {
	case session.RoleOperator, session.RoleOrgAdmin:
		// 全 org
	case session.RoleTeamLeader:
		t := c.TeamID
		teamID = &t
	default: // member 看本人
		m := c.MemberID
		applicantID = &m
	}
	return s.store.ListApprovals(ctx, c.OrgID, state, teamID, applicantID, limit, offset)
}

// dispatchApproval 把通过的申请下发:增额→建 quota grant + 重算 override;开模型→建 model_add grant(记录)。
func (s *Service) dispatchApproval(ctx context.Context, a *model.Approval, applicant *model.Member) error {
	expireAt, err := durationToExpiry(a.Payload.Duration, s.now())
	if err != nil {
		return err
	}
	operator := fmt.Sprintf("approval:%d", a.ID)
	if a.RequestType == model.ReqModelOpen {
		_, err := s.store.CreateGrant(ctx, &model.Grant{
			OrgID: a.OrgID, MemberID: a.ApplicantID, GrantType: model.GrantModelAdd,
			Payload: model.GrantPayload{Model: a.Payload.Model}, Operator: operator, ExpireAt: expireAt,
		})
		return err
	}
	if _, err := s.store.CreateGrant(ctx, &model.Grant{
		OrgID: a.OrgID, MemberID: a.ApplicantID, GrantType: model.GrantQuotaAdd,
		Payload: model.GrantPayload{Delta: a.Payload.Amount, Duration: a.Payload.Duration}, Operator: operator, ExpireAt: expireAt,
	}); err != nil {
		return err
	}
	if applicant.BootstrapState == model.BootstrapDone && applicant.NewapiUserID != 0 {
		if _, err := s.applyMemberOverride(ctx, applicant); err != nil {
			return err
		}
	}
	return nil
}

// memberHasModel 报告成员当前层级模型集是否含 model(空集=继承,视为含)。
func (s *Service) memberHasModel(ctx context.Context, m *model.Member, modelName string) bool {
	if m.TierID == nil {
		return true
	}
	t, err := s.store.GetTier(ctx, m.OrgID, *m.TierID)
	if err != nil || len(t.ModelSet) == 0 {
		return true
	}
	for _, mm := range t.ModelSet {
		if mm == modelName {
			return true
		}
	}
	return false
}

func durationDays(d string) int {
	switch d {
	case "", "today":
		return 1
	case "3d":
		return 3
	case "week":
		return 7
	default:
		if t, err := time.Parse(time.RFC3339, d); err == nil {
			days := int(time.Until(t).Hours()/24) + 1
			if days < 1 {
				days = 1
			}
			return days
		}
		return 1
	}
}
