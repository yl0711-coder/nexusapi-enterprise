// 历史回填 · ledger 对账告警安全网(交付批次24-§9,放量闸最后一块)。真 rc.4。
// 已回填完成的组织,ReconcileBackfillLedger 周期比对 SUM(usage_ledger) 与 new-api stat 权威值:
//   无漂移 → 不告警;注入漂移(伪造多余 ledger 行)→ 必产 backfill_ledger_mismatch 审计。
// 这是"回填/forward 静默多算或漏算"上线后难改风险的收口:发现窗口从月级压到一个对账周期。
package integration

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/nexusapi-platform/enterprise/adapter/newapi"
	"github.com/nexusapi-platform/enterprise/service"
)

func TestIntegration_BackfillLedgerReconcileAlarm(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	svc, store, upstream, _, _ := concSvc(t, ctx, "nexus_bfrec", true)
	newapiSQLDSN := os.Getenv("NEXUS_IT_NEWAPI_SQL_DSN")
	db := store.DB()

	entCred := mkEnterpriseUser(t, ctx, upstream, "recent")
	tokenID, err := upstream.CreateToken(ctx, entCred, newapi.TokenSpec{Name: "rec_tok", UnlimitedQuota: true, ExpiredTime: -1})
	if err != nil {
		t.Fatalf("造令牌失败: %v", err)
	}
	if err := upstream.AddOrgUsableGroup(ctx, "default", "vip"); err != nil {
		t.Fatalf("预配分组失败: %v", err)
	}
	uid := int64(entCred.NewapiUserID)

	// 历史日志(全在边界下游=构造时预置水位之前)。
	now := time.Now().Unix()
	var histSum int64
	for _, s := range []struct{ ts, q int64 }{{now - 30000, 1000}, {now - 20000, 2000}, {now - 10000, 3000}} {
		seedLogWithUsername(t, newapiSQLDSN, uid, int64(tokenID), "recent", "gpt-rec", s.q, s.ts)
		histSum += s.q
	}

	res, err := svc.CreateOrg(ctx, opcOperator(), service.CreateOrgInput{
		Name: "对账企业", Slug: "rec-ok", AdminEmail: "rec@t.local", NewapiUserGroup: "default",
		Associate: &service.AssociateOrgInput{NewapiUserID: int64(entCred.NewapiUserID), AccessToken: entCred.AccessToken, NamePolicy: "inherit"},
	})
	if err != nil {
		t.Fatalf("关联失败: %v", err)
	}
	orgID := res.Org.ID

	// 回填到 done。
	for i := 0; i < 10; i++ {
		if e := svc.RunBackfillSlice(ctx); e != nil {
			t.Fatalf("回填失败: %v", e)
		}
		if j, _ := store.GetBackfillJob(ctx, orgID); j.Status == "done" {
			break
		}
	}

	mismatchCount := func() int {
		var n int
		_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_log WHERE org_id=? AND action='backfill_ledger_mismatch'`, orgID).Scan(&n)
		return n
	}

	// 案例1:无漂移(SUM(ledger)==stat 权威)→ 对账不告警。
	if err := svc.ReconcileBackfillLedger(ctx); err != nil {
		t.Fatalf("对账(无漂移)失败: %v", err)
	}
	if n := mismatchCount(); n != 0 {
		t.Fatalf("无漂移时不应告警,实产 %d 条 mismatch 审计", n)
	}

	// 案例2:注入漂移(伪造一条多余 ledger 行)→ SUM(ledger)>权威 → 必告警。
	if _, err := db.ExecContext(ctx,
		`INSERT INTO usage_ledger (org_id, member_id, newapi_user_id, key_id, model_name, time_bucket, consumed_quota, log_max_ts)
		 VALUES (?, 0, ?, 0, 'drift', '2020-01-01 00:00:00', 999, '2020-01-01 00:00:00')`, orgID, uid); err != nil {
		t.Fatalf("注入漂移失败: %v", err)
	}
	if err := svc.ReconcileBackfillLedger(ctx); err != nil {
		t.Fatalf("对账(有漂移)失败: %v", err)
	}
	if n := mismatchCount(); n != 1 {
		t.Fatalf("注入漂移后应产恰 1 条 mismatch 审计,实 %d", n)
	}
	// 审计详情记 diff。
	var detail sql.NullString
	_ = db.QueryRowContext(ctx, `SELECT detail FROM audit_log WHERE org_id=? AND action='backfill_ledger_mismatch' LIMIT 1`, orgID).Scan(&detail)

	t.Logf("回填-台账对账告警 ok: 无漂移不报(ledger==stat==%d);注入 +999 漂移后精确产 1 条 mismatch 审计(detail=%s)", histSum, detail.String)
}
