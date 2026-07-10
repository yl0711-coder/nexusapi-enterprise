// 45号返工补测:完整日志镜像切片(RunLogMirrorSlice)真栈覆盖。
// 动机:45号-16 删门B backfill LEFT JOIN 时,ListLogMirrorSources 残留 b.newapi_username
// 引用,SQL 直接 Unknown column——集成栈此前对镜像链路零覆盖,CI 全绿生产(dev 栈)炸,
// 与 P1-1/P1-2 同属"测试没走真代码路径"缝。本测钉死:源 SQL 可执行 + 上游按 username
// 拉取 + 落镜像表 + 游标推进,全链真 MySQL + 真 rc.4。
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
	"github.com/nexusapi-platform/enterprise/adapter/newapi"
	"github.com/nexusapi-platform/enterprise/pkg/session"
	"github.com/nexusapi-platform/enterprise/repo"
	"github.com/nexusapi-platform/enterprise/service"
)

func TestIntegration_LogMirrorSlice(t *testing.T) {
	dsn := os.Getenv("NEXUS_IT_DSN")
	newapiURL := os.Getenv("NEXUS_IT_NEWAPI_URL")
	newapiSQLDSN := os.Getenv("NEXUS_IT_NEWAPI_SQL_DSN")
	if dsn == "" || newapiURL == "" || newapiSQLDSN == "" {
		t.Skip("跳过:需 NEXUS_IT_DSN / NEXUS_IT_NEWAPI_URL / NEXUS_IT_NEWAPI_SQL_DSN")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	ensureDatabase(t, newapiSQLDSN, "nexus_lmirror")
	if !strings.Contains(dsn, "/nexus?") {
		t.Fatalf("NEXUS_IT_DSN 不含 /nexus? 无法派生隔离库 DSN: %q", dsn)
	}
	mirrorDSN := strings.Replace(dsn, "/nexus?", "/nexus_lmirror?", 1)
	store, err := repo.Open(ctx, mirrorDSN)
	if err != nil {
		t.Fatalf("连 nexus_lmirror 失败: %v", err)
	}
	defer store.Close()
	defer func() { _, _ = store.DB().Exec("DROP DATABASE IF EXISTS nexus_lmirror") }()
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	adminToken, adminUID := setupRC4(t, newapiURL)
	signer, err := session.NewSigner([]byte("integration-test-session-key-32b!!"), time.Hour)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	upstream := newapi.New(newapi.Config{BaseURL: newapiURL, AdminToken: adminToken, AdminUserID: adminUID, Timeout: 15 * time.Second}, nil)
	svc := service.New(service.Deps{Store: store, Upstream: upstream, Keyring: mustKeyring(t), Signer: signer, Logger: log})

	// 组织=金库 user(独有 username,天然与共享 newapi 库其它测试日志隔离)。
	const orgID, treasuryUID = int64(1), int64(97001)
	const treasuryUser = "lmirror_org1"
	db := store.DB()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO organization (id, name, slug, newapi_user_id, newapi_username) VALUES (?, 'lmirror-org', 'lmirror-slug', ?, ?)`,
		orgID, treasuryUID, treasuryUser); err != nil {
		t.Fatalf("建组织失败: %v", err)
	}

	// 回归钉:源 SQL 必须可执行且含本组织(45号-16 曾残留 b.newapi_username → Unknown column)。
	srcs, err := store.ListLogMirrorSources(ctx, 20)
	if err != nil {
		t.Fatalf("🔴ListLogMirrorSources SQL 执行失败(45号-16 类残留回归): %v", err)
	}
	found := false
	for _, s := range srcs {
		if s.OrgID == orgID && s.Username == treasuryUser {
			found = true
		}
	}
	if !found {
		t.Fatalf("镜像源应含本组织(username=%s), 实=%v", treasuryUser, srcs)
	}

	// 造带 username 的消费日志(镜像按 username 拉;落在滞后窗口之前)。
	logTS := time.Now().Unix() - 120
	seedNamedLog(t, newapiSQLDSN, treasuryUID, treasuryUser, "gpt-lmirror", 111000, logTS)
	seedNamedLog(t, newapiSQLDSN, treasuryUID, treasuryUser, "gpt-lmirror", 222000, logTS+1)

	if err := svc.RunLogMirrorSlice(ctx); err != nil {
		t.Fatalf("RunLogMirrorSlice 失败: %v", err)
	}

	var rows int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM org_newapi_log WHERE org_id = ? AND model_name = 'gpt-lmirror'`, orgID).Scan(&rows); err != nil {
		t.Fatalf("查镜像表失败: %v", err)
	}
	if rows != 2 {
		t.Fatalf("🔴镜像表应落 2 条本组织日志, 实=%d(上游拉取或落库断裂)", rows)
	}
	var curTS int64
	if err := db.QueryRowContext(ctx,
		`SELECT cursor_ts FROM org_newapi_log_cursor WHERE org_id = ?`, orgID).Scan(&curTS); err != nil {
		t.Fatalf("查镜像游标失败: %v", err)
	}
	if curTS <= 0 {
		t.Fatalf("🔴镜像游标应推进(>0), 实=%d", curTS)
	}
	t.Logf("45号补测 log-mirror ok: 源SQL可执行+按username拉取2条落镜像+游标推进到%d", curTS)
}

// seedNamedLog 造带 username 的 type=2 消费日志(镜像链路按 username 拉取,必须非空)。
func seedNamedLog(t *testing.T, dsn string, userID int64, username, model string, quota, createdAt int64) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("连 newapi 库失败: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(
		"INSERT INTO logs (user_id, created_at, type, content, username, token_name, model_name, quota, "+
			"prompt_tokens, completion_tokens, use_time, is_stream, channel_id, channel_name, token_id, `group`, ip, request_id, other) "+
			"VALUES (?, ?, 2, '', ?, '', ?, ?, 100, 200, 1, 0, 0, '', 0, 'default', '', '', '')",
		userID, createdAt, username, model, quota); err != nil {
		t.Fatalf("造带名消费日志失败: %v", err)
	}
}
