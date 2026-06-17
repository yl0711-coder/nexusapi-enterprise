package service

import (
	"context"
	"errors"

	"github.com/nexusapi-platform/enterprise/model"
	"github.com/nexusapi-platform/enterprise/pkg/apperr"
	"github.com/nexusapi-platform/enterprise/pkg/session"
	"github.com/nexusapi-platform/enterprise/repo"
)

// CreateTeamInput 建团队入参(E14,组织级,仅组织管理员)。
type CreateTeamInput struct {
	Name          string
	DefaultTierID *int64
}

// CreateTeam 在组织下建团队(E14:组织管理员;团队负责人无权——组织级操作)。
func (s *Service) CreateTeam(ctx context.Context, c session.Claims, orgID int64, in CreateTeamInput) (*model.Team, error) {
	if err := assertOrgScope(c, orgID); err != nil {
		return nil, err
	}
	if err := assertRole(c, session.RoleOrgAdmin); err != nil {
		return nil, err
	}
	if in.Name == "" {
		return nil, apperr.InvalidParam("团队名称必填")
	}
	if err := checkLen("团队名称", in.Name, maxNameLen); err != nil {
		return nil, err
	}
	id, err := s.store.CreateTeam(ctx, &model.Team{OrgID: orgID, Name: in.Name, DefaultTierID: in.DefaultTierID})
	if errors.Is(err, repo.ErrConflict) {
		return nil, apperr.Conflict("同组织内团队名已存在")
	}
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	s.audit(ctx, c, orgID, "create_team", "team", &id, map[string]any{"name": in.Name})
	return s.store.GetTeam(ctx, orgID, id)
}

// ListTeams 列出组织下团队(组织管理员 / 团队负责人可见)。
func (s *Service) ListTeams(ctx context.Context, c session.Claims, orgID int64) ([]*model.Team, error) {
	if err := assertOrgScope(c, orgID); err != nil {
		return nil, err
	}
	if err := assertRole(c, session.RoleOperator, session.RoleOrgAdmin, session.RoleTeamLeader); err != nil {
		return nil, err
	}
	return s.store.ListTeams(ctx, orgID)
}
