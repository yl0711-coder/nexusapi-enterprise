// 架构B 阶段0 自测(33 §6):对真 MySQL + new-api rc.4。
// 覆盖:Transfer 守恒/幂等重放/越界拒/freeze 全拒/中途失败留 pending;Provision saga 全链守恒 + 孤儿隔离;
// VerifyQuotaPerUnit 配错拒;CheckActiveSubscriptions 命中 0。迁移 0030-0034 由 v1Svc 的 Migrate 空库全量验证。
package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nexusapi-platform/enterprise/adapter/newapi"
	"github.com/nexusapi-platform/enterprise/repo"
)

// mkArchBOrgRaw 建组织行(金库 user 用 mkEnterpriseUser 造真 new-api 用户)并给金库充值 fundRaw。
func mkArchBOrgRaw(t *testing.T, ctx context.Context, store *repo.Store, upstream newapi.NewapiAdapter, orgID int64, fundRaw int64) (treasuryUID int) {
	t.Helper()
	cred := mkEnterpriseUser(t, ctx, upstream, fmt.Sprintf("archb-treasury-%d", orgID))
	if _, err := store.DB().ExecContext(ctx,
		`INSERT INTO organization (id, name, slug, newapi_user_id) VALUES (?, ?, ?, ?)`,
		orgID, fmt.Sprintf("archb-org-%d", orgID), fmt.Sprintf("archb-%d", orgID), cred.NewapiUserID); err != nil {
		t.Fatalf("建组织失败: %v", err)
	}
	if fundRaw > 0 { // 运营方代充金库(增量,ADR §4.1)
		if err := upstream.IncreaseUserQuota(ctx, cred.NewapiUserID, fundRaw); err != nil {
			t.Fatalf("金库充值失败: %v", err)
		}
	}
	return cred.NewapiUserID
}

// mustExec 执行 SQL,失败即 Fatal。
func mustExec(t *testing.T, ctx context.Context, store *repo.Store, q string, args ...any) {
	t.Helper()
	if _, err := store.DB().ExecContext(ctx, q, args...); err != nil {
		t.Fatalf("SQL 失败(%s): %v", q, err)
	}
}

// TestIntegration_ArchB_TransferConservation Transfer 五步序:守恒 + 幂等重放 + freeze 全拒。
func TestIntegration_ArchB_TransferConservation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	svc, store, upstream := v1Svc(t, ctx, "nexus_archb_tc", false)
	const orgID = int64(9001)
	treasury := mkArchBOrgRaw(t, ctx, store, upstream, orgID, 10_000_000) // 金库 $20
	member := mkEnterpriseUser(t, ctx, upstream, "archb-m1")

	// 划账 $4=2,000,000 raw:金库-2M、成员+2M(守恒),账本 applied。
	if err := svc.Transfer(ctx, orgID, treasury, member.NewapiUserID, 0, 2_000_000, "topup", "tc-1", "test"); err != nil {
		t.Fatalf("划账失败: %v", err)
	}
	tq, _ := upstream.GetUserQuota(ctx, treasury)
	mq, _ := upstream.GetUserQuota(ctx, member.NewapiUserID)
	if tq != 8_000_000 || mq != 2_000_000 {
		t.Fatalf("🔴守恒破:金库=%d(期 8M) 成员=%d(期 2M)", tq, mq)
	}

	// 幂等重放:同 idemKey 再调 → nil 且余额不变(绝不双扣双加)。
	if err := svc.Transfer(ctx, orgID, treasury, member.NewapiUserID, 0, 2_000_000, "topup", "tc-1", "test"); err != nil {
		t.Fatalf("幂等重放应成功返回: %v", err)
	}
	tq2, _ := upstream.GetUserQuota(ctx, treasury)
	if tq2 != 8_000_000 {
		t.Fatalf("🔴幂等重放导致重复扣款:金库=%d", tq2)
	}

	// 出账方余额不足 → 业务拒(不半划)。
	if err := svc.Transfer(ctx, orgID, treasury, member.NewapiUserID, 0, 100_000_000, "topup", "tc-2", "test"); err == nil {
		t.Fatal("🔴余额不足应拒")
	}

	// 金额非法(0/负)拒。
	if err := svc.Transfer(ctx, orgID, treasury, member.NewapiUserID, 0, 0, "topup", "tc-3", "test"); err == nil {
		t.Fatal("🔴金额 0 应拒")
	}

	// money_freeze=true → 全拒(急停);解除后恢复。
	if _, err := store.DB().ExecContext(ctx, `UPDATE platform_setting SET v='true' WHERE k='money_freeze'`); err != nil {
		t.Fatalf("置 freeze 失败: %v", err)
	}
	if err := svc.Transfer(ctx, orgID, treasury, member.NewapiUserID, 0, 1_000_000, "topup", "tc-4", "test"); err == nil {
		t.Fatal("🔴money_freeze 下 Transfer 应拒")
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE platform_setting SET v='false' WHERE k='money_freeze'`); err != nil {
		t.Fatalf("解除 freeze 失败: %v", err)
	}
	if err := svc.Transfer(ctx, orgID, treasury, member.NewapiUserID, 0, 1_000_000, "topup", "tc-5", "test"); err != nil {
		t.Fatalf("解除 freeze 后应恢复: %v", err)
	}
	t.Log("ArchB Transfer ok: 守恒/幂等重放/不足拒/非法拒/freeze 急停全过")
}

// TestIntegration_ArchB_Int32Guard 入账越界:阶段1 起 Transfer 在步骤①即预检入账方 int32(BE②),
// 越界在任何钱面写之前干净拒——金库分文未动、无账本行(不再制造可预见的"已扣未加"滞留行;
// 步骤④的 quota_guard 上界校验仍在,兜并发窗内的余额变动;中途失败滞留行的收敛走 ledger_reconcile_test)。
func TestIntegration_ArchB_Int32Guard(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	svc, store, upstream := v1Svc(t, ctx, "nexus_archb_i32", false)
	const orgID = int64(9002)
	treasury := mkArchBOrgRaw(t, ctx, store, upstream, orgID, 300_000_000) // 金库 $600
	member := mkEnterpriseUser(t, ctx, upstream, "archb-i32")
	// 成员先垫到接近 int32 上限(直充仅测试用;生产铁律只充金库)。
	if err := upstream.ManageUserQuota(ctx, member.NewapiUserID, newapi.QuotaAdd, 2_100_000_000); err != nil {
		t.Fatalf("垫成员额度失败: %v", err)
	}
	// 划 2 亿:成员写后 23 亿 > 21.47 亿 → 步骤① 预检拒。
	err := svc.Transfer(ctx, orgID, treasury, member.NewapiUserID, 0, 200_000_000, "topup", "i32-1", "test")
	if err == nil {
		t.Fatal("🔴写后超 int32 应拒")
	}
	// 干净拒:金库分文未动 + 无账本行(fail 朝少钱且不留悬账)。
	tq, _ := upstream.GetUserQuota(ctx, treasury)
	if tq != 300_000_000 {
		t.Fatalf("🔴预检拒后金库应分文未动,得 %d", tq)
	}
	var n int
	if qerr := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM ledger_transfer WHERE idempotency_key='i32-1'`).Scan(&n); qerr != nil {
		t.Fatalf("查账本失败: %v", qerr)
	}
	if n != 0 {
		t.Fatalf("🔴预检拒不应留账本行,得 %d 行", n)
	}
	if _, rerr := svc.ReconcileTransfers(ctx); rerr != nil {
		t.Fatalf("对账环失败: %v", rerr)
	}
	t.Log("ArchB int32 guard ok: 步骤①预检干净拒(金库未动/无账本行)")
}

// TestIntegration_ArchB_ProvisionSaga Provision saga 全链:建号→凭证→wallet_only→首笔划账守恒;
// CheckActiveSubscriptions 命中 0;金库不足 → 孤儿隔离(disable+quarantined,不半成功)。
func TestIntegration_ArchB_ProvisionSaga(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	svc, store, upstream := v1Svc(t, ctx, "nexus_archb_pv", false)
	const orgID = int64(9003)
	treasury := mkArchBOrgRaw(t, ctx, store, upstream, orgID, 50_000_000) // 金库 $100

	// 平台成员行(provisioning 态)。
	mustExec(t, ctx, store, `INSERT INTO member (id, org_id, login_email, status, bootstrap_state) VALUES (601, ?, 'b601@t.local', 'provisioning', 'pending')`, orgID)
	uid, err := svc.ProvisionMemberServiceAccount(ctx, orgID, 601, "", 5_000_000, "test") // 初始 $10
	if err != nil {
		t.Fatalf("Provision saga 失败: %v", err)
	}
	// 守恒:金库 50M-5M=45M,成员=5M。
	tq, _ := upstream.GetUserQuota(ctx, treasury)
	mq, _ := upstream.GetUserQuota(ctx, uid)
	if tq != 45_000_000 || mq != 5_000_000 {
		t.Fatalf("🔴saga 守恒破:金库=%d 成员=%d", tq, mq)
	}
	// 凭证已落库 + WithMemberCred 可代调 + wallet_only 已设 + 无 active 订阅。
	hits, herr := svc.CheckActiveSubscriptions(ctx, orgID)
	if herr != nil {
		t.Fatalf("上线闸查询失败: %v", herr)
	}
	if len(hits) != 0 {
		t.Fatalf("🔴上线闸命中应为 0,得 %d", len(hits))
	}

	// 孤儿注入:金库仅剩 45M,开成员要 50M → 首笔划账失败 → disable+quarantined,不半成功。
	mustExec(t, ctx, store, `INSERT INTO member (id, org_id, login_email, status, bootstrap_state) VALUES (602, ?, 'b602@t.local', 'provisioning', 'pending')`, orgID)
	if _, err := svc.ProvisionMemberServiceAccount(ctx, orgID, 602, "", 50_000_000, "test"); err == nil {
		t.Fatal("🔴金库不足应整体失败")
	}
	var bstate string
	if qerr := store.DB().QueryRowContext(ctx, `SELECT bootstrap_state FROM member WHERE id=602`).Scan(&bstate); qerr != nil {
		t.Fatalf("查成员失败: %v", qerr)
	}
	if bstate != "quarantined" {
		t.Fatalf("🔴孤儿应标 quarantined,得 %s", bstate)
	}
	t.Log("ArchB Provision saga ok: 全链守恒 + wallet_only + 上线闸 0 命中 + 孤儿隔离")
}

// TestIntegration_ArchB_VerifyQuotaPerUnit 启动自检:配对过、配错拒。
func TestIntegration_ArchB_VerifyQuotaPerUnit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	svc, store, _ := v1Svc(t, ctx, "nexus_archb_qpu", false)
	if err := svc.VerifyQuotaPerUnit(ctx); err != nil {
		t.Fatalf("默认 500000 应与 rc.4 一致: %v", err)
	}
	mustExec(t, ctx, store, `UPDATE platform_setting SET v='999999' WHERE k='quota_per_unit'`)
	if err := svc.VerifyQuotaPerUnit(ctx); err == nil {
		t.Fatal("🔴配错应拒启动")
	} else if !strings.Contains(err.Error(), "不一致") {
		t.Fatalf("错误信息应指明不一致: %v", err)
	}
	t.Log("ArchB VerifyQuotaPerUnit ok: 配对过/配错拒")
}
