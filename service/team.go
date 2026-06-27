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
	if err := checkName("团队名称", in.Name, maxNameLen); err != nil {
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

// TeamWithCount 团队 + active 成员数(F4 列表/详情)。
type TeamWithCount struct {
	*model.Team
	MemberCount int `json:"member_count"`
}

// ListTeamsWithCounts 列团队 + 每团队 active 成员数(F4,GROUP BY 一次批量,禁 N+1)。
func (s *Service) ListTeamsWithCounts(ctx context.Context, c session.Claims, orgID int64) ([]TeamWithCount, error) {
	if err := assertOrgScope(c, orgID); err != nil {
		return nil, err
	}
	if err := assertRole(c, session.RoleOperator, session.RoleOrgAdmin); err != nil {
		return nil, err
	}
	teams, err := s.store.ListTeams(ctx, orgID)
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	counts, err := s.store.CountActiveMembersByTeam(ctx, orgID)
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	out := make([]TeamWithCount, 0, len(teams))
	for _, t := range teams {
		out = append(out, TeamWithCount{Team: t, MemberCount: counts[t.ID]})
	}
	return out, nil
}

// GetTeamDetail 团队详情 + active 成员数(F1 详情/F4-1;跨 org → 404,org_id 谓词强制)。
func (s *Service) GetTeamDetail(ctx context.Context, c session.Claims, orgID, teamID int64) (*TeamWithCount, error) {
	if err := assertOrgScope(c, orgID); err != nil {
		return nil, err
	}
	if err := assertRole(c, session.RoleOperator, session.RoleOrgAdmin); err != nil {
		return nil, err
	}
	t, err := s.store.GetTeam(ctx, orgID, teamID)
	if errors.Is(err, repo.ErrNotFound) {
		return nil, apperr.NotFound("团队不存在")
	}
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	n, err := s.store.CountActiveMembersInTeam(ctx, orgID, teamID)
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	return &TeamWithCount{Team: t, MemberCount: n}, nil
}

// UpdateTeam 改团队名(F1·AC-F1-1;org_admin;跨 org → 404)。
func (s *Service) UpdateTeam(ctx context.Context, c session.Claims, orgID, teamID int64, name string) (*model.Team, error) {
	if err := assertOrgScope(c, orgID); err != nil {
		return nil, err
	}
	if err := assertRole(c, session.RoleOrgAdmin); err != nil {
		return nil, err
	}
	if err := checkName("团队名称", name, maxNameLen); err != nil {
		return nil, err
	}
	if _, err := s.store.GetTeam(ctx, orgID, teamID); errors.Is(err, repo.ErrNotFound) {
		return nil, apperr.NotFound("团队不存在")
	}
	if err := s.store.UpdateTeamName(ctx, orgID, teamID, name); errors.Is(err, repo.ErrConflict) {
		return nil, apperr.Conflict("同组织内团队名已存在")
	} else if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	s.audit(ctx, c, orgID, "update_team", "team", &teamID, map[string]any{"name": name})
	return s.store.GetTeam(ctx, orgID, teamID)
}

// ArchiveTeam 归档团队(F1·AC-F1-2/3/4;status=archived,归档前须无 active 成员)。
func (s *Service) ArchiveTeam(ctx context.Context, c session.Claims, orgID, teamID int64) error {
	if err := assertOrgScope(c, orgID); err != nil {
		return err
	}
	if err := assertRole(c, session.RoleOrgAdmin); err != nil {
		return err
	}
	if _, err := s.store.GetTeam(ctx, orgID, teamID); errors.Is(err, repo.ErrNotFound) {
		return apperr.NotFound("团队不存在")
	}
	n, err := s.store.CountActiveMembersInTeam(ctx, orgID, teamID)
	if err != nil {
		return apperr.Internal("").WithCause(err)
	}
	if n > 0 {
		return apperr.InvalidParam("该团队下仍有在用成员,请先把成员转出或停用再归档")
	}
	if err := s.store.UpdateTeamStatus(ctx, orgID, teamID, model.StatusArchived); err != nil {
		return apperr.Internal("").WithCause(err)
	}
	s.audit(ctx, c, orgID, "archive_team", "team", &teamID, nil)
	return nil
}

// UnarchiveTeam 撤销归档(F1·AC-F1-2;status=active)。
func (s *Service) UnarchiveTeam(ctx context.Context, c session.Claims, orgID, teamID int64) error {
	if err := assertOrgScope(c, orgID); err != nil {
		return err
	}
	if err := assertRole(c, session.RoleOrgAdmin); err != nil {
		return err
	}
	if _, err := s.store.GetTeam(ctx, orgID, teamID); errors.Is(err, repo.ErrNotFound) {
		return apperr.NotFound("团队不存在")
	}
	if err := s.store.UpdateTeamStatus(ctx, orgID, teamID, model.StatusActive); err != nil {
		return apperr.Internal("").WithCause(err)
	}
	s.audit(ctx, c, orgID, "unarchive_team", "team", &teamID, nil)
	return nil
}
