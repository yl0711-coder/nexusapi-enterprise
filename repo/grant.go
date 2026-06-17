package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/nexusapi-platform/enterprise/model"
)

// CreateGrant 建一条临时授予(active),返回 id。
func (s *Store) CreateGrant(ctx context.Context, g *model.Grant) (int64, error) {
	payload, err := json.Marshal(g.Payload)
	if err != nil {
		return 0, err
	}
	if g.Status == "" {
		g.Status = model.GrantStatusActive
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO member_grant (org_id, member_id, grant_type, payload, reason, operator, expire_at, status)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		g.OrgID, g.MemberID, g.GrantType, string(payload), g.Reason, g.Operator, g.ExpireAt, g.Status)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// GetGrant 取一条 grant(强制 org_id 谓词)。
func (s *Store) GetGrant(ctx context.Context, orgID, id int64) (*model.Grant, error) {
	row := s.db.QueryRowContext(ctx, grantSelect+` WHERE id = ? AND org_id = ?`, id, orgID)
	return scanGrant(row)
}

// ListActiveQuotaGrants 列出某成员当前 active 的额度类 grant(quota_add/quota_sub),供 override 合成。
func (s *Store) ListActiveQuotaGrants(ctx context.Context, orgID, memberID int64) ([]*model.Grant, error) {
	rows, err := s.db.QueryContext(ctx,
		grantSelect+` WHERE org_id = ? AND member_id = ? AND status = 'active'
		    AND grant_type IN ('quota_add','quota_sub') ORDER BY id`, orgID, memberID)
	if err != nil {
		return nil, err
	}
	return scanGrants(rows)
}

// ListGrantsByMember 列出某成员的全部 grant(分页,倒序)。
func (s *Store) ListGrantsByMember(ctx context.Context, orgID, memberID int64, limit, offset int) ([]*model.Grant, int, error) {
	var total int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM member_grant WHERE org_id = ? AND member_id = ?`, orgID, memberID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx,
		grantSelect+` WHERE org_id = ? AND member_id = ? ORDER BY id DESC LIMIT ? OFFSET ?`,
		orgID, memberID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	gs, err := scanGrants(rows)
	return gs, total, err
}

// ListExpiredActiveGrants 列出全平台已到期但仍 active 的 grant(worker 扫到期反向用)。
// 跨 org(worker 是 leader 单写者,不按租户隔离)。limit 限批,避免一次拉太多。
func (s *Store) ListExpiredActiveGrants(ctx context.Context, now time.Time, limit int) ([]*model.Grant, error) {
	rows, err := s.db.QueryContext(ctx,
		grantSelect+` WHERE status = 'active' AND expire_at <= ? ORDER BY expire_at LIMIT ?`, now, limit)
	if err != nil {
		return nil, err
	}
	return scanGrants(rows)
}

// MarkGrantReverted 把 grant 置 expired/revoked 并记反向时间(乐观:仅当当前仍 active 才改,
// 防 worker 与人工撤销重复反向)。返回是否本次成功标记(false=已被他人处理)。
func (s *Store) MarkGrantReverted(ctx context.Context, id int64, newStatus string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE member_grant SET status = ?, reverted_at = CURRENT_TIMESTAMP(3)
		 WHERE id = ? AND status = 'active'`, newStatus, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

const grantSelect = `SELECT id, org_id, member_id, grant_type, payload, reason, operator,
	effective_at, expire_at, status, reverted_at, created_at FROM member_grant`

func scanGrant(r rowScanner) (*model.Grant, error) {
	var g model.Grant
	var payload string
	err := r.Scan(&g.ID, &g.OrgID, &g.MemberID, &g.GrantType, &payload, &g.Reason, &g.Operator,
		&g.EffectiveAt, &g.ExpireAt, &g.Status, &g.RevertedAt, &g.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(payload), &g.Payload)
	return &g, nil
}

func scanGrants(rows *sql.Rows) ([]*model.Grant, error) {
	defer rows.Close()
	var out []*model.Grant
	for rows.Next() {
		g, err := scanGrant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}
