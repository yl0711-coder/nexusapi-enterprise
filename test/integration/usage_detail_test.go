// 明细落库测试(v2 M2-2):验证结算把每条 new-api 日志落成 usage_detail 一行(下钻读本库),
// 含 key_id 归因 + prompt/completion token 指标;幂等(同 newapi_log_id 只落一次);90 天保留清理。
// 隔离库 nexus_det,observe 服务(落账不扣钱),游标水位框住只处理本测试日志。
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
	"github.com/nexusapi-platform/enterprise/model"
	"github.com/nexusapi-platform/enterprise/pkg/session"
	"github.com/nexusapi-platform/enterprise/repo"
	"github.com/nexusapi-platform/enterprise/service"
)

func TestIntegration_UsageDetail(t *testing.T) {
	dsn := os.Getenv("NEXUS_IT_DSN")
	newapiURL := os.Getenv("NEXUS_IT_NEWAPI_URL")
	newapiSQLDSN := os.Getenv("NEXUS_IT_NEWAPI_SQL_DSN")
	if dsn == "" || newapiURL == "" || newapiSQLDSN == "" {
		t.Skip("跳过:需 NEXUS_IT_DSN / NEXUS_IT_NEWAPI_URL / NEXUS_IT_NEWAPI_SQL_DSN")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	ensureDatabase(t, newapiSQLDSN, "nexus_det")
	if !strings.Contains(dsn, "/nexus?") {
		t.Fatalf("NEXUS_IT_DSN 不含 /nexus?: %q", dsn)
	}
	store, err := repo.Open(ctx, strings.Replace(dsn, "/nexus?", "/nexus_det?", 1))
	if err != nil {
		t.Fatalf("连 nexus_det 失败: %v", err)
	}
	defer store.Close()
	defer func() { _, _ = store.DB().Exec("DROP DATABASE IF EXISTS nexus_det") }()
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	adminToken, adminUID := setupRC4(t, newapiURL)
	keyring := mustKeyring(t)
	signer, err := session.NewSigner([]byte("integration-test-session-key-32b!!"), time.Hour)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	upstream := newapi.New(newapi.Config{BaseURL: newapiURL, AdminToken: adminToken, AdminUserID: adminUID, Timeout: 15 * time.Second}, nil)
	svc := service.New(service.Deps{Store: store, Upstream: upstream, Keyring: keyring, Signer: signer, Logger: log})

	const orgID, memberID, userID, tok1 = int64(1), int64(1), int64(91001), int64(91011)
	db := store.DB()
	if _, err := db.ExecContext(ctx, `INSERT INTO organization (id, name, slug) VALUES (?, 'det-org', 'det-org-slug')`, orgID); err != nil {
		t.Fatalf("建组织失败: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO member (id, org_id, login_email) VALUES (?, ?, 'det@test.local')`, memberID, orgID); err != nil {
		t.Fatalf("建成员失败: %v", err)
	}
	masked := "sk-dddd...v001"
	t1 := tok1
	m1 := &model.Member{ID: memberID, OrgID: orgID, NewapiTokenID: &t1, KeyMasked: &masked, KeyRotation: 1} // 模型2:归因走 token
	if err := store.FinalizeBootstrap(ctx, m1, "nexus_m1_v1"); err != nil {
		t.Fatalf("FinalizeBootstrap 失败: %v", err)
	}
	var wantKeyID int64
	if err := db.QueryRowContext(ctx, `SELECT id FROM member_key_slot WHERE member_id = ? AND is_primary = 1`, memberID).Scan(&wantKeyID); err != nil {
		t.Fatalf("查主 key 槽失败: %v", err)
	}

	// 游标水位框住:只处理本测试新造日志。
	ndb, err := sql.Open("mysql", newapiSQLDSN)
	if err != nil {
		t.Fatalf("连 newapi 库失败: %v", err)
	}
	defer ndb.Close()
	var maxLogID int64
	_ = ndb.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM logs`).Scan(&maxLogID)
	logTS := time.Now().Unix() - 10
	if _, err := db.ExecContext(ctx,
		`INSERT INTO settlement_cursor (org_id, last_settled_ts, last_settled_log_id) VALUES (0, ?, ?)`,
		logTS-1, maxLogID); err != nil {
		t.Fatalf("置结算游标失败: %v", err)
	}

	// 造 3 条消费日志(同 user/token,模型 gpt-det,quota 1000/2000/3000)。seedConsumptionLogTok 固定 prompt=100/completion=200。
	seedConsumptionLogTok(t, newapiSQLDSN, userID, tok1, "gpt-det", 1000, logTS)
	seedConsumptionLogTok(t, newapiSQLDSN, userID, tok1, "gpt-det", 2000, logTS)
	seedConsumptionLogTok(t, newapiSQLDSN, userID, tok1, "gpt-det", 3000, logTS)

	if _, err := svc.RunSettlement(ctx); err != nil {
		t.Fatalf("结算失败: %v", err)
	}

	// 1) usage_detail 应有 3 行,每行 key_id=主槽、prompt=100/completion=200、quota 各异、log_id 互异。
	rows, err := db.QueryContext(ctx,
		`SELECT key_id, prompt_tokens, completion_tokens, consumed_quota, newapi_log_id FROM usage_detail
		  WHERE org_id=? AND newapi_user_id=? AND model_name='gpt-det' ORDER BY consumed_quota`, orgID, userID)
	if err != nil {
		t.Fatalf("查 usage_detail 失败: %v", err)
	}
	var quotas []int64
	logIDs := map[int64]bool{}
	var sumQ int64
	for rows.Next() {
		var keyID, pt, ct, q, lid int64
		if err := rows.Scan(&keyID, &pt, &ct, &q, &lid); err != nil {
			rows.Close()
			t.Fatalf("扫 detail 失败: %v", err)
		}
		if keyID != wantKeyID {
			rows.Close()
			t.Fatalf("明细 key_id 应=%d(主槽归因),实=%d", wantKeyID, keyID)
		}
		if pt != 100 || ct != 200 {
			rows.Close()
			t.Fatalf("明细 token 指标应 prompt=100/completion=200,实=%d/%d", pt, ct)
		}
		quotas = append(quotas, q)
		logIDs[lid] = true
		sumQ += q
	}
	rows.Close()
	if len(quotas) != 3 || sumQ != 6000 {
		t.Fatalf("明细应 3 行合计 6000,实 %d 行合计 %d: %v", len(quotas), sumQ, quotas)
	}
	if len(logIDs) != 3 {
		t.Fatalf("明细 newapi_log_id 应 3 个互异,实 %d", len(logIDs))
	}

	// 2) 幂等:同一 newapi_log_id 重复落两次(模拟重叠窗口/重试)→ 只 1 行。
	dupRow := repo.DetailRow{OrgID: orgID, MemberID: memberID, NewapiUserID: userID, KeyID: wantKeyID, ModelName: "dup", NewapiLogID: 999999, ConsumedQuota: 42, LogTS: time.Now().UTC()}
	for i := 0; i < 2; i++ {
		if err := store.WithTx(ctx, func(tx *sql.Tx) error { return store.InsertUsageDetailTx(ctx, tx, []repo.DetailRow{dupRow}) }); err != nil {
			t.Fatalf("幂等插入失败: %v", err)
		}
	}
	var dupCnt int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_detail WHERE newapi_log_id=999999`).Scan(&dupCnt); err != nil {
		t.Fatalf("查重复行失败: %v", err)
	}
	if dupCnt != 1 {
		t.Fatalf("幂等失败:同 newapi_log_id 应 1 行,实 %d", dupCnt)
	}

	// 3) 保留期可配(24-§6 / AC-8):插一条 100 天前明细。默认保留期=0 → 不清理(永久保留);设 N=90 才清 N 天前。
	oldRow := repo.DetailRow{OrgID: orgID, MemberID: memberID, NewapiUserID: userID, KeyID: wantKeyID, ModelName: "old", NewapiLogID: 888888, ConsumedQuota: 7, LogTS: time.Now().Add(-100 * 24 * time.Hour).UTC()}
	if err := store.WithTx(ctx, func(tx *sql.Tx) error { return store.InsertUsageDetailTx(ctx, tx, []repo.DetailRow{oldRow}) }); err != nil {
		t.Fatalf("插旧明细失败: %v", err)
	}
	// 3a) 默认 svc(retention=0):PurgeOldUsageDetail 应为 no-op,100 天前的行仍在(全历史回填随时可查的前提)。
	if err := svc.PurgeOldUsageDetail(ctx); err != nil {
		t.Fatalf("默认清理失败: %v", err)
	}
	var oldCnt0 int
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_detail WHERE newapi_log_id=888888`).Scan(&oldCnt0)
	if oldCnt0 != 1 {
		t.Fatalf("AC-8 默认保留期=0 应不清理,100 天前明细应仍在,实 %d 行", oldCnt0)
	}
	// 3b) retention=90 的 svc(同一 store):清 100 天前、近 3 行保留。
	svcPurge := service.New(service.Deps{Store: store, Upstream: upstream, Keyring: keyring, Signer: signer, Logger: log, UsageDetailRetentionDays: 90})
	if err := svcPurge.PurgeOldUsageDetail(ctx); err != nil {
		t.Fatalf("配置清理失败: %v", err)
	}
	var oldCnt, recentCnt int
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_detail WHERE newapi_log_id=888888`).Scan(&oldCnt)
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_detail WHERE org_id=? AND model_name='gpt-det'`, orgID).Scan(&recentCnt)
	if oldCnt != 0 {
		t.Fatalf("AC-8 retention=90 应清 100 天前,实仍 %d 行", oldCnt)
	}
	if recentCnt != 3 {
		t.Fatalf("近期明细应保留 3 行,实 %d", recentCnt)
	}

	t.Logf("M2-2 明细落库 ok: 结算落 3 行逐条(key_id=%d 归因/token 100·200/合计 6000);幂等同 log_id 1 行;AC-8 保留期默认0不清、配90清旧留新", wantKeyID)
}
