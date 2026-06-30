// 托管多桶/入账/续充真账测试(v2 R3,涉钱必真账):对真 newapi 验
//   ① 入账用 add 非 override(窗口=旧值+额,不是覆盖);② 守恒:已释放(窗口增量)+托管 = 总充值,绝不超拨;
//   ③ 窗口封顶 escrowWindowCap、溢出入托管;④ 续充把托管并入窗口(add)。
package integration

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/nexusapi-platform/enterprise/adapter/newapi"
	"github.com/nexusapi-platform/enterprise/pkg/session"
	"github.com/nexusapi-platform/enterprise/repo"
	"github.com/nexusapi-platform/enterprise/service"
)

// escrowSvc 起一套对真 MySQL(独立库 dbName)+ 真 newapi 的非 observe 服务(escrow 涉钱测试公共脚手架)。
func escrowSvc(t *testing.T, ctx context.Context, dbName string) (*service.Service, *repo.Store, newapi.NewapiAdapter) {
	t.Helper()
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
	svc := service.New(service.Deps{Store: store, Upstream: upstream, Keyring: keyring, Signer: signer, Logger: log})
	return svc, store, upstream
}

// F1/F2:并发入账无丢失更新 + 守恒 + 无 seq 撞 uk/越 cap(R5 修复:per-org 锁 + 单事务 FOR UPDATE)。
func TestIntegration_EscrowConcurrency(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	svc, store, upstream := escrowSvc(t, ctx, "nexus_escc")
	const orgID = int64(101) // 各 escrow 测试用不同 orgID:共享同一 newapi,同 orgID→同 org username→adopt 撞密码
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO organization (id, name, slug) VALUES (?, 'escc-org', 'escc-slug')`, orgID); err != nil {
		t.Fatalf("建组织失败: %v", err)
	}
	opc := session.Claims{Role: session.RoleOperator, OrgID: orgID}
	cred, err := svc.EnsureOrgProvisioned(ctx, orgID, "escc-org")
	if err != nil {
		t.Fatalf("开通失败: %v", err)
	}
	base, _ := upstream.GetUserQuota(ctx, cred.NewapiUserID) // 清零后应=0

	const N = 8
	const A = int64(100_000_000) // 8*A=8e8 < cap 2e9,全进窗口
	var wg sync.WaitGroup
	errs := make([]error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = svc.Recharge(ctx, opc, orgID, service.RechargeInput{AmountQuota: A, TransferNo: fmt.Sprintf("escc-%d", i)})
		}(i)
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Fatalf("并发入账#%d 失败(seq 撞 uk/锁?): %v", i, e)
		}
	}
	window, _ := upstream.GetUserQuota(ctx, cred.NewapiUserID)
	bal, _ := svc.GetDerivedBalance(ctx, opc, orgID)
	if window-base != int64(N)*A { // 无丢失更新:N 笔 add 全到账
		t.Fatalf("🔴并发入账丢失更新:窗口增量=%d 应=%d(N*A)——锁/事务失守", window-base, int64(N)*A)
	}
	released := window - base
	if released+bal.HoldingQuota != int64(N)*A {
		t.Fatalf("🔴守恒破:已释放%d+托管%d 应=%d", released, bal.HoldingQuota, int64(N)*A)
	}
	t.Logf("F1/F2 并发入账真账 ok: %d 笔并发无丢失更新(窗口=N*A=%d)+守恒,无 seq 撞 uk/越 cap", N, int64(N)*A)
}

// F3/F4:退款真减 newapi 窗口(非只减死账=双付)+ 对账自愈 newapi 写残窗(R5 修复)。
func TestIntegration_EscrowRefundReconcile(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	svc, store, upstream := escrowSvc(t, ctx, "nexus_escrr")
	const orgID = int64(102) // 不同 orgID 隔离 newapi org user(见 EscrowConcurrency 注释)
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO organization (id, name, slug) VALUES (?, 'escrr-org', 'escrr-slug')`, orgID); err != nil {
		t.Fatalf("建组织失败: %v", err)
	}
	opc := session.Claims{Role: session.RoleOperator, OrgID: orgID}
	cred, err := svc.EnsureOrgProvisioned(ctx, orgID, "escrr-org")
	if err != nil {
		t.Fatalf("开通失败: %v", err)
	}
	base, _ := upstream.GetUserQuota(ctx, cred.NewapiUserID)

	const A = int64(200_000_000)
	if _, err := svc.Recharge(ctx, opc, orgID, service.RechargeInput{AmountQuota: A, TransferNo: "rr-1"}); err != nil {
		t.Fatalf("入账失败: %v", err)
	}
	w1, _ := upstream.GetUserQuota(ctx, cred.NewapiUserID)

	// F3:退款 Z → newapi 窗口必须真减 Z(不是只减 company_balance 死账)。
	const Z = int64(50_000_000)
	if _, err := svc.DebitBalance(ctx, opc, orgID, service.DebitInput{AmountQuota: Z, Reason: "线下退款冲正"}); err != nil {
		t.Fatalf("退款失败: %v", err)
	}
	w2, _ := upstream.GetUserQuota(ctx, cred.NewapiUserID)
	if w1-w2 != Z {
		t.Fatalf("🔴F3 退款未真减 newapi 窗口(=双付):窗口 %d→%d 应减 %d", w1, w2, Z)
	}
	bal, _ := svc.GetDerivedBalance(ctx, opc, orgID)
	released := w2 - base
	if released+bal.HoldingQuota != A-Z {
		t.Fatalf("🔴退款后守恒破:已释放%d+托管%d 应=充值−退款%d", released, bal.HoldingQuota, A-Z)
	}

	// F4:模拟入账/续充/退款的 newapi 写失败残窗(手工 subtract D 不动 DB 桶1)→ 对账纠回。
	const D = int64(30_000_000)
	if err := upstream.ManageUserQuota(ctx, cred.NewapiUserID, newapi.QuotaSubtract, D); err != nil {
		t.Fatalf("模拟残窗失败: %v", err)
	}
	if err := svc.ReconcileEscrow(ctx); err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	wHealed, _ := upstream.GetUserQuota(ctx, cred.NewapiUserID)
	if wHealed != w2 {
		t.Fatalf("🔴F4 对账未自愈残窗:漂移后应纠回 %d,实=%d", w2, wHealed)
	}
	t.Logf("F3/F4 真账 ok: 退款真减 newapi 窗口(−%d)+守恒;对账自愈漂移窗口(−%d 纠回 %d)", Z, D, w2)
}

func TestIntegration_EscrowRecharge(t *testing.T) {
	dsn := os.Getenv("NEXUS_IT_DSN")
	newapiURL := os.Getenv("NEXUS_IT_NEWAPI_URL")
	newapiSQLDSN := os.Getenv("NEXUS_IT_NEWAPI_SQL_DSN")
	if dsn == "" || newapiURL == "" || newapiSQLDSN == "" {
		t.Skip("跳过:需 NEXUS_IT_DSN / NEXUS_IT_NEWAPI_URL / NEXUS_IT_NEWAPI_SQL_DSN")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	ensureDatabase(t, newapiSQLDSN, "nexus_escrow")
	if !strings.Contains(dsn, "/nexus?") {
		t.Fatalf("NEXUS_IT_DSN 不含 /nexus?: %q", dsn)
	}
	store, err := repo.Open(ctx, strings.Replace(dsn, "/nexus?", "/nexus_escrow?", 1))
	if err != nil {
		t.Fatalf("连 nexus_escrow 失败: %v", err)
	}
	defer store.Close()
	defer func() { _, _ = store.DB().Exec("DROP DATABASE IF EXISTS nexus_escrow") }()
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	adminToken, adminUID := setupRC4(t, newapiURL)
	keyring := mustKeyring(t)
	signer, _ := session.NewSigner([]byte("integration-test-session-key-32b!!"), time.Hour)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	upstream := newapi.New(newapi.Config{BaseURL: newapiURL, AdminToken: adminToken, AdminUserID: adminUID, Timeout: 15 * time.Second}, nil)
	svc := service.New(service.Deps{Store: store, Upstream: upstream, Keyring: keyring, Signer: signer, Logger: log})

	const orgID = int64(103) // 不同 orgID 隔离 newapi org user(见 EscrowConcurrency 注释)
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO organization (id, name, slug) VALUES (?, 'escrow-org', 'escrow-org-slug')`, orgID); err != nil {
		t.Fatalf("建组织失败: %v", err)
	}
	opc := session.Claims{Role: session.RoleOperator, OrgID: orgID}

	// 先开通 org user 拿基线窗口(newapi 建用户可能带默认额度,故用增量判定)。
	cred, err := svc.EnsureOrgProvisioned(ctx, orgID, "escrow-org")
	if err != nil {
		t.Fatalf("开通 org user 失败: %v", err)
	}
	base, err := upstream.GetUserQuota(ctx, cred.NewapiUserID)
	if err != nil {
		t.Fatalf("读基线窗口失败: %v", err)
	}

	const r1 = int64(1_000_000_000) // $2000 < 窗口上限 2e9
	if _, err := svc.Recharge(ctx, opc, orgID, service.RechargeInput{AmountQuota: r1, TransferNo: "esc-t1"}); err != nil {
		t.Fatalf("入账1失败: %v", err)
	}
	w1, _ := upstream.GetUserQuota(ctx, cred.NewapiUserID)
	if w1-base != r1 { // ① add 非 override:窗口 = 基线 + r1(增量)
		t.Fatalf("🔴 入账应 add 增量:窗口 base=%d → w1=%d,增量应=%d 实=%d(若=r1 不含base 则是 override)", base, w1, r1, w1-base)
	}
	bal1, _ := svc.GetDerivedBalance(ctx, opc, orgID)
	if bal1.HoldingQuota != 0 {
		t.Fatalf("小额入账不应有托管,实 holding=%d", bal1.HoldingQuota)
	}

	// 入账2:大额溢出窗口 → 桶1 填到上限、余下入托管。
	const r2 = int64(2_000_000_000)
	if _, err := svc.Recharge(ctx, opc, orgID, service.RechargeInput{AmountQuota: r2, TransferNo: "esc-t2"}); err != nil {
		t.Fatalf("入账2失败: %v", err)
	}
	w2, _ := upstream.GetUserQuota(ctx, cred.NewapiUserID)
	bal2, _ := svc.GetDerivedBalance(ctx, opc, orgID)
	if w2 > 2_000_000_000 { // ③ 窗口封顶
		t.Fatalf("🔴 窗口超上限 escrowWindowCap:w2=%d", w2)
	}
	// ② 守恒:已释放(窗口增量 w2-base)+ 托管 = 总充值 r1+r2,绝不超拨。
	released := w2 - base
	if released+bal2.HoldingQuota != r1+r2 {
		t.Fatalf("🔴 守恒破:已释放%d + 托管%d = %d 应=总充值%d", released, bal2.HoldingQuota, released+bal2.HoldingQuota, r1+r2)
	}
	if bal2.HoldingQuota <= 0 {
		t.Fatalf("大额入账溢出应入托管,实 holding=%d(窗口 w2=%d)", bal2.HoldingQuota, w2)
	}

	// ④ 续充:模拟窗口被消费(subtract)腾出空间,再把托管并入窗口(add)。
	const consume = int64(800_000_000)
	if err := upstream.ManageUserQuota(ctx, cred.NewapiUserID, newapi.QuotaSubtract, consume); err != nil {
		t.Fatalf("模拟消费失败: %v", err)
	}
	wBefore, _ := upstream.GetUserQuota(ctx, cred.NewapiUserID)
	holdBefore := bal2.HoldingQuota
	if _, err := svc.RefillWindow(ctx, opc, orgID); err != nil {
		t.Fatalf("续充失败: %v", err)
	}
	wAfter, _ := upstream.GetUserQuota(ctx, cred.NewapiUserID)
	balAfter, _ := svc.GetDerivedBalance(ctx, opc, orgID)
	merged := wAfter - wBefore
	if merged <= 0 {
		t.Fatalf("续充应把托管 add 进窗口:窗口 %d→%d", wBefore, wAfter)
	}
	if holdBefore-balAfter.HoldingQuota != merged { // 续充守恒:托管减少 = 窗口增加
		t.Fatalf("🔴 续充守恒破:托管减 %d 应=窗口增 %d", holdBefore-balAfter.HoldingQuota, merged)
	}
	if wAfter > 2_000_000_000 {
		t.Fatalf("🔴 续充后窗口超上限:%d", wAfter)
	}
	// 总账守恒不变:续充只是托管→窗口搬运,(窗口增量+托管)仍=总充值。
	if (wAfter-base+consume)+balAfter.HoldingQuota != r1+r2 {
		t.Fatalf("🔴 续充后总账守恒破:已释放%d+消费%d+托管%d 应=总充值%d", wAfter-base, consume, balAfter.HoldingQuota, r1+r2)
	}

	t.Logf("R3 托管/入账/续充真账 ok: add非override(w增量=r1) + 守恒(已释放+托管=总充值) + 窗口封顶 + 溢出入托管 + 续充并桶")
}
