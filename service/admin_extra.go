package service

import (
	"context"
	"errors"
	"time"

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
	// C11:时区须为合法 IANA 名(否则周期重置按组织时区会解析失败)。
	if timezone != nil && *timezone != "" {
		if _, err := time.LoadLocation(*timezone); err != nil {
			return nil, apperr.InvalidParam("时区非法(须为 IANA 名,如 Asia/Shanghai)")
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

// 审批阈值(Get/SetApprovalRules)与配额策略(List/SetQuotaPolicy)service 方法已随
// 审批/周期重置机器退役删除(33 §12-4;handler/路由已先行摘除,此处清扫残留)。

// UpdateMember 改成员团队/层级/显示名(PATCH /members/:id,A/L)。架构B:改档只改关联,不回溯下发
// (override 机器已退役);调额走显式 quota:grant。displayName 管理员可随时改。
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
	// 架构B:改团队/层级只更新平台侧关联;成员额度=其 new-api user.quota,不随改档自动调整,
	// 需显式 quota:grant(GrantMemberQuota→Transfer,金库↔成员)重新分配(A 版 override 回溯已退役)。
	_ = m // m 仅用于前置校验;架构B 下改档不再据其重算下发
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
	// A3:改角色自增 epoch,作废该成员旧 token——堵"角色漂移"(降级者旧 token 仍是旧角色、可在 12h 内自改回)。
	if err := s.store.BumpMemberSessionEpoch(ctx, orgID, memberID); err != nil {
		return apperr.Internal("").WithCause(err)
	}
	s.audit(ctx, c, orgID, "assign_role", "member", &memberID, map[string]any{"role": role})
	return nil
}

