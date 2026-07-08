// 架构B 阶段1(BE②)对账环精确补齐 · 对抗注入(33 §11-3 / 34 §5.2 / 设计 §2.6 八窗):
// 对真 MySQL + rc.4,用可编程故障 upstream 在五步序每个崩溃窗注入,逐窗断言三件事:
//   1) 收敛后金库+成员双端真值守恒(不多发不少发,精确到 raw);
//   2) 账本终态正确(applied/failed 与钱面一致);
//   3) 修复动作不重复(再跑 reconcile 结果不变)。
// 另覆盖:money_freeze 只报不修 / 恒等式抓违规直充 / 非 leader no-op / partial 退回 / int32 不可达退回。
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

	"github.com/nexusapi-platform/enterprise/adapter/newapi"
	"github.com/nexusapi-platform/enterprise/pkg/session"
	"github.com/nexusapi-platform/enterprise/repo"
	"github.com/nexusapi-platform/enterprise/service"
)

// faultUpstream 可编程故障注入(单发触发,触发后自动复位——放行对账环的修复调用)。
//   - decMode:  "fail"=不发出直接报错(W1) / "lost"=真扣成功但响应丢失(W2)
//   - decPartial: >0 时真扣该值并如实返回(模拟 clamp 部分扣,partial_debit 路径)
//   - incMode:  "fail"=不发出直接报错(W3) / "lost"=真加成功但响应丢失(W4)
type faultUpstream struct {
	newapi.NewapiAdapter
	mu         sync.Mutex
	decMode    string
	decPartial int64
	incMode    string
}

func (f *faultUpstream) DecreaseUserQuota(ctx context.Context, userID int, amountRaw int64) (int64, error) {
	f.mu.Lock()
	mode, partial := f.decMode, f.decPartial
	f.decMode, f.decPartial = "", 0
	f.mu.Unlock()
	switch {
	case mode == "fail":
		return 0, fmt.Errorf("注入W1:出账未发出即失败")
	case mode == "lost":
		if _, err := f.NewapiAdapter.DecreaseUserQuota(ctx, userID, amountRaw); err != nil {
			return 0, err
		}
		return 0, fmt.Errorf("注入W2:出账已落地但响应丢失")
	case partial > 0:
		return f.NewapiAdapter.DecreaseUserQuota(ctx, userID, partial)
	}
	return f.NewapiAdapter.DecreaseUserQuota(ctx, userID, amountRaw)
}

func (f *faultUpstream) IncreaseUserQuota(ctx context.Context, userID int, amountRaw int64) error {
	f.mu.Lock()
	mode := f.incMode
	f.incMode = ""
	f.mu.Unlock()
	switch mode {
	case "fail":
		return fmt.Errorf("注入W3:入账未发出即失败")
	case "lost":
		if err := f.NewapiAdapter.IncreaseUserQuota(ctx, userID, amountRaw); err != nil {
			return err
		}
		return fmt.Errorf("注入W4:入账已落地但响应丢失")
	}
	return f.NewapiAdapter.IncreaseUserQuota(ctx, userID, amountRaw)
}

// ledgerSvc 起一套架构B 钱核心测试栈:真 MySQL + rc.4 + 故障注入 upstream。
func ledgerSvc(t *testing.T, ctx context.Context, dbName string, leader bool) (*service.Service, *repo.Store, newapi.NewapiAdapter, *faultUpstream) {
	t.Helper()
	dsn := os.Getenv("NEXUS_IT_DSN")
	newapiURL := os.Getenv("NEXUS_IT_NEWAPI_URL")
	newapiSQLDSN := os.Getenv("NEXUS_IT_NEWAPI_SQL_DSN")
	if dsn == "" || newapiURL == "" || newapiSQLDSN == "" {
		t.Skip("跳过:需 NEXUS_IT_DSN / NEXUS_IT_NEWAPI_URL / NEXUS_IT_NEWAPI_SQL_DSN")
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
	adminToken, adminUID := setupRC4(t, newapiURL)
	signer, _ := session.NewSigner([]byte("integration-test-session-key-32b!!"), time.Hour)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	real := newapi.New(newapi.Config{BaseURL: newapiURL, AdminToken: adminToken, AdminUserID: adminUID, Timeout: 15 * time.Second}, nil)
	fault := &faultUpstream{NewapiAdapter: real}
	svc := service.New(service.Deps{Store: store, Upstream: fault, Keyring: mustKeyring(t), Signer: signer, Logger: log,
		FundingEnabled: false, Leadership: service.NewEnvLeadership(leader)})
	return svc, store, real, fault
}

// mkLedgerOrg 建组织(金库=真 new-api user,记 newapi_username 供对账环日志取证)并代充金库。
func mkLedgerOrg(t *testing.T, ctx context.Context, store *repo.Store, real newapi.NewapiAdapter, orgID int64, name string, fundRaw int64) int {
	t.Helper()
	cred := mkEnterpriseUser(t, ctx, real, name)
	mustExec(t, ctx, store,
		`INSERT INTO organization (id, name, slug, newapi_user_id, newapi_username) VALUES (?, ?, ?, ?, ?)`,
		orgID, name, name, cred.NewapiUserID, name)
	if fundRaw > 0 {
		if err := real.IncreaseUserQuota(ctx, cred.NewapiUserID, fundRaw); err != nil {
			t.Fatalf("金库代充失败: %v", err)
		}
	}
	return cred.NewapiUserID
}

// mkLedgerMember 建成员(真 new-api user + member 行,username 供恒等式/日志取证)。
func mkLedgerMember(t *testing.T, ctx context.Context, store *repo.Store, real newapi.NewapiAdapter, orgID, memberID int64, name string) int {
	t.Helper()
	cred := mkEnterpriseUser(t, ctx, real, name)
	mustExec(t, ctx, store,
		`INSERT INTO member (id, org_id, login_email, status, bootstrap_state, newapi_user_id, newapi_username)
		 VALUES (?, ?, ?, 'active', 'done', ?, ?)`,
		memberID, orgID, name+"@t.local", cred.NewapiUserID, name)
	return cred.NewapiUserID
}

// backdateLedger 把账本行回拨 3 分钟,使其进入对账环的滞留判定窗(2 分钟阈值)。
func backdateLedger(t *testing.T, ctx context.Context, store *repo.Store, idemKey string) {
	t.Helper()
	mustExec(t, ctx, store,
		`UPDATE ledger_transfer SET created_at = DATE_SUB(created_at, INTERVAL 3 MINUTE) WHERE idempotency_key = ?`, idemKey)
}

// ledgerRow 读账本行终态。
func ledgerRow(t *testing.T, ctx context.Context, store *repo.Store, idemKey string) (status, phase, failReason string, debited int64) {
	t.Helper()
	if err := store.DB().QueryRowContext(ctx,
		`SELECT status, phase, fail_reason, debited_raw FROM ledger_transfer WHERE idempotency_key = ?`, idemKey).
		Scan(&status, &phase, &failReason, &debited); err != nil {
		t.Fatalf("读账本行 %s 失败: %v", idemKey, err)
	}
	return
}

// mustQuota 断言 new-api user 真值(fromDB)。
func mustQuota(t *testing.T, ctx context.Context, real newapi.NewapiAdapter, uid int, want int64, what string) {
	t.Helper()
	got, err := real.GetUserQuota(ctx, uid)
	if err != nil {
		t.Fatalf("读 %s quota 失败: %v", what, err)
	}
	if got != want {
		t.Fatalf("🔴%s 守恒破: got %d want %d", what, got, want)
	}
}

// reconcileTwice 跑两轮对账环并断言第二轮无新修复动作(幂等收敛)。返回首轮 drift。
func reconcileTwice(t *testing.T, ctx context.Context, svc *service.Service) []service.Drift {
	t.Helper()
	d1, err := svc.ReconcileTransfers(ctx)
	if err != nil {
		t.Fatalf("对账环第一轮失败: %v", err)
	}
	if _, err := svc.ReconcileTransfers(ctx); err != nil {
		t.Fatalf("对账环第二轮失败: %v", err)
	}
	return d1
}

const amt = int64(2_000_000) // $4

// TestIntegration_LedgerW1_DebitNeverSent W1(②后③前/③未发出):recorded 行 → 判未落地 → failed 关单,金库分文未动。
func TestIntegration_LedgerW1_DebitNeverSent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	svc, store, real, fault := ledgerSvc(t, ctx, "nexus_lw1", true)
	tre := mkLedgerOrg(t, ctx, store, real, 9101, "lw1-tre", 10_000_000)
	mem := mkLedgerMember(t, ctx, store, real, 9101, 811, "lw1-mem")

	fault.decMode = "fail"
	if err := svc.Transfer(ctx, 9101, tre, mem, 811, amt, "topup", "lw1-a", "test"); err == nil {
		t.Fatal("注入后应报错")
	}
	if st, ph, _, _ := ledgerRow(t, ctx, store, "lw1-a"); st != "pending" || ph != "recorded" {
		t.Fatalf("应留 pending/recorded,得 %s/%s", st, ph)
	}
	backdateLedger(t, ctx, store, "lw1-a")
	reconcileTwice(t, ctx, svc)
	st, _, fr, _ := ledgerRow(t, ctx, store, "lw1-a")
	if st != "failed" || fr != "debit_not_landed" {
		t.Fatalf("🔴应判 failed/debit_not_landed,得 %s/%s", st, fr)
	}
	mustQuota(t, ctx, real, tre, 10_000_000, "金库")
	mustQuota(t, ctx, real, mem, 0, "成员")
	t.Log("W1 ok: 出账未发出 → 关单,双端分文未动")
}

// TestIntegration_LedgerW2_DebitLostResponse W2(③落地未记 J1):日志主证据判已扣 → roll-forward 补入账 → applied,守恒精确。
func TestIntegration_LedgerW2_DebitLostResponse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	svc, store, real, fault := ledgerSvc(t, ctx, "nexus_lw2", true)
	tre := mkLedgerOrg(t, ctx, store, real, 9102, "lw2-tre", 10_000_000)
	mem := mkLedgerMember(t, ctx, store, real, 9102, 812, "lw2-mem")

	fault.decMode = "lost"
	if err := svc.Transfer(ctx, 9102, tre, mem, 812, amt, "topup", "lw2-a", "test"); err == nil {
		t.Fatal("注入后应报错")
	}
	mustQuota(t, ctx, real, tre, 8_000_000, "金库(已扣)")
	if st, ph, _, _ := ledgerRow(t, ctx, store, "lw2-a"); st != "pending" || ph != "recorded" {
		t.Fatalf("应留 pending/recorded,得 %s/%s", st, ph)
	}
	backdateLedger(t, ctx, store, "lw2-a")
	reconcileTwice(t, ctx, svc)
	st, ph, _, deb := ledgerRow(t, ctx, store, "lw2-a")
	if st != "applied" || ph != "credited" || deb != amt {
		t.Fatalf("🔴应 roll-forward 至 applied/credited/deb=%d,得 %s/%s/%d", amt, st, ph, deb)
	}
	mustQuota(t, ctx, real, tre, 8_000_000, "金库")
	mustQuota(t, ctx, real, mem, amt, "成员(补齐一次,绝非两次)")
	t.Log("W2 ok: 出账已落地未记 → 日志证据 roll-forward,精确补齐入账")
}

// TestIntegration_LedgerW3_CreditNeverSent W3(③后④前):debited 行 → 日志判未入账 + 恒等式共同确认 → 补入账(唯一自动补发点)。
func TestIntegration_LedgerW3_CreditNeverSent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	svc, store, real, fault := ledgerSvc(t, ctx, "nexus_lw3", true)
	tre := mkLedgerOrg(t, ctx, store, real, 9103, "lw3-tre", 10_000_000)
	mem := mkLedgerMember(t, ctx, store, real, 9103, 813, "lw3-mem")

	fault.incMode = "fail"
	if err := svc.Transfer(ctx, 9103, tre, mem, 813, amt, "topup", "lw3-a", "test"); err == nil {
		t.Fatal("注入后应报错")
	}
	mustQuota(t, ctx, real, tre, 8_000_000, "金库(已扣)")
	mustQuota(t, ctx, real, mem, 0, "成员(未加)")
	if st, ph, _, _ := ledgerRow(t, ctx, store, "lw3-a"); st != "pending" || ph != "debited" {
		t.Fatalf("应留 pending/debited,得 %s/%s", st, ph)
	}
	backdateLedger(t, ctx, store, "lw3-a")
	reconcileTwice(t, ctx, svc)
	if st, _, _, _ := ledgerRow(t, ctx, store, "lw3-a"); st != "applied" {
		t.Fatalf("🔴应补齐至 applied,得 %s", st)
	}
	mustQuota(t, ctx, real, tre, 8_000_000, "金库")
	mustQuota(t, ctx, real, mem, amt, "成员(精确补齐一次)")
	t.Log("W3 ok: 已扣未加 → 双证据确认缺口 → 精确补齐")
}

// TestIntegration_LedgerW4_CreditLostResponse W4(④落地未记 J2):日志判已入账 → 只补状态,绝不二次入账。
func TestIntegration_LedgerW4_CreditLostResponse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	svc, store, real, fault := ledgerSvc(t, ctx, "nexus_lw4", true)
	tre := mkLedgerOrg(t, ctx, store, real, 9104, "lw4-tre", 10_000_000)
	mem := mkLedgerMember(t, ctx, store, real, 9104, 814, "lw4-mem")

	fault.incMode = "lost"
	if err := svc.Transfer(ctx, 9104, tre, mem, 814, amt, "topup", "lw4-a", "test"); err == nil {
		t.Fatal("注入后应报错")
	}
	mustQuota(t, ctx, real, mem, amt, "成员(实际已入账)")
	if st, ph, _, _ := ledgerRow(t, ctx, store, "lw4-a"); st != "pending" || ph != "debited" {
		t.Fatalf("应留 pending/debited,得 %s/%s", st, ph)
	}
	backdateLedger(t, ctx, store, "lw4-a")
	reconcileTwice(t, ctx, svc)
	if st, ph, _, _ := ledgerRow(t, ctx, store, "lw4-a"); st != "applied" || ph != "credited" {
		t.Fatalf("🔴应补状态至 applied/credited,得 %s/%s", st, ph)
	}
	mustQuota(t, ctx, real, tre, 8_000_000, "金库")
	mustQuota(t, ctx, real, mem, amt, "成员(严禁二次入账)")
	t.Log("W4 ok: 入账已落地未记 → 只补状态,未双发")
}

// TestIntegration_LedgerW5_AppliedLost W5(④后⑤前,credited 未 applied):零歧义补状态。
func TestIntegration_LedgerW5_AppliedLost(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	svc, store, real, _ := ledgerSvc(t, ctx, "nexus_lw5", true)
	tre := mkLedgerOrg(t, ctx, store, real, 9105, "lw5-tre", 10_000_000)
	mem := mkLedgerMember(t, ctx, store, real, 9105, 815, "lw5-mem")

	if err := svc.Transfer(ctx, 9105, tre, mem, 815, amt, "topup", "lw5-a", "test"); err != nil {
		t.Fatalf("划账失败: %v", err)
	}
	// 模拟"⑤未落":把 applied 拨回 pending(phase 保持 credited)。
	mustExec(t, ctx, store, `UPDATE ledger_transfer SET status='pending', applied_at=NULL WHERE idempotency_key='lw5-a'`)
	backdateLedger(t, ctx, store, "lw5-a")
	reconcileTwice(t, ctx, svc)
	if st, _, _, _ := ledgerRow(t, ctx, store, "lw5-a"); st != "applied" {
		t.Fatalf("🔴credited 行应补 applied,得 %s", st)
	}
	mustQuota(t, ctx, real, tre, 8_000_000, "金库")
	mustQuota(t, ctx, real, mem, amt, "成员")
	t.Log("W5 ok: credited 行零歧义补状态")
}

// TestIntegration_LedgerW6_PartialDebitRefund W6(部分扣):绝不带部分金额入账 → 两阶段退回出账方 → failed/refunded,净效果 0。
func TestIntegration_LedgerW6_PartialDebitRefund(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	svc, store, real, fault := ledgerSvc(t, ctx, "nexus_lw6", true)
	tre := mkLedgerOrg(t, ctx, store, real, 9106, "lw6-tre", 10_000_000)
	mem := mkLedgerMember(t, ctx, store, real, 9106, 816, "lw6-mem")

	fault.decPartial = 500_000 // 期望扣 2M,实扣 0.5M(模拟并发 clamp)
	if err := svc.Transfer(ctx, 9106, tre, mem, 816, amt, "topup", "lw6-a", "test"); err == nil {
		t.Fatal("部分扣应报错")
	}
	mustQuota(t, ctx, real, tre, 9_500_000, "金库(已被部分扣)")
	st0, ph0, fr0, deb0 := ledgerRow(t, ctx, store, "lw6-a")
	if st0 != "pending" || ph0 != "debited" || fr0 != "partial_debit" || deb0 != 500_000 {
		t.Fatalf("应 pending/debited/partial_debit/500000,得 %s/%s/%s/%d", st0, ph0, fr0, deb0)
	}
	backdateLedger(t, ctx, store, "lw6-a")
	reconcileTwice(t, ctx, svc)
	st, _, fr, _ := ledgerRow(t, ctx, store, "lw6-a")
	if st != "failed" || fr != "refunded" {
		t.Fatalf("🔴应退回并关单 failed/refunded,得 %s/%s", st, fr)
	}
	mustQuota(t, ctx, real, tre, 10_000_000, "金库(退回后复原)")
	mustQuota(t, ctx, real, mem, 0, "成员(分文未得)")
	t.Log("W6 ok: 部分扣 → 退回出账方,净效果 0")
}

// TestIntegration_LedgerW7_CreditBlockedInt32Refund W7(入账 int32 不可达):debited 行判未入账且永远进不去 → 退回金库。
func TestIntegration_LedgerW7_CreditBlockedInt32Refund(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	svc, store, real, fault := ledgerSvc(t, ctx, "nexus_lw7", true)
	tre := mkLedgerOrg(t, ctx, store, real, 9107, "lw7-tre", 2_100_000_000) // 金库近 int32 上限
	mem := mkLedgerMember(t, ctx, store, real, 9107, 817, "lw7-mem")

	// 垫高成员至 1.85e9(经真划账,账本可对):此时 1.85e9+2e8 仍 ≤ int32,可再划。
	if err := svc.Transfer(ctx, 9107, tre, mem, 817, 1_850_000_000, "topup", "lw7-pad1", "test"); err != nil {
		t.Fatalf("垫高划账失败: %v", err)
	}
	// 注入 W3:2e8 已扣未加(此刻入账仍可达)。
	fault.incMode = "fail"
	if err := svc.Transfer(ctx, 9107, tre, mem, 817, 200_000_000, "topup", "lw7-a", "test"); err == nil {
		t.Fatal("注入后应报错")
	}
	// 崩溃后运营方再代充金库 1e8(账外合法,增量代充),随后成员被(合法划账)垫到 2.0e9
	// → 滞留行的 2e8 永远进不去(2.0e9+2e8 > 2.147e9)。
	if err := real.IncreaseUserQuota(ctx, tre, 100_000_000); err != nil {
		t.Fatalf("金库二次代充失败: %v", err)
	}
	if err := svc.Transfer(ctx, 9107, tre, mem, 817, 150_000_000, "topup", "lw7-pad2", "test"); err != nil {
		t.Fatalf("二次垫高失败: %v", err)
	}
	mustQuota(t, ctx, real, mem, 2_000_000_000, "成员(垫高后)")
	backdateLedger(t, ctx, store, "lw7-a")
	reconcileTwice(t, ctx, svc)
	st, _, fr, _ := ledgerRow(t, ctx, store, "lw7-a")
	if st != "failed" || fr != "refunded" {
		t.Fatalf("🔴int32 不可达应退回关单 failed/refunded,得 %s/%s", st, fr)
	}
	// 金库 = 2.1e9 − 1.85e9 − 2e8(扣) + 1e8(代充) − 1.5e8(pad2) + 2e8(退回) = 2e8。
	mustQuota(t, ctx, real, tre, 200_000_000, "金库(退回后)")
	mustQuota(t, ctx, real, mem, 2_000_000_000, "成员(未被强塞越界)")
	t.Log("W7 ok: 入账 int32 不可达 → 退回金库,不越界不丢钱")
}

// TestIntegration_LedgerW8_FreezeBlocksRepair W8(money_freeze):检测/告警恒开,修复写(含账本状态写)全停;解冻后收敛。
func TestIntegration_LedgerW8_FreezeBlocksRepair(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	svc, store, real, fault := ledgerSvc(t, ctx, "nexus_lw8", true)
	tre := mkLedgerOrg(t, ctx, store, real, 9108, "lw8-tre", 10_000_000)
	mem := mkLedgerMember(t, ctx, store, real, 9108, 818, "lw8-mem")

	fault.incMode = "fail"
	if err := svc.Transfer(ctx, 9108, tre, mem, 818, amt, "topup", "lw8-a", "test"); err == nil {
		t.Fatal("注入后应报错")
	}
	backdateLedger(t, ctx, store, "lw8-a")
	mustExec(t, ctx, store, `UPDATE platform_setting SET v='true' WHERE k='money_freeze'`)
	drifts, err := svc.ReconcileTransfers(ctx)
	if err != nil {
		t.Fatalf("冻结下对账环应可跑(只读): %v", err)
	}
	if len(drifts) == 0 {
		t.Fatal("🔴冻结下检测应恒开(漂移必须上报)")
	}
	if st, _, _, _ := ledgerRow(t, ctx, store, "lw8-a"); st != "pending" {
		t.Fatalf("🔴冻结下不得修复(含账本状态写),得 %s", st)
	}
	mustQuota(t, ctx, real, mem, 0, "成员(冻结下不得补发)")
	// 冻结下 Transfer 全拒。
	if err := svc.Transfer(ctx, 9108, tre, mem, 818, amt, "topup", "lw8-b", "test"); err == nil {
		t.Fatal("🔴冻结下 Transfer 应拒")
	}
	mustExec(t, ctx, store, `UPDATE platform_setting SET v='false' WHERE k='money_freeze'`)
	reconcileTwice(t, ctx, svc)
	if st, _, _, _ := ledgerRow(t, ctx, store, "lw8-a"); st != "applied" {
		t.Fatalf("🔴解冻后应收敛 applied,得 %s", st)
	}
	mustQuota(t, ctx, real, tre, 8_000_000, "金库")
	mustQuota(t, ctx, real, mem, amt, "成员")
	t.Log("W8 ok: 冻结只报不修,解冻后精确收敛")
}

// TestIntegration_LedgerIdentityScan 恒等式扫描:违规直充成员(绕平台 ManageUser add)→ 正漂移立即告警(只报不修)。
func TestIntegration_LedgerIdentityScan(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	svc, store, real, _ := ledgerSvc(t, ctx, "nexus_lids", true)
	tre := mkLedgerOrg(t, ctx, store, real, 9109, "lids-tre", 10_000_000)
	mem := mkLedgerMember(t, ctx, store, real, 9109, 819, "lids-mem")
	if err := svc.Transfer(ctx, 9109, tre, mem, 819, amt, "topup", "lids-a", "test"); err != nil {
		t.Fatalf("划账失败: %v", err)
	}
	// 违规直充(绕平台,充值铁律只充金库)。
	if err := real.IncreaseUserQuota(ctx, mem, 700_000); err != nil {
		t.Fatalf("直充注入失败: %v", err)
	}
	drifts, err := svc.ReconcileTransfers(ctx) // 首轮触发恒等式全量扫描
	if err != nil {
		t.Fatalf("对账环失败: %v", err)
	}
	found := false
	for _, d := range drifts {
		if d.Kind == "identity_mismatch" && strings.Contains(d.Detail, "drift=+700000") {
			found = true
		}
	}
	if !found {
		t.Fatalf("🔴违规直充应被恒等式抓到(drift=+700000),得 %+v", drifts)
	}
	// 只报不修:成员余额保持直充后的值(修复走人工 runbook)。
	mustQuota(t, ctx, real, mem, amt+700_000, "成员(扫描不自动改)")
	t.Log("IdentityScan ok: 违规直充立即告警,只报不修")
}

// TestIntegration_LedgerNonLeaderNoop 非 leader:ReconcileTransfers / RunSubscriptionTopup 全 no-op(不读不写不告警)。
func TestIntegration_LedgerNonLeaderNoop(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	svc, store, real, fault := ledgerSvc(t, ctx, "nexus_lnl", false) // leader=false
	tre := mkLedgerOrg(t, ctx, store, real, 9110, "lnl-tre", 10_000_000)
	mem := mkLedgerMember(t, ctx, store, real, 9110, 820, "lnl-mem")

	// Transfer 本身不是 worker,非 leader 也可走(请求路径);制造滞留行。
	fault.incMode = "fail"
	_ = svc.Transfer(ctx, 9110, tre, mem, 820, amt, "topup", "lnl-a", "test")
	backdateLedger(t, ctx, store, "lnl-a")
	drifts, err := svc.ReconcileTransfers(ctx)
	if err != nil || drifts != nil {
		t.Fatalf("🔴非 leader 应 no-op,得 drifts=%v err=%v", drifts, err)
	}
	if st, _, _, _ := ledgerRow(t, ctx, store, "lnl-a"); st != "pending" {
		t.Fatalf("🔴非 leader 不得修复,得 %s", st)
	}
	if err := svc.RunSubscriptionTopup(ctx); err != nil {
		t.Fatalf("非 leader topup 应静默 no-op: %v", err)
	}
	t.Log("NonLeader ok: 对账环/补满 worker 全 no-op(leader-gated)")
}
