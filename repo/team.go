package repo

import (
	"context"
	"database/sql"
	"errors"

	"github.com/nexusapi-platform/enterprise/model"
)

// CreateTeam 在 org 下建团队。同 org 内重名 → ErrConflict。
func (s *Store) CreateTeam(ctx context.Context, t *model.Team) (int64, error) {
	if t.Status == "" {
		t.Status = model.StatusActive
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO team (org_id, name, leader_member_id, default_tier_id, status)
		 VALUES (?, ?, ?, ?, ?)`,
		t.OrgID, t.Name, t.LeaderMemberID, t.DefaultTierID, t.Status)
	if err != nil {
		if isDupKey(err) {
			return 0, ErrConflict
		}
		return 0, err
	}
	return res.LastInsertId()
}

// GetTeam 取团队,强制 org_id 谓词(跨 org 视为不存在 → ErrNotFound)。
func (s *Store) GetTeam(ctx context.Context, orgID, id int64) (*model.Team, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, org_id, name, leader_member_id, default_tier_id, status, created_at, updated_at
		 FROM team WHERE id = ? AND org_id = ? AND deleted_at IS NULL`, id, orgID)
	return scanTeam(row)
}

// ListTeams 列出 org 下团队。
func (s *Store) ListTeams(ctx context.Context, orgID int64) ([]*model.Team, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, org_id, name, leader_member_id, default_tier_id, status, created_at, updated_at
		 FROM team WHERE org_id = ? AND deleted_at IS NULL ORDER BY id`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Team
	for rows.Next() {
		t, err := scanTeam(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// UpdateTeamName 改团队名(同 org 重名 → ErrConflict)。
func (s *Store) UpdateTeamName(ctx context.Context, orgID, teamID int64, name string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE team SET name = ? WHERE id = ? AND org_id = ? AND deleted_at IS NULL`, name, teamID, orgID)
	if err != nil && isDupKey(err) {
		return ErrConflict
	}
	return err
}

// UpdateTeamStatus 改团队状态(归档/恢复:status=archived/active,软隐藏不物理删,不碰 deleted_at)。
func (s *Store) UpdateTeamStatus(ctx context.Context, orgID, teamID int64, status string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE team SET status = ? WHERE id = ? AND org_id = ? AND deleted_at IS NULL`, status, teamID, orgID)
	return err
}

// CountActiveMembersInTeam 数某团队 active 成员(归档前置校验 AC-F1-3)。
func (s *Store) CountActiveMembersInTeam(ctx context.Context, orgID, teamID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM member WHERE org_id = ? AND team_id = ? AND status = ?`,
		orgID, teamID, model.MemberStatusActive).Scan(&n)
	return n, err
}

// CountActiveMembersByTeam 批量数每团队 active 成员数(F4,GROUP BY 一次查,禁 N+1)。返回 team_id→数。
func (s *Store) CountActiveMembersByTeam(ctx context.Context, orgID int64) (map[int64]int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT team_id, COUNT(*) FROM member
		 WHERE org_id = ? AND team_id IS NOT NULL AND status = ? GROUP BY team_id`,
		orgID, model.MemberStatusActive)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int{}
	for rows.Next() {
		var tid int64
		var n int
		if err := rows.Scan(&tid, &n); err != nil {
			return nil, err
		}
		out[tid] = n
	}
	return out, rows.Err()
}

func scanTeam(r rowScanner) (*model.Team, error) {
	var t model.Team
	err := r.Scan(&t.ID, &t.OrgID, &t.Name, &t.LeaderMemberID, &t.DefaultTierID, &t.Status, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}
