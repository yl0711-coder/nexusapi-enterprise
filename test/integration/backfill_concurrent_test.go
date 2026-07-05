// 历史回填 · 并发放量闸(交付批次24,AC-3/AC-11 + 守卫反证)。真 rc.4、不 mock。
//   Test A(守恒,正面):门B 关联(走 B_ts==0 强制基线守卫)→ 回填 + forward + 持续发新请求**三者真并发**,
//     历史日志含 600s rescan 重叠带;跑完对账 SUM(ledger)==SUM(detail)==new-api /api/log/stat 权威值(不自证)。
//   Test B(反证,确定性):把边界改回被否的 boundary_ts=now,复现 ledger 双算(forward 主窗口不查 detail 幂等)——
//     复现得出来,才证明"强制基线守卫"真在防它。
package integration

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/nexusapi-platform/enterprise/adapter/newapi"
	"github.com/nexusapi-platform/enterprise/pkg/session"
	"github.com/nexusapi-platform/enterprise/repo"
	"github.com/nexusapi-platform/enterprise/service"
)

// concSvc 同 v1Svc,但**只调一次 setupRC4** 并返回 admin token/uid——供对账直连 new-api /api/log/stat 复用同一 token
// (rc.4 的 GET /api/user/token 一调即旋转,二次 setupRC4 会作废 upstream 的 token,故不能各取各的)。
func concSvc(t *testing.T, ctx context.Context, dbName string, observe bool) (*service.Service, *repo.Store, newapi.NewapiAdapter, string, int) {
	t.Helper()
	dsn := os.Getenv("NEXUS_IT_DSN")
	newapiURL, newapiSQLDSN := mustNewapiEnv(t)
	if dsn == "" {
		t.Skip("跳过:需 NEXUS_IT_DSN")
	}
	ensureDatabase(t, newapiSQLDSN, dbName)
	store, err := repo.Open(ctx, strings.Replace(dsn, "/nexus?", "/"+dbName+"?", 1))
	if err != nil {
		t.Fatalf("连 %s 失败: %v", dbName, err)
	}
	t.Cleanup(func() { _, _ = store.DB().Exec("DROP DATABASE IF EXISTS " + dbName); store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO settlement_cursor (org_id, last_settled_ts, last_settled_log_id) VALUES (0, UNIX_TIMESTAMP(), 0)`); err != nil {
		t.Fatalf("置结算水位失败: %v", err)
	}
	adminToken, adminUID := setupRC4(t, newapiURL) // 只此一次,token 不再被二次 setupRC4 旋转作废
	signer, _ := session.NewSigner([]byte("integration-test-session-key-32b!!"), time.Hour)
	lg := slog.New(slog.NewTextHandler(io.Discard, nil))
	upstream := newapi.New(newapi.Config{BaseURL: newapiURL, AdminToken: adminToken, AdminUserID: adminUID, Timeout: 15 * time.Second}, nil)
	svc := service.New(service.Deps{Store: store, Upstream: upstream, Keyring: mustKeyring(t), Signer: signer, Logger: lg,
		ObserveMode: observe, FundingEnabled: false})
	return svc, store, upstream, adminToken, adminUID
}

// newapiUserStatQuota 用 new-api 官方 GET /api/log/stat 取该 username 的权威总消耗(SumUsedQuota,对账口径,不自证)。
func newapiUserStatQuota(t *testing.T, base, adminToken string, adminUID int, username string) int64 {
	t.Helper()
	u := fmt.Sprintf("%s/api/log/stat?type=2&username=%s&start_timestamp=0&end_timestamp=%d",
		base, url.QueryEscape(username), time.Now().Unix()+3600)
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("New-Api-User", strconv.Itoa(adminUID))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stat 请求失败: %v", err)
	}
	defer resp.Body.Close()
	var env struct {
		Data struct {
			Quota int64 `json:"quota"`
		} `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&env)
	return env.Data.Quota
}

func TestIntegration_BackfillForwardConcurrentConservation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	svc, store, upstream, adminToken, adminUID := concSvc(t, ctx, "nexus_bfconc", true) // observe:落账不扣钱,专验守恒
	newapiURL, newapiSQLDSN := os.Getenv("NEXUS_IT_NEWAPI_URL"), os.Getenv("NEXUS_IT_NEWAPI_SQL_DSN")
	db := store.DB()

	// AC-12 前置:清 cursor(未初始化)→ 关联触发走 B_ts==0 强制基线路径。
	if _, err := db.ExecContext(ctx, `DELETE FROM settlement_cursor WHERE org_id = 0`); err != nil {
		t.Fatalf("清 cursor 失败: %v", err)
	}

	// 企业用户 + 一枚令牌(历史与新日志都挂它;门B 关联后导入为成员)。
	entCred := mkEnterpriseUser(t, ctx, upstream, "concent")
	tokenID, err := upstream.CreateToken(ctx, entCred, newapi.TokenSpec{Name: "conc_tok", UnlimitedQuota: true, ExpiredTime: -1})
	if err != nil {
		t.Fatalf("造企业令牌失败: %v", err)
	}
	if err := upstream.AddOrgUsableGroup(ctx, "default", "vip"); err != nil {
		t.Fatalf("预配可用分组失败: %v", err)
	}
	uid := int64(entCred.NewapiUserID)

	// 历史日志(关联前,全在边界下游):含 600s rescan 重叠带([now-605,now-6])的三条 + 更老两条。
	now := time.Now().Unix()
	hist := []struct{ ts, q int64 }{
		{now - 5000, 100}, {now - 3000, 200}, // 纯回填区
		{now - 500, 400}, {now - 300, 800}, {now - 100, 1600}, // rescan 重叠带
	}
	var histSum int64
	for _, h := range hist {
		seedLogWithUsername(t, newapiSQLDSN, uid, int64(tokenID), "concent", "gpt-conc", h.q, h.ts)
		histSum += h.q
	}

	// 门B 关联 → enqueueBackfill 走强制基线守卫、插 pending 任务。
	opc := session.Claims{Role: session.RoleOperator}
	res, err := svc.CreateOrg(ctx, opc, service.CreateOrgInput{
		Name: "并发企业", Slug: "conc-ok", AdminEmail: "conc@t.local", NewapiUserGroup: "default",
		Associate: &service.AssociateOrgInput{NewapiUserID: int64(entCred.NewapiUserID), AccessToken: entCred.AccessToken, NamePolicy: "inherit"},
	})
	if err != nil {
		t.Fatalf("门B 关联失败: %v", err)
	}
	orgID := res.Org.ID

	// 并发相:forward 循环 + 回填循环 + 持续发新请求(ts=当前,>边界,归 forward),三者真并发抢 settlementMu。
	ndb, err := sql.Open("mysql", newapiSQLDSN)
	if err != nil {
		t.Fatalf("连 newapi 库失败: %v", err)
	}
	defer ndb.Close()
	var newSum int64
	seedNew := func(q int64) {
		_, e := ndb.Exec(
			"INSERT INTO logs (user_id, created_at, type, content, username, token_name, model_name, quota, prompt_tokens, completion_tokens, use_time, is_stream, channel_id, channel_name, token_id, `group`, ip, request_id, other) "+
				"VALUES (?, ?, 2, '', 'concent', '', 'gpt-conc', ?, 10, 20, 1, 0, 0, '', ?, 'default', '', '', '')",
			uid, time.Now().Unix(), q, tokenID)
		if e != nil {
			t.Errorf("发新日志失败: %v", e) // Errorf 可在非测试 goroutine 调用(FailNow 不可)
			return
		}
		atomic.AddInt64(&newSum, q)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() { defer wg.Done(); for { select { case <-stop: return; default: }; _, _ = svc.RunSettlement(ctx); time.Sleep(40 * time.Millisecond) } }()
	wg.Add(1)
	go func() { defer wg.Done(); for { select { case <-stop: return; default: }; _ = svc.RunBackfillSlice(ctx); time.Sleep(30 * time.Millisecond) } }()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 8; i++ {
			select {
			case <-stop:
				return
			default:
			}
			seedNew(10000)
			time.Sleep(150 * time.Millisecond)
		}
	}()
	time.Sleep(4 * time.Second) // 并发窗口
	close(stop)
	wg.Wait()

	// Drain:sleep 过滞后窗口(5s)使全部新日志可见,再把 forward + 回填跑到收敛,确保无残留。
	time.Sleep(6 * time.Second)
	for i := 0; i < 5; i++ {
		if _, e := svc.RunSettlement(ctx); e != nil {
			t.Fatalf("drain forward 失败: %v", e)
		}
		if e := svc.RunBackfillSlice(ctx); e != nil {
			t.Fatalf("drain 回填失败: %v", e)
		}
	}
	job, _ := store.GetBackfillJob(ctx, orgID)
	if job == nil || job.Status != "done" {
		t.Fatalf("回填应收敛 done,实=%+v", job)
	}

	// 对账(不自证):new-api stat 为权威;ledger 与 detail 都须精确等于它,不多(双算)不少(漏)。
	want := newapiUserStatQuota(t, newapiURL, adminToken, adminUID, "concent")
	expectSeeded := histSum + atomic.LoadInt64(&newSum)
	if want != expectSeeded {
		t.Fatalf("stat 权威值 %d != 实际种入 %d(历史 %d + 新 %d)——rc.4 未记全,测试前提破", want, expectSeeded, histSum, newSum)
	}
	var ledgerSum, detailSum int64
	_ = db.QueryRowContext(ctx, `SELECT COALESCE(SUM(consumed_quota),0) FROM usage_ledger WHERE org_id=?`, orgID).Scan(&ledgerSum)
	_ = db.QueryRowContext(ctx, `SELECT COALESCE(SUM(consumed_quota),0) FROM usage_detail WHERE org_id=?`, orgID).Scan(&detailSum)
	if ledgerSum != want {
		t.Fatalf("AC-3/AC-11 守恒破:SUM(ledger)=%d != new-api 权威 %d(多=双算/少=漏),并发下 settlementMu 未根治", ledgerSum, want)
	}
	if detailSum != want {
		t.Fatalf("AC-3/AC-11 守恒破:SUM(detail)=%d != new-api 权威 %d", detailSum, want)
	}
	t.Logf("并发守恒 ok: 回填+forward+发请求三者并发(含600s重叠带+B_ts==0强制基线),ledger==detail==stat==%d,不多不少", want)
}

// TestIntegration_GuardCounterProof_BoundaryNowDoubleCounts 守卫反证(确定性):
// 若把边界改回被否的 boundary_ts=now,回填吃 [now-lag,now),forward 基线后**主窗口**也吃这段
// (主窗口只按 id 水位去重、不查 detail 幂等),两边各 += 一次 → ledger 双算。复现得出来,证明强制基线守卫必要。
func TestIntegration_GuardCounterProof_BoundaryNowDoubleCounts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	svc, store, upstream, adminToken, adminUID := concSvc(t, ctx, "nexus_bfcp", true)
	newapiURL, newapiSQLDSN := os.Getenv("NEXUS_IT_NEWAPI_URL"), os.Getenv("NEXUS_IT_NEWAPI_SQL_DSN")
	db := store.DB()

	entCred := mkEnterpriseUser(t, ctx, upstream, "cpent")
	tokenID, err := upstream.CreateToken(ctx, entCred, newapi.TokenSpec{Name: "cp_tok", UnlimitedQuota: true, ExpiredTime: -1})
	if err != nil {
		t.Fatalf("造令牌失败: %v", err)
	}
	if err := upstream.AddOrgUsableGroup(ctx, "default", "vip"); err != nil {
		t.Fatalf("预配分组失败: %v", err)
	}
	uid := int64(entCred.NewapiUserID)
	res, err := svc.CreateOrg(ctx, opcOperator(), service.CreateOrgInput{
		Name: "反证企业", Slug: "cp-ok", AdminEmail: "cp@t.local", NewapiUserGroup: "default",
		Associate: &service.AssociateOrgInput{NewapiUserID: int64(entCred.NewapiUserID), AccessToken: entCred.AccessToken, NamePolicy: "inherit"},
	})
	if err != nil {
		t.Fatalf("关联失败: %v", err)
	}
	orgID := res.Org.ID

	// 造 3 条落在 [now-lag, now) 的日志(会被"boundary_ts=now 回填"与"forward 主窗口"同时吃)。
	now := time.Now().Unix()
	var seeded int64
	for _, ts := range []int64{now - 4, now - 3, now - 2} {
		seedLogWithUsername(t, newapiSQLDSN, uid, int64(tokenID), "cpent", "gpt-cp", 1000, ts)
		seeded += 1000
	}
	var maxLogID int64
	ndb, _ := sql.Open("mysql", newapiSQLDSN)
	defer ndb.Close()
	_ = ndb.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM logs`).Scan(&maxLogID)

	// 把 forward 水位置于这些日志之前(id 水位低于它们、时间窗覆盖)——模拟 forward 尚未处理这段。
	if _, err := db.ExecContext(ctx, `INSERT INTO settlement_cursor (org_id,last_settled_ts,last_settled_log_id) VALUES (0,?,?) ON DUPLICATE KEY UPDATE last_settled_ts=VALUES(last_settled_ts), last_settled_log_id=VALUES(last_settled_log_id)`,
		now-600, maxLogID-3); err != nil {
		t.Fatalf("置 cursor 失败: %v", err)
	}
	// 被否的边界:boundary_ts=now(直接插任务,绕过强制基线守卫)。回填吃 [.., now)=全部这三条。
	if _, err := db.ExecContext(ctx, `UPDATE org_backfill_job SET boundary_ts=?, boundary_log_id=?, cursor_ts=?, status='pending' WHERE org_id=?`,
		now, maxLogID, now, orgID); err != nil {
		t.Fatalf("改边界为 now 失败: %v", err)
	}

	// 先回填(落这三条到 ledger+detail),再等过滞后窗口后跑 forward(主窗口再吃一次)。
	for i := 0; i < 5; i++ {
		if e := svc.RunBackfillSlice(ctx); e != nil {
			t.Fatalf("回填失败: %v", e)
		}
		if j, _ := store.GetBackfillJob(ctx, orgID); j.Status == "done" {
			break
		}
	}
	time.Sleep(6 * time.Second) // 过滞后窗口,使 forward 主窗口能处理 now-4..now-2
	if _, e := svc.RunSettlement(ctx); e != nil {
		t.Fatalf("forward 失败: %v", e)
	}

	want := newapiUserStatQuota(t, newapiURL, adminToken, adminUID, "cpent")
	if want != seeded {
		t.Fatalf("stat 权威值 %d != 种入 %d,前提破", want, seeded)
	}
	var ledgerSum, detailSum int64
	_ = db.QueryRowContext(ctx, `SELECT COALESCE(SUM(consumed_quota),0) FROM usage_ledger WHERE org_id=?`, orgID).Scan(&ledgerSum)
	_ = db.QueryRowContext(ctx, `SELECT COALESCE(SUM(consumed_quota),0) FROM usage_detail WHERE org_id=?`, orgID).Scan(&detailSum)
	// 反证成立的标志:detail 幂等仍=权威(3000),但 ledger 被回填+forward主窗口各算一次=双算(>权威)。
	if detailSum != want {
		t.Fatalf("detail 应=权威 %d(唯一键幂等),实 %d", want, detailSum)
	}
	if ledgerSum <= want {
		t.Fatalf("反证失败:boundary_ts=now 本应复现 ledger 双算(>权威 %d),实 ledger=%d ——说明重叠没发生,反证前提不成立", want, ledgerSum)
	}
	t.Logf("守卫反证成立: boundary_ts=now 下 ledger=%d 双算(>权威 %d),detail=%d(幂等未双)——证明强制基线守卫必要,主窗口确不查 detail 幂等", ledgerSum, want, detailSum)
}

func opcOperator() session.Claims { return session.Claims{Role: session.RoleOperator} }

// mustNewapiEnv 取 rc.4 URL 与 newapi 库 DSN(缺则 skip)。
func mustNewapiEnv(t *testing.T) (string, string) {
	t.Helper()
	apiURL, sqlDSN := os.Getenv("NEXUS_IT_NEWAPI_URL"), os.Getenv("NEXUS_IT_NEWAPI_SQL_DSN")
	if apiURL == "" || sqlDSN == "" {
		t.Skip("跳过:需 NEXUS_IT_NEWAPI_URL / NEXUS_IT_NEWAPI_SQL_DSN")
	}
	return apiURL, sqlDSN
}
