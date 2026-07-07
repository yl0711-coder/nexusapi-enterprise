// 架构B 阶段1(BE① 身份层)集成测试:硬停 fan-out + 恢复选择性解禁(真 MySQL + rc.4)。
// 质量门铁律:以容器集成栈跑数为准(本机 NEXUS_IT_* 未设时全 SKIP)。
package integration

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/nexusapi-platform/enterprise/model"
	"github.com/nexusapi-platform/enterprise/pkg/session"
)

// queryNewapiUserStatus 直查 new-api 库 users.status(1=enabled 2=disabled;与 v1_test 同法)。
func queryNewapiUserStatus(t *testing.T, ctx context.Context, uid int) int {
	t.Helper()
	db, err := sql.Open("mysql", os.Getenv("NEXUS_IT_NEWAPI_SQL_DSN"))
	if err != nil {
		t.Fatalf("连 newapi 库失败: %v", err)
	}
	defer db.Close()
	var st int
	if err := db.QueryRowContext(ctx, `SELECT status FROM users WHERE id = ?`, uid).Scan(&st); err != nil {
		t.Fatalf("查 user %d 状态失败: %v", uid, err)
	}
	return st
}

// TestIntegration_ArchB_HardStopFanout 硬停 = disable 金库 + fan-out disable 全部成员 user(漏一个=有人还在花);
// 解除 = enable 金库 + 只 enable 平台侧 active 成员(个别停用的不解——其 disable 语义独立于硬停)。
func TestIntegration_ArchB_HardStopFanout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	svc, store, upstream := v1Svc(t, ctx, "nexus_archb_hsf", false)
	const orgID = int64(9101)
	treasury := mkArchBOrgRaw(t, ctx, store, upstream, orgID, 20_000_000)

	// 两个真服务账号成员:m1 active、m2 事先被单独停用。
	mustExec(t, ctx, store, `INSERT INTO member (id, org_id, login_email, status, bootstrap_state) VALUES (911, ?, 'hsf1@t.local', 'provisioning', 'pending')`, orgID)
	mustExec(t, ctx, store, `INSERT INTO member (id, org_id, login_email, status, bootstrap_state) VALUES (912, ?, 'hsf2@t.local', 'provisioning', 'pending')`, orgID)
	uid1, e1 := svc.ProvisionMemberServiceAccount(ctx, orgID, 911, "", 2_000_000, "test")
	if e1 != nil {
		t.Fatalf("开通 m1 失败: %v", e1)
	}
	uid2, e2 := svc.ProvisionMemberServiceAccount(ctx, orgID, 912, "", 2_000_000, "test")
	if e2 != nil {
		t.Fatalf("开通 m2 失败: %v", e2)
	}
	if err := store.ActivatePlatformAccount(ctx, orgID, 911); err != nil {
		t.Fatalf("m1 置 active 失败: %v", err)
	}
	if err := store.ActivatePlatformAccount(ctx, orgID, 912); err != nil {
		t.Fatalf("m2 置 active 失败: %v", err)
	}
	admin := session.Claims{Role: session.RoleOrgAdmin, OrgID: orgID, MemberID: 999}
	if err := svc.SetMemberStatus(ctx, admin, orgID, 912, false); err != nil { // m2 单独停用
		t.Fatalf("停用 m2 失败: %v", err)
	}

	opc := session.Claims{Role: session.RoleOperator, OrgID: orgID}
	// 硬停:金库 + 全部成员 user disable(含已停用的 m2,幂等)。
	if err := svc.HardStopOrg(ctx, opc, orgID, true); err != nil {
		t.Fatalf("硬停失败: %v", err)
	}
	assertUserStatus := func(uid, want int, label string) {
		t.Helper()
		if st := queryNewapiUserStatus(t, ctx, uid); st != want {
			t.Fatalf("🔴%s user=%d status=%d want %d", label, uid, st, want)
		}
	}
	assertUserStatus(treasury, 2, "硬停后金库应 disabled")
	assertUserStatus(uid1, 2, "硬停 fan-out 后 m1 应 disabled")
	assertUserStatus(uid2, 2, "硬停 fan-out 后 m2 应 disabled")

	// 解除:金库 + m1 enable;m2 平台侧 disabled → 维持 disable(它的停用独立于硬停)。
	if err := svc.HardStopOrg(ctx, opc, orgID, false); err != nil {
		t.Fatalf("解除失败: %v", err)
	}
	assertUserStatus(treasury, 1, "解除后金库应 enabled")
	assertUserStatus(uid1, 1, "解除后 active 成员 m1 应 enabled")
	assertUserStatus(uid2, 2, "解除后单独停用的 m2 应维持 disabled")

	org, _ := store.GetOrganization(ctx, orgID)
	if org.Status != model.OrgStatusActive {
		t.Fatalf("解除后组织状态应 active,实=%s", org.Status)
	}
	t.Log("架构B 硬停 fan-out 真账 ok: 停=金库+全员 disable;解=金库+active 成员 enable,单独停用者不误解")
}
