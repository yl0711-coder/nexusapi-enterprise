package repo

import (
	"context"
	"database/sql"
	"errors"
	"time"

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

// GetEscrowConfig 读组织续充阈值配置(0021);无行 → ErrNotFound(调用方用 DEFAULT_NEW 兜底)。
func (s *Store) GetEscrowConfig(ctx context.Context, orgID int64) (*model.OrgEscrowConfig, error) {
	var c model.OrgEscrowConfig
	err := s.db.QueryRowContext(ctx,
		`SELECT org_id, threshold_auto, threshold_manual_override, consumed_baseline, updated_at FROM org_escrow_config WHERE org_id = ?`, orgID).
		Scan(&c.OrgID, &c.ThresholdAuto, &c.ThresholdManualOverride, &c.ConsumedBaseline, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// UpsertThresholdAuto 写每天重算的自动阈值(不动手动覆盖)。
func (s *Store) UpsertThresholdAuto(ctx context.Context, orgID, auto int64) error {
	// 强制 bump updated_at(即使 threshold_auto 值不变),供"每天重算"的陈旧判定;否则值不变时 updated_at 不动→每 tick 重算。
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO org_escrow_config (org_id, threshold_auto) VALUES (?, ?)
		 ON DUPLICATE KEY UPDATE threshold_auto = VALUES(threshold_auto), updated_at = CURRENT_TIMESTAMP(3)`, orgID, auto)
	return err
}

// SetThresholdOverride 运维手动覆盖阈值(nil=清除覆盖回落自动值)。
func (s *Store) SetThresholdOverride(ctx context.Context, orgID int64, override *int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO org_escrow_config (org_id, threshold_manual_override) VALUES (?, ?)
		 ON DUPLICATE KEY UPDATE threshold_manual_override = VALUES(threshold_manual_override)`, orgID, override)
	return err
}

// EscrowUsageStats 近 windowStart 起的补货点输入:peakHourly=某小时最大 Σconsumed_quota、maxSingle=单笔最大;
// earliest=组织全期最早日志时刻(判"历史<7天"用)。无数据 → earliest 无效。
func (s *Store) EscrowUsageStats(ctx context.Context, orgID int64, windowStart time.Time) (peakHourly, maxSingle int64, earliest sql.NullTime, err error) {
	var ph, ms sql.NullInt64
	if e := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(h),0), COALESCE(MAX(m),0) FROM (
		    SELECT SUM(consumed_quota) AS h, MAX(consumed_quota) AS m
		    FROM usage_detail WHERE org_id = ? AND log_ts >= ?
		    GROUP BY FLOOR(UNIX_TIMESTAMP(log_ts)/3600)
		 ) t`, orgID, windowStart).Scan(&ph, &ms); e != nil {
		return 0, 0, sql.NullTime{}, e
	}
	if e := s.db.QueryRowContext(ctx,
		`SELECT MIN(log_ts) FROM usage_detail WHERE org_id = ?`, orgID).Scan(&earliest); e != nil {
		return 0, 0, sql.NullTime{}, e
	}
	return ph.Int64, ms.Int64, earliest, nil
}

// MergeHoldingIntoActiveTx 在调用方事务上把托管桶 FIFO 并入桶1,合计最多 maxMerge;返回实并入额。
// 自动/手工续充共用:补窗口到上限(maxMerge=上限−窗口)。无 active 桶或无托管 → 0。
func (s *Store) MergeHoldingIntoActiveTx(ctx context.Context, x dbtx, orgID, maxMerge int64) (int64, error) {
	if maxMerge <= 0 {
		return 0, nil
	}
	active, aerr := s.GetActiveEscrowBucketTx(ctx, x, orgID)
	if errors.Is(aerr, ErrNotFound) {
		return 0, nil // 无窗口桶可并入
	}
	if aerr != nil {
		return 0, aerr
	}
	remaining := maxMerge
	var merged int64
	for remaining > 0 {
		h, herr := s.NextHoldingBucketTx(ctx, x, orgID)
		if errors.Is(herr, ErrNotFound) {
			break
		}
		if herr != nil {
			return 0, herr
		}
		take := h.Amount
		if take > remaining {
			take = remaining
		}
		if take >= h.Amount {
			if e := s.UpdateEscrowBucketTx(ctx, x, h.ID, 0, model.EscrowMerged); e != nil {
				return 0, e
			}
		} else if e := s.UpdateEscrowBucketTx(ctx, x, h.ID, h.Amount-take, model.EscrowHolding); e != nil {
			return 0, e
		}
		merged += take
		remaining -= take
	}
	if merged > 0 {
		if e := s.UpdateEscrowBucketTx(ctx, x, active.ID, active.Amount+merged, model.EscrowActive); e != nil {
			return 0, e
		}
	}
	return merged, nil
}

// SetEscrowConsumedBaseline 快照某组织的"已消费基线"(B1:funding 激活后首次对账时置为当前 SUM(ledger))。
func (s *Store) SetEscrowConsumedBaseline(ctx context.Context, orgID, baseline int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO org_escrow_config (org_id, consumed_baseline) VALUES (?, ?)
		 ON DUPLICATE KEY UPDATE consumed_baseline = VALUES(consumed_baseline)`, orgID, baseline)
	return err
}

// GetEscrowBaselines 批量取所有已快照的"已消费基线"(B1:ReconcileBalanceLedger 逐组织减基线用)。
func (s *Store) GetEscrowBaselines(ctx context.Context) (map[int64]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT org_id, consumed_baseline FROM org_escrow_config WHERE consumed_baseline IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int64{}
	for rows.Next() {
		var oid, b int64
		if err := rows.Scan(&oid, &b); err != nil {
			return nil, err
		}
		out[oid] = b
	}
	return out, rows.Err()
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
