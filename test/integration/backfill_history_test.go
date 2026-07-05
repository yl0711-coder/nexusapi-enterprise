// 历史日志回填 · happy-path 集成(交付批次24 步骤4,AC-2/AC-7 核心):
// 门B 关联型组织的全部历史消费日志(关联前、全在边界 B 下游)经回填 worker 全量、正确落进
// usage_ledger(聚合)+ usage_detail(逐条),状态 done、起点正确;回填只写报表两表,绝不扣钱。
// 边界(ts,id)词典序切分见纯函数单测 TestUnit_belongsToBackfill;此处验端到端全量正确 + 断点收敛。
package integration

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/nexusapi-platform/enterprise/repo"
)

func TestIntegration_BackfillHistoryHappyPath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	svc, store, _, _ := escrowSvc(t, ctx, "nexus_bfh")
	newapiSQLDSN := os.Getenv("NEXUS_IT_NEWAPI_SQL_DSN")

	const orgID, memberID = int64(821), int64(1)
	db := store.DB()
	if _, err := db.ExecContext(ctx, `INSERT INTO organization (id, name, slug) VALUES (?, 'bfh-org', 'bfh-slug')`, orgID); err != nil {
		t.Fatalf("建组织失败: %v", err)
	}
	cred, perr := svc.EnsureOrgProvisioned(ctx, orgID, "bfh-org")
	if perr != nil {
		t.Fatalf("开通失败: %v", perr)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO member (id, org_id, login_email, status, bootstrap_state) VALUES (?, ?, 'bfh@t.local', 'active', 'done')`,
		memberID, orgID); err != nil {
		t.Fatalf("建成员失败: %v", err)
	}
	tokenID := selfServeMemberToken(t, ctx, svc, store, orgID, memberID)

	// 项B:门A 组织 new-api 用户名是随机存库的 ent_<...>;回填按此 username 精确过滤,只拉本组织日志。
	var uname sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT newapi_username FROM organization WHERE id = ?`, orgID).Scan(&uname); err != nil {
		t.Fatalf("读 newapi_username 失败: %v", err)
	}
	if !uname.Valid || uname.String == "" {
		t.Fatalf("组织 newapi_username 应已落库(项B),实为空")
	}

	// 造 3 条"关联前"历史消费日志(全在边界下游),带本组织 username + user_id + tokenID。
	now := time.Now().Unix()
	seeds := []struct {
		ts, quota int64
	}{
		{now - 30000, 100000},
		{now - 20000, 200000},
		{now - 10000, 300000},
	}
	const wantSum = int64(600000)
	oldestTS := seeds[0].ts
	for _, s := range seeds {
		seedLogWithUsername(t, newapiSQLDSN, int64(cred.NewapiUserID), tokenID, uname.String, "gpt-bfh", s.quota, s.ts)
	}

	// 关联瞬间快照的边界 B:boundary_ts=now(晚于全部历史)、boundary_log_id=当前最大 log id
	// → 全部历史 (ts<now) 属回填侧。插 pending 任务(等价步骤5触发,此处手工插以单测 worker)。
	ndb, err := sql.Open("mysql", newapiSQLDSN)
	if err != nil {
		t.Fatalf("连 newapi 库失败: %v", err)
	}
	defer ndb.Close()
	var maxLogID int64
	_ = ndb.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM logs`).Scan(&maxLogID)
	if err := store.InsertBackfillJob(ctx, &repo.BackfillJob{
		OrgID: orgID, NewapiUserID: int64(cred.NewapiUserID), NewapiUsername: uname.String,
		BoundaryTS: now, BoundaryLogID: maxLogID, CursorTS: now,
	}); err != nil {
		t.Fatalf("插回填任务失败: %v", err)
	}

	// 跑回填分片直到 done(量小,通常一片即完;bounded 防死循环)。
	var job *repo.BackfillJob
	for i := 0; i < 10; i++ {
		if err := svc.RunBackfillSlice(ctx); err != nil {
			t.Fatalf("回填分片失败: %v", err)
		}
		job, err = store.GetBackfillJob(ctx, orgID)
		if err != nil {
			t.Fatalf("读任务失败: %v", err)
		}
		if job.Status == "done" {
			break
		}
	}
	if job == nil || job.Status != "done" {
		t.Fatalf("回填应收敛到 done,实=%+v", job)
	}

	// AC-2:ledger 全量精确 == 历史总消耗;detail 行数 == 历史条数。
	var ledgerSum int64
	_ = db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(consumed_quota),0) FROM usage_ledger WHERE org_id = ? AND model_name = 'gpt-bfh'`, orgID).Scan(&ledgerSum)
	if ledgerSum != wantSum {
		t.Fatalf("AC-2 回填 ledger 应精确 == 历史总量 %d,实 %d", wantSum, ledgerSum)
	}
	var detailRows int
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_detail WHERE org_id = ?`, orgID).Scan(&detailRows)
	if detailRows != len(seeds) {
		t.Fatalf("AC-7 usage_detail 行数应 == 历史条数 %d,实 %d", len(seeds), detailRows)
	}
	// 起点正确:earliest_seen_ts == 最老一条。
	if !job.EarliestSeenTS.Valid || job.EarliestSeenTS.Int64 != oldestTS {
		t.Fatalf("earliest_seen_ts 应 == 最老日志 ts=%d,实=%v", oldestTS, job.EarliestSeenTS)
	}
	if job.RowsIngested != int64(len(seeds)) {
		t.Fatalf("rows_ingested 应 == %d,实 %d", len(seeds), job.RowsIngested)
	}

	// AC-4 幂等重跑:重新回填再跑一遍,ledger 总量不变(不翻倍)、detail 无重复行。
	if err := store.RequeueBackfillJob(ctx, orgID); err != nil {
		t.Fatalf("重新回填失败: %v", err)
	}
	for i := 0; i < 10; i++ {
		if err := svc.RunBackfillSlice(ctx); err != nil {
			t.Fatalf("重跑回填分片失败: %v", err)
		}
		job, _ = store.GetBackfillJob(ctx, orgID)
		if job.Status == "done" {
			break
		}
	}
	var ledgerSum2 int64
	_ = db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(consumed_quota),0) FROM usage_ledger WHERE org_id = ? AND model_name = 'gpt-bfh'`, orgID).Scan(&ledgerSum2)
	var detailRows2 int
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_detail WHERE org_id = ?`, orgID).Scan(&detailRows2)
	if ledgerSum2 != wantSum || detailRows2 != len(seeds) {
		t.Fatalf("AC-4 幂等重跑不得翻倍:ledger %d->%d、detail %d->%d(应不变)", wantSum, ledgerSum2, len(seeds), detailRows2)
	}

	// AC-5(涉钱零影响):回填只写报表两表,绝不建/动 company_balance。
	var balRows int
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM company_balance WHERE org_id = ? AND total_consumed <> 0`, orgID).Scan(&balRows)
	if balRows != 0 {
		t.Fatalf("AC-5 回填绝不扣钱:company_balance.total_consumed 应恒 0,实有 %d 行非零", balRows)
	}

	t.Logf("回填 happy-path ok: 全量 ledger=%d(3 条)、detail=%d、起点=%d、重跑不翻倍、零扣钱", ledgerSum, detailRows, oldestTS)
}

// seedLogWithUsername 造一条带 username + token_id 的 type=2 消费日志(回填按 username 精确过滤,须带真实 username)。
func seedLogWithUsername(t *testing.T, dsn string, userID, tokenID int64, username, model string, quota, createdAt int64) {
	t.Helper()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("连 newapi 库失败: %v", err)
	}
	defer db.Close()
	_, err = db.Exec(
		"INSERT INTO logs (user_id, created_at, type, content, username, token_name, model_name, quota, "+
			"prompt_tokens, completion_tokens, use_time, is_stream, channel_id, channel_name, token_id, `group`, ip, request_id, other) "+
			"VALUES (?, ?, 2, '', ?, '', ?, ?, 100, 200, 1, 0, 0, '', ?, 'default', '', '', '')",
		userID, createdAt, username, model, quota, tokenID)
	if err != nil {
		t.Fatalf("造带 username 的消费日志失败: %v", err)
	}
}
