// commitUsageOnly 抽取回归 · 三态钱路径 golden(交付批次「历史日志全量回填」步骤3,文档24-§4.4)。
//
// 背景:回填与 forward 结算共用"只写报表"落账段,故把 settlement.go:287-299(ledger 桶 + usage_detail)
// 抽成 commitUsageOnly(ctx,tx,aggs,details)。扣余额(DeductBalanceTx)+ 推全局水位(AdvanceCursorTx)
// 留在 RunSettlement 事务后半段、forward 独有。本测试锁死抽取"逐字节不变":对固定日志集,三个状态下
//   - 落账(ledger 精确值 + detail 行数)与推水位(精确 log_id)**三态完全一致**(证 commitUsageOnly + 推水位没被动坏);
//   - 扣余额与"扣费审计"只在 observe=false 且该组织 billing_enabled=true 时发生(证钱门仍在 commitUsageOnly 之外)。
//
// 三态(302-320 的 observeMode + per-org billing_enabled 两道门):
//  1. observe=true                      → 不扣;ledger+detail+推水位照常。
//  2. observe=false + billing_enabled=0 → 也不扣(v1 裁定B,观测期主力路径,最易漏,单列一态);落账+推水位照常。
//  3. observe=false + billing_enabled=1 → 扣;并触发扣费审计(postBal 驱动的提交后副作用)。
//
// 解耦:setup(开通/建 token/充值)一律走非 observe svc(可靠路径);仅 RunSettlement 那一下换成待测状态的 svc。
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

type goldenObs struct {
	ledger      int64 // SUM(usage_ledger.consumed_quota) 本 org+model
	detailRows  int   // usage_detail 本 org 行数
	consumed    int64 // company_balance.total_consumed
	balance     int64 // company_balance.balance
	cursorTS    int64
	cursorLogID int64
	seedLogID   int64
	deductAudit int // audit_log action=settlement_deduct 本 org 计数
}

func TestIntegration_CommitUsageOnlyThreeStateGolden(t *testing.T) {
	const A = int64(50_000_000) // 充值
	const C = int64(20_000_000) // 单条消费

	// 每态独立库 + 独立 orgID,虚拟机水位各自框窗口,互不干扰。
	s1 := runGoldenState(t, "nexus_golden1", 811, true, false, A, C)  // observe
	s2 := runGoldenState(t, "nexus_golden2", 812, false, false, A, C) // live + billing off(观测期主力,最易漏)
	s3 := runGoldenState(t, "nexus_golden3", 813, false, true, A, C)  // live + billing on

	// (a) 落账 ledger 精确值:三态完全一致 == C。
	for name, o := range map[string]goldenObs{"observe": s1, "live+billoff": s2, "live+billon": s3} {
		if o.ledger != C {
			t.Errorf("[%s] ledger 应精确 == C=%d(commitUsageOnly 落账三态须一致),实=%d", name, C, o.ledger)
		}
		if o.detailRows != 1 {
			t.Errorf("[%s] usage_detail 应恰 1 行,实=%d", name, o.detailRows)
		}
		// (c) 推水位:精确推到 seed 日志的 log_id;ts 已越过日志时间。
		if o.cursorLogID != o.seedLogID {
			t.Errorf("[%s] 水位 log_id 应精确 == seedLogID=%d,实=%d", name, o.seedLogID, o.cursorLogID)
		}
	}

	// (b) 扣余额:态1/2 为 0(balance 原样 == A),态3 精确扣 C(balance == A−C)。
	if s1.consumed != 0 || s1.balance != A {
		t.Errorf("态1(observe)不得扣钱:应 consumed=0 balance=A=%d,实 consumed=%d balance=%d", A, s1.consumed, s1.balance)
	}
	if s2.consumed != 0 || s2.balance != A {
		t.Errorf("态2(live+billing off)不得扣钱(v1 裁定B):应 consumed=0 balance=A=%d,实 consumed=%d balance=%d", A, s2.consumed, s2.balance)
	}
	if s3.consumed != C || s3.balance != A-C {
		t.Errorf("态3(live+billing on)应精确扣 C=%d:balance 应==A−C=%d,实 consumed=%d balance=%d", C, A-C, s3.consumed, s3.balance)
	}

	// (d) 提交后副作用(扣费审计,postBal 驱动)只在态3触发 → 证 postBal 生成段没被 commitUsageOnly 带走。
	if s1.deductAudit != 0 || s2.deductAudit != 0 {
		t.Errorf("态1/2 不得有 settlement_deduct 审计(postBal 应为空),实 s1=%d s2=%d", s1.deductAudit, s2.deductAudit)
	}
	if s3.deductAudit != 1 {
		t.Errorf("态3 应有恰 1 条 settlement_deduct 审计(postBal 驱动的提交后副作用照常),实=%d", s3.deductAudit)
	}
}

// runGoldenState 跑单个状态:setup 全走非 observe svc;仅 RunSettlement 用 settleObserve 决定的 svc。
func runGoldenState(t *testing.T, dbName string, orgID int64, settleObserve, billing bool, A, C int64) goldenObs {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	dsn := os.Getenv("NEXUS_IT_DSN")
	newapiURL := os.Getenv("NEXUS_IT_NEWAPI_URL")
	newapiSQLDSN := os.Getenv("NEXUS_IT_NEWAPI_SQL_DSN")
	if dsn == "" || newapiURL == "" || newapiSQLDSN == "" {
		t.Skip("跳过:需 NEXUS_IT_DSN / NEXUS_IT_NEWAPI_URL / NEXUS_IT_NEWAPI_SQL_DSN")
	}
	ensureDatabase(t, newapiSQLDSN, dbName)
	if !strings.Contains(dsn, "/nexus?") {
		t.Fatalf("NEXUS_IT_DSN 不含 /nexus?: %q", dsn)
	}
	store, err := repo.Open(ctx, strings.Replace(dsn, "/nexus?", "/"+dbName+"?", 1))
	if err != nil {
		t.Fatalf("连 %s 失败: %v", dbName, err)
	}
	t.Cleanup(func() { _, _ = store.DB().Exec("DROP DATABASE IF EXISTS " + dbName); store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	adminToken, adminUID := setupRC4(t, newapiURL)
	keyring := mustKeyring(t)
	signer, _ := session.NewSigner([]byte("integration-test-session-key-32b!!"), time.Hour)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	upstream := newapi.New(newapi.Config{BaseURL: newapiURL, AdminToken: adminToken, AdminUserID: adminUID, Timeout: 15 * time.Second}, nil)
	deps := service.Deps{Store: store, Upstream: upstream, Keyring: keyring, Signer: signer, Logger: log, FundingEnabled: true}
	svcLive := service.New(deps)     // setup + 态2/3 结算
	depsObs := deps                  // 同依赖,仅观测位不同
	depsObs.ObserveMode = true       //
	svcObserve := service.New(depsObs) // 态1 结算

	db := store.DB()
	be := 0
	if billing {
		be = 1
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO organization (id, name, slug, billing_enabled) VALUES (?, ?, ?, ?)`,
		orgID, dbName+"-org", dbName+"-slug", be); err != nil {
		t.Fatalf("建组织失败: %v", err)
	}

	const memberID = int64(1)
	cred, perr := svcLive.EnsureOrgProvisioned(ctx, orgID, dbName+"-org")
	if perr != nil {
		t.Fatalf("开通失败: %v", perr)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO member (id, org_id, login_email, status, bootstrap_state) VALUES (?, ?, ?, 'active', 'done')`,
		memberID, orgID, dbName+"@t.local"); err != nil {
		t.Fatalf("建成员失败: %v", err)
	}
	tokenID := selfServeMemberToken(t, ctx, svcLive, store, orgID, memberID)

	opc := session.Claims{Role: session.RoleOperator, OrgID: orgID}
	if _, err := svcLive.Recharge(ctx, opc, orgID, service.RechargeInput{AmountQuota: A, TransferNo: "g-1"}); err != nil {
		t.Fatalf("充值失败: %v", err)
	}

	// 框结算水位:since = logTS−1,只处理本态刚造的这条日志(id 高于此前 MAX)。
	ndb, err := sql.Open("mysql", newapiSQLDSN)
	if err != nil {
		t.Fatalf("连 newapi 库失败: %v", err)
	}
	defer ndb.Close()
	var maxBefore int64
	_ = ndb.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM logs`).Scan(&maxBefore)
	logTS := time.Now().Unix() - 300 // 安全早于结算滞后窗口(5s)
	if _, err := db.ExecContext(ctx,
		`INSERT INTO settlement_cursor (org_id, last_settled_ts, last_settled_log_id) VALUES (0, ?, ?)
		   ON DUPLICATE KEY UPDATE last_settled_ts = VALUES(last_settled_ts), last_settled_log_id = VALUES(last_settled_log_id)`,
		logTS-1, maxBefore); err != nil {
		t.Fatalf("置结算水位失败: %v", err)
	}
	seedConsumptionLogTok(t, newapiSQLDSN, int64(cred.NewapiUserID), tokenID, "gpt-golden", C, logTS)
	var seedLogID int64
	_ = ndb.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM logs`).Scan(&seedLogID)
	if seedLogID <= maxBefore {
		t.Fatalf("seed 日志未落库:maxBefore=%d seedLogID=%d", maxBefore, seedLogID)
	}

	settleSvc := svcLive
	if settleObserve {
		settleSvc = svcObserve
	}
	if _, err := settleSvc.RunSettlement(ctx); err != nil {
		t.Fatalf("结算失败: %v", err)
	}

	var o goldenObs
	o.seedLogID = seedLogID
	_ = db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(consumed_quota),0) FROM usage_ledger WHERE org_id = ? AND model_name = 'gpt-golden'`, orgID).Scan(&o.ledger)
	_ = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM usage_detail WHERE org_id = ?`, orgID).Scan(&o.detailRows)
	_ = db.QueryRowContext(ctx,
		`SELECT COALESCE(total_consumed,0), COALESCE(balance,0) FROM company_balance WHERE org_id = ?`, orgID).Scan(&o.consumed, &o.balance)
	_ = db.QueryRowContext(ctx,
		`SELECT COALESCE(last_settled_ts,0), COALESCE(last_settled_log_id,0) FROM settlement_cursor WHERE org_id = 0`).Scan(&o.cursorTS, &o.cursorLogID)
	_ = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM audit_log WHERE org_id = ? AND action = 'settlement_deduct'`, orgID).Scan(&o.deductAudit)

	if o.cursorTS < logTS {
		t.Errorf("[%s] 水位 ts 应越过日志时间 logTS=%d,实=%d", dbName, logTS, o.cursorTS)
	}
	return o
}
