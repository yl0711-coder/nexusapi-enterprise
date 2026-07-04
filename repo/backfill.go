package repo

import (
	"context"
	"database/sql"
	"errors"
)

// BackfillJob 是一个组织的历史日志回填任务(org_backfill_job 一行,24-§7.1)。
// 边界 B=(BoundaryTS, BoundaryLogID) 是关联瞬间快照的全局 forward 水位;回填从 CursorTS 往 0 走,
// 只处理 (ts,id) <= B 的日志(belongsToBackfill),forward 只处理 > B,两流日志级别不相交(24-§3)。
type BackfillJob struct {
	OrgID          int64
	NewapiUserID   int64
	NewapiUsername string
	BoundaryTS     int64
	BoundaryLogID  int64
	CursorTS       int64 // 回填进度(往 0 递减);= BoundaryTS 表示未开始
	EarliestSeenTS sql.NullInt64
	Status         string // pending|running|done|failed
	RowsIngested   int64
	LastError      sql.NullString
}

// InsertBackfillJob 插入一条 pending 回填任务(门B 关联成功后触发,24-§7.3)。
// 幂等:该组织已有任务则不动(ON DUPLICATE 空更新),避免重复触发把进度冲掉。
func (s *Store) InsertBackfillJob(ctx context.Context, j *BackfillJob) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO org_backfill_job
		    (org_id, newapi_user_id, newapi_username, boundary_ts, boundary_log_id, cursor_ts, status)
		 VALUES (?, ?, ?, ?, ?, ?, 'pending')
		 ON DUPLICATE KEY UPDATE org_id = org_id`,
		j.OrgID, j.NewapiUserID, j.NewapiUsername, j.BoundaryTS, j.BoundaryLogID, j.CursorTS)
	return err
}

// NextBackfillJob 取最早一个未完成(pending/running)的回填任务;无则返回 (nil, nil)。
// 仅由 settlement-worker 单 goroutine 调用(串行单写者,不需 FOR UPDATE)。
func (s *Store) NextBackfillJob(ctx context.Context) (*BackfillJob, error) {
	var j BackfillJob
	err := s.db.QueryRowContext(ctx,
		`SELECT org_id, newapi_user_id, newapi_username, boundary_ts, boundary_log_id, cursor_ts,
		        earliest_seen_ts, status, rows_ingested, last_error
		   FROM org_backfill_job
		  WHERE status IN ('pending','running')
		  ORDER BY created_at ASC
		  LIMIT 1`).
		Scan(&j.OrgID, &j.NewapiUserID, &j.NewapiUsername, &j.BoundaryTS, &j.BoundaryLogID, &j.CursorTS,
			&j.EarliestSeenTS, &j.Status, &j.RowsIngested, &j.LastError)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &j, nil
}

// GetBackfillJob 取某组织的回填任务(前端状态端点用);无则返回 (nil, nil)。
func (s *Store) GetBackfillJob(ctx context.Context, orgID int64) (*BackfillJob, error) {
	var j BackfillJob
	err := s.db.QueryRowContext(ctx,
		`SELECT org_id, newapi_user_id, newapi_username, boundary_ts, boundary_log_id, cursor_ts,
		        earliest_seen_ts, status, rows_ingested, last_error
		   FROM org_backfill_job WHERE org_id = ?`, orgID).
		Scan(&j.OrgID, &j.NewapiUserID, &j.NewapiUsername, &j.BoundaryTS, &j.BoundaryLogID, &j.CursorTS,
			&j.EarliestSeenTS, &j.Status, &j.RowsIngested, &j.LastError)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &j, nil
}

// SetBackfillStatus 置任务状态(pending→running→done),并清 last_error。
func (s *Store) SetBackfillStatus(ctx context.Context, orgID int64, status string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE org_backfill_job SET status = ?, last_error = NULL WHERE org_id = ?`, status, orgID)
	return err
}

// SetBackfillError 记一次错误但**保持 status=running**:非致命错(API 抖动/DB 顿)下轮从 cursor_ts 续跑
// (detail 幂等,重跑安全,24-§9)。不落 failed 以免任务卡死需人工。
func (s *Store) SetBackfillError(ctx context.Context, orgID int64, msg string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE org_backfill_job SET last_error = ? WHERE org_id = ?`, msg, orgID)
	return err
}

// UpdateBackfillProgress 落一个子窗口进度:cursor_ts 下移、rows_ingested 累加、
// earliest_seen_ts 取更早(仅当本窗口见到日志,earliest>0)。
func (s *Store) UpdateBackfillProgress(ctx context.Context, orgID, cursorTS, addRows, earliest int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE org_backfill_job
		    SET cursor_ts = ?,
		        rows_ingested = rows_ingested + ?,
		        earliest_seen_ts = CASE WHEN ? > 0
		                                THEN LEAST(COALESCE(earliest_seen_ts, ?), ?)
		                                ELSE earliest_seen_ts END
		  WHERE org_id = ?`,
		cursorTS, addRows, earliest, earliest, earliest, orgID)
	return err
}

// RequeueBackfillJob 重新回填(运营方按钮,幂等,24-§9):cursor_ts 回到 boundary_ts、status=pending、清错。
// 重跑靠 detail 幂等去重,不会双算;rows_ingested 保留(重跑几乎全部命中已存在、新增为 0)。
func (s *Store) RequeueBackfillJob(ctx context.Context, orgID int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE org_backfill_job
		    SET status = 'pending', cursor_ts = boundary_ts, earliest_seen_ts = NULL, last_error = NULL
		  WHERE org_id = ?`, orgID)
	return err
}
