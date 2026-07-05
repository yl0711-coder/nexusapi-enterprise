// 全平台深审 · A 档修复回归(A7 迁移重跑幂等 / A8 team_leader 配额读过滤)。真 MySQL。
package integration

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/nexusapi-platform/enterprise/pkg/session"
	"github.com/nexusapi-platform/enterprise/repo"
)

// TestIntegration_A7_MigrationResumeIdempotent:A7——多语句迁移中途被杀后重跑,已应用语句(列/键已存在)
// 被容忍视为已应用、迁移续成,不再永久卡死。模拟:删 0016 的 schema_migration 记录 → 重跑其 ADD COLUMN 撞 1060 → 应成功。
func TestIntegration_A7_MigrationResumeIdempotent(t *testing.T) {
	dsn := os.Getenv("NEXUS_IT_DSN")
	newapiSQLDSN := os.Getenv("NEXUS_IT_NEWAPI_SQL_DSN")
	if dsn == "" || newapiSQLDSN == "" {
		t.Skip("跳过:需 NEXUS_IT_DSN / NEXUS_IT_NEWAPI_SQL_DSN")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ensureDatabase(t, newapiSQLDSN, "nexus_a7")
	store, err := repo.Open(ctx, strings.Replace(dsn, "/nexus?", "/nexus_a7?", 1))
	if err != nil {
		t.Fatalf("连库失败: %v", err)
	}
	t.Cleanup(func() { _, _ = store.DB().Exec("DROP DATABASE IF EXISTS nexus_a7"); store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("首次迁移失败: %v", err)
	}
	// 模拟 0016 已应用但未记录(某语句后进程被杀,filename 没写入 schema_migration)。
	if _, err := store.DB().ExecContext(ctx, `DELETE FROM schema_migration WHERE filename = ?`, "0016_ledger_keyid.sql"); err != nil {
		t.Fatalf("删迁移记录失败: %v", err)
	}
	// 重跑:0016 的 ADD COLUMN 撞 1060(列已存在)——A7 容忍视为已应用 → 迁移应续成,不卡死。
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("A7 迁移重跑应容忍已应用语句续成,实失败: %v", err)
	}
	t.Logf("A7 ok: 删 0016 记录后重跑(ADD COLUMN 撞 1060)被容忍、迁移续成不卡死")
}

// TestIntegration_A8_TeamLeaderQuotaReadFilter:A8——team_leader 读配额策略只返本团队,org_admin 看全 org。
func TestIntegration_A8_TeamLeaderQuotaReadFilter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	svc, store, _ := v1Svc(t, ctx, "nexus_a8", true)
	const orgID, teamA, teamB = int64(1), int64(10), int64(20)
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO organization (id, name, slug) VALUES (?, 'a8-org', 'a8-slug')`, orgID); err != nil {
		t.Fatalf("建组织失败: %v", err)
	}
	for _, p := range []*repo.QuotaPolicy{
		{OrgID: orgID, Scope: "team", ScopeID: teamA, Period: "monthly", LimitQuota: 100},
		{OrgID: orgID, Scope: "team", ScopeID: teamB, Period: "monthly", LimitQuota: 200},
		{OrgID: orgID, Scope: "org", ScopeID: orgID, Period: "monthly", LimitQuota: 999},
	} {
		if err := store.UpsertQuotaPolicy(ctx, p); err != nil {
			t.Fatalf("建策略失败: %v", err)
		}
	}

	admin := session.Claims{Role: session.RoleOrgAdmin, OrgID: orgID}
	all, err := svc.ListQuotaPolicies(ctx, admin, orgID)
	if err != nil {
		t.Fatalf("org_admin 读失败: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("org_admin 应看到全部 3 条,实 %d", len(all))
	}

	tl := session.Claims{Role: session.RoleTeamLeader, OrgID: orgID, TeamID: teamA}
	mine, err := svc.ListQuotaPolicies(ctx, tl, orgID)
	if err != nil {
		t.Fatalf("team_leader 读失败: %v", err)
	}
	if len(mine) != 1 || mine[0].ScopeID != teamA {
		t.Fatalf("A8 team_leader 只应看到本团队(teamA=%d)策略,实 %d 条: %+v", teamA, len(mine), mine)
	}
	t.Logf("A8 ok: team_leader 读配额只返本团队(1 条,scope_id=%d);org_admin 看全 org(3 条)", teamA)
}
