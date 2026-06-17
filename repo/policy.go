package repo

import (
	"context"
	"time"
)

// QuotaPolicy 对应 quota_policy 表(09 §6)。
type QuotaPolicy struct {
	ID          int64
	OrgID       int64
	Scope       string // org/team/member
	ScopeID     int64
	Period      string // daily/weekly/monthly
	LimitQuota  int64
	ResetAnchor string
	Status      string
	LastResetAt *time.Time
}

// MarkPolicyReset 记录策略本次重置点(周期重置 worker 用)。
func (s *Store) MarkPolicyReset(ctx context.Context, id int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE quota_policy SET last_reset_at = ? WHERE id = ?`, at, id)
	return err
}

// ListQuotaPolicies 列组织配额策略。
func (s *Store) ListQuotaPolicies(ctx context.Context, orgID int64) ([]*QuotaPolicy, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, org_id, scope, scope_id, period, limit_quota, reset_anchor, status
		 FROM quota_policy WHERE org_id = ? ORDER BY id`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*QuotaPolicy
	for rows.Next() {
		var p QuotaPolicy
		if err := rows.Scan(&p.ID, &p.OrgID, &p.Scope, &p.ScopeID, &p.Period, &p.LimitQuota, &p.ResetAnchor, &p.Status); err != nil {
			return nil, err
		}
		out = append(out, &p)
	}
	return out, rows.Err()
}

// UpsertQuotaPolicy 建/改配额策略(唯一键 org+scope+scope_id+period;在则更新)。
func (s *Store) UpsertQuotaPolicy(ctx context.Context, p *QuotaPolicy) error {
	if p.ResetAnchor == "" {
		p.ResetAnchor = "00:00"
	}
	if p.Status == "" {
		p.Status = "active"
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO quota_policy (org_id, scope, scope_id, period, limit_quota, reset_anchor, status)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE limit_quota=VALUES(limit_quota), reset_anchor=VALUES(reset_anchor), status=VALUES(status)`,
		p.OrgID, p.Scope, p.ScopeID, p.Period, p.LimitQuota, p.ResetAnchor, p.Status)
	return err
}

// ListActivePoliciesForReset 列全平台 active 策略(周期重置 worker 用,跨 org)。
func (s *Store) ListActivePoliciesForReset(ctx context.Context) ([]*QuotaPolicy, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, org_id, scope, scope_id, period, limit_quota, reset_anchor, status, last_reset_at
		 FROM quota_policy WHERE status = 'active'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*QuotaPolicy
	for rows.Next() {
		var p QuotaPolicy
		if err := rows.Scan(&p.ID, &p.OrgID, &p.Scope, &p.ScopeID, &p.Period, &p.LimitQuota, &p.ResetAnchor, &p.Status, &p.LastResetAt); err != nil {
			return nil, err
		}
		out = append(out, &p)
	}
	return out, rows.Err()
}
