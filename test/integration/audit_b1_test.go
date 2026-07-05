// 全平台深审 · B1 escrow 已消费基线回归。真 rc.4 + funding on。
package integration

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/nexusapi-platform/enterprise/pkg/session"
	"github.com/nexusapi-platform/enterprise/service"
)

// TestIntegration_B1_EscrowBaselineExcludesObserve:B1——escrow 对账不因 funding 激活前(观测期)的历史消费
// 冲掉 v2 首充的窗口(客户真亏);首次对账快照该消费为基线,此后只算增量。
func TestIntegration_B1_EscrowBaselineExcludesObserve(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	svc, store, upstream, _ := escrowSvc(t, ctx, "nexus_b1") // funding on
	const orgID = int64(840)
	db := store.DB()
	if _, err := db.ExecContext(ctx, `INSERT INTO organization (id, name, slug, billing_enabled) VALUES (?, 'b1-org', 'b1-slug', 1)`, orgID); err != nil {
		t.Fatalf("建组织失败: %v", err)
	}
	opc := session.Claims{Role: session.RoleOperator, OrgID: orgID}
	cred, perr := svc.EnsureOrgProvisioned(ctx, orgID, "b1-org")
	if perr != nil {
		t.Fatalf("开通失败: %v", perr)
	}

	// 模拟观测期历史消费:直接插一条 ledger(funding 激活前的消费,SUM(ledger) 计入)。
	const Cold = int64(30_000_000)
	if _, err := db.ExecContext(ctx,
		`INSERT INTO usage_ledger (org_id, member_id, newapi_user_id, key_id, model_name, time_bucket, consumed_quota, log_max_ts)
		 VALUES (?, 0, ?, 0, 'obs', '2020-01-01 00:00:00', ?, '2020-01-01 00:00:00')`,
		orgID, cred.NewapiUserID, Cold); err != nil {
		t.Fatalf("注入观测期消费失败: %v", err)
	}

	// v2 首充 A:释放窗口 A 到 new-api user quota。
	const A = int64(50_000_000)
	if _, err := svc.Recharge(ctx, opc, orgID, service.RechargeInput{AmountQuota: A, TransferNo: "b1-1"}); err != nil {
		t.Fatalf("首充失败: %v", err)
	}
	before, _ := upstream.GetUserQuota(ctx, cred.NewapiUserID)

	// 对账:B1 基线排除观测期 Cold → 窗口不被冲(无基线会被减到 A−Cold)。
	if err := svc.ReconcileEscrow(ctx); err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	after, _ := upstream.GetUserQuota(ctx, cred.NewapiUserID)
	if after < before {
		t.Fatalf("B1:对账不应因观测期历史消费(%d)减窗口——after=%d < before=%d(无基线会被冲成 A−Cold)", Cold, after, before)
	}
	// 基线已快照 = Cold(此后只算增量)。
	var baseline sql.NullInt64
	_ = db.QueryRowContext(ctx, `SELECT consumed_baseline FROM org_escrow_config WHERE org_id=?`, orgID).Scan(&baseline)
	if !baseline.Valid || baseline.Int64 != Cold {
		t.Fatalf("B1:基线应快照 = 观测期消费 %d,实 %v", Cold, baseline)
	}
	t.Logf("B1 ok: 基线快照=%d(排除观测期历史)、v2 窗口未被冲(before=%d after=%d)", Cold, before, after)
}
