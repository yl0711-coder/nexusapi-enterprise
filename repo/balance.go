package repo

import (
	"context"
	"database/sql"
	"errors"

	"github.com/nexusapi-platform/enterprise/model"
)

// GetOrCreateBalance 取组织余额行,不存在则建一行(余额 0)。每组织一行。
func (s *Store) GetOrCreateBalance(ctx context.Context, orgID int64) (*model.Balance, error) {
	b, err := s.getBalance(ctx, orgID)
	if err == nil {
		return b, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	// 不存在 → 建(并发下唯一键兜底:撞了再读一次)。
	if _, ierr := s.db.ExecContext(ctx,
		`INSERT INTO company_balance (org_id) VALUES (?)`, orgID); ierr != nil && !isDupKey(ierr) {
		return nil, ierr
	}
	return s.getBalance(ctx, orgID)
}

func (s *Store) getBalance(ctx context.Context, orgID int64) (*model.Balance, error) {
	var b model.Balance
	err := s.db.QueryRowContext(ctx,
		`SELECT org_id, total_recharged, total_consumed, total_refunded, balance, low_watermark, version
		 FROM company_balance WHERE org_id = ?`, orgID).Scan(
		&b.OrgID, &b.TotalRecharged, &b.TotalConsumed, &b.TotalRefunded, &b.Balance, &b.LowWatermark, &b.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// AddRecharge 在一个事务内:写 recharge(transfer_no 幂等)+ 累加 company_balance(乐观锁)。
// 返回入账后的余额。重复 transfer_no → ErrConflict(由调用方按"已入账"幂等处理)。
func (s *Store) AddRecharge(ctx context.Context, r *model.Recharge) (*model.Balance, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck

	// 确保余额行存在。
	if _, err := tx.ExecContext(ctx, `INSERT IGNORE INTO company_balance (org_id) VALUES (?)`, r.OrgID); err != nil {
		return nil, err
	}

	// 写入账记录(幂等键 transfer_no)。
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO recharge (org_id, amount, amount_cny, transfer_no, operator, note)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		r.OrgID, r.Amount, r.AmountCNY, r.TransferNo, r.Operator, r.Note); err != nil {
		if isDupKey(err) {
			return nil, ErrConflict
		}
		return nil, err
	}

	// 累加余额(乐观锁:读 version → 带 version 写)。同事务内无并发,version 主要为跨事务防护。
	var ver int64
	if err := tx.QueryRowContext(ctx,
		`SELECT version FROM company_balance WHERE org_id = ? FOR UPDATE`, r.OrgID).Scan(&ver); err != nil {
		return nil, err
	}
	// 注意 MySQL 单表 UPDATE 左到右求值、后续赋值会看到前面已更新的列:
	// 故 balance 赋值放在 total_recharged 之前,用其**原值**算,避免把 amount 加两次。
	res, err := tx.ExecContext(ctx,
		`UPDATE company_balance
		    SET balance = total_recharged + ? - total_consumed - total_refunded,
		        total_recharged = total_recharged + ?,
		        version = version + 1
		  WHERE org_id = ? AND version = ?`, r.Amount, r.Amount, r.OrgID, ver)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, ErrOptimisticLock
	}

	var b model.Balance
	if err := tx.QueryRowContext(ctx,
		`SELECT org_id, total_recharged, total_consumed, total_refunded, balance, low_watermark, version
		 FROM company_balance WHERE org_id = ?`, r.OrgID).Scan(
		&b.OrgID, &b.TotalRecharged, &b.TotalConsumed, &b.TotalRefunded, &b.Balance, &b.LowWatermark, &b.Version); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &b, nil
}

// DebitBalance 减余额冲正(退款,US-12 执行半段,R2-S1 修正):**不动 total_recharged**,
// 改 total_refunded += amount(守恒 balance = 充值 - 退款 - 消耗),并落独立 refund 流水。
// amount 必须 ≤ 当前余额(冲正不得使余额为负)。乐观锁。返回扣后余额。
func (s *Store) DebitBalance(ctx context.Context, orgID, amount int64, reason, operator string) (*model.Balance, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck
	var ver, bal int64
	if err := tx.QueryRowContext(ctx,
		`SELECT version, balance FROM company_balance WHERE org_id = ? FOR UPDATE`, orgID).Scan(&ver, &bal); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if amount > bal {
		return nil, ErrInsufficientBalance
	}
	// 独立退款流水(可变 recharge 表保持不动,审计可追溯)。
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO refund (org_id, amount, reason, operator) VALUES (?, ?, ?, ?)`, orgID, amount, reason, operator); err != nil {
		return nil, err
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE company_balance
		    SET balance = total_recharged - total_consumed - total_refunded - ?,
		        total_refunded = total_refunded + ?,
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

// ErrInsufficientBalance 表示冲正/退款金额超过当前余额。
var ErrInsufficientBalance = errors.New("repo: 余额不足以冲正")

// SetLowWatermark 设组织低位告警阈值。
func (s *Store) SetLowWatermark(ctx context.Context, orgID, watermark int64) error {
	if _, err := s.db.ExecContext(ctx, `INSERT IGNORE INTO company_balance (org_id) VALUES (?)`, orgID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE company_balance SET low_watermark = ? WHERE org_id = ?`, watermark, orgID)
	return err
}

// ListRecharges 列组织入账记录(分页,倒序)。
func (s *Store) ListRecharges(ctx context.Context, orgID int64, limit, offset int) ([]*model.Recharge, int, error) {
	var total int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM recharge WHERE org_id = ?`, orgID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, org_id, amount, amount_cny, transfer_no, operator, note, recharged_at
		 FROM recharge WHERE org_id = ? ORDER BY id DESC LIMIT ? OFFSET ?`, orgID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []*model.Recharge
	for rows.Next() {
		var r model.Recharge
		if err := rows.Scan(&r.ID, &r.OrgID, &r.Amount, &r.AmountCNY, &r.TransferNo, &r.Operator, &r.Note, &r.RechargedAt); err != nil {
			return nil, 0, err
		}
		out = append(out, &r)
	}
	return out, total, rows.Err()
}

// CreateRechargeRequest 建一条申请充值/退款申请(不改余额)。
func (s *Store) CreateRechargeRequest(ctx context.Context, rq *model.RechargeRequest) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO recharge_request (org_id, request_type, amount, note, applicant, status)
		 VALUES (?, ?, ?, ?, ?, 'pending')`,
		rq.OrgID, rq.RequestType, rq.Amount, rq.Note, rq.Applicant)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListRechargeRequests 列申请(分页,倒序)。
func (s *Store) ListRechargeRequests(ctx context.Context, orgID int64, limit, offset int) ([]*model.RechargeRequest, int, error) {
	var total int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM recharge_request WHERE org_id = ?`, orgID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, org_id, request_type, amount, note, applicant, status, processed_by, processed_at, created_at
		 FROM recharge_request WHERE org_id = ? ORDER BY id DESC LIMIT ? OFFSET ?`, orgID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []*model.RechargeRequest
	for rows.Next() {
		var rq model.RechargeRequest
		if err := rows.Scan(&rq.ID, &rq.OrgID, &rq.RequestType, &rq.Amount, &rq.Note, &rq.Applicant,
			&rq.Status, &rq.ProcessedBy, &rq.ProcessedAt, &rq.CreatedAt); err != nil {
			return nil, 0, err
		}
		out = append(out, &rq)
	}
	return out, total, rows.Err()
}
