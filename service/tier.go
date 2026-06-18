package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/nexusapi-platform/enterprise/model"
	"github.com/nexusapi-platform/enterprise/pkg/apperr"
	"github.com/nexusapi-platform/enterprise/pkg/session"
	"github.com/nexusapi-platform/enterprise/repo"
)

// CreateTierInput 建层级入参(E15,仅组织管理员)。
type CreateTierInput struct {
	Name         string
	ModelSet     []string
	ModelCap     map[string]int64
	DailyLimit   *int64
	WeeklyLimit  *int64
	MonthlyLimit *int64
	NewapiGroup  *string
}

// CreateTier 建层级(E15:组织管理员)。
func (s *Service) CreateTier(ctx context.Context, c session.Claims, orgID int64, in CreateTierInput) (*model.Tier, error) {
	if err := assertOrgScope(c, orgID); err != nil {
		return nil, err
	}
	if err := assertRole(c, session.RoleOrgAdmin); err != nil {
		return nil, err
	}
	if in.Name == "" {
		return nil, apperr.InvalidParam("层级名称必填")
	}
	if err := checkLen("层级名称", in.Name, maxNameLen); err != nil {
		return nil, err
	}
	// T17-5/D4:配了计费分组则配置期硬预检(分组存在 + 模型集 ⊆ 分组可用模型),不满足 422,
	// 别等开通/调用才 503。default 不校验(回落默认、天然存在)。
	if in.NewapiGroup != nil {
		if err := s.validateTierGroup(ctx, *in.NewapiGroup, in.ModelSet); err != nil {
			return nil, err
		}
	}
	id, err := s.store.CreateTier(ctx, &model.Tier{
		OrgID: orgID, Name: in.Name, ModelSet: in.ModelSet, ModelCap: in.ModelCap,
		DailyLimit: in.DailyLimit, WeeklyLimit: in.WeeklyLimit, MonthlyLimit: in.MonthlyLimit,
		NewapiGroup: in.NewapiGroup,
	})
	if errors.Is(err, repo.ErrConflict) {
		return nil, apperr.Conflict("同组织内层级名已存在")
	}
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	s.audit(ctx, c, orgID, "create_tier", "tier", &id, map[string]any{"name": in.Name})
	return s.store.GetTier(ctx, orgID, id)
}

// UpdateTierInput 改层级入参(T10;nil 字段=不改)。
type UpdateTierInput struct {
	Name         *string
	ModelSet     []string
	ModelCap     map[string]int64
	DailyLimit   *int64
	WeeklyLimit  *int64
	MonthlyLimit *int64
	NewapiGroup  *string
	SetModelSet  bool // 显式置空模型集(区分"不改"与"清空继承")
	SetModelCap  bool
}

// UpdateTier 改层级(T10:组织管理员)。改后对引用该层级的成员重算 override 下发(当期上限按新档)。
func (s *Service) UpdateTier(ctx context.Context, c session.Claims, orgID, tierID int64, in UpdateTierInput) (*model.Tier, error) {
	if err := assertOrgScope(c, orgID); err != nil {
		return nil, err
	}
	if err := assertRole(c, session.RoleOrgAdmin); err != nil {
		return nil, err
	}
	t, err := s.store.GetTier(ctx, orgID, tierID)
	if errors.Is(err, repo.ErrNotFound) {
		return nil, apperr.NotFound("层级不存在")
	}
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	if in.Name != nil {
		if *in.Name == "" {
			return nil, apperr.InvalidParam("层级名称不能为空")
		}
		if err := checkLen("层级名称", *in.Name, maxNameLen); err != nil {
			return nil, err
		}
		t.Name = *in.Name
	}
	if in.SetModelSet {
		t.ModelSet = in.ModelSet
	}
	if in.SetModelCap {
		t.ModelCap = in.ModelCap
	}
	if in.DailyLimit != nil {
		t.DailyLimit = in.DailyLimit
	}
	if in.WeeklyLimit != nil {
		t.WeeklyLimit = in.WeeklyLimit
	}
	if in.MonthlyLimit != nil {
		t.MonthlyLimit = in.MonthlyLimit
	}
	if in.NewapiGroup != nil {
		t.NewapiGroup = in.NewapiGroup
	}
	// T17-5/D4:改了分组或模型集,按改后的有效组合做配置期硬预检(分组存在 + 模型集 ⊆ 分组可用模型)。
	if t.NewapiGroup != nil {
		if err := s.validateTierGroup(ctx, *t.NewapiGroup, t.ModelSet); err != nil {
			return nil, err
		}
	}
	if err := s.store.UpdateTier(ctx, t); err != nil {
		if errors.Is(err, repo.ErrConflict) {
			return nil, apperr.Conflict("同组织内层级名已存在")
		}
		if errors.Is(err, repo.ErrNotFound) {
			return nil, apperr.NotFound("层级不存在")
		}
		return nil, apperr.Internal("").WithCause(err)
	}
	// 改层级后:对引用该层级、已就绪的成员重算 override 下发(当期上限按新档,best-effort)。
	if members, lerr := s.store.ListMembersByTier(ctx, orgID, tierID); lerr == nil {
		for _, m := range members {
			if m.BootstrapState == model.BootstrapDone && m.NewapiUserID != 0 {
				if _, aerr := s.applyMemberOverride(ctx, m); aerr != nil {
					s.log.Error("改层级后成员 override 重算失败(待对账/重试)", "member_id", m.ID, "tier_id", tierID, "err", aerr)
				}
			}
		}
	}
	s.audit(ctx, c, orgID, "update_tier", "tier", &tierID, map[string]any{"name": t.Name})
	return s.store.GetTier(ctx, orgID, tierID)
}

// validateTierGroup 配置期校验计费分组(T17-5/D4):分组须存在 + 模型集 ⊆ 该分组可用模型。
// group 为空 / default 免校验(回落默认、天然存在);模型集为空(继承)免模型校验。数据源 /api/pricing(D6)。
func (s *Service) validateTierGroup(ctx context.Context, group string, modelSet []string) error {
	if group == "" || group == "default" {
		return nil
	}
	ratios, err := s.upstream.ListGroupRatios(ctx)
	if err != nil {
		return mapUpstream(err)
	}
	if _, ok := ratios[group]; !ok {
		return apperr.InvalidParam(fmt.Sprintf("计费分组 %q 不存在(上游未配),请先在 new-api 建该分组", group))
	}
	if len(modelSet) == 0 {
		return nil
	}
	g2m, err := s.upstream.ListGroupModels(ctx)
	if err != nil {
		return mapUpstream(err)
	}
	avail := map[string]bool{}
	for _, m := range g2m[group] {
		avail[m] = true
	}
	var missing []string
	for _, m := range modelSet {
		if !avail[m] {
			missing = append(missing, m)
		}
	}
	if len(missing) > 0 {
		return apperr.InvalidParam(fmt.Sprintf("计费分组 %q 无以下模型的可用渠道:%v;请调整模型集或换分组", group, missing))
	}
	return nil
}

// DeleteTier 删层级(T10:组织管理员)。被成员引用 / 是组织默认档 → 拒并提示占用,防误删。
func (s *Service) DeleteTier(ctx context.Context, c session.Claims, orgID, tierID int64) error {
	if err := assertOrgScope(c, orgID); err != nil {
		return err
	}
	if err := assertRole(c, session.RoleOrgAdmin); err != nil {
		return err
	}
	t, err := s.store.GetTier(ctx, orgID, tierID)
	if errors.Is(err, repo.ErrNotFound) {
		return apperr.NotFound("层级不存在")
	}
	if err != nil {
		return apperr.Internal("").WithCause(err)
	}
	if t.IsDefault {
		return apperr.New(apperr.CodeConflictDup, 409, "该层级是组织默认档,请先改设其它默认档再删")
	}
	used, err := s.store.CountMembersUsingTier(ctx, orgID, tierID)
	if err != nil {
		return apperr.Internal("").WithCause(err)
	}
	if used > 0 {
		return apperr.New(apperr.CodeConflictDup, 409, fmt.Sprintf("该层级仍被 %d 名成员使用,请先迁移成员到其它层级再删", used))
	}
	if err := s.store.SoftDeleteTier(ctx, orgID, tierID); err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return apperr.NotFound("层级不存在")
		}
		return apperr.Internal("").WithCause(err)
	}
	s.audit(ctx, c, orgID, "delete_tier", "tier", &tierID, nil)
	return nil
}

// ListTiers 列出组织下层级(组织管理员)。
func (s *Service) ListTiers(ctx context.Context, c session.Claims, orgID int64) ([]*model.Tier, error) {
	if err := assertOrgScope(c, orgID); err != nil {
		return nil, err
	}
	if err := assertRole(c, session.RoleOperator, session.RoleOrgAdmin); err != nil {
		return nil, err
	}
	return s.store.ListTiers(ctx, orgID)
}

// SetDefaultTier 设组织默认层级(E16 / US-10:组织管理员,默认层级全组织唯一)。
func (s *Service) SetDefaultTier(ctx context.Context, c session.Claims, orgID, tierID int64) error {
	if err := assertOrgScope(c, orgID); err != nil {
		return err
	}
	if err := assertRole(c, session.RoleOrgAdmin); err != nil {
		return err
	}
	err := s.store.SetDefaultTier(ctx, orgID, tierID)
	if errors.Is(err, repo.ErrNotFound) {
		return apperr.NotFound("层级不存在")
	}
	if err != nil {
		return apperr.Internal("").WithCause(err)
	}
	s.audit(ctx, c, orgID, "set_default_tier", "tier", &tierID, nil)
	return nil
}
