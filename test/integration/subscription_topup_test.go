// 架构B 阶段1(BE②)订阅补满 / 追加划账 / 离职退额 · 集成验收(真 MySQL + rc.4):
// 补满到目标(D=目标−剩余)/ 幂等桶键防双补(含未选主双跑对抗)/ 跳 disabled、offboarded、quarantined、
// fixed 档 / 金库不足 fail-open 不半划 / 帽兜底 / money_freeze 跳过 /
// GrantMemberQuota(帽校验+支持态红线)/ RefundMemberBalance(disable-first 红线+sweep+幂等)。
package integration

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/nexusapi-platform/enterprise/adapter/newapi"
	"github.com/nexusapi-platform/enterprise/pkg/session"
	"github.com/nexusapi-platform/enterprise/repo"
	"github.com/nexusapi-platform/enterprise/service"
)

// mkSubTier 建订阅档位。
func mkSubTier(t *testing.T, ctx context.Context, store *repo.Store, tierID, orgID int64, amountRaw int64, period string) {
	t.Helper()
	mustExec(t, ctx, store,
		`INSERT INTO tier (id, org_id, name, quota_type, amount_raw, reset_period, status)
		 VALUES (?, ?, ?, 'subscription', ?, ?, 'active')`,
		tierID, orgID, fmt.Sprintf("sub-%d", tierID), amountRaw, period)
}

// bindTier 成员挂档位。
func bindTier(t *testing.T, ctx context.Context, store *repo.Store, memberID, tierID int64) {
	t.Helper()
	mustExec(t, ctx, store, `UPDATE member SET tier_id = ? WHERE id = ?`, tierID, memberID)
}

// countApplied 数某幂等键前缀的 applied 账本行。
func countApplied(t *testing.T, ctx context.Context, store *repo.Store, likeKey string) int {
	t.Helper()
	var n int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM ledger_transfer WHERE idempotency_key LIKE ? AND status='applied'`, likeKey).Scan(&n); err != nil {
		t.Fatalf("数账本失败: %v", err)
	}
	return n
}

// TestIntegration_SubscriptionTopup_BasicAndIdempotent 补满到目标 + 桶键幂等(同实例去重/新实例重放都不双补)。
func TestIntegration_SubscriptionTopup_BasicAndIdempotent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	svc, store, real, _ := ledgerSvc(t, ctx, "nexus_st1", true)
	tre := mkLedgerOrg(t, ctx, store, real, 9201, "st1-tre", 50_000_000)
	mem := mkLedgerMember(t, ctx, store, real, 9201, 901, "st1-mem")
	mkSubTier(t, ctx, store, 701, 9201, 5_000_000, "daily")
	bindTier(t, ctx, store, 901, 701)

	if err := svc.RunSubscriptionTopup(ctx); err != nil {
		t.Fatalf("补满失败: %v", err)
	}
	mustQuota(t, ctx, real, mem, 5_000_000, "成员(补满到目标 $10)")
	mustQuota(t, ctx, real, tre, 45_000_000, "金库")
	if n := countApplied(t, ctx, store, "subtopup:901:%"); n != 1 {
		t.Fatalf("应恰 1 笔补满账本行,得 %d", n)
	}
	// 同实例再跑:进程内桶去重 → 不动。
	if err := svc.RunSubscriptionTopup(ctx); err != nil {
		t.Fatalf("重跑失败: %v", err)
	}
	mustQuota(t, ctx, real, mem, 5_000_000, "成员(同桶不重复补)")
	// 模拟重启(新 Service 实例,内存去重丢失):账本幂等键兜底,依然不双补。
	svc2 := ledgerSvcAttach(t, store, real)
	if err := svc2.RunSubscriptionTopup(ctx); err != nil {
		t.Fatalf("新实例重跑失败: %v", err)
	}
	mustQuota(t, ctx, real, mem, 5_000_000, "成员(重启重放不双补)")
	if n := countApplied(t, ctx, store, "subtopup:901:%"); n != 1 {
		t.Fatalf("重启重放后仍应恰 1 笔,得 %d", n)
	}
	t.Log("SubTopup basic ok: 补满/同桶幂等/重启重放不双补")
}

// TestIntegration_SubscriptionTopup_ConcurrentDoubleRun 未选主双跑对抗(34 §5.2):两个实例并发同 tick,
// 账本幂等键结构性防双补——成员恰好补一次。
func TestIntegration_SubscriptionTopup_ConcurrentDoubleRun(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	svc, store, real, _ := ledgerSvc(t, ctx, "nexus_st2", true)
	tre := mkLedgerOrg(t, ctx, store, real, 9202, "st2-tre", 50_000_000)
	mem := mkLedgerMember(t, ctx, store, real, 9202, 902, "st2-mem")
	mkSubTier(t, ctx, store, 702, 9202, 5_000_000, "daily")
	bindTier(t, ctx, store, 902, 702)
	svcB := ledgerSvcAttach(t, store, real) // 第二"节点"(独立内存态,同库同上游)

	var wg sync.WaitGroup
	for _, s := range []*service.Service{svc, svcB} {
		wg.Add(1)
		go func(s *service.Service) { defer wg.Done(); _ = s.RunSubscriptionTopup(ctx) }(s)
	}
	wg.Wait()
	mustQuota(t, ctx, real, mem, 5_000_000, "成员(并发双跑仅补一次)")
	mustQuota(t, ctx, real, tre, 45_000_000, "金库(未被双扣)")
	if n := countApplied(t, ctx, store, "subtopup:902:%"); n != 1 {
		t.Fatalf("并发双跑应恰 1 笔 applied,得 %d", n)
	}
	t.Log("SubTopup concurrent ok: 未选主双跑,幂等键兜底不双补")
}

// TestIntegration_SubscriptionTopup_SkipsAndCapAndDLEZero 跳过面(disabled/offboarded/quarantined/fixed 档)+
// 帽兜底 + D<=0 跳过 + money_freeze 跳过。
func TestIntegration_SubscriptionTopup_SkipsAndCapAndDLEZero(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	svc, store, real, _ := ledgerSvc(t, ctx, "nexus_st3", true)
	tre := mkLedgerOrg(t, ctx, store, real, 9203, "st3-tre", 900_000_000)
	mkSubTier(t, ctx, store, 703, 9203, 5_000_000, "daily")
	mkSubTier(t, ctx, store, 704, 9203, 600_000_000, "daily") // 超帽档(帽默认 5e8)
	mustExec(t, ctx, store, `INSERT INTO tier (id, org_id, name, quota_type, amount_raw, reset_period, status)
		 VALUES (705, 9203, 'fixed-tier', 'fixed', 5000000, NULL, 'active')`)

	mDisabled := mkLedgerMember(t, ctx, store, real, 9203, 911, "st3-dis")
	bindTier(t, ctx, store, 911, 703)
	mustExec(t, ctx, store, `UPDATE member SET status='disabled' WHERE id=911`)
	mOffboard := mkLedgerMember(t, ctx, store, real, 9203, 912, "st3-off")
	bindTier(t, ctx, store, 912, 703)
	mustExec(t, ctx, store, `UPDATE member SET status='offboarded', deleted_at=NOW(3) WHERE id=912`)
	mQuarant := mkLedgerMember(t, ctx, store, real, 9203, 913, "st3-qua")
	bindTier(t, ctx, store, 913, 703)
	mustExec(t, ctx, store, `UPDATE member SET bootstrap_state='quarantined', status='disabled' WHERE id=913`)
	mFixed := mkLedgerMember(t, ctx, store, real, 9203, 914, "st3-fix")
	bindTier(t, ctx, store, 914, 705)
	mCapped := mkLedgerMember(t, ctx, store, real, 9203, 915, "st3-cap")
	bindTier(t, ctx, store, 915, 704)
	mFull := mkLedgerMember(t, ctx, store, real, 9203, 916, "st3-full")
	bindTier(t, ctx, store, 916, 703)
	if err := svc.Transfer(ctx, 9203, tre, mFull, 916, 6_000_000, "topup", "st3-prefill", "test"); err != nil {
		t.Fatalf("预填失败: %v", err) // 已超目标(6M > 5M)→ D<=0
	}

	if err := svc.RunSubscriptionTopup(ctx); err != nil {
		t.Fatalf("补满失败: %v", err)
	}
	mustQuota(t, ctx, real, mDisabled, 0, "disabled 成员(必须跳过)")
	mustQuota(t, ctx, real, mOffboard, 0, "offboarded 成员(必须跳过——给离职的人送钱=打穿退额)")
	mustQuota(t, ctx, real, mQuarant, 0, "quarantined 孤儿(必须跳过)")
	mustQuota(t, ctx, real, mFixed, 0, "fixed 档成员(不参与周期补满)")
	mustQuota(t, ctx, real, mCapped, 500_000_000, "超帽档成员(帽兜底 $1000)")
	mustQuota(t, ctx, real, mFull, 6_000_000, "已超目标成员(D<=0 不动、不回收)")

	// money_freeze:worker 整轮跳过(新成员不被补)。
	mFrozen := mkLedgerMember(t, ctx, store, real, 9203, 917, "st3-frz")
	bindTier(t, ctx, store, 917, 703)
	mustExec(t, ctx, store, `UPDATE platform_setting SET v='true' WHERE k='money_freeze'`)
	if err := svc.RunSubscriptionTopup(ctx); err != nil {
		t.Fatalf("冻结下 worker 应静默: %v", err)
	}
	mustQuota(t, ctx, real, mFrozen, 0, "冻结下新成员(不得补)")
	mustExec(t, ctx, store, `UPDATE platform_setting SET v='false' WHERE k='money_freeze'`)
	if err := svc.RunSubscriptionTopup(ctx); err != nil {
		t.Fatalf("解冻后补满失败: %v", err)
	}
	mustQuota(t, ctx, real, mFrozen, 5_000_000, "解冻后新成员(同桶补上)")
	t.Log("SubTopup skips ok: 四类跳过 + 帽兜底 + D<=0 + freeze")
}

// TestIntegration_SubscriptionTopup_TreasuryInsufficient 金库不足:不半划、不停服(fail-open),
// 代充到位后同桶自动补上(幂等键支持迟到补满)。
func TestIntegration_SubscriptionTopup_TreasuryInsufficient(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	svc, store, real, _ := ledgerSvc(t, ctx, "nexus_st4", true)
	tre := mkLedgerOrg(t, ctx, store, real, 9204, "st4-tre", 1_000_000) // 金库仅 $2
	mem := mkLedgerMember(t, ctx, store, real, 9204, 921, "st4-mem")
	mkSubTier(t, ctx, store, 706, 9204, 5_000_000, "daily")
	bindTier(t, ctx, store, 921, 706)

	if err := svc.RunSubscriptionTopup(ctx); err != nil {
		t.Fatalf("金库不足不应报错(fail-open): %v", err)
	}
	mustQuota(t, ctx, real, mem, 0, "成员(不半划:一分都不划)")
	mustQuota(t, ctx, real, tre, 1_000_000, "金库(分文未动)")
	// 运营方代充到位 → 同一周期桶自动补上。
	if err := real.IncreaseUserQuota(ctx, tre, 9_000_000); err != nil {
		t.Fatalf("代充失败: %v", err)
	}
	if err := svc.RunSubscriptionTopup(ctx); err != nil {
		t.Fatalf("代充后补满失败: %v", err)
	}
	mustQuota(t, ctx, real, mem, 5_000_000, "成员(迟到补满)")
	t.Log("SubTopup insufficient ok: 不半划不停服,代充后同桶补上")
}

// TestIntegration_GrantAndRefund 追加划账(帽校验/支持态红线)+ 离职退额(disable-first 红线/sweep/幂等)。
func TestIntegration_GrantAndRefund(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	svc, store, real, _ := ledgerSvc(t, ctx, "nexus_gr1", true)
	tre := mkLedgerOrg(t, ctx, store, real, 9205, "gr1-tre", 600_000_000)
	mem := mkLedgerMember(t, ctx, store, real, 9205, 931, "gr1-mem")

	admin := session.Claims{Role: session.RoleOrgAdmin, OrgID: 9205, MemberID: 1}
	// 追加划账 $6。
	if err := svc.GrantMemberQuota(ctx, admin, 9205, 931, 3_000_000, "项目加急", "k1"); err != nil {
		t.Fatalf("追加划账失败: %v", err)
	}
	mustQuota(t, ctx, real, mem, 3_000_000, "成员(追加后)")
	// 超成员帽(默认 5e8)拒。
	if err := svc.GrantMemberQuota(ctx, admin, 9205, 931, 499_000_000, "超帽", "k2"); err == nil {
		t.Fatal("🔴超成员帽应拒")
	}
	// 支持态动钱红线。
	sup := admin
	sup.SupportSessionID = 7
	if err := svc.GrantMemberQuota(ctx, sup, 9205, 931, 1_000_000, "支持态", "k3"); err == nil {
		t.Fatal("🔴支持态动钱应拒(红线)")
	}
	// 成员 active 时退额必须拒(disable-first 红线)。
	if _, err := svc.RefundMemberBalance(ctx, 9205, 931, "test"); err == nil {
		t.Fatal("🔴active 成员退额应拒")
	}
	// disable 后退额:实时余额全额回金库。
	mustExec(t, ctx, store, `UPDATE member SET status='disabled' WHERE id=931`)
	refunded, err := svc.RefundMemberBalance(ctx, 9205, 931, "test")
	if err != nil {
		t.Fatalf("退额失败: %v", err)
	}
	if refunded != 3_000_000 {
		t.Fatalf("🔴应退 3000000,得 %d", refunded)
	}
	mustQuota(t, ctx, real, mem, 0, "成员(退空)")
	mustQuota(t, ctx, real, tre, 600_000_000, "金库(全额回笼)")
	// 幂等安全:再退一次 = 0(sweep 读实时余额)。
	refunded2, err := svc.RefundMemberBalance(ctx, 9205, 931, "test")
	if err != nil || refunded2 != 0 {
		t.Fatalf("二次退额应为 0,得 %d err=%v", refunded2, err)
	}
	// 账本 reason 正确。
	var n int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM ledger_transfer WHERE member_id=931 AND reason='offboard_refund' AND status='applied'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("退额账本行应恰 1 笔 applied,得 %d err=%v", n, err)
	}
	t.Log("Grant/Refund ok: 帽校验/支持态红线/disable-first/sweep 幂等/账本归因")
}

// ledgerSvcAttach 复用既有 store+upstream 再起一个 Service(模拟第二节点/重启后的新进程:
// 内存去重态清零,只剩账本幂等键这道结构性防线)。
func ledgerSvcAttach(t *testing.T, store *repo.Store, up newapi.NewapiAdapter) *service.Service {
	t.Helper()
	signer, _ := session.NewSigner([]byte("integration-test-session-key-32b!!"), time.Hour)
	return service.New(service.Deps{Store: store, Upstream: up, Keyring: mustKeyring(t), Signer: signer,
		FundingEnabled: false, Leadership: service.NewEnvLeadership(true)})
}
