// 回填测试(v2 M0-S1):验证迁移 0015 把"升级前已存在的成员"正确回填成 member_key_slot +
// member_key_token。主 e2e 是空库迁移后再建成员(回填空操作),覆盖不到升级回填这条最该测的路径
// (「有用户后再改最贵」)。本测试在独立库 nexus_backfill 上:全量迁移 → 插入预存成员 →
// **重跑 0015 真实 SQL**(CREATE TABLE IF NOT EXISTS 幂等 + INSERT...SELECT 回填) → 断言。
package integration

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/nexusapi-platform/enterprise/migrations"
	"github.com/nexusapi-platform/enterprise/repo"
)

func TestIntegration_MigrationBackfill_KeySlot(t *testing.T) {
	dsn := os.Getenv("NEXUS_IT_DSN")
	if dsn == "" {
		t.Skip("跳过:未设 NEXUS_IT_DSN")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// 1) 独立库,避免污染主 e2e 的 nexus 库。
	ensureDatabase(t, dsn, "nexus_backfill")
	if !strings.Contains(dsn, "/nexus?") {
		t.Fatalf("NEXUS_IT_DSN 不含 /nexus? 无法派生回填库 DSN: %q", dsn)
	}
	bfDSN := strings.Replace(dsn, "/nexus?", "/nexus_backfill?", 1)

	store, err := repo.Open(ctx, bfDSN)
	if err != nil {
		t.Fatalf("连回填库失败: %v", err)
	}
	defer store.Close()
	db := store.DB()
	defer func() { _, _ = db.Exec("DROP DATABASE IF EXISTS nexus_backfill") }()

	// 2) 全量迁移(空库 → 建全部表,0015 回填空操作)。
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	// 3) 插入"升级前已存在"的成员:一个有令牌(应被回填)、一个无令牌(平台账号,不应被回填)。
	const tokenID int64 = 777
	const rotation = 2
	const masked = "sk-aaaa...wxyz"
	res, err := db.ExecContext(ctx,
		`INSERT INTO member (org_id, login_email, newapi_token_id, key_masked, key_rotation, status, bootstrap_state)
		 VALUES (1, 'backfill-keyed@test.local', ?, ?, ?, 'active', 'done')`, tokenID, masked, rotation)
	if err != nil {
		t.Fatalf("插入有令牌成员失败: %v", err)
	}
	keyedID, _ := res.LastInsertId()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO member (org_id, login_email, newapi_token_id, status, bootstrap_state, role)
		 VALUES (1, 'backfill-noklet@test.local', NULL, 'active', 'done', 'org_admin')`); err != nil {
		t.Fatalf("插入无令牌成员失败: %v", err)
	}

	// 4) 重跑 0015 真实 SQL 复现升级回填(读迁移文件,按语句切分执行)。
	raw, err := migrations.FS.ReadFile("0015_member_key_1n.sql")
	if err != nil {
		t.Fatalf("读 0015 迁移失败: %v", err)
	}
	for _, stmt := range splitMigrationSQL(string(raw)) {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("执行 0015 语句失败: %v\n语句: %.120s", err, stmt)
		}
	}

	// 5) 断言:有令牌成员被回填成 1 主槽 + 1 current 令牌,且字段全对。
	var slotID int64
	var isPrimary bool
	var slotStatus string
	if err := db.QueryRowContext(ctx,
		`SELECT id, is_primary, status FROM member_key_slot WHERE member_id = ?`, keyedID).
		Scan(&slotID, &isPrimary, &slotStatus); err != nil {
		t.Fatalf("查回填 slot 失败(应有 1 行): %v", err)
	}
	if !isPrimary || slotStatus != "active" {
		t.Fatalf("slot 字段不符: is_primary=%v status=%q(应 true/active)", isPrimary, slotStatus)
	}

	var gotKeyID, gotTokenID int64
	var gotName, gotMasked, gotStatus string
	var gotCurrent bool
	var gotRotation int
	if err := db.QueryRowContext(ctx,
		`SELECT key_id, newapi_token_id, token_name, key_masked, is_current, rotation, status
		   FROM member_key_token WHERE member_id = ?`, keyedID).
		Scan(&gotKeyID, &gotTokenID, &gotName, &gotMasked, &gotCurrent, &gotRotation, &gotStatus); err != nil {
		t.Fatalf("查回填 token 失败(应有 1 行): %v", err)
	}
	// token_name 必须与 deriveTokenName 的 nexus_m%d_v%d 完全一致(归因映射键,改命名须同步本断言)。
	wantName := fmt.Sprintf("nexus_m%d_v%d", keyedID, rotation)
	switch {
	case gotKeyID != slotID:
		t.Fatalf("token.key_id=%d 应指向 slot.id=%d", gotKeyID, slotID)
	case gotTokenID != tokenID:
		t.Fatalf("token.newapi_token_id=%d 应=%d", gotTokenID, tokenID)
	case gotName != wantName:
		t.Fatalf("token_name=%q 应=%q", gotName, wantName)
	case gotMasked != masked:
		t.Fatalf("key_masked=%q 应=%q", gotMasked, masked)
	case !gotCurrent:
		t.Fatalf("is_current 应为 true")
	case gotRotation != rotation:
		t.Fatalf("rotation=%d 应=%d", gotRotation, rotation)
	case gotStatus != "active":
		t.Fatalf("status=%q 应=active", gotStatus)
	}

	// 6) 断言:无令牌成员(平台账号)不被回填(slot=0 行)。
	var noktSlots int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM member_key_slot s JOIN member m ON m.id = s.member_id
		  WHERE m.login_email = 'backfill-noklet@test.local'`).Scan(&noktSlots); err != nil {
		t.Fatalf("查无令牌成员 slot 数失败: %v", err)
	}
	if noktSlots != 0 {
		t.Fatalf("无令牌(NULL token)成员不应被回填,实得 %d 个 slot", noktSlots)
	}

	// 7) 总量自洽:slot 数 == token 数 == 有令牌成员数(本测试=1)。
	var slots, toks int
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM member_key_slot`).Scan(&slots)
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM member_key_token`).Scan(&toks)
	if slots != 1 || toks != 1 {
		t.Fatalf("回填总量不符: slots=%d tokens=%d(应各 1)", slots, toks)
	}

	t.Logf("回填 ok: 有令牌成员→1 主槽+1 current 令牌(key_id=%d,token_name=%s),无令牌账号不回填", slotID, wantName)
}

// splitMigrationSQL 复刻 repo.splitSQL 的切分口径(逐行剥 -- 注释 + 按 ; 分割),供测试重跑迁移文件。
// 与生产迁移执行同口径,确保测的就是真实落库行为。
func splitMigrationSQL(s string) []string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	var out []string
	for _, part := range strings.Split(b.String(), ";") {
		if strings.TrimSpace(part) != "" {
			out = append(out, part)
		}
	}
	return out
}

// queryMemberKey 查成员 member_key 状态(M1 1:N 归因验证,供主 e2e 在开通/轮换点断言):
// 返回 槽数 / 令牌行数(含历史) / current 令牌数 / current 令牌名。
func queryMemberKey(t *testing.T, db *sql.DB, orgID, memberID int64) (slots, tokens, currents int, currentName string) {
	t.Helper()
	if err := db.QueryRow(`SELECT COUNT(*) FROM member_key_slot WHERE org_id=? AND member_id=?`, orgID, memberID).Scan(&slots); err != nil {
		t.Fatalf("查 key 槽数失败: %v", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM member_key_token WHERE org_id=? AND member_id=?`, orgID, memberID).Scan(&tokens); err != nil {
		t.Fatalf("查 key 令牌数失败: %v", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*), COALESCE(MAX(token_name),'') FROM member_key_token
	                        WHERE org_id=? AND member_id=? AND is_current=1`, orgID, memberID).Scan(&currents, &currentName); err != nil {
		t.Fatalf("查 current 令牌失败: %v", err)
	}
	return
}
