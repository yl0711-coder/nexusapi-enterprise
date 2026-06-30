// observe 漏闸回归测试(总监审计 2026-06-30,B/C):观测模式下 quota-worker 绝不写 new-api/不停服。
// 锁住两条闸:ResetDuePolicies(周期重置写绝对额度→可停服)与 ReconcileOrphans(禁用 new-api 用户)。
// 核心:observe 下有"待重置"策略也必须返回 0 且不 MarkPolicyReset(last_reset_at 仍 NULL);去掉闸→会被标记→本测试失败。
// 故意用 nil upstream:闸放行则永不触达上游;若闸回归,代码会走到上游写路径,以可见失败暴露(防静默回归)。
package integration

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/nexusapi-platform/enterprise/repo"
	"github.com/nexusapi-platform/enterprise/service"
)

func TestIntegration_ObserveQuotaWorkerGate(t *testing.T) {
	dsn := os.Getenv("NEXUS_IT_DSN")
	newapiSQLDSN := os.Getenv("NEXUS_IT_NEWAPI_SQL_DSN")
	if dsn == "" || newapiSQLDSN == "" {
		t.Skip("跳过:需 NEXUS_IT_DSN / NEXUS_IT_NEWAPI_SQL_DSN")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ensureDatabase(t, newapiSQLDSN, "nexus_gate")
	if !strings.Contains(dsn, "/nexus?") {
		t.Fatalf("NEXUS_IT_DSN 不含 /nexus?: %q", dsn)
	}
	store, err := repo.Open(ctx, strings.Replace(dsn, "/nexus?", "/nexus_gate?", 1))
	if err != nil {
		t.Fatalf("连 nexus_gate 失败: %v", err)
	}
	defer store.Close()
	defer func() { _, _ = store.DB().Exec("DROP DATABASE IF EXISTS nexus_gate") }()
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	// nil upstream:observe 闸放行则不触达;闸回归→走到 ManageUserQuota/SetUserStatus 会 panic 暴露。
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := service.New(service.Deps{Store: store, Logger: log, ObserveMode: true})

	const orgID = int64(1)
	db := store.DB()
	if _, err := db.ExecContext(ctx, `INSERT INTO organization (id, name, slug) VALUES (?, 'gate-org', 'gate-org-slug')`, orgID); err != nil {
		t.Fatalf("建组织失败: %v", err)
	}

	// 一条"待重置"的日策略(last_reset_at NULL → 跨当日 0 点边界 → 非 observe 下必触发重置+MarkPolicyReset)。
	if err := store.UpsertQuotaPolicy(ctx, &repo.QuotaPolicy{OrgID: orgID, Scope: "org", ScopeID: orgID, Period: "daily", LimitQuota: 1_000_000}); err != nil {
		t.Fatalf("建策略失败: %v", err)
	}

	// 一个 bootstrap_failed 孤儿成员(有 newapi_user_id):非 observe 下 ReconcileOrphans 会对其 SetUserStatus。
	if _, err := db.ExecContext(ctx,
		`INSERT INTO member (id, org_id, login_email, bootstrap_state, newapi_user_id) VALUES (1, ?, 'orphan@test.local', 'failed', 990001)`, orgID); err != nil {
		t.Fatalf("建孤儿成员失败: %v", err)
	}

	// B:ResetDuePolicies observe 下必须返回 0 且不标记重置。
	n, err := svc.ResetDuePolicies(ctx)
	if err != nil || n != 0 {
		t.Fatalf("observe 下 ResetDuePolicies 应 (0,nil),实 (%d,%v)", n, err)
	}
	var lastReset sql.NullTime
	if err := db.QueryRowContext(ctx, `SELECT last_reset_at FROM quota_policy WHERE org_id = ?`, orgID).Scan(&lastReset); err != nil {
		t.Fatalf("查 last_reset_at 失败: %v", err)
	}
	if lastReset.Valid {
		t.Fatalf("observe 闸失效:策略被 MarkPolicyReset(last_reset_at=%v)——闸已回归,周期重置会写 new-api 绝对额度可停服", lastReset.Time)
	}

	// C:ReconcileOrphans observe 下必须返回 0(不触达 new-api 禁用)。
	on, oerr := svc.ReconcileOrphans(ctx)
	if oerr != nil || on != 0 {
		t.Fatalf("observe 下 ReconcileOrphans 应 (0,nil),实 (%d,%v)", on, oerr)
	}

	t.Logf("observe 漏闸回归 ok: ResetDuePolicies/ReconcileOrphans 在观测下均短路返 0、不写 new-api(策略未被标记重置)")
}
