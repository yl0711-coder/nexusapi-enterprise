// 架构B 阶段0(33 §2-0031)+ 阶段1(0035,BE②):分配账本 ledger_transfer 存取。
// 账本 = 划账守恒真相(复式记账):幂等靠 idempotency_key 唯一键;pending 行是对账环"读-核-补"的输入。
// 阶段1 增加 saga 步骤日志(phase/debited_raw/from_balance_before/fail_reason):
// 每完成一步上游写立即记进度,使对账环能精确区分 未扣/已扣未加/已加未记,绝不多退/少退。
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

// LedgerPhase saga 步骤日志(0035):上游写进度,单向 recorded → debited → credited。
// status 与 phase 正交:status 是业务终态机,phase 是钱面进度真相(对账环判定依据)。
const (
	PhaseRecorded = "recorded" // 意图已记;出账未确认(可能已发出未落日志——极窄崩溃窗)
	PhaseDebited  = "debited"  // 出账已确认(实扣 debited_raw);入账未确认
	PhaseCredited = "credited" // 入账已确认;只差 status 落 applied
)

// fail_reason 约定值(对账环/审计可依赖)。
// refunding/refunded 通用于两类退回:partial_debit(普通模式实扣不足)与 credit_blocked(入账 int32 不可达)。
const (
	FailDebitNotLanded = "debit_not_landed" // 对账环判定:出账从未落地,安全关单
	FailPartialDebit   = "partial_debit"    // 普通模式出账实扣 < 期望(并发抖动),待对账环退回
	FailRefunding      = "refunding"        // 退回中(对账环两阶段标记,防重复退)
	FailRefunded       = "refunded"         // 已退回出账方,关单(净效果 0)
	FailSweepEmpty     = "sweep_empty"      // 实扣 0(余额已被并发清空),无净效果关单
)

type LedgerTransfer struct {
	ID                int64      `json:"id"`
	OrgID             int64      `json:"org_id"`
	FromUserID        int64      `json:"from_user_id"`
	ToUserID          int64      `json:"to_user_id"`
	MemberID          int64      `json:"member_id"`
	AmountRaw         int64      `json:"amount_raw"`
	DebitedRaw        int64      `json:"debited_raw"`
	IdempotencyKey    string     `json:"idempotency_key"`
	Status            string     `json:"status"`
	Phase             string     `json:"phase"`
	FromBalanceBefore *int64     `json:"from_balance_before,omitempty"`
	Reason            string     `json:"reason"`
	FailReason        string     `json:"fail_reason,omitempty"`
	CreatedBy         string     `json:"created_by"`
	CreatedAt         time.Time  `json:"created_at"`
	AppliedAt         *time.Time `json:"applied_at,omitempty"`
}

// InsertTransferPending 落划账意图(pending/recorded)。幂等键冲突返回已存在行(created=false 表示"重放命中,未新建")。
func (s *Store) InsertTransferPending(ctx context.Context, t *LedgerTransfer) (existing *LedgerTransfer, created bool, err error) {
	res, ierr := s.db.ExecContext(ctx,
		`INSERT INTO ledger_transfer (org_id, from_user_id, to_user_id, member_id, amount_raw, idempotency_key, status, phase, from_balance_before, reason, created_by)
		 VALUES (?, ?, ?, ?, ?, ?, 'pending', 'recorded', ?, ?, ?)`,
		t.OrgID, t.FromUserID, t.ToUserID, t.MemberID, t.AmountRaw, t.IdempotencyKey, t.FromBalanceBefore, t.Reason, t.CreatedBy)
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
	t.Phase = PhaseRecorded
	return t, true, nil
}

// MarkTransferDebited saga 日志 J1:出账已确认(recorded→debited,记实扣值)。
// RowsAffected=0 即状态机违例(并发/重复),返回 ErrNotFound 让调用方停手告警——钱面日志写绝不静默吞。
func (s *Store) MarkTransferDebited(ctx context.Context, id int64, debitedRaw int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE ledger_transfer SET phase = 'debited', debited_raw = ? WHERE id = ? AND status = 'pending' AND phase = 'recorded'`,
		debitedRaw, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkTransferCredited saga 日志 J2:入账已确认(debited→credited)。
func (s *Store) MarkTransferCredited(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE ledger_transfer SET phase = 'credited' WHERE id = ? AND status = 'pending' AND phase = 'debited'`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkTransferApplied 置 applied(划账双边完成)。仅从 pending+credited 迁移(状态机单向,防对账环与主路径互踩)。
func (s *Store) MarkTransferApplied(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE ledger_transfer SET status = 'applied', applied_at = CURRENT_TIMESTAMP(3) WHERE id = ? AND status = 'pending' AND phase = 'credited'`, id)
	return err
}

// MarkTransferFailed 置 failed(对账环判死:双边都未发生/已回齐)。仅从 pending 迁移。
func (s *Store) MarkTransferFailed(ctx context.Context, id int64) error {
	return s.MarkTransferFailedWithReason(ctx, id, "")
}

// MarkTransferFailedWithReason 置 failed 并记判死原因(对账环取证可追溯)。仅从 pending 迁移。
func (s *Store) MarkTransferFailedWithReason(ctx context.Context, id int64, failReason string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE ledger_transfer SET status = 'failed', fail_reason = ?, applied_at = CURRENT_TIMESTAMP(3) WHERE id = ? AND status = 'pending'`,
		failReason, id)
	return err
}

// SetTransferFailReason 单向推进 pending 行的 fail_reason(对账环两阶段退回标记:partial_debit → refunding)。
// expect 谓词 + RowsAffected 保证不重复推进(防并发重复退)。
func (s *Store) SetTransferFailReason(ctx context.Context, id int64, expect, next string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE ledger_transfer SET fail_reason = ? WHERE id = ? AND status = 'pending' AND fail_reason = ?`,
		next, id, expect)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// GetTransferByIdemKey 按幂等键读账本行(重放路径)。
func (s *Store) GetTransferByIdemKey(ctx context.Context, idemKey string) (*LedgerTransfer, error) {
	row := s.db.QueryRowContext(ctx, ledgerSelect+` WHERE idempotency_key = ?`, idemKey)
	return scanLedger(row)
}

// GetTransfer 按 id 读账本行(对账环收敛前在 org 锁内重读最新态)。
func (s *Store) GetTransfer(ctx context.Context, id int64) (*LedgerTransfer, error) {
	row := s.db.QueryRowContext(ctx, ledgerSelect+` WHERE id = ?`, id)
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

// SumAppliedNetByUser 某 new-api user 的 Σ划账入账 - Σ划账出账(applied 口径)。
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

// SumJournaledNetByUser 某 new-api user 的**已确认**净入账(phase 口径,比 applied 更精确:
// 滞留行的已确认部分也计入):
//   入账计 phase='credited' 的 amount_raw;
//   出账计 phase IN ('debited','credited') 的 debited_raw,但 refunded 行净效果 0(已扣又已退);
// excludeID 排除"正在判定的本行"(对账环恒等式判定用;全量扫描传 0)。
// ambiguous 返回该 user 相关的"钱面进度不确定"行数(recorded 可能已扣未记 / refunding 可能已退未记 /
// 作为入账方 phase=debited 可能已加未记):>0 时本轮**禁止**下守恒判定,调用方必须跳过。
func (s *Store) SumJournaledNetByUser(ctx context.Context, userID, excludeID int64) (net int64, ambiguous int, err error) {
	var in, out sql.NullInt64
	if err = s.db.QueryRowContext(ctx,
		`SELECT SUM(CASE WHEN to_user_id = ? AND phase = 'credited' THEN amount_raw ELSE 0 END),
		        SUM(CASE WHEN from_user_id = ? AND phase IN ('debited','credited') AND fail_reason <> 'refunded' THEN debited_raw ELSE 0 END)
		   FROM ledger_transfer WHERE (to_user_id = ? OR from_user_id = ?) AND id <> ?`,
		userID, userID, userID, userID, excludeID).Scan(&in, &out); err != nil {
		return 0, 0, err
	}
	var amb sql.NullInt64
	if err = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM ledger_transfer
		  WHERE status = 'pending' AND id <> ?
		    AND ((from_user_id = ? AND phase = 'recorded')
		      OR (from_user_id = ? AND fail_reason = 'refunding')
		      OR (to_user_id = ? AND phase = 'debited'))`,
		excludeID, userID, userID, userID).Scan(&amb); err != nil {
		return 0, 0, err
	}
	return in.Int64 - out.Int64, int(amb.Int64), nil
}

// SumJournaledDeltaAfter 本行(afterID)之后、同 user 的**已确认**账本净变动(recorded 行相对取证法用):
// delta = Σ入账(phase='credited') + Σ退回到账(fail_reason='refunded' 的出账行退回) − Σ出账(phase≥debited)。
// 注意 refunded 行:出账+退回相抵净 0(出账计入、退回也计入,两项相消)。
// ambiguous 同 SumJournaledNetByUser 口径(id > afterID 范围内):>0 时锚点不可信,调用方停手。
func (s *Store) SumJournaledDeltaAfter(ctx context.Context, userID, afterID int64) (delta int64, ambiguous int, err error) {
	var in, refundBack, out sql.NullInt64
	if err = s.db.QueryRowContext(ctx,
		`SELECT SUM(CASE WHEN to_user_id = ? AND phase = 'credited' THEN amount_raw ELSE 0 END),
		        SUM(CASE WHEN from_user_id = ? AND fail_reason = 'refunded' THEN debited_raw ELSE 0 END),
		        SUM(CASE WHEN from_user_id = ? AND phase IN ('debited','credited') THEN debited_raw ELSE 0 END)
		   FROM ledger_transfer WHERE (to_user_id = ? OR from_user_id = ?) AND id > ?`,
		userID, userID, userID, userID, userID, afterID).Scan(&in, &refundBack, &out); err != nil {
		return 0, 0, err
	}
	var amb sql.NullInt64
	if err = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM ledger_transfer
		  WHERE status = 'pending' AND id > ?
		    AND ((from_user_id = ? AND phase = 'recorded')
		      OR (from_user_id = ? AND fail_reason = 'refunding')
		      OR (to_user_id = ? AND phase = 'debited'))`,
		afterID, userID, userID, userID).Scan(&amb); err != nil {
		return 0, 0, err
	}
	return in.Int64 + refundBack.Int64 - out.Int64, int(amb.Int64), nil
}

// 日志证据多重集的操作方向(对账环"manage 日志主证据",组长裁定 33-§12-8)。
const (
	OpDebit  = -1 // 减少(subtract)
	OpCredit = +1 // 增加(add)
)

// ListJournaledOpsSince 取某 user 自 since 起、账本**已确认**的同方向钱面操作金额(排除 excludeID 行),
// 供对账环日志主证据做多重集扣除:new-api manage 日志出现的操作 − 账本已确认的操作 = 未记账残差。
//   OpDebit  → 该 user 作为出账方、phase 已达 debited/credited 的实扣值(退回不消灭"曾 subtract 一次"的日志事实);
//   OpCredit → 该 user 作为入账方、phase 已达 credited 的 amount_raw,加上该 user 作为出账方已完成退回
//               (refunded)的 debited_raw(退回 = 对出账方的一次 add,manage 日志同样可见)。
func (s *Store) ListJournaledOpsSince(ctx context.Context, userID int64, since time.Time, excludeID int64, kind int) ([]int64, error) {
	var q string
	args := []any{userID, since, excludeID}
	switch kind {
	case OpDebit:
		q = `SELECT debited_raw FROM ledger_transfer
		      WHERE from_user_id = ? AND created_at >= ? AND id <> ? AND phase IN ('debited','credited') AND debited_raw > 0`
	case OpCredit:
		q = `SELECT amount_raw FROM ledger_transfer
		      WHERE to_user_id = ? AND created_at >= ? AND id <> ? AND phase = 'credited'
		     UNION ALL
		     SELECT debited_raw FROM ledger_transfer
		      WHERE from_user_id = ? AND created_at >= ? AND id <> ? AND fail_reason = 'refunded' AND debited_raw > 0`
		args = append(args, userID, since, excludeID)
	default:
		return nil, errors.New("repo: 非法 op kind")
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

const ledgerSelect = `SELECT id, org_id, from_user_id, to_user_id, member_id, amount_raw, debited_raw, idempotency_key, status, phase, from_balance_before, reason, fail_reason, created_by, created_at, applied_at FROM ledger_transfer`

type ledgerScanner interface{ Scan(dest ...any) error }

func scanLedger(r ledgerScanner) (*LedgerTransfer, error) {
	var t LedgerTransfer
	var applied sql.NullTime
	var fbb sql.NullInt64
	if err := r.Scan(&t.ID, &t.OrgID, &t.FromUserID, &t.ToUserID, &t.MemberID, &t.AmountRaw, &t.DebitedRaw,
		&t.IdempotencyKey, &t.Status, &t.Phase, &fbb, &t.Reason, &t.FailReason, &t.CreatedBy, &t.CreatedAt, &applied); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if applied.Valid {
		t.AppliedAt = &applied.Time
	}
	if fbb.Valid {
		v := fbb.Int64
		t.FromBalanceBefore = &v
	}
	return &t, nil
}
