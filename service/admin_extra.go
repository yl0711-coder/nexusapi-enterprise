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
	// GZ-05 修复A:update 路径此前对 name 连长度都不校,直接落库 → 配合前端 onclick 拼参可存储型 XSS。
	// 这里补 checkName(长度 + HTML/JS 危险字符二道闸)。
	if name != nil {
		if err := checkName("组织名称", *name, maxNameLen); err != nil {
			return nil, err
		}
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

// UpdateMember 改成员团队/层级/显示名(PATCH /members/:id,A/L)。改层级会重算 override 下发。
// v1 加 displayName(19-F2:导入/开通的成员管理员可随时改名)。
func (s *Service) UpdateMember(ctx context.Context, c session.Claims, orgID, memberID int64, teamID, tierID *int64, displayName *string) (*model.Member, error) {
	m, err := s.loadManageableMember(ctx, c, orgID, memberID)
	if err != nil {
		return nil, err
	}
	if displayName != nil {
		if err := checkName("显示名", *displayName, maxNameLen); err != nil {
			return nil, err
		}
		if err := s.store.UpdateMemberDisplayName(ctx, orgID, memberID, *displayName); err != nil {
			return nil, apperr.Internal("").WithCause(err)
		}
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
	// 改团队或层级 → 重算 override:团队级显式策略与团队默认档都进基线(resolveBaseQuota),
	// 故改 team 同样会变基线,不能只在改 tier 时重算(否则 new-api quota 留旧团队基线,直到下次 reset 才自愈)。
	// nil=未改(UpdateMemberTeamTier 用 COALESCE 不动该列),只在非 nil 时同步内存态 m,保持与 DB 一致。
	if teamID != nil {
		m.TeamID = teamID
	}
	if tierID != nil {
		m.TierID = tierID
	}
	if teamID != nil || tierID != nil {
		if _, err := s.applyMemberOverride(ctx, m); err != nil {
			s.log.Warn("改团队/层级后重算 override 失败", "member_id", memberID, "err", err)
		}
	}
	s.audit(ctx, c, orgID, "update_member", "member", &memberID, map[string]any{"team_id": teamID, "tier_id": tierID, "renamed": displayName != nil})
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
	all, err := s.store.ListQuotaPolicies(ctx, orgID)
	if err != nil {
		return nil, err
	}
	// A8:team_leader 只能读**本团队**策略(与写侧 SetQuotaPolicy 同口径:仅 team 维度、本 TeamID);
	// org_admin/operator 看全 org。避免越团队读到别团队/别人的额度上限(org 内信息泄露)。
	if c.Role == session.RoleTeamLeader {
		var mine []*repo.QuotaPolicy
		for _, p := range all {
			if p.Scope == "team" && p.ScopeID == c.TeamID {
				mine = append(mine, p)
			}
		}
		return mine, nil
	}
	return all, nil
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
	// team_leader 只能设本团队(team 维度)策略,不得越权设 org/他团队额度策略(P1-2)。
	// org_admin/operator 不受此限。本端点 MVP 白名单外(灰度不可达),开控制面第一天即生效。
	if c.Role == session.RoleTeamLeader && (p.Scope != "team" || p.ScopeID != c.TeamID) {
		return apperr.Forbidden("团队负责人只能设置本团队的额度策略")
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
