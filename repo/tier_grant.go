// 架构B 阶段1(BE① / 组长契约增补 33 §12):档位授权 tier_grant CRUD。
// target_type:all(全组织,target_id=0)| member | team;唯一键 uk_tier_grant 防重复授权。
package repo

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// TierGrant 档位授权行(API 规范名:grant_id/target_type/target_id)。
type TierGrant struct {
	GrantID    int64     `json:"grant_id"`
	OrgID      int64     `json:"-"`
	TierID     int64     `json:"tier_id"`
	TargetType string    `json:"target_type"` // all | member | team
	TargetID   int64     `json:"target_id"`   // all 恒 0
	CreatedAt  time.Time `json:"created_at"`
}

// CreateTierGrant 建授权(重复=ErrConflict)。
func (s *Store) CreateTierGrant(ctx context.Context, g *TierGrant, createdBy string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO tier_grant (org_id, tier_id, target_type, target_id, created_by) VALUES (?, ?, ?, ?, ?)`,
		g.OrgID, g.TierID, g.TargetType, g.TargetID, createdBy)
	if err != nil {
		if isDupKey(err) {
			return 0, ErrConflict
		}
		return 0, err
	}
	return res.LastInsertId()
}

// DeleteTierGrant 按 grant_id 删授权(强制 org+tier 谓词;不存在=ErrNotFound)。
func (s *Store) DeleteTierGrant(ctx context.Context, orgID, tierID, grantID int64) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM tier_grant WHERE id = ? AND org_id = ? AND tier_id = ?`, grantID, orgID, tierID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListTierGrants 列某档位授权(tier 读取随对象带出 grants 数组)。
func (s *Store) ListTierGrants(ctx context.Context, orgID, tierID int64) ([]TierGrant, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, org_id, tier_id, target_type, target_id, created_at
		   FROM tier_grant WHERE org_id = ? AND tier_id = ? ORDER BY id`, orgID, tierID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]TierGrant, 0)
	for rows.Next() {
		var g TierGrant
		if err := rows.Scan(&g.GrantID, &g.OrgID, &g.TierID, &g.TargetType, &g.TargetID, &g.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// GetTierGrant 取单条授权(删授权前校验归属)。
func (s *Store) GetTierGrant(ctx context.Context, orgID, grantID int64) (*TierGrant, error) {
	var g TierGrant
	err := s.db.QueryRowContext(ctx,
		`SELECT id, org_id, tier_id, target_type, target_id, created_at
		   FROM tier_grant WHERE id = ? AND org_id = ?`, grantID, orgID).
		Scan(&g.GrantID, &g.OrgID, &g.TierID, &g.TargetType, &g.TargetID, &g.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &g, nil
}
