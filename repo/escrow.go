package repo

import (
	"context"
	"database/sql"
	"errors"

	"github.com/nexusapi-platform/enterprise/model"
)

// escrowSelect 托管桶字段(模型2 R3,0020)。
const escrowSelect = `SELECT id, org_id, seq, amount, status, threshold, created_at, updated_at FROM escrow_bucket`

func scanEscrow(r rowScanner) (*model.EscrowBucket, error) {
	var b model.EscrowBucket
	if err := r.Scan(&b.ID, &b.OrgID, &b.Seq, &b.Amount, &b.Status, &b.Threshold, &b.CreatedAt, &b.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &b, nil
}

// GetActiveEscrowBucket 取组织的可花窗口桶(seq=1/status=active,镜像进 org user.quota)。不存在 → ErrNotFound。
func (s *Store) GetActiveEscrowBucket(ctx context.Context, orgID int64) (*model.EscrowBucket, error) {
	row := s.db.QueryRowContext(ctx, escrowSelect+` WHERE org_id = ? AND status = ? ORDER BY seq LIMIT 1`, orgID, model.EscrowActive)
	return scanEscrow(row)
}

// ListEscrowBuckets 列组织全部桶(按 seq)。读穿余额/对账用。
func (s *Store) ListEscrowBuckets(ctx context.Context, orgID int64) ([]*model.EscrowBucket, error) {
	rows, err := s.db.QueryContext(ctx, escrowSelect+` WHERE org_id = ? ORDER BY seq`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.EscrowBucket
	for rows.Next() {
		b, err := scanEscrow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// SumHoldingEscrow Σ组织托管(status=holding)桶额(读穿余额=桶1读穿 + 托管之和)。
func (s *Store) SumHoldingEscrow(ctx context.Context, orgID int64) (int64, error) {
	var q sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT SUM(amount) FROM escrow_bucket WHERE org_id = ? AND status = ?`, orgID, model.EscrowHolding).Scan(&q)
	if err != nil {
		return 0, err
	}
	return q.Int64, nil
}

// NextHoldingBucket 取一个最早的 holding 桶(续充时并入桶1);无 → ErrNotFound。
func (s *Store) NextHoldingBucket(ctx context.Context, orgID int64) (*model.EscrowBucket, error) {
	row := s.db.QueryRowContext(ctx, escrowSelect+` WHERE org_id = ? AND status = ? ORDER BY seq LIMIT 1`, orgID, model.EscrowHolding)
	return scanEscrow(row)
}

// MaxEscrowSeq 组织当前最大桶序(新建桶取 +1);无桶返 0。
func (s *Store) MaxEscrowSeq(ctx context.Context, orgID int64) (int, error) {
	var seq sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(seq) FROM escrow_bucket WHERE org_id = ?`, orgID).Scan(&seq); err != nil {
		return 0, err
	}
	return int(seq.Int64), nil
}

// GetActiveEscrowBucketTx 取桶1 并 FOR UPDATE 锁行(在调用方事务上;入账/续充/退款读改写同事务防并发丢失更新)。
func (s *Store) GetActiveEscrowBucketTx(ctx context.Context, x dbtx, orgID int64) (*model.EscrowBucket, error) {
	row := x.QueryRowContext(ctx, escrowSelect+` WHERE org_id = ? AND status = ? ORDER BY seq LIMIT 1 FOR UPDATE`, orgID, model.EscrowActive)
	return scanEscrow(row)
}

// NextHoldingBucketTx 取最早 holding 桶并 FOR UPDATE 锁行(续充并入,同事务防并发重复并桶=超拨)。
func (s *Store) NextHoldingBucketTx(ctx context.Context, x dbtx, orgID int64) (*model.EscrowBucket, error) {
	row := x.QueryRowContext(ctx, escrowSelect+` WHERE org_id = ? AND status = ? ORDER BY seq LIMIT 1 FOR UPDATE`, orgID, model.EscrowHolding)
	return scanEscrow(row)
}

// MaxEscrowSeqTx 在调用方事务上取最大桶序(同事务内分配 holding seq,防并发撞 uk_escrow_org_seq)。
func (s *Store) MaxEscrowSeqTx(ctx context.Context, x dbtx, orgID int64) (int, error) {
	var seq sql.NullInt64
	if err := x.QueryRowContext(ctx, `SELECT MAX(seq) FROM escrow_bucket WHERE org_id = ?`, orgID).Scan(&seq); err != nil {
		return 0, err
	}
	return int(seq.Int64), nil
}

// SumOrgConsumed Σ 组织累计已消费(usage_ledger,bigint)。escrow 对账用**我方账本**算已消费——
// 彻底不碰 new-api used_quota(int32 会溢出 + 将被定期清零)。目标窗口 = 已释放(桶1) − 本值。
func (s *Store) SumOrgConsumed(ctx context.Context, orgID int64) (int64, error) {
	var q sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		`SELECT SUM(consumed_quota) FROM usage_ledger WHERE org_id = ?`, orgID).Scan(&q); err != nil {
		return 0, err
	}
	return q.Int64, nil
}

// ListEscrowOrgIDs 列所有有桶的组织 id(escrow 对账 worker 逐组织核窗口)。
func (s *Store) ListEscrowOrgIDs(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT org_id FROM escrow_bucket`)
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

// ReduceEscrowTx 退款减额(在调用方事务上,FOR UPDATE):**先减托管桶(后进先出)、再减桶1**。
// 返回 windowDec = 从桶1 减掉的额(调用方据此对 newapi 窗口 subtract;托管减不动 newapi)。
// 托管+桶1 仍不够 → ErrInsufficientBalance(理论被 amount≤可用 校验挡)。
func (s *Store) ReduceEscrowTx(ctx context.Context, x dbtx, orgID, amount int64) (windowDec int64, err error) {
	remaining := amount
	// 托管桶后进先出(seq desc)逐个减。
	rows, err := x.QueryContext(ctx, escrowSelect+` WHERE org_id = ? AND status = ? ORDER BY seq DESC FOR UPDATE`, orgID, model.EscrowHolding)
	if err != nil {
		return 0, err
	}
	var holdings []*model.EscrowBucket
	for rows.Next() {
		b, serr := scanEscrow(rows)
		if serr != nil {
			rows.Close()
			return 0, serr
		}
		holdings = append(holdings, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, h := range holdings {
		if remaining <= 0 {
			break
		}
		dec := remaining
		if dec > h.Amount {
			dec = h.Amount
		}
		newAmt := h.Amount - dec
		status := model.EscrowHolding
		if newAmt == 0 {
			status = model.EscrowMerged // 减空的托管桶置 merged(不再计入托管之和)
		}
		if e := s.UpdateEscrowBucketTx(ctx, x, h.ID, newAmt, status); e != nil {
			return 0, e
		}
		remaining -= dec
	}
	// 余下从桶1(active)减,对应 newapi 窗口要减。
	if remaining > 0 {
		active, gerr := s.GetActiveEscrowBucketTx(ctx, x, orgID)
		if gerr != nil {
			if errors.Is(gerr, ErrNotFound) {
				return 0, ErrInsufficientBalance
			}
			return 0, gerr
		}
		dec := remaining
		if dec > active.Amount {
			dec = active.Amount
		}
		if e := s.UpdateEscrowBucketTx(ctx, x, active.ID, active.Amount-dec, model.EscrowActive); e != nil {
			return 0, e
		}
		windowDec = dec
		remaining -= dec
	}
	if remaining > 0 {
		return 0, ErrInsufficientBalance
	}
	return windowDec, nil
}

// CreateEscrowBucketTx 建桶(在调用方事务上,供入账与平台库记录同事务)。返回新 id。
func (s *Store) CreateEscrowBucketTx(ctx context.Context, x dbtx, b *model.EscrowBucket) (int64, error) {
	if b.Status == "" {
		b.Status = model.EscrowHolding
	}
	res, err := x.ExecContext(ctx,
		`INSERT INTO escrow_bucket (org_id, seq, amount, status, threshold) VALUES (?, ?, ?, ?, ?)`,
		b.OrgID, b.Seq, b.Amount, b.Status, b.Threshold)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateEscrowBucketTx 改桶额/状态(续充:旧 holding→merged、桶1 amount 增;在调用方事务上)。
func (s *Store) UpdateEscrowBucketTx(ctx context.Context, x dbtx, id, amount int64, status string) error {
	_, err := x.ExecContext(ctx,
		`UPDATE escrow_bucket SET amount = ?, status = ? WHERE id = ?`, amount, status, id)
	return err
}
