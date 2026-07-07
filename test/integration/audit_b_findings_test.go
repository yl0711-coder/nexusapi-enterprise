// 全平台深审 · B 档修复回归。真 rc.4 + 真 MySQL。
package integration

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// TestIntegration_B3_MissingBalanceNoDeadlock:B3(架构B更新,BE③ 扣款分支拆除):
// 原场景"billing_enabled 但无 company_balance 行→隔离跳过扣费+settlement_missing_balance 告警"随扣款分支退役;
// 保留的锁死点:该形态组织的结算**不报错/不卡死**(水位照常推进)、消费照落 ledger、绝不自动建 balance 行。
func TestIntegration_B3_MissingBalanceNoDeadlock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	svc, store, _, _ := escrowSvc(t, ctx, "nexus_b3") // 非 observe(触达扣费路径)
	newapiSQLDSN := os.Getenv("NEXUS_IT_NEWAPI_SQL_DSN")
	const orgID, memberID = int64(830), int64(1)
	db := store.DB()
	// 开 billing_enabled 但**不充值**(无 company_balance 行)。
	if _, err := db.ExecContext(ctx, `INSERT INTO organization (id, name, slug, billing_enabled) VALUES (?, 'b3-org', 'b3-slug', 1)`, orgID); err != nil {
		t.Fatalf("建组织失败: %v", err)
	}
	cred, perr := svc.EnsureOrgProvisioned(ctx, orgID, "b3-org")
	if perr != nil {
		t.Fatalf("开通失败: %v", perr)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO member (id, org_id, login_email, status, bootstrap_state) VALUES (?, ?, 'b3@t.local', 'active', 'done')`, memberID, orgID); err != nil {
		t.Fatalf("建成员失败: %v", err)
	}
	tokenID := selfServeMemberToken(t, ctx, svc, store, orgID, memberID)

	ndb, _ := sql.Open("mysql", newapiSQLDSN)
	defer ndb.Close()
	var maxLogID int64
	_ = ndb.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM logs`).Scan(&maxLogID)
	logTS := time.Now().Unix() - 300
	if _, err := db.ExecContext(ctx, `UPDATE settlement_cursor SET last_settled_ts=?, last_settled_log_id=? WHERE org_id=0`, logTS-1, maxLogID); err != nil {
		t.Fatalf("置水位失败: %v", err)
	}
	const C = int64(7_000_000)
	seedConsumptionLogTok(t, newapiSQLDSN, int64(cred.NewapiUserID), tokenID, "gpt-b3", C, logTS)

	// 结算:缺 balance 行**不应报错/卡死**。
	if _, err := svc.RunSettlement(ctx); err != nil {
		t.Fatalf("B3:缺 company_balance 行不应让结算报错/卡死,实: %v", err)
	}

	// 水位已推进(卡死的话 log_id 会停在 maxLogID 不动)。
	var curLogID int64
	_ = db.QueryRowContext(ctx, `SELECT last_settled_log_id FROM settlement_cursor WHERE org_id=0`).Scan(&curLogID)
	if curLogID <= maxLogID {
		t.Fatalf("B3:水位应推进过 seed 日志(不卡死),实 log_id=%d 未过 %d", curLogID, maxLogID)
	}
	// 消费仍落 ledger(报表);未自动建 balance 行(隔离跳过,非 GetOrCreate 建负余额)。
	var ledgerSum int64
	_ = db.QueryRowContext(ctx, `SELECT COALESCE(SUM(consumed_quota),0) FROM usage_ledger WHERE org_id=?`, orgID).Scan(&ledgerSum)
	if ledgerSum != C {
		t.Fatalf("B3:消费应仍落 ledger(报表)=%d,实 %d", C, ledgerSum)
	}
	var balRows int
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM company_balance WHERE org_id=?`, orgID).Scan(&balRows)
	if balRows != 0 {
		t.Fatalf("B3:未充值组织不应被自动建 balance 行,实 %d 行", balRows)
	}
	// 架构B:扣款分支已删,settlement_missing_balance 告警不复存在(0 条即对;>0 说明扣款路径没拆净)。
	var alerts int
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_log WHERE org_id=? AND action='settlement_missing_balance'`, orgID).Scan(&alerts)
	if alerts != 0 {
		t.Fatalf("B3(架构B):不应再产 settlement_missing_balance 告警(扣款分支已拆),实 %d 条", alerts)
	}
	t.Logf("B3(架构B) ok: 缺 balance 行结算不卡死(水位推进 log_id=%d)、消费落 ledger=%d、不自动建行、无扣款告警", curLogID, ledgerSum)
}
