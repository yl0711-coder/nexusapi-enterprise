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
