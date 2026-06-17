package service

import (
	"context"
	"errors"
	"time"

	"github.com/nexusapi-platform/enterprise/adapter/newapi"
	"github.com/nexusapi-platform/enterprise/model"
	"github.com/nexusapi-platform/enterprise/pkg/apperr"
	"github.com/nexusapi-platform/enterprise/pkg/session"
	"github.com/nexusapi-platform/enterprise/repo"
)

// DefaultBaseQuota 是层级未设任何周期上限时的兜底基线额度(quota)。25_000_000 = $50(09 §16 锚定)。
const DefaultBaseQuota int64 = 25_000_000

// tierBaseQuota 取层级的基线额度:优先 monthly→weekly→daily,都没有则兜底默认(03 §3.1 当期上限)。
func tierBaseQuota(t *model.Tier) int64 {
	if t == nil {
		return DefaultBaseQuota
	}
	switch {
	case t.MonthlyLimit != nil:
		return *t.MonthlyLimit
	case t.WeeklyLimit != nil:
		return *t.WeeklyLimit
	case t.DailyLimit != nil:
		return *t.DailyLimit
	default:
		return DefaultBaseQuota
	}
}

// computeOverride 合成成员当期 override 额度 = 层级基线 + 活跃额度类 grant 之和(clamp 到 0)。
// 生效优先级(02 §3:个人临时 > 层级);团队/组织默认在 tier 未绑时由 DefaultBaseQuota 兜底,
// 更细的多级合成在后续里程碑细化。
func (s *Service) computeOverride(ctx context.Context, m *model.Member) (int64, error) {
	var tier *model.Tier
	if m.TierID != nil {
		t, err := s.store.GetTier(ctx, m.OrgID, *m.TierID)
		if err != nil && !errors.Is(err, repo.ErrNotFound) {
			return 0, err
		}
		tier = t
	}
	base := tierBaseQuota(tier)
	grants, err := s.store.ListActiveQuotaGrants(ctx, m.OrgID, m.ID)
	if err != nil {
		return 0, err
	}
	override := base
	for _, g := range grants {
		override += g.Payload.Delta
	}
	if override < 0 {
		override = 0
	}
	return override, nil
}

// applyMemberOverride 重算并经 adapter(限速出口)把成员 override quota 下发到 new-api。
// 返回下发的 override 值。new-api 用户必须已 bootstrap(有 newapi_user_id)。
func (s *Service) applyMemberOverride(ctx context.Context, m *model.Member) (int64, error) {
	override, err := s.computeOverride(ctx, m)
	if err != nil {
		return 0, err
	}
	if err := s.upstream.ManageUserQuota(ctx, int(m.NewapiUserID), newapi.QuotaOverride, override); err != nil {
		return 0, mapUpstream(err)
	}
	return override, nil
}

// AdjustQuotaInput 是临时调额入参(US-03 / 10 §1.8.2)。
type AdjustQuotaInput struct {
	DeltaQuota int64  // 可正可负(收回)
	Duration   string // today / 3d / week
	Reason     string
}

// AdjustQuotaResult 调额产物。
type AdjustQuotaResult struct {
	NewCapQuota int64
	GrantID     int64
	ExpireAt    time.Time
}

// AdjustQuota 临时调额(US-03,E07):建 grant → 重算 override → 经 adapter 下发 → 审计。
// 到期由 quota-worker 反向(03 §3.4)。RBAC:组织管理员(本 org)/ 团队负责人(本 team)。
func (s *Service) AdjustQuota(ctx context.Context, c session.Claims, orgID, memberID int64, in AdjustQuotaInput) (*AdjustQuotaResult, error) {
	m, err := s.loadManageableMember(ctx, c, orgID, memberID)
	if err != nil {
		return nil, err
	}
	expireAt, err := durationToExpiry(in.Duration, s.now())
	if err != nil {
		return nil, err
	}
	grantType := model.GrantQuotaAdd
	if in.DeltaQuota < 0 {
		grantType = model.GrantQuotaSub
	}
	var reason *string
	if in.Reason != "" {
		reason = &in.Reason
	}
	grantID, err := s.store.CreateGrant(ctx, &model.Grant{
		OrgID: orgID, MemberID: memberID, GrantType: grantType,
		Payload:  model.GrantPayload{Delta: in.DeltaQuota, Duration: in.Duration},
		Reason:   reason, Operator: actorOf(c), ExpireAt: expireAt,
	})
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	newCap, err := s.applyMemberOverride(ctx, m)
	if err != nil {
		return nil, err
	}
	s.audit(ctx, c, orgID, "adjust_quota", "member", &memberID, map[string]any{
		"delta": in.DeltaQuota, "duration": in.Duration, "new_cap": newCap, "grant_id": grantID,
	})
	return &AdjustQuotaResult{NewCapQuota: newCap, GrantID: grantID, ExpireAt: expireAt}, nil
}

// TempPermissionInput 是临时权限入参(US-04)。
type TempPermissionInput struct {
	Type     string    // account_ttl / model_add
	Model    string    // model_add 时:放开的模型
	ExpireAt time.Time // 到期时间(account_ttl=账号有效期止;model_add=收回时点)
	Reason   string
}

// SetTempPermission 设临时权限(US-04,E08):account_ttl(到期停号)/ model_add(临时放开模型)。
// model_add 本期仅记录 grant(令牌当前不限模型,模型硬隔离见 03 §3.4.1,enforcement 待后续)。
func (s *Service) SetTempPermission(ctx context.Context, c session.Claims, orgID, memberID int64, in TempPermissionInput) (*model.Grant, error) {
	m, err := s.loadManageableMember(ctx, c, orgID, memberID)
	if err != nil {
		return nil, err
	}
	_ = m
	if in.Type != model.GrantAccountTTL && in.Type != model.GrantModelAdd {
		return nil, apperr.InvalidParam("临时权限类型须为 account_ttl 或 model_add")
	}
	if in.ExpireAt.Before(s.now()) {
		return nil, apperr.InvalidParam("到期时间须晚于当前")
	}
	if in.Type == model.GrantModelAdd && in.Model == "" {
		return nil, apperr.InvalidParam("model_add 须指定模型")
	}
	var reason *string
	if in.Reason != "" {
		reason = &in.Reason
	}
	grantID, err := s.store.CreateGrant(ctx, &model.Grant{
		OrgID: orgID, MemberID: memberID, GrantType: in.Type,
		Payload:  model.GrantPayload{Model: in.Model},
		Reason:   reason, Operator: actorOf(c), ExpireAt: in.ExpireAt,
	})
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	s.audit(ctx, c, orgID, "set_temp_permission", "member", &memberID, map[string]any{
		"type": in.Type, "model": in.Model, "expire_at": in.ExpireAt.Format(time.RFC3339),
	})
	return s.store.GetGrant(ctx, orgID, grantID)
}

// ListGrants 列成员临时权限(分页)。
func (s *Service) ListGrants(ctx context.Context, c session.Claims, orgID, memberID int64, limit, offset int) ([]*model.Grant, int, error) {
	if _, err := s.loadManageableMember(ctx, c, orgID, memberID); err != nil {
		return nil, 0, err
	}
	return s.store.ListGrantsByMember(ctx, orgID, memberID, limit, offset)
}

// RevokeGrant 提前撤销临时权限(立即反向,不等到期,03 §3.4)。
func (s *Service) RevokeGrant(ctx context.Context, c session.Claims, orgID, grantID int64) error {
	g, err := s.store.GetGrant(ctx, orgID, grantID)
	if errors.Is(err, repo.ErrNotFound) {
		return apperr.NotFound("临时权限不存在")
	}
	if err != nil {
		return apperr.Internal("").WithCause(err)
	}
	m, err := s.loadManageableMember(ctx, c, orgID, g.MemberID)
	if err != nil {
		return err
	}
	if g.Status != model.GrantStatusActive {
		return apperr.New(apperr.CodeInvalidParam, 409, "该临时权限已结束,无需撤销")
	}
	marked, err := s.store.MarkGrantReverted(ctx, grantID, model.GrantStatusRevoked)
	if err != nil {
		return apperr.Internal("").WithCause(err)
	}
	if !marked {
		return apperr.New(apperr.CodeOptimisticLock, 409, "该临时权限已被处理")
	}
	// 额度类撤销 → 重算 override 下发(扣回临时额)。
	if g.GrantType == model.GrantQuotaAdd || g.GrantType == model.GrantQuotaSub {
		if _, err := s.applyMemberOverride(ctx, m); err != nil {
			return err
		}
	}
	s.audit(ctx, c, orgID, "revoke_grant", "member", &g.MemberID, map[string]any{"grant_id": grantID, "type": g.GrantType})
	return nil
}

// SetMemberStatus 停用/恢复成员(US-05,E09):enable/disable new-api 用户 + 改 member.status。
// disable:key 立即失效;恢复:enable,key 复用不重建(03 §3.3)。
func (s *Service) SetMemberStatus(ctx context.Context, c session.Claims, orgID, memberID int64, enabled bool) error {
	m, err := s.loadManageableMember(ctx, c, orgID, memberID)
	if err != nil {
		return err
	}
	if err := s.upstream.SetUserStatus(ctx, int(m.NewapiUserID), enabled); err != nil {
		return mapUpstream(err)
	}
	status := model.MemberStatusDisabled
	if enabled {
		status = model.MemberStatusActive
	}
	if err := s.store.UpdateMemberStatus(ctx, orgID, memberID, status); err != nil {
		return apperr.Internal("").WithCause(err)
	}
	s.audit(ctx, c, orgID, "set_member_status", "member", &memberID, map[string]any{"enabled": enabled})
	return nil
}

// loadManageableMember 取成员并做"可管理"RBAC 校验:组织管理员(本 org)/ 团队负责人(本 team);
// 成员/运营方拒。校验通过返回成员(且已 bootstrap done,有 newapi_user_id)。
func (s *Service) loadManageableMember(ctx context.Context, c session.Claims, orgID, memberID int64) (*model.Member, error) {
	if err := assertOrgScope(c, orgID); err != nil {
		return nil, err
	}
	if err := assertRole(c, session.RoleOrgAdmin, session.RoleTeamLeader); err != nil {
		return nil, err
	}
	m, err := s.store.GetMember(ctx, orgID, memberID)
	if errors.Is(err, repo.ErrNotFound) {
		return nil, apperr.NotFound("成员不存在")
	}
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	if c.Role == session.RoleTeamLeader {
		if err := assertTeamScope(c, m.TeamID); err != nil {
			return nil, err
		}
	}
	if m.BootstrapState != model.BootstrapDone || m.NewapiUserID == 0 {
		return nil, apperr.New(apperr.CodeInvalidParam, 409, "该成员尚未就绪(无可用 new-api 用户)")
	}
	return m, nil
}

// durationToExpiry 把时长标识换成到期时间(UTC;时区精化见后续)。
// today=次日 0 点;3d=+72h;week=+7 天。也接受 RFC3339 绝对时间。
func durationToExpiry(duration string, now time.Time) (time.Time, error) {
	switch duration {
	case "today", "":
		y, mo, d := now.Date()
		return time.Date(y, mo, d, 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1), nil
	case "3d":
		return now.Add(72 * time.Hour), nil
	case "week":
		return now.AddDate(0, 0, 7), nil
	default:
		if t, err := time.Parse(time.RFC3339, duration); err == nil && t.After(now) {
			return t, nil
		}
		return time.Time{}, apperr.InvalidParam("时长须为 today/3d/week 或未来的 RFC3339 时间")
	}
}
