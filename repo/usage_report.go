package repo

import (
	"context"
	"strings"
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
func (s *Store) AggregateUsageByTime(ctx context.Context, orgID int64, since time.Time, granularity string, memberFilter, keyFilter *int64) ([]UsageTimePoint, error) {
	periodExpr := timeBucketPeriodExpr(granularity)
	cond := "org_id = ? AND time_bucket >= ?"
	args := []any{orgID, since}
	if memberFilter != nil {
		cond += " AND member_id = ?" // 模型2:按 member_id(成员共享 org user)
		args = append(args, *memberFilter)
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

// DetailRow 是一条逐条用量明细(usage_detail 一行,对应一条 new-api 消费日志)。
type DetailRow struct {
	OrgID            int64
	MemberID         int64
	NewapiUserID     int64
	KeyID            int64
	TeamID           *int64
	ModelName        string
	NewapiLogID      int64
	PromptTokens     int64
	CompletionTokens int64
	ConsumedQuota    int64
	LogTS            time.Time
}

// InsertUsageDetailTx 批量落逐条明细(INSERT IGNORE 幂等:同 newapi_log_id 只落一次)。
// 在调用方事务上执行(与 usage_ledger 落账、推水位同一事务原子提交)。分批避免单语句参数过多。
func (s *Store) InsertUsageDetailTx(ctx context.Context, x dbtx, rows []DetailRow) error {
	const cols = 11
	const batch = 500
	for start := 0; start < len(rows); start += batch {
		end := start + batch
		if end > len(rows) {
			end = len(rows)
		}
		chunk := rows[start:end]
		var sb strings.Builder
		sb.WriteString("INSERT IGNORE INTO usage_detail (org_id, member_id, newapi_user_id, key_id, team_id, model_name, newapi_log_id, prompt_tokens, completion_tokens, consumed_quota, log_ts) VALUES ")
		args := make([]any, 0, len(chunk)*cols)
		for i, r := range chunk {
			if i > 0 {
				sb.WriteString(",")
			}
			sb.WriteString("(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
			args = append(args, r.OrgID, r.MemberID, r.NewapiUserID, r.KeyID, r.TeamID, r.ModelName, r.NewapiLogID, r.PromptTokens, r.CompletionTokens, r.ConsumedQuota, r.LogTS)
		}
		if _, err := x.ExecContext(ctx, sb.String(), args...); err != nil {
			return err
		}
	}
	return nil
}

// FilterExistingDetailLogIDs M3 补漏扫描(20-§6):批量查哪些 newapi_log_id 已入 usage_detail。
// 重扫 overlap 区间时,已落过明细的行跳过(ledger 桶是累加、非按行幂等,靠这个查重防重复计入)。
func (s *Store) FilterExistingDetailLogIDs(ctx context.Context, logIDs []int64) (map[int64]bool, error) {
	out := map[int64]bool{}
	const batch = 500
	for start := 0; start < len(logIDs); start += batch {
		end := start + batch
		if end > len(logIDs) {
			end = len(logIDs)
		}
		chunk := logIDs[start:end]
		var sb strings.Builder
		sb.WriteString("SELECT newapi_log_id FROM usage_detail WHERE newapi_log_id IN (")
		args := make([]any, 0, len(chunk))
		for i, id := range chunk {
			if i > 0 {
				sb.WriteString(",")
			}
			sb.WriteString("?")
			args = append(args, id)
		}
		sb.WriteString(")")
		rows, err := s.db.QueryContext(ctx, sb.String(), args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			out[id] = true
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// DetailRecord 是读出的一条明细(下钻列表用)。
type DetailRecord struct {
	LogTS            time.Time
	ModelName        string
	KeyID            int64
	MemberID         int64
	NewapiUserID     int64
	PromptTokens     int64
	CompletionTokens int64
	ConsumedQuota    int64
}

// DetailFilter 下钻明细过滤(org 必给;member/key/model 可选;Since 时间下界)。
type DetailFilter struct {
	OrgID    int64
	MemberID *int64 // 平台 member_id
	KeyID    *int64
	Model    string
	Since    time.Time
}

func detailWhere(f DetailFilter) (string, []any) {
	cond := "org_id = ? AND log_ts >= ?"
	args := []any{f.OrgID, f.Since}
	if f.MemberID != nil {
		cond += " AND member_id = ?"
		args = append(args, *f.MemberID)
	}
	if f.KeyID != nil {
		cond += " AND key_id = ?"
		args = append(args, *f.KeyID)
	}
	if f.Model != "" {
		cond += " AND model_name = ?"
		args = append(args, f.Model)
	}
	return cond, args
}

// ListUsageDetail 按过滤分页列逐条明细(log_ts 倒序,最近在前)。只读本库,不查 new-api。
func (s *Store) ListUsageDetail(ctx context.Context, f DetailFilter, limit, offset int) ([]DetailRecord, error) {
	cond, args := detailWhere(f)
	args = append(args, limit, offset)
	rows, err := s.db.QueryContext(ctx,
		`SELECT log_ts, model_name, key_id, member_id, newapi_user_id, prompt_tokens, completion_tokens, consumed_quota
		   FROM usage_detail WHERE `+cond+` ORDER BY log_ts DESC, id DESC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DetailRecord
	for rows.Next() {
		var r DetailRecord
		if err := rows.Scan(&r.LogTS, &r.ModelName, &r.KeyID, &r.MemberID, &r.NewapiUserID, &r.PromptTokens, &r.CompletionTokens, &r.ConsumedQuota); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CountUsageDetail 统计同过滤的明细总行数(分页 total)。
func (s *Store) CountUsageDetail(ctx context.Context, f DetailFilter) (int64, error) {
	cond, args := detailWhere(f)
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_detail WHERE `+cond, args...).Scan(&n)
	return n, err
}

// PurgeUsageDetailBefore 删除 log_ts < cutoff 的明细(90 天保留清理)。分批删,避免长事务/大锁。返回删除行数。
func (s *Store) PurgeUsageDetailBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	var total int64
	for {
		res, err := s.db.ExecContext(ctx, `DELETE FROM usage_detail WHERE log_ts < ? LIMIT 5000`, cutoff)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
		if n < 5000 {
			break
		}
	}
	return total, nil
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
