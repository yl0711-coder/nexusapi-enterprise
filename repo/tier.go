package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/nexusapi-platform/enterprise/model"
)

// CreateTier 建层级。同 org 内重名 → ErrConflict。
func (s *Store) CreateTier(ctx context.Context, t *model.Tier) (int64, error) {
	if t.Status == "" {
		t.Status = model.StatusActive
	}
	var modelSet any
	if len(t.ModelSet) > 0 {
		b, _ := json.Marshal(t.ModelSet)
		modelSet = string(b)
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO tier (org_id, name, model_set, daily_limit, weekly_limit, monthly_limit, newapi_group, is_default, status)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.OrgID, t.Name, modelSet, t.DailyLimit, t.WeeklyLimit, t.MonthlyLimit, t.NewapiGroup, t.IsDefault, t.Status)
	if err != nil {
		if isDupKey(err) {
			return 0, ErrConflict
		}
		return 0, err
	}
	return res.LastInsertId()
}

// GetTier 取层级,强制 org_id 谓词。
func (s *Store) GetTier(ctx context.Context, orgID, id int64) (*model.Tier, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, org_id, name, model_set, daily_limit, weekly_limit, monthly_limit, newapi_group, is_default, status, created_at, updated_at
		 FROM tier WHERE id = ? AND org_id = ? AND deleted_at IS NULL`, id, orgID)
	return scanTier(row)
}

// ListTiers 列出 org 下层级。
func (s *Store) ListTiers(ctx context.Context, orgID int64) ([]*model.Tier, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, org_id, name, model_set, daily_limit, weekly_limit, monthly_limit, newapi_group, is_default, status, created_at, updated_at
		 FROM tier WHERE org_id = ? AND deleted_at IS NULL ORDER BY id`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Tier
	for rows.Next() {
		t, err := scanTier(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SetDefaultTier 把 tierID 设为组织唯一默认层级(US-10):事务内先清同 org 其余默认、再置本条。
func (s *Store) SetDefaultTier(ctx context.Context, orgID, tierID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	// 校验该层级属本 org 且存在。
	var exists int
	if err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM tier WHERE id = ? AND org_id = ? AND deleted_at IS NULL`, tierID, orgID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE tier SET is_default = 0 WHERE org_id = ? AND is_default = 1`, orgID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE tier SET is_default = 1 WHERE id = ? AND org_id = ?`, tierID, orgID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE organization SET default_tier_id = ? WHERE id = ?`, tierID, orgID); err != nil {
		return err
	}
	return tx.Commit()
}

func scanTier(r rowScanner) (*model.Tier, error) {
	var t model.Tier
	var modelSet sql.NullString
	err := r.Scan(&t.ID, &t.OrgID, &t.Name, &modelSet, &t.DailyLimit, &t.WeeklyLimit,
		&t.MonthlyLimit, &t.NewapiGroup, &t.IsDefault, &t.Status, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if modelSet.Valid && modelSet.String != "" {
		_ = json.Unmarshal([]byte(modelSet.String), &t.ModelSet)
	}
	return &t, nil
}
