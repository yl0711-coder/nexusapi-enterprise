// 归因测试(v2 M0-S2):验证结算把日志按 token_id 映射回平台稳定 key_id,写进 usage_ledger.key_id。
// 重点验"轮换不断历史":同一 key 槽轮换出 T1(旧/superseded)→ T2(新/current),两条 token 的消费日志
// 必须都归到同一个 key_id(聚合到同一 ledger 行)。隔离做法:独立库 nexus_attr + 游标水位精确框住只处理本测试日志。
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

func TestIntegration_KeyIDAttribution(t *testing.T) {
	dsn := os.Getenv("NEXUS_IT_DSN")
	newapiURL := os.Getenv("NEXUS_IT_NEWAPI_URL")
	newapiSQLDSN := os.Getenv("NEXUS_IT_NEWAPI_SQL_DSN")
	if dsn == "" || newapiURL == "" || newapiSQLDSN == "" {
		t.Skip("跳过:需 NEXUS_IT_DSN / NEXUS_IT_NEWAPI_URL / NEXUS_IT_NEWAPI_SQL_DSN")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// 1) 独立库 + observe 服务(落账不扣钱,不依赖 company_balance)。
	// 用 newapi 库连接建 nexus_attr(本测试按文件名序最先跑,此时 nexus 库尚未被主 e2e 创建;
	// newapi 库由 mysql 容器初始化即存在,可靠)。
	ensureDatabase(t, newapiSQLDSN, "nexus_attr")
	if !strings.Contains(dsn, "/nexus?") {
		t.Fatalf("NEXUS_IT_DSN 不含 /nexus? 无法派生隔离库 DSN: %q", dsn)
	}
	attrDSN := strings.Replace(dsn, "/nexus?", "/nexus_attr?", 1)
	store, err := repo.Open(ctx, attrDSN)
	if err != nil {
		t.Fatalf("连 nexus_attr 失败: %v", err)
	}
	defer store.Close()
	defer func() { _, _ = store.DB().Exec("DROP DATABASE IF EXISTS nexus_attr") }()
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
	svc := service.New(service.Deps{Store: store, Upstream: upstream, Keyring: keyring, Signer: signer, Logger: log, ObserveMode: true})

	// 2) 直接用真实 repo 接口造 member + 轮换的两条 token 行(走 FinalizeBootstrap/UpdateMemberKey 真实写链)。
	const orgID, memberID, userID = int64(1), int64(1), int64(90001)
	const tok1, tok2, tok3 = int64(90011), int64(90012), int64(90013)
	db := store.DB()
	if _, err := db.ExecContext(ctx, `INSERT INTO organization (id, name, slug) VALUES (?, 'attr-org', 'attr-org-slug')`, orgID); err != nil {
		t.Fatalf("建组织失败: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO member (id, org_id, login_email) VALUES (?, ?, 'attr@test.local')`, memberID, orgID); err != nil {
		t.Fatalf("建成员失败: %v", err)
	}
	masked1, masked2 := "sk-aaaa...v001", "sk-bbbb...v002"
	t1 := tok1
	m1 := &model.Member{ID: memberID, OrgID: orgID, NewapiUserID: userID, NewapiTokenID: &t1, KeyMasked: &masked1, KeyRotation: 1}
	if err := store.FinalizeBootstrap(ctx, m1, "nexus_m1_v1"); err != nil {
		t.Fatalf("FinalizeBootstrap(建主 key)失败: %v", err)
	}
	// 轮换:旧 token(T1)置 superseded,新 token(T2)成 current,同一 key 槽。
	if err := store.UpdateMemberKey(ctx, orgID, memberID, tok2, masked2, "nexus_m1_v2", 2); err != nil {
		t.Fatalf("UpdateMemberKey(轮换)失败: %v", err)
	}
	var wantKeyID int64
	if err := db.QueryRowContext(ctx, `SELECT id FROM member_key_slot WHERE member_id = ? AND is_primary = 1`, memberID).Scan(&wantKeyID); err != nil {
		t.Fatalf("查主 key 槽失败: %v", err)
	}
	// 真 1:N:再建一把独立 key(新槽 slot2,令牌 T3),其消费应归到不同 key_id。
	key2, err := store.CreateAdditionalKey(ctx, orgID, memberID, tok3, "nexus_m1_k2_v1", "sk-cccc...v003", 1)
	if err != nil {
		t.Fatalf("建第二把 key 失败: %v", err)
	}
	if key2 == wantKeyID || key2 == 0 {
		t.Fatalf("第二把 key 应有不同的非零 key_id, 实=%d(主槽=%d)", key2, wantKeyID)
	}

	// 3) 用游标水位精确框住:只处理本测试之后新造的日志(id > 当前最大 log id),避开共享 newapi 库里其它测试的日志。
	ndb, err := sql.Open("mysql", newapiSQLDSN)
	if err != nil {
		t.Fatalf("连 newapi 库失败: %v", err)
	}
	defer ndb.Close()
	var maxLogID int64
	_ = ndb.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM logs`).Scan(&maxLogID)

	logTS := time.Now().Unix() - 10 // 早于结算滞后窗口(5s),确保落入结算窗口
	if _, err := db.ExecContext(ctx,
		`INSERT INTO settlement_cursor (org_id, last_settled_ts, last_settled_log_id) VALUES (0, ?, ?)`,
		logTS-1, maxLogID); err != nil {
		t.Fatalf("置结算游标失败: %v", err)
	}

	// 4) 造消费日志:T1(旧)+T2(新)属主槽(应聚合到同一 key_id);T3 属第二把 key(应归到 key2)。
	seedConsumptionLogTok(t, newapiSQLDSN, userID, tok1, "gpt-attr", 111000, logTS)
	seedConsumptionLogTok(t, newapiSQLDSN, userID, tok2, "gpt-attr", 222000, logTS)
	seedConsumptionLogTok(t, newapiSQLDSN, userID, tok3, "gpt-attr", 444000, logTS)

	// 5) 跑结算(observe:落账不扣钱)。
	if _, err := svc.RunSettlement(ctx); err != nil {
		t.Fatalf("结算失败: %v", err)
	}

	// 6) 断言:按 key_id 聚合应得两行——主槽(T1+T2=333000)、第二把 key(T3=444000),各自 key_id。
	rows, err := db.QueryContext(ctx,
		`SELECT key_id, consumed_quota FROM usage_ledger WHERE org_id = ? AND newapi_user_id = ? AND model_name = 'gpt-attr'`,
		orgID, userID)
	if err != nil {
		t.Fatalf("查 ledger 失败: %v", err)
	}
	defer rows.Close()
	byKey := map[int64]int64{}
	for rows.Next() {
		var k, q int64
		if err := rows.Scan(&k, &q); err != nil {
			t.Fatalf("扫 ledger 失败: %v", err)
		}
		byKey[k] = q
	}
	if len(byKey) != 2 {
		t.Fatalf("应得 2 个 key_id 各一行(主槽聚合 + 第二把 key),实得 %d: %v", len(byKey), byKey)
	}
	if byKey[wantKeyID] != 333000 {
		t.Fatalf("主槽 key_id=%d 应聚合 T1+T2=333000(轮换前后归同一 key),实 %d", wantKeyID, byKey[wantKeyID])
	}
	if byKey[key2] != 444000 {
		t.Fatalf("第二把 key key_id=%d 应=T3=444000(1:N 独立归因),实 %d", key2, byKey[key2])
	}
	if _, ok := byKey[0]; ok {
		t.Fatalf("出现 key_id=0 未归因行,token_id→key_id 映射有漏: %v", byKey)
	}
	t.Logf("M0-S2/1:N 归因 ok: 主槽 key_id=%d 聚合轮换前后 T1+T2=333000;第二把 key key_id=%d 独立归因 T3=444000", wantKeyID, key2)
}

// seedConsumptionLogTok 造一条带 token_id 的 type=2 消费日志(归因测试用)。
func seedConsumptionLogTok(t *testing.T, dsn string, userID, tokenID int64, model string, quota, createdAt int64) {
	t.Helper()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("连 newapi 库失败: %v", err)
	}
	defer db.Close()
	_, err = db.Exec(
		"INSERT INTO logs (user_id, created_at, type, content, username, token_name, model_name, quota, "+
			"prompt_tokens, completion_tokens, use_time, is_stream, channel_id, channel_name, token_id, `group`, ip, request_id, other) "+
			"VALUES (?, ?, 2, '', '', '', ?, ?, 100, 200, 1, 0, 0, '', ?, 'default', '', '', '')",
		userID, createdAt, model, quota, tokenID)
	if err != nil {
		t.Fatalf("造带 token 的消费日志失败: %v", err)
	}
}
