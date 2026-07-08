package repo

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Cursor 是结算水位(settlement_cursor 一行)。org_id=0 为全局 leader 水位。
// LastSettledLogID:已结算到的最大 new-api 日志 id(log-id 级去重,修少收 bug)。
type Cursor struct {
	OrgID            int64
	LastSettledTS    int64
	LastSettledLogID int64
	Version          int64
}

// GetOrCreateCursor 取(或建)结算水位行。
func (s *Store) GetOrCreateCursor(ctx context.Context, orgID int64) (*Cursor, error) {
	var c Cursor
	err := s.db.QueryRowContext(ctx,
		`SELECT org_id, last_settled_ts, last_settled_log_id, version FROM settlement_cursor WHERE org_id = ?`, orgID).
		Scan(&c.OrgID, &c.LastSettledTS, &c.LastSettledLogID, &c.Version)
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
	return &Cursor{OrgID: orgID, LastSettledTS: 0, LastSettledLogID: 0, Version: 0}, nil
}

// AdvanceCursor 乐观推进水位(ts 与 log_id 都单调前进,GREATEST 防回退)。version 乐观锁防并发。
func (s *Store) AdvanceCursor(ctx context.Context, orgID, newTS, newLogID, version int64) (bool, error) {
	return s.AdvanceCursorTx(ctx, s.db, orgID, newTS, newLogID, version)
}

// AdvanceCursorTx 同 AdvanceCursor,但在调用方提供的 execer(*sql.DB 或外层 *sql.Tx)上执行,
// 供 GZ-01 把推水位收进结算事务;返回 ok=是否命中(version 未变)。ok=false 即有并发写者,调用方须回滚。
func (s *Store) AdvanceCursorTx(ctx context.Context, x dbtx, orgID, newTS, newLogID, version int64) (bool, error) {
	res, err := x.ExecContext(ctx,
		`UPDATE settlement_cursor
		    SET last_settled_ts = GREATEST(last_settled_ts, ?),
		        last_settled_log_id = GREATEST(last_settled_log_id, ?),
		        last_run_at = CURRENT_TIMESTAMP(3), version = version + 1
		  WHERE org_id = ? AND version = ?`, newTS, newLogID, orgID, version)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// LedgerBucket 是一条结算桶(usage_ledger 一行)。
// KeyID(v2 M0-S2):平台稳定 key_id(0=未归因);去重键含 key_id,使同 (org,user,model,桶) 下不同 key 各成一行。
type LedgerBucket struct {
	OrgID         int64
	MemberID      int64
	NewapiUserID  int64
	KeyID         int64
	TeamID        *int64
	ModelName     string
	TimeBucket    time.Time
	ConsumedQuota int64
	LogMaxTS      time.Time
}

// AddToLedgerBucket 把本轮新增消耗累加进结算桶(org+user+model+小时桶)。
// 去重已由 log-id 水位在 service 层保证(每条日志只扣一次),故这里对桶做累加而非 INSERT IGNORE——
// 修少收 bug 的关键:同一小时桶后续运行的新增 delta 不再被丢弃。usage_ledger 仅作看板/软限额聚合。
func (s *Store) AddToLedgerBucket(ctx context.Context, b *LedgerBucket) error {
	return s.AddToLedgerBucketTx(ctx, s.db, b)
}

// AddToLedgerBucketTx 同 AddToLedgerBucket,但在调用方提供的 execer(*sql.DB 或外层 *sql.Tx)上执行,
// 供 GZ-01 把落账收进结算事务(与扣余额、推水位同一事务原子提交)。
func (s *Store) AddToLedgerBucketTx(ctx context.Context, x dbtx, b *LedgerBucket) error {
	_, err := x.ExecContext(ctx,
		`INSERT INTO usage_ledger
		    (org_id, member_id, newapi_user_id, key_id, team_id, model_name, time_bucket, consumed_quota, log_max_ts)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE consumed_quota = consumed_quota + VALUES(consumed_quota),
		                         log_max_ts = GREATEST(log_max_ts, VALUES(log_max_ts))`,
		b.OrgID, b.MemberID, b.NewapiUserID, b.KeyID, b.TeamID, b.ModelName, b.TimeBucket, b.ConsumedQuota, b.LogMaxTS)
	return err
}

// DeductBalance/DeductBalanceTx(company_balance 扣款)已随结算扣款分支拆除删除(33 §12-9,第二账退役)。

// AggregateUsageLedger 从已结算台账聚合用量(看板主数据源,B4:分页安全、不压 new-api)。
// since 起的 time_bucket;memberFilter!=nil 只算该成员。模型2:按 member_id 聚合(成员共享 org user,
// 绝不能按 newapi_user_id);ledger.member_id 由结算正确归因写入。返回 按模型 / 按成员(member_id) / 总量。
func (s *Store) AggregateUsageLedger(ctx context.Context, orgID int64, since time.Time, memberFilter *int64) (byModel map[string]int64, byMember map[int64]int64, total int64, err error) {
	byModel = map[string]int64{}
	byMember = map[int64]int64{}
	cond := "org_id = ? AND time_bucket >= ?"
	args := []any{orgID, since}
	if memberFilter != nil {
		cond += " AND member_id = ?"
		args = append(args, *memberFilter)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT model_name, member_id, SUM(consumed_quota) FROM usage_ledger WHERE `+cond+` GROUP BY model_name, member_id`, args...)
	if err != nil {
		return nil, nil, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var model string
		var mid, q int64
		if err := rows.Scan(&model, &mid, &q); err != nil {
			return nil, nil, 0, err
		}
		byModel[model] += q
		byMember[mid] += q
		total += q
	}
	return byModel, byMember, total, rows.Err()
}

// AggregateUsageLedgerByTeam 团队下钻聚合(口径A:JOIN member.team_id 现算,不在 ledger 固化 team 列)。
// teamID==0 → 未分组(member.team_id IS NULL)。返回该团队当前成员的 byModel/byMember/total。模型2:JOIN/聚合按 member_id。
func (s *Store) AggregateUsageLedgerByTeam(ctx context.Context, orgID int64, since time.Time, teamID int64) (byModel map[string]int64, byMember map[int64]int64, total int64, err error) {
	byModel = map[string]int64{}
	byMember = map[int64]int64{}
	args := []any{orgID, since}
	teamCond := "m.team_id = ?"
	if teamID == 0 {
		teamCond = "m.team_id IS NULL"
	} else {
		args = append(args, teamID)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT ul.model_name, ul.member_id, SUM(ul.consumed_quota)
		 FROM usage_ledger ul JOIN member m ON m.id = ul.member_id AND m.org_id = ul.org_id
		 WHERE ul.org_id = ? AND ul.time_bucket >= ? AND `+teamCond+`
		 GROUP BY ul.model_name, ul.member_id`, args...)
	if err != nil {
		return nil, nil, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var model string
		var mid, q int64
		if err := rows.Scan(&model, &mid, &q); err != nil {
			return nil, nil, 0, err
		}
		byModel[model] += q
		byMember[mid] += q
		total += q
	}
	return byModel, byMember, total, rows.Err()
}

// SumMemberModelToday 累计某成员某模型在 since 之后的已结算消耗(单模型软限额 E4 用)。
// 模型2:按 member_id 过滤(成员共享 org user,绝不能按 newapi_user_id=会算成整组织)。
func (s *Store) SumMemberModelToday(ctx context.Context, orgID, memberID int64, model string, since time.Time) (int64, error) {
	var q sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT SUM(consumed_quota) FROM usage_ledger
		 WHERE org_id = ? AND member_id = ? AND model_name = ? AND time_bucket >= ?`,
		orgID, memberID, model, since).Scan(&q)
	if err != nil {
		return 0, err
	}
	return q.Int64, nil
}

// SumLedgerByOrgForBucket 汇总某整点小时桶各组织的已结算消耗(计费对账用,只读)。
func (s *Store) SumLedgerByOrgForBucket(ctx context.Context, bucket time.Time) (map[int64]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT org_id, SUM(consumed_quota) FROM usage_ledger WHERE time_bucket = ? GROUP BY org_id`, bucket)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int64{}
	for rows.Next() {
		var orgID, q int64
		if err := rows.Scan(&orgID, &q); err != nil {
			return nil, err
		}
		out[orgID] = q
	}
	return out, rows.Err()
}

// SumLedgerConsumedByOrg 汇总每组织在 usage_ledger 的累计消耗(余额-台账对账用,GZ-01 D4,只读)。
func (s *Store) SumLedgerConsumedByOrg(ctx context.Context) (map[int64]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT org_id, SUM(consumed_quota) FROM usage_ledger GROUP BY org_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int64{}
	for rows.Next() {
		var orgID, q int64
		if err := rows.Scan(&orgID, &q); err != nil {
			return nil, err
		}
		out[orgID] = q
	}
	return out, rows.Err()
}

// ListOrgTotalConsumed 读每组织 company_balance.total_consumed(余额-台账对账用,GZ-01 D4,只读)。
func (s *Store) ListOrgTotalConsumed(ctx context.Context) (map[int64]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT org_id, total_consumed FROM company_balance`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int64{}
	for rows.Next() {
		var orgID, q int64
		if err := rows.Scan(&orgID, &q); err != nil {
			return nil, err
		}
		out[orgID] = q
	}
	return out, rows.Err()
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

// ListBillingEnabledOrgs 已随扣款分支退役删除(结算全组织统一落账,不再按 billing_enabled 分流)。

// ListActiveOverridableMembers/scanMembersRows 已随 override 下发机器退役删除(33 §12-4)。
