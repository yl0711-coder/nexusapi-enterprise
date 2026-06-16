package service

import (
	"context"
	"errors"

	"github.com/nexusapi-platform/enterprise/model"
	"github.com/nexusapi-platform/enterprise/pkg/apperr"
	"github.com/nexusapi-platform/enterprise/pkg/session"
	"github.com/nexusapi-platform/enterprise/repo"
)

// CreateTierInput 建层级入参(E15,仅组织管理员)。
type CreateTierInput struct {
	Name         string
	ModelSet     []string
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
	id, err := s.store.CreateTier(ctx, &model.Tier{
		OrgID: orgID, Name: in.Name, ModelSet: in.ModelSet,
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
