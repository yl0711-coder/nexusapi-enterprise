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
