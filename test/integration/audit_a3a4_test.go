// 全平台深审 · A3 会话代次(session_epoch)回归。真 MySQL。
package integration

import (
	"context"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/nexusapi-platform/enterprise/pkg/session"
)

// TestIntegration_A3_SessionEpochRevocation:A3——改角色/禁用后旧 token(旧 epoch/disabled)即失效,新 token 有效。
func TestIntegration_A3_SessionEpochRevocation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	svc, store, _ := v1Svc(t, ctx, "nexus_a3ep", true)
	const orgID, memberID = int64(1), int64(1)
	db := store.DB()
	if _, err := db.ExecContext(ctx, `INSERT INTO organization (id, name, slug) VALUES (?, 'a3-org', 'a3-slug')`, orgID); err != nil {
		t.Fatalf("建组织失败: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO member (id, org_id, login_email, role, status, bootstrap_state) VALUES (?, ?, 'a3@t.local', 'member', 'active', 'done')`,
		memberID, orgID); err != nil {
		t.Fatalf("建成员失败: %v", err)
	}

	// 初始:epoch 0、active → 会话有效。
	claims := session.Claims{OrgID: orgID, MemberID: memberID, Role: session.RoleMember, Epoch: 0}
	if err := svc.ValidateSession(ctx, claims); err != nil {
		t.Fatalf("初始会话应有效: %v", err)
	}

	// 角色漂移堵:改角色 → epoch 自增 → 旧 token(epoch 0)失效。
	admin := session.Claims{Role: session.RoleOrgAdmin, OrgID: orgID}
	if err := svc.AssignRole(ctx, admin, orgID, memberID, string(session.RoleTeamLeader)); err != nil {
		t.Fatalf("改角色失败: %v", err)
	}
	if err := svc.ValidateSession(ctx, claims); err == nil {
		t.Fatalf("A3:改角色后旧 token(epoch 0)应失效——堵角色漂移(降级者旧 token 自改回)")
	}

	// 新 token(epoch 1)有效。
	claims.Epoch, claims.Role = 1, session.RoleTeamLeader
	if err := svc.ValidateSession(ctx, claims); err != nil {
		t.Fatalf("改角色后新 token(epoch 1)应有效: %v", err)
	}

	// 禁用即失效:置 disabled → 下一请求即拒(不必等 12h)。
	if _, err := db.ExecContext(ctx, `UPDATE member SET status = 'disabled' WHERE id = ?`, memberID); err != nil {
		t.Fatalf("置禁用失败: %v", err)
	}
	if err := svc.ValidateSession(ctx, claims); err == nil {
		t.Fatalf("A3:禁用后下一请求应即失效")
	}
	t.Logf("A3 ok: 改角色 epoch 自增作废旧 token(角色漂移堵)+ 新 token 有效;禁用即失效")
}
