package repo

import (
	"context"
	"time"
)

// UsageTimePoint 是用量时间序列的一个点(折线图;period 标签 + 该期消耗 quota)。
// Period 形如 day=2026-06-01 / week=2026-W23(ISO 周,周一起) / month=2026-06。
type UsageTimePoint struct {
	Period   string
	Consumed int64
}

// timeBucketPeriodExpr 把 usage_ledger.time_bucket(UTC 小时桶)按粒度折成 UTC+8 自然边界的 period 标签。
// 先 +8 小时转成 UTC+8 墙钟时间再取边界(13 §六:一律 UTC+8,自然日/周/月)。granularity 由调用方白名单收敛,
// 非 day/week/month 一律回落 day(故此处拼进 SQL 安全,无注入面)。
func timeBucketPeriodExpr(granularity string) string {
	switch granularity {
	case "week":
		return `DATE_FORMAT(DATE_ADD(time_bucket, INTERVAL 8 HOUR), '%x-W%v')`
	case "month":
		return `DATE_FORMAT(DATE_ADD(time_bucket, INTERVAL 8 HOUR), '%Y-%m')`
	default:
		return `DATE_FORMAT(DATE_ADD(time_bucket, INTERVAL 8 HOUR), '%Y-%m-%d')`
	}
}

// AggregateUsageByTime 按时间粒度(day/week/month,UTC+8 自然边界)聚合 usage_ledger 消耗,出时间序列(折线图)。
// userFilter!=nil 只算该 new-api user;keyFilter!=nil 只算该平台 key_id。结果按 period 升序(时间正序)。
// 只读 ledger,分页安全、不压 new-api;90 天看板用 since=now-90d。
func (s *Store) AggregateUsageByTime(ctx context.Context, orgID int64, since time.Time, granularity string, userFilter, keyFilter *int64) ([]UsageTimePoint, error) {
	periodExpr := timeBucketPeriodExpr(granularity)
	cond := "org_id = ? AND time_bucket >= ?"
	args := []any{orgID, since}
	if userFilter != nil {
		cond += " AND newapi_user_id = ?"
		args = append(args, *userFilter)
	}
	if keyFilter != nil {
		cond += " AND key_id = ?"
		args = append(args, *keyFilter)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+periodExpr+` AS period, SUM(consumed_quota) AS consumed
		   FROM usage_ledger WHERE `+cond+`
		  GROUP BY period ORDER BY period`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageTimePoint
	for rows.Next() {
		var p UsageTimePoint
		if err := rows.Scan(&p.Period, &p.Consumed); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// AggregateUsageLedgerByKey 按平台稳定 key_id 聚合组织消耗(v2 报表 key 维度)。userFilter!=nil 只算该 user。
// 返回 key_id -> 消耗 quota(key_id=0 为未归因桶,前端可显示「未归因」)。只读 ledger。
func (s *Store) AggregateUsageLedgerByKey(ctx context.Context, orgID int64, since time.Time, userFilter *int64) (map[int64]int64, error) {
	cond := "org_id = ? AND time_bucket >= ?"
	args := []any{orgID, since}
	if userFilter != nil {
		cond += " AND newapi_user_id = ?"
		args = append(args, *userFilter)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT key_id, SUM(consumed_quota) FROM usage_ledger WHERE `+cond+` GROUP BY key_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int64{}
	for rows.Next() {
		var keyID, q int64
		if err := rows.Scan(&keyID, &q); err != nil {
			return nil, err
		}
		out[keyID] = q
	}
	return out, rows.Err()
}
