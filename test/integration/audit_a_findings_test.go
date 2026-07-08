// 全平台深审 · A 档修复回归(A7 迁移重跑幂等 / A8 team_leader 配额读过滤)。真 MySQL。
package integration

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
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

// TestIntegration_A8_TeamLeaderQuotaReadFilter 已随配额策略机器退役删除(33 §12-4)。
