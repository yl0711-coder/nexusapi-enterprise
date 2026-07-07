// 架构B 阶段0(33 §2-0031):分配账本 ledger_transfer 存取。
// 账本 = 划账守恒真相(复式记账):幂等靠 idempotency_key 唯一键;pending 行是对账环"读-核-补"的输入。
package repo

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// LedgerStatus 划账账本行状态机:pending(意图已记) → applied(双边完成) | failed(对账环判死)。
const (
	LedgerPending = "pending"
	LedgerApplied = "applied"
	LedgerFailed  = "failed"
)

type LedgerTransfer struct {
	ID             int64      `json:"id"`
	OrgID          int64      `json:"org_id"`
	FromUserID     int64      `json:"from_user_id"`
	ToUserID       int64      `json:"to_user_id"`
	MemberID       int64      `json:"member_id"`
	AmountRaw      int64      `json:"amount_raw"`
	IdempotencyKey string     `json:"idempotency_key"`
	Status         string     `json:"status"`
	Reason         string     `json:"reason"`
	CreatedBy      string     `json:"created_by"`
	CreatedAt      time.Time  `json:"created_at"`
	AppliedAt      *time.Time `json:"applied_at,omitempty"`
}

// InsertTransferPending 落划账意图(pending)。幂等键冲突返回已存在行(ok=false 表示"重放命中,未新建")。
func (s *Store) InsertTransferPending(ctx context.Context, t *LedgerTransfer) (existing *LedgerTransfer, created bool, err error) {
	res, ierr := s.db.ExecContext(ctx,
		`INSERT INTO ledger_transfer (org_id, from_user_id, to_user_id, member_id, amount_raw, idempotency_key, status, reason, created_by)
		 VALUES (?, ?, ?, ?, ?, ?, 'pending', ?, ?)`,
		t.OrgID, t.FromUserID, t.ToUserID, t.MemberID, t.AmountRaw, t.IdempotencyKey, t.Reason, t.CreatedBy)
	if ierr != nil {
		if isDupKey(ierr) { // 重放:读回已存在行,由调用方按其状态处置
			row, gerr := s.GetTransferByIdemKey(ctx, t.IdempotencyKey)
			if gerr != nil {
				return nil, false, gerr
			}
			return row, false, nil
		}
		return nil, false, ierr
	}
	id, _ := res.LastInsertId()
	t.ID = id
	t.Status = LedgerPending
	return t, true, nil
}

// MarkTransferApplied 置 applied(划账双边完成)。仅从 pending 迁移(状态机单向,防对账环与主路径互踩)。
func (s *Store) MarkTransferApplied(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE ledger_transfer SET status = 'applied', applied_at = CURRENT_TIMESTAMP(3) WHERE id = ? AND status = 'pending'`, id)
	return err
}

// MarkTransferFailed 置 failed(对账环判死:双边都未发生/已回齐)。仅从 pending 迁移。
func (s *Store) MarkTransferFailed(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE ledger_transfer SET status = 'failed', applied_at = CURRENT_TIMESTAMP(3) WHERE id = ? AND status = 'pending'`, id)
	return err
}

// GetTransferByIdemKey 按幂等键读账本行(重放路径)。
func (s *Store) GetTransferByIdemKey(ctx context.Context, idemKey string) (*LedgerTransfer, error) {
	row := s.db.QueryRowContext(ctx, ledgerSelect+` WHERE idempotency_key = ?`, idemKey)
	return scanLedger(row)
}

// ListPendingTransfers 列滞留 pending 行(对账环输入;olderThan 防抓到还在飞的主路径事务)。
func (s *Store) ListPendingTransfers(ctx context.Context, olderThan time.Duration, limit int) ([]*LedgerTransfer, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		ledgerSelect+` WHERE status = 'pending' AND created_at < (NOW(3) - INTERVAL ? SECOND) ORDER BY id ASC LIMIT ?`,
		int64(olderThan.Seconds()), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*LedgerTransfer
	for rows.Next() {
		t, serr := scanLedger(rows)
		if serr != nil {
			return nil, serr
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// LedgerFilter 账本流水查询(可见性分级由 service 层按角色收敛入参:超管全量/组织传 orgID/成员传 memberID)。
type LedgerFilter struct {
	OrgID    int64 // 0=全平台(仅超管)
	MemberID int64 // >0=仅该成员相关(成员看自己的到账)
	ToUserID int64 // >0=仅入账到该 new-api user 的行(BE③ 成员可见性:仅 to_user=本人,33 §3.5 /me/ledger)
	Limit    int
	Offset   int
}

func (s *Store) ListTransfers(ctx context.Context, f LedgerFilter) ([]*LedgerTransfer, int, error) {
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	where, args := "1=1", []any{}
	if f.OrgID > 0 {
		where += " AND org_id = ?"
		args = append(args, f.OrgID)
	}
	if f.MemberID > 0 {
		where += " AND member_id = ?"
		args = append(args, f.MemberID)
	}
	if f.ToUserID > 0 {
		where += " AND to_user_id = ?"
		args = append(args, f.ToUserID)
	}
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ledger_transfer WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx,
		ledgerSelect+` WHERE `+where+` ORDER BY id DESC LIMIT ? OFFSET ?`, append(args, f.Limit, f.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []*LedgerTransfer
	for rows.Next() {
		t, serr := scanLedger(rows)
		if serr != nil {
			return nil, 0, serr
		}
		out = append(out, t)
	}
	return out, total, rows.Err()
}

// SumAppliedIncomingByUser 某 new-api user 的 Σ划账入账 - Σ划账出账(applied 口径)。
// 对账交叉恒等式「成员 quota 净增 == Σ划账净入账」的账本侧(31-ADR §4.1 护栏,专抓违规直充成员)。
func (s *Store) SumAppliedNetByUser(ctx context.Context, userID int64) (int64, error) {
	var in, out sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		`SELECT SUM(CASE WHEN to_user_id = ? THEN amount_raw ELSE 0 END),
		        SUM(CASE WHEN from_user_id = ? THEN amount_raw ELSE 0 END)
		   FROM ledger_transfer WHERE status = 'applied' AND (to_user_id = ? OR from_user_id = ?)`,
		userID, userID, userID, userID).Scan(&in, &out); err != nil {
		return 0, err
	}
	return in.Int64 - out.Int64, nil
}

const ledgerSelect = `SELECT id, org_id, from_user_id, to_user_id, member_id, amount_raw, idempotency_key, status, reason, created_by, created_at, applied_at FROM ledger_transfer`

type ledgerScanner interface{ Scan(dest ...any) error }

func scanLedger(r ledgerScanner) (*LedgerTransfer, error) {
	var t LedgerTransfer
	var applied sql.NullTime
	if err := r.Scan(&t.ID, &t.OrgID, &t.FromUserID, &t.ToUserID, &t.MemberID, &t.AmountRaw,
		&t.IdempotencyKey, &t.Status, &t.Reason, &t.CreatedBy, &t.CreatedAt, &applied); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if applied.Valid {
		t.AppliedAt = &applied.Time
	}
	return &t, nil
}
