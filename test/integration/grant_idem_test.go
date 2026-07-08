// P2-1 涉钱回归锁(39号复签必带条件,涉钱评审总监 2026-07-08):
// 追加划账幂等键重放必须**先验参数一致**——同一 idempotency_key 第二次传不同金额,
// 必须 409 拒绝且金库/成员额度**分毫未动**(绝不回"成功"假到账);同键同额重放=幂等成功且不双划。
// 守恒栈原有 SettlementDeductDedup 只盖"同参重放不双扣",盖不到"异参重放要拒"——本测锁死该分支,
// 防未来重构静默回归到"同键不同额回 ok 实际没划"的掉单老 bug。
package integration

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/nexusapi-platform/enterprise/model"
	"github.com/nexusapi-platform/enterprise/pkg/apperr"
	"github.com/nexusapi-platform/enterprise/pkg/session"
)

func TestIntegration_GrantIdemKeyAmountMismatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	svc, store, upstream, _ := escrowSvc(t, ctx, "nexus_gidem")
	newapiSQLDSN := os.Getenv("NEXUS_IT_NEWAPI_SQL_DSN")
	const orgID, memberID = int64(901), int64(1)
	// 金库 + 注资 + 档位 + 成员 saga 开通(架构B 数据形状,照 MemberLifecycle)。
	treasuryCred := mkEnterpriseUser(t, ctx, upstream, "gidem-treasury")
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO organization (id, name, slug, newapi_user_id) VALUES (?, 'gidem-org', 'gidem-slug', ?)`, orgID, treasuryCred.NewapiUserID); err != nil {
		t.Fatalf("建组织失败: %v", err)
	}
	if err := upstream.IncreaseUserQuota(ctx, treasuryCred.NewapiUserID, 10_000_000); err != nil {
		t.Fatalf("金库注资失败: %v", err)
	}
	amount := int64(2_000_000)
	tierID, terr := store.CreateTier(ctx, &model.Tier{OrgID: orgID, Name: "gidem-tier", AmountRaw: &amount})
	if terr != nil {
		t.Fatalf("建档失败: %v", terr)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO member (id, org_id, tier_id, login_email, status, bootstrap_state) VALUES (?, ?, ?, 'gidem@t.local', 'provisioning', 'pending')`, memberID, orgID, tierID); err != nil {
		t.Fatalf("建成员失败: %v", err)
	}
	memberUID, perr := svc.ProvisionMemberServiceAccount(ctx, orgID, memberID, "", amount, "test")
	if perr != nil {
		t.Fatalf("开通服务账号失败: %v", perr)
	}
	if err := store.ActivatePlatformAccount(ctx, orgID, memberID); err != nil {
		t.Fatalf("置 active 失败: %v", err)
	}

	ndb, err := sql.Open("mysql", newapiSQLDSN)
	if err != nil {
		t.Fatalf("连 newapi 库失败: %v", err)
	}
	defer ndb.Close()
	quotaOf := func(uid int) (q int64) {
		if err := ndb.QueryRowContext(ctx, `SELECT quota FROM users WHERE id = ?`, uid).Scan(&q); err != nil {
			t.Fatalf("查 user %d quota 失败: %v", uid, err)
		}
		return
	}

	admin := session.Claims{Role: session.RoleOrgAdmin, OrgID: orgID, MemberID: 999}
	const a1, a2 = int64(1_000_000), int64(3_000_000)

	// ① 第一笔:同键首划 a1 → 成功;守恒断言(金库减 a1、成员加 a1)。
	tBefore, mBefore := quotaOf(treasuryCred.NewapiUserID), quotaOf(memberUID)
	if err := svc.GrantMemberQuota(ctx, admin, orgID, memberID, a1, "first", "gidem-key-1"); err != nil {
		t.Fatalf("首笔划账失败: %v", err)
	}
	tAfter1, mAfter1 := quotaOf(treasuryCred.NewapiUserID), quotaOf(memberUID)
	if tAfter1 != tBefore-a1 || mAfter1 != mBefore+a1 {
		t.Fatalf("🔴首笔守恒断言失败:金库 %d→%d(应减 %d),成员 %d→%d(应加 %d)", tBefore, tAfter1, a1, mBefore, mAfter1, a1)
	}

	// ② 核心:同键第二次传**不同金额** a2 → 必须 409 拒绝,且金库/成员额度分毫未动(绝不假到账)。
	err2 := svc.GrantMemberQuota(ctx, admin, orgID, memberID, a2, "mismatch", "gidem-key-1")
	if err2 == nil {
		t.Fatalf("🔴同键不同额应 409 拒绝,实回成功(假到账老 bug 回归!)")
	}
	var ae *apperr.Error
	if !errors.As(err2, &ae) || ae.HTTPStatus != 409 || !strings.Contains(ae.Message, "幂等键冲突") {
		t.Fatalf("🔴同键不同额应 409 幂等键冲突,实: %v", err2)
	}
	tAfter2, mAfter2 := quotaOf(treasuryCred.NewapiUserID), quotaOf(memberUID)
	if tAfter2 != tAfter1 || mAfter2 != mAfter1 {
		t.Fatalf("🔴异参重放被拒后额度必须分毫未动:金库 %d→%d,成员 %d→%d", tAfter1, tAfter2, mAfter1, mAfter2)
	}

	// ③ 同键**同额**重放 → 幂等成功,且不双划(额度仍与首笔后一致)。
	if err := svc.GrantMemberQuota(ctx, admin, orgID, memberID, a1, "replay-same", "gidem-key-1"); err != nil {
		t.Fatalf("同键同额重放应幂等成功,实错: %v", err)
	}
	tAfter3, mAfter3 := quotaOf(treasuryCred.NewapiUserID), quotaOf(memberUID)
	if tAfter3 != tAfter1 || mAfter3 != mAfter1 {
		t.Fatalf("🔴同参重放不得双划:金库 %d→%d,成员 %d→%d", tAfter1, tAfter3, mAfter1, mAfter3)
	}

	t.Logf("P2-1 回归锁 ok: 首划守恒(金库-%d/成员+%d);同键异额 409 且额度分毫未动;同键同额幂等不双划", a1, a1)
}
