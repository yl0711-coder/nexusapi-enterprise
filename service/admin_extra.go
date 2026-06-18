package service

import (
	"context"
	"errors"

	"github.com/nexusapi-platform/enterprise/model"
	"github.com/nexusapi-platform/enterprise/pkg/apperr"
	"github.com/nexusapi-platform/enterprise/pkg/session"
	"github.com/nexusapi-platform/enterprise/repo"
)

// UpdateOrgSettings 改组织设置(E19,组织管理员:名称/时区/默认层级)。
func (s *Service) UpdateOrgSettings(ctx context.Context, c session.Claims, orgID int64, name, timezone *string, defaultTierID *int64) (*model.Organization, error) {
	if err := assertOrgScope(c, orgID); err != nil {
		return nil, err
	}
	if err := assertRole(c, session.RoleOrgAdmin); err != nil {
		return nil, err
	}
	if defaultTierID != nil {
		if _, err := s.store.GetTier(ctx, orgID, *defaultTierID); errors.Is(err, repo.ErrNotFound) {
			return nil, apperr.InvalidParam("默认层级不存在")
		}
	}
	if err := s.store.UpdateOrgSettings(ctx, orgID, name, timezone, defaultTierID); err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	s.audit(ctx, c, orgID, "update_org_settings", "organization", &orgID, nil)
	return s.store.GetOrganization(ctx, orgID)
}

// SetOrgDefaultTokenGroup 设组织级默认令牌计价分组(D1 两级的回落层,T17;仅运营方·动钱相邻)。
// group 为空/nil → 清空回落 default;非空 → 校验该分组在上游存在再写。改后影响新开通成员的回落分组。
func (s *Service) SetOrgDefaultTokenGroup(ctx context.Context, c session.Claims, orgID int64, group *string) (*model.Organization, error) {
	if err := assertRole(c, session.RoleOperator); err != nil {
		return nil, err
	}
	if _, err := s.store.GetOrganization(ctx, orgID); errors.Is(err, repo.ErrNotFound) {
		return nil, apperr.NotFound("组织不存在")
	} else if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	if group != nil && *group != "" && *group != "default" {
		ratios, err := s.upstream.ListGroupRatios(ctx)
		if err != nil {
			return nil, mapUpstream(err)
		}
		if _, ok := ratios[*group]; !ok {
			return nil, apperr.InvalidParam("计费分组不存在(上游未配)")
		}
	}
	if group != nil && *group == "" {
		group = nil // 空串视为清空
	}
	if err := s.store.SetOrgDefaultTokenGroup(ctx, orgID, group); err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return nil, apperr.NotFound("组织不存在")
		}
		return nil, apperr.Internal("").WithCause(err)
	}
	s.audit(ctx, c, orgID, "set_org_default_token_group", "organization", &orgID, map[string]any{"group": group})
	return s.store.GetOrganization(ctx, orgID)
}

// GetApprovalRules 取审批阈值(O/A)。
func (s *Service) GetApprovalRules(ctx context.Context, c session.Claims, orgID int64) (*repo.ApprovalRules, error) {
	if err := assertOrgScope(c, orgID); err != nil {
		return nil, err
	}
	if err := assertRole(c, session.RoleOperator, session.RoleOrgAdmin); err != nil {
		return nil, err
	}
	return s.store.GetApprovalRules(ctx, orgID)
}

// SetApprovalRules 配置审批阈值(E13,组织管理员;改阈值只影响新申请)。
func (s *Service) SetApprovalRules(ctx context.Context, c session.Claims, orgID int64, autoMax, l1Max *int64, autoDays *int) (*repo.ApprovalRules, error) {
	if err := assertOrgScope(c, orgID); err != nil {
		return nil, err
	}
	if err := assertRole(c, session.RoleOrgAdmin); err != nil {
		return nil, err
	}
	if err := s.store.SetApprovalRules(ctx, orgID, autoMax, l1Max, autoDays); err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	s.audit(ctx, c, orgID, "set_approval_rules", "organization", &orgID, nil)
	return s.store.GetApprovalRules(ctx, orgID)
}

// UpdateMember 改成员团队/层级(PATCH /members/:id,A/L)。改层级会重算 override 下发。
func (s *Service) UpdateMember(ctx context.Context, c session.Claims, orgID, memberID int64, teamID, tierID *int64) (*model.Member, error) {
	m, err := s.loadManageableMember(ctx, c, orgID, memberID)
	if err != nil {
		return nil, err
	}
	if teamID != nil {
		if _, err := s.store.GetTeam(ctx, orgID, *teamID); errors.Is(err, repo.ErrNotFound) {
			return nil, apperr.InvalidParam("团队不存在")
		}
	}
	if tierID != nil {
		if _, err := s.store.GetTier(ctx, orgID, *tierID); errors.Is(err, repo.ErrNotFound) {
			return nil, apperr.InvalidParam("层级不存在")
		}
	}
	if err := s.store.UpdateMemberTeamTier(ctx, orgID, memberID, teamID, tierID); err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	// 改层级 → 重算 override(基线变了)。
	if tierID != nil {
		m.TierID = tierID
		if _, err := s.applyMemberOverride(ctx, m); err != nil {
			s.log.Warn("改层级后重算 override 失败", "member_id", memberID, "err", err)
		}
	}
	s.audit(ctx, c, orgID, "update_member", "member", &memberID, map[string]any{"team_id": teamID, "tier_id": tierID})
	return s.store.GetMember(ctx, orgID, memberID)
}

// AssignRole 任命/变更成员角色(E17,组织管理员)。
func (s *Service) AssignRole(ctx context.Context, c session.Claims, orgID, memberID int64, role string) error {
	if err := assertOrgScope(c, orgID); err != nil {
		return err
	}
	if err := assertRole(c, session.RoleOrgAdmin); err != nil {
		return err
	}
	switch role {
	case string(session.RoleOrgAdmin), string(session.RoleTeamLeader), string(session.RoleMember):
	default:
		return apperr.InvalidParam("角色须为 org_admin/team_leader/member(operator 不可任命)")
	}
	if _, err := s.store.GetMember(ctx, orgID, memberID); errors.Is(err, repo.ErrNotFound) {
		return apperr.NotFound("成员不存在")
	}
	if err := s.store.UpdateMemberRole(ctx, orgID, memberID, role); err != nil {
		return apperr.Internal("").WithCause(err)
	}
	s.audit(ctx, c, orgID, "assign_role", "member", &memberID, map[string]any{"role": role})
	return nil
}

// ListQuotaPolicies 列配额策略(A/L)。
func (s *Service) ListQuotaPolicies(ctx context.Context, c session.Claims, orgID int64) ([]*repo.QuotaPolicy, error) {
	if err := assertOrgScope(c, orgID); err != nil {
		return nil, err
	}
	if err := assertRole(c, session.RoleOrgAdmin, session.RoleTeamLeader); err != nil {
		return nil, err
	}
	return s.store.ListQuotaPolicies(ctx, orgID)
}

// SetQuotaPolicy 建/改配额策略(A/L)。
func (s *Service) SetQuotaPolicy(ctx context.Context, c session.Claims, orgID int64, p *repo.QuotaPolicy) error {
	if err := assertOrgScope(c, orgID); err != nil {
		return err
	}
	if err := assertRole(c, session.RoleOrgAdmin, session.RoleTeamLeader); err != nil {
		return err
	}
	if p.Scope != "org" && p.Scope != "team" && p.Scope != "member" {
		return apperr.InvalidParam("scope 须为 org/team/member")
	}
	if p.Period != "daily" && p.Period != "weekly" && p.Period != "monthly" {
		return apperr.InvalidParam("period 须为 daily/weekly/monthly")
	}
	p.OrgID = orgID
	if err := s.store.UpsertQuotaPolicy(ctx, p); err != nil {
		return apperr.Internal("").WithCause(err)
	}
	s.audit(ctx, c, orgID, "set_quota_policy", "quota_policy", &p.ScopeID, map[string]any{"scope": p.Scope, "period": p.Period, "limit": p.LimitQuota})
	return nil
}
