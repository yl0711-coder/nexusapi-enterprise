package repo

import (
	"context"
	"database/sql"
)

// dbtx 是 *sql.DB 与 *sql.Tx 的公共最小执行器接口(GZ-01:结算钱链原子化)。
// repo 的事务感知方法(...Tx)接受它,从而既能在 autocommit(*sql.DB)下单独调用,
// 也能被收进一个外层事务(*sql.Tx)里原子提交。
type dbtx interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// 编译期断言:*sql.DB 与 *sql.Tx 都满足 dbtx。
var (
	_ dbtx = (*sql.DB)(nil)
	_ dbtx = (*sql.Tx)(nil)
)

// WithTx 在一个事务里执行 fn:fn 返回 nil 则 Commit,否则 Rollback。
// 用于把"写 ledger + 扣余额 + 推水位"做成一个原子提交(GZ-01 修复1/3):
// 任一步失败或水位推进未命中 → fn 返错 → 整批回滚,本轮不推水位、下轮干净重做。
// defer Rollback 在 Commit 成功后是 no-op(sql.ErrTxDone),也兜住 fn panic。
func (s *Store) WithTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
