// v1.1 项B 验收(23-§B-5):门A 组织 new-api 用户名改随机名存库 + 撞名归属校验闸(只 adopt 自己的名、
// 外部撞名重生成或报冲突、绝不 disable 外部用户)。用 v1Svc(funding 关的 v1 生产形态)。
package integration

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nexusapi-platform/enterprise/adapter/newapi"
)

// T-B1/B2:建组织 new-api 用户名为随机(ent_ 前缀、非 org<id>)且落库;幂等重开同组织读库同名、不重复建/不换名。
func TestIntegration_OrgUsernameRandomStored(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	svc, store, _ := v1Svc(t, ctx, "nexus_bun", true)
	const orgID = int64(950)
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO organization (id, name, slug) VALUES (?, 'bun-org', 'bun-slug')`, orgID); err != nil {
		t.Fatalf("建组织失败: %v", err)
	}
	cred1, perr := svc.EnsureOrgProvisioned(ctx, orgID, "bun-org")
	if perr != nil {
		t.Fatalf("首次开通失败: %v", perr)
	}
	org, _ := store.GetOrganization(ctx, orgID)
	if org.NewapiUsername == nil || *org.NewapiUsername == "" {
		t.Fatalf("🔴项B:门A 组织应把随机 new-api 用户名落库,实为空")
	}
	uname := *org.NewapiUsername
	if !strings.HasPrefix(uname, "ent_") {
		t.Fatalf("🔴用户名应为随机 ent_ 前缀,实=%q", uname)
	}
	if uname == "org950" || strings.HasPrefix(uname, "org9") {
		t.Fatalf("🔴用户名不应是可猜的 org<id>,实=%q", uname)
	}
	// 幂等重开(快路径 loadOrgCred):同 user、同名,不重复建、不换随机名。
	cred2, perr := svc.EnsureOrgProvisioned(ctx, orgID, "bun-org")
	if perr != nil {
		t.Fatalf("重开失败: %v", perr)
	}
	if cred2.NewapiUserID != cred1.NewapiUserID {
		t.Fatalf("🔴幂等重开应同一 org user:%d != %d", cred2.NewapiUserID, cred1.NewapiUserID)
	}
	org2, _ := store.GetOrganization(ctx, orgID)
	if org2.NewapiUsername == nil || *org2.NewapiUsername != uname {
		t.Fatalf("🔴幂等重开不应换随机名:%q → %v", uname, org2.NewapiUsername)
	}
	// new-api 侧确认该随机名的用户存在。
	ndb, _ := sql.Open("mysql", os.Getenv("NEXUS_IT_NEWAPI_SQL_DSN"))
	defer ndb.Close()
	var cnt int
	_ = ndb.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE username=? AND id=?`, uname, cred1.NewapiUserID).Scan(&cnt)
	if cnt != 1 {
		t.Fatalf("🔴new-api 侧应有该随机名用户,实=%d", cnt)
	}
	t.Logf("项B T-B1/B2 真账 ok: 门A 用户名随机(%s,非 org<id>)已落库;幂等重开同 user 同名不重复建不换名", uname)
}

// T-B3/B5/B6:撞名归属闸(适配器级,可确定性复现)——
//   AllowAdopt=false 撞已存在用户 → 返 UsernameConflict、绝不接管、**绝不 disable 那个用户**;
//   AllowAdopt=true 撞自己的名 → adopt 复用同 user。
func TestIntegration_BootstrapUsernameConflictGuard(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	_, _, upstream := v1Svc(t, ctx, "nexus_bcg", true)
	ndb, _ := sql.Open("mysql", os.Getenv("NEXUS_IT_NEWAPI_SQL_DSN"))
	defer ndb.Close()
	const uname = "bcg_ext_user"
	const pw = "Test1234!pwOk"

	// 预置一个"已存在"用户(代表外部用户):AllowAdopt=false 首建成功。
	first, err := upstream.BootstrapMember(ctx, newapi.BootstrapInput{
		OrgID: 9001, MemberID: 0, Username: uname, Password: pw, DisplayName: "ext", SkipToken: true, AllowAdopt: false,
	})
	if err != nil {
		t.Fatalf("预置用户失败: %v", err)
	}
	extID := first.NewapiUserID

	// T-B3/B5:再用 AllowAdopt=false 撞同名 → 必须返 UsernameConflict,且不接管、不 disable 外部用户。
	_, cerr := upstream.BootstrapMember(ctx, newapi.BootstrapInput{
		OrgID: 9002, MemberID: 0, Username: uname, Password: pw, DisplayName: "x", SkipToken: true, AllowAdopt: false,
	})
	if !newapi.IsUsernameConflict(cerr) {
		t.Fatalf("🔴项B:AllowAdopt=false 撞已存在用户应返 UsernameConflict,实=%v", cerr)
	}
	var st int
	_ = ndb.QueryRowContext(ctx, `SELECT status FROM users WHERE id=?`, extID).Scan(&st)
	if st != 1 {
		t.Fatalf("🔴项B 红线:撞名绝不能 disable 外部用户,实 status=%d(1=正常)", st)
	}

	// T-B6:AllowAdopt=true(重开自己的名)→ adopt 复用同一 user,不新建。
	adopt, aerr := upstream.BootstrapMember(ctx, newapi.BootstrapInput{
		OrgID: 9001, MemberID: 0, Username: uname, Password: pw, DisplayName: "ext", SkipToken: true, AllowAdopt: true,
	})
	if aerr != nil {
		t.Fatalf("🔴AllowAdopt=true 应 adopt 复用,实错: %v", aerr)
	}
	if adopt.NewapiUserID != extID {
		t.Fatalf("🔴adopt 应复用同一 user %d,实=%d", extID, adopt.NewapiUserID)
	}
	t.Logf("项B T-B3/B5/B6 真账 ok: AllowAdopt=false 撞名→UsernameConflict+外部用户不被disable(status=1);AllowAdopt=true→adopt复用同user")
}
