package repo

import (
	"context"
	"database/sql"
	"errors"
)

// IdempotencyRecord 是一条幂等记录(10 §1.6)。
type IdempotencyRecord struct {
	State      string // in_progress / done
	HTTPStatus int
	Response   []byte // 成功时回放的响应信封 JSON
}

// BeginIdempotent 尝试以 (org_id, endpoint, key) 占位:
//   - 首次 → 插入 in_progress,返回 (nil, true, nil):调用方继续执行并随后 FinishIdempotent。
//   - 已存在 done → 返回 (record, false, nil):调用方回放原响应。
//   - 已存在 in_progress → 返回 ErrIdempotentInProgress:调用方返 409。
func (s *Store) BeginIdempotent(ctx context.Context, orgID int64, endpoint, key string) (*IdempotencyRecord, bool, error) {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO platform_idempotency (org_id, endpoint, idempotency_key, state) VALUES (?, ?, ?, 'in_progress')`,
		orgID, endpoint, key)
	if err == nil {
		return nil, true, nil // 抢到首占位
	}
	if !isDupKey(err) {
		return nil, false, err
	}
	// 已有记录:读其状态。
	var rec IdempotencyRecord
	var httpStatus sql.NullInt64
	var resp sql.NullString
	row := s.db.QueryRowContext(ctx,
		`SELECT state, http_status, response_snapshot FROM platform_idempotency
		 WHERE org_id = ? AND endpoint = ? AND idempotency_key = ?`, orgID, endpoint, key)
	if err := row.Scan(&rec.State, &httpStatus, &resp); err != nil {
		return nil, false, err
	}
	if rec.State != "done" {
		return nil, false, ErrIdempotentInProgress
	}
	if httpStatus.Valid {
		rec.HTTPStatus = int(httpStatus.Int64)
	}
	if resp.Valid {
		rec.Response = []byte(resp.String)
	}
	return &rec, false, nil
}

// FinishIdempotent 标记幂等记录为 done 并存回放快照。
func (s *Store) FinishIdempotent(ctx context.Context, orgID int64, endpoint, key string, httpStatus int, response []byte) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE platform_idempotency SET state='done', http_status=?, response_snapshot=?
		 WHERE org_id = ? AND endpoint = ? AND idempotency_key = ?`,
		httpStatus, response, orgID, endpoint, key)
	return err
}

// DropIdempotent 在执行失败时删除占位,允许客户端重试(失败不应永久占用幂等键)。
func (s *Store) DropIdempotent(ctx context.Context, orgID int64, endpoint, key string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM platform_idempotency WHERE org_id = ? AND endpoint = ? AND idempotency_key = ? AND state = 'in_progress'`,
		orgID, endpoint, key)
	return err
}

// ErrIdempotentInProgress 表示同一幂等键的请求正在处理中(返 409,10 §1.6)。
var ErrIdempotentInProgress = errors.New("repo: 幂等键命中进行中请求")
