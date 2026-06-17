package repo

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/nexusapi-platform/enterprise/model"
)

// Cursor 是结算水位(settlement_cursor 一行)。org_id=0 为全局 leader 水位。
type Cursor struct {
	OrgID         int64
	LastSettledTS int64
	Version       int64
}

// GetOrCreateCursor 取(或建)结算水位行。
func (s *Store) GetOrCreateCursor(ctx context.Context, orgID int64) (*Cursor, error) {
	var c Cursor
	err := s.db.QueryRowContext(ctx,
		`SELECT org_id, last_settled_ts, version FROM settlement_cursor WHERE org_id = ?`, orgID).
		Scan(&c.OrgID, &c.LastSettledTS, &c.Version)
	if err == nil {
		return &c, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if _, ierr := s.db.ExecContext(ctx,
		`INSERT INTO settlement_cursor (org_id, last_settled_ts) VALUES (?, 0)`, orgID); ierr != nil && !isDupKey(ierr) {
		return nil, ierr
	}
	return &Cursor{OrgID: orgID, LastSettledTS: 0, Version: 0}, nil
}

// AdvanceCursor 乐观推进水位到 ts(仅当 version 未变且新 ts 更大)。
func (s *Store) AdvanceCursor(ctx context.Context, orgID, newTS, version int64) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE settlement_cursor
		    SET last_settled_ts = ?, last_run_at = CURRENT_TIMESTAMP(3), version = version + 1
		  WHERE org_id = ? AND version = ? AND ? > last_settled_ts`, newTS, orgID, version, newTS)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// LedgerBucket 是一条结算桶(usage_ledger 一行)。
type LedgerBucket struct {
	OrgID         int64
	MemberID      int64
	NewapiUserID  int64
	TeamID        *int64
	ModelName     string
	TimeBucket    time.Time
	ConsumedQuota int64
	LogMaxTS      time.Time
}

// UpsertLedgerBucket 去重插入一条结算桶:命中去重键(org+user+model+桶)→ 不重复累加。
// 返回是否为本次新插入(true=计入扣费;false=重复,已结算过)。
func (s *Store) UpsertLedgerBucket(ctx context.Context, b *LedgerBucket) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT IGNORE INTO usage_ledger
		    (org_id, member_id, newapi_user_id, team_id, model_name, time_bucket, consumed_quota, log_max_ts)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		b.OrgID, b.MemberID, b.NewapiUserID, b.TeamID, b.ModelName, b.TimeBucket, b.ConsumedQuota, b.LogMaxTS)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// DeductBalance 乐观扣减组织余额:total_consumed += amount,balance 重算。返回扣后余额。
func (s *Store) DeductBalance(ctx context.Context, orgID, amount int64) (*model.Balance, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck

	var ver int64
	if err := tx.QueryRowContext(ctx,
		`SELECT version FROM company_balance WHERE org_id = ? FOR UPDATE`, orgID).Scan(&ver); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	// balance 赋值放前面用原值算,避免 MySQL 左到右求值把 amount 减两次(同 AddRecharge 的坑)。
	res, err := tx.ExecContext(ctx,
		`UPDATE company_balance
		    SET balance = total_recharged - total_consumed - total_refunded - ?,
		        total_consumed = total_consumed + ?,
		        version = version + 1
		  WHERE org_id = ? AND version = ?`, amount, amount, orgID, ver)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, ErrOptimisticLock
	}
	var b model.Balance
	if err := tx.QueryRowContext(ctx,
		`SELECT org_id, total_recharged, total_consumed, total_refunded, balance, low_watermark, version
		 FROM company_balance WHERE org_id = ?`, orgID).Scan(
		&b.OrgID, &b.TotalRecharged, &b.TotalConsumed, &b.TotalRefunded, &b.Balance, &b.LowWatermark, &b.Version); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &b, nil
}

// AggregateUsageLedger 从已结算台账聚合用量(看板主数据源,B4:分页安全、不压 new-api)。
// since 起的 time_bucket;userFilter!=nil 只算该 new-api user。返回 按模型 / 按用户 的消耗 + 总量。
func (s *Store) AggregateUsageLedger(ctx context.Context, orgID int64, since time.Time, userFilter *int64) (byModel map[string]int64, byUser map[int64]int64, total int64, err error) {
	byModel = map[string]int64{}
	byUser = map[int64]int64{}
	cond := "org_id = ? AND time_bucket >= ?"
	args := []any{orgID, since}
	if userFilter != nil {
		cond += " AND newapi_user_id = ?"
		args = append(args, *userFilter)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT model_name, newapi_user_id, SUM(consumed_quota) FROM usage_ledger WHERE `+cond+` GROUP BY model_name, newapi_user_id`, args...)
	if err != nil {
		return nil, nil, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var model string
		var uid, q int64
		if err := rows.Scan(&model, &uid, &q); err != nil {
			return nil, nil, 0, err
		}
		byModel[model] += q
		byUser[uid] += q
		total += q
	}
	return byModel, byUser, total, rows.Err()
}

// SumMemberModelToday 累计某成员某模型在 since 之后的已结算消耗(单模型软限额 E4 用)。
func (s *Store) SumMemberModelToday(ctx context.Context, orgID, newapiUserID int64, model string, since time.Time) (int64, error) {
	var q sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT SUM(consumed_quota) FROM usage_ledger
		 WHERE org_id = ? AND newapi_user_id = ? AND model_name = ? AND time_bucket >= ?`,
		orgID, newapiUserID, model, since).Scan(&q)
	if err != nil {
		return 0, err
	}
	return q.Int64, nil
}

// GetMemberByNewapiUserID 按 new-api user_id 反查成员(结算把 log 映射到成员/组织)。无 org 谓词(leader 跨租户)。
func (s *Store) GetMemberByNewapiUserID(ctx context.Context, newapiUserID int64) (*model.Member, error) {
	row := s.db.QueryRowContext(ctx, memberSelect+` WHERE newapi_user_id = ? AND deleted_at IS NULL`, newapiUserID)
	return scanMember(row)
}

// OrgBillingFlags 是逐组织灰度开关。
type OrgBillingFlags struct {
	BillingEnabled  bool
	HardStopEnabled bool
}

// GetOrgBillingFlags 取组织计费灰度开关。
func (s *Store) GetOrgBillingFlags(ctx context.Context, orgID int64) (*OrgBillingFlags, error) {
	var f OrgBillingFlags
	err := s.db.QueryRowContext(ctx,
		`SELECT billing_enabled, hard_stop_enabled FROM organization WHERE id = ? AND deleted_at IS NULL`, orgID).
		Scan(&f.BillingEnabled, &f.HardStopEnabled)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// SetOrgBillingFlags 设组织计费灰度开关(nil=不改)。
func (s *Store) SetOrgBillingFlags(ctx context.Context, orgID int64, billing, hardStop *bool) error {
	if billing != nil {
		if _, err := s.db.ExecContext(ctx, `UPDATE organization SET billing_enabled = ? WHERE id = ?`, *billing, orgID); err != nil {
			return err
		}
	}
	if hardStop != nil {
		if _, err := s.db.ExecContext(ctx, `UPDATE organization SET hard_stop_enabled = ? WHERE id = ?`, *hardStop, orgID); err != nil {
			return err
		}
	}
	return nil
}

// ListBillingEnabledOrgs 列出开了计费的组织 id(settlement 只结这些)。
func (s *Store) ListBillingEnabledOrgs(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM organization WHERE billing_enabled = 1 AND deleted_at IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ListActiveOverridableMembers 列组织内已就绪成员(bootstrap done + 有 newapi_user_id),供硬停/恢复批量 override。
func (s *Store) ListActiveOverridableMembers(ctx context.Context, orgID int64) ([]*model.Member, error) {
	rows, err := s.db.QueryContext(ctx,
		memberSelect+` WHERE org_id = ? AND deleted_at IS NULL AND bootstrap_state = 'done' AND newapi_user_id IS NOT NULL`, orgID)
	if err != nil {
		return nil, err
	}
	return scanMembersRows(rows)
}

func scanMembersRows(rows *sql.Rows) ([]*model.Member, error) {
	defer rows.Close()
	var out []*model.Member
	for rows.Next() {
		m, err := scanMember(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
