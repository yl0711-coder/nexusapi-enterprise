// 深审 C22:管理员重置成员登录密码。真 MySQL。
package integration

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/nexusapi-platform/enterprise/pkg/session"
)

func TestIntegration_C22_ResetMemberPassword(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	svc, store, _ := v1Svc(t, ctx, "nexus_c22", true)
	const orgID, memberID = int64(1), int64(1)
	db := store.DB()
	if _, err := db.ExecContext(ctx, `INSERT INTO organization (id, name, slug) VALUES (?, 'c22-org', 'c22-slug')`, orgID); err != nil {
		t.Fatalf("建组织失败: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO member (id, org_id, login_email, role, status, bootstrap_state) VALUES (?, ?, 'c22@t.local', 'member', 'active', 'done')`,
		memberID, orgID); err != nil {
		t.Fatalf("建成员失败: %v", err)
	}

	// 非管理员(成员本人)不可重置他人/触发管理端点。
	memberClaims := session.Claims{Role: session.RoleMember, OrgID: orgID, MemberID: memberID}
	if _, err := svc.ResetMemberPassword(ctx, memberClaims, orgID, memberID); err == nil {
		t.Fatalf("C22:成员角色不应能调用重置(仅 org_admin/team_leader)")
	}

	// org_admin 重置 → 返回新密码 + bump epoch(作废旧会话)+ 落密码哈希。
	admin := session.Claims{Role: session.RoleOrgAdmin, OrgID: orgID}
	pw, err := svc.ResetMemberPassword(ctx, admin, orgID, memberID)
	if err != nil {
		t.Fatalf("重置失败: %v", err)
	}
	if len(pw) < 8 {
		t.Fatalf("C22:应返回新初始密码(>=8),实 %q", pw)
	}
	var epoch int
	var hash sql.NullString
	_ = db.QueryRowContext(ctx, `SELECT session_epoch, platform_password_hash FROM member WHERE id=?`, memberID).Scan(&epoch, &hash)
	if epoch != 1 {
		t.Fatalf("C22:重置应 bump epoch 作废旧会话,实 epoch=%d", epoch)
	}
	if !hash.Valid || hash.String == "" {
		t.Fatalf("C22:重置应落新密码哈希")
	}
	t.Logf("C22 ok: org_admin 重置返回新密码(len=%d)、epoch bump=%d 作废旧会话、密码哈希已落;成员角色被拒", len(pw), epoch)
}
