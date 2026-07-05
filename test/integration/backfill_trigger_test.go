// 历史回填 · 触发点 + B_ts==0 守卫集成(交付批次24 步骤5,AC-1/AC-12):
// 门B 关联成功 → 生成 pending 回填任务(边界=关联瞬间 forward 水位、username=企业自己的名);
// 且 cursor 未初始化时守卫强制 forward 基线,boundary_ts 必 >0(绝不 0=静默丢全部历史)。
package integration

import (
	"context"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/nexusapi-platform/enterprise/pkg/session"
	"github.com/nexusapi-platform/enterprise/service"
)

func TestIntegration_BackfillTriggerAndGuard(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	svc, store, upstream := v1Svc(t, ctx, "nexus_bftrig", true) // observe(v1 生产形态)
	db := store.DB()

	// AC-12 前置:清掉 v1Svc 预置的 settlement_cursor,模拟"forward 尚未首跑"(cursor 未初始化 → B_ts==0)。
	if _, err := db.ExecContext(ctx, `DELETE FROM settlement_cursor WHERE org_id = 0`); err != nil {
		t.Fatalf("清 cursor 失败: %v", err)
	}

	entCred := mkEnterpriseUser(t, ctx, upstream, "bftriguser")
	if err := upstream.AddOrgUsableGroup(ctx, "default", "vip"); err != nil {
		t.Fatalf("预配可用分组失败: %v", err)
	}
	opc := session.Claims{Role: session.RoleOperator}
	res, err := svc.CreateOrg(ctx, opc, service.CreateOrgInput{
		Name: "回填触发企业", Slug: "bftrig-ok", AdminEmail: "bftrig@t.local", NewapiUserGroup: "default",
		Associate: &service.AssociateOrgInput{NewapiUserID: int64(entCred.NewapiUserID), AccessToken: entCred.AccessToken, NamePolicy: "inherit"},
	})
	if err != nil {
		t.Fatalf("门B 关联失败: %v", err)
	}
	orgID := res.Org.ID

	// AC-1:关联成功 → 生成 pending 回填任务;username/user_id 正确。
	job, err := store.GetBackfillJob(ctx, orgID)
	if err != nil || job == nil {
		t.Fatalf("AC-1 关联应生成回填任务,实 job=%v err=%v", job, err)
	}
	if job.Status != "pending" {
		t.Errorf("AC-1 新任务应 pending,实=%s", job.Status)
	}
	if job.NewapiUsername != "bftriguser" {
		t.Errorf("AC-1 任务 username 应=bftriguser(企业自己的 new-api 名),实=%q", job.NewapiUsername)
	}
	if job.NewapiUserID != int64(entCred.NewapiUserID) {
		t.Errorf("AC-1 任务 user_id 应=%d,实=%d", entCred.NewapiUserID, job.NewapiUserID)
	}

	// AC-12:cursor 未初始化下,守卫强制 forward 基线 → boundary_ts 必 >0(绝不是 0=静默丢史);
	// cursor_ts==boundary_ts(未开始)。若守卫失效(直接以 (0,0) 为界)boundary_ts 会是 0。
	if job.BoundaryTS <= 0 {
		t.Fatalf("AC-12 守卫失效:cursor 未初始化时 boundary_ts 应被强制基线为 >0,实=%d(=0 会静默丢全部历史)", job.BoundaryTS)
	}
	if job.CursorTS != job.BoundaryTS {
		t.Errorf("AC-12 未开始任务 cursor_ts 应==boundary_ts=%d,实=%d", job.BoundaryTS, job.CursorTS)
	}
	// 守卫确实推了 forward 基线:settlement_cursor 现在应存在且 ts>0。
	var curTS int64
	_ = db.QueryRowContext(ctx, `SELECT COALESCE(last_settled_ts,0) FROM settlement_cursor WHERE org_id=0`).Scan(&curTS)
	if curTS <= 0 {
		t.Errorf("AC-12 守卫应已强制 forward 基线(cursor ts>0),实=%d", curTS)
	}

	t.Logf("回填触发+守卫 ok: 关联生成 pending 任务(username=%s user_id=%d);cursor 未初始化下守卫强制基线 boundary_ts=%d>0(不丢史)",
		job.NewapiUsername, job.NewapiUserID, job.BoundaryTS)
}
