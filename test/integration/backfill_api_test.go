// 历史回填 · 状态查询 + 重新回填接口契约(交付批次24-§9,前端所依赖)。真 rc.4。
// 验 RBAC(运营方跨组织可读/可重跑;org_admin 读本组织可、越组织拒、不可重跑)+ 视图字段(done/起点/行数→pending)。
package integration

import (
	"context"
	"os"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/nexusapi-platform/enterprise/adapter/newapi"
	"github.com/nexusapi-platform/enterprise/pkg/session"
	"github.com/nexusapi-platform/enterprise/service"
)

func TestIntegration_BackfillStatusAndRequeue(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	svc, store, upstream, _, _ := concSvc(t, ctx, "nexus_bfapi", true)
	nsql := os.Getenv("NEXUS_IT_NEWAPI_SQL_DSN")

	entCred := mkEnterpriseUser(t, ctx, upstream, "apient")
	tokenID, err := upstream.CreateToken(ctx, entCred, newapi.TokenSpec{Name: "api_tok", UnlimitedQuota: true, ExpiredTime: -1})
	if err != nil {
		t.Fatalf("造令牌失败: %v", err)
	}
	if err := upstream.AddOrgUsableGroup(ctx, "default", "vip"); err != nil {
		t.Fatalf("预配分组失败: %v", err)
	}
	uid := int64(entCred.NewapiUserID)
	now := time.Now().Unix()
	seedLogWithUsername(t, nsql, uid, int64(tokenID), "apient", "gpt-api", 500, now-10000)

	res, err := svc.CreateOrg(ctx, opcOperator(), service.CreateOrgInput{
		Name: "接口企业", Slug: "api-ok", AdminEmail: "api@t.local", NewapiUserGroup: "default",
		Associate: &service.AssociateOrgInput{NewapiUserID: int64(entCred.NewapiUserID), AccessToken: entCred.AccessToken, NamePolicy: "inherit"},
	})
	if err != nil {
		t.Fatalf("关联失败: %v", err)
	}
	orgID := res.Org.ID
	for i := 0; i < 10; i++ {
		_ = svc.RunBackfillSlice(ctx)
		if j, _ := store.GetBackfillJob(ctx, orgID); j.Status == "done" {
			break
		}
	}

	op := opcOperator()
	admin := session.Claims{Role: session.RoleOrgAdmin, OrgID: orgID}

	// 视图:done + 起点 + 行数(运营方可读)。
	v, err := svc.GetBackfillStatus(ctx, op, orgID)
	if err != nil {
		t.Fatalf("运营方读状态失败: %v", err)
	}
	if v.Status != "done" {
		t.Fatalf("应 done,实 %s", v.Status)
	}
	if v.EarliestSeenTS == nil || *v.EarliestSeenTS != now-10000 {
		t.Fatalf("起点应=%d,实 %v", now-10000, v.EarliestSeenTS)
	}
	if v.RowsIngested != 1 {
		t.Fatalf("应灌 1 条,实 %d", v.RowsIngested)
	}
	// org_admin 读本组织可。
	if _, err := svc.GetBackfillStatus(ctx, admin, orgID); err != nil {
		t.Fatalf("org_admin 读本组织状态应可: %v", err)
	}
	// 越组织(org_admin 的 OrgID 不符)→ 拒。
	if _, err := svc.GetBackfillStatus(ctx, session.Claims{Role: session.RoleOrgAdmin, OrgID: orgID + 999}, orgID); err == nil {
		t.Fatalf("越组织读状态应拒")
	}

	// 重新回填:仅运营方。
	if err := svc.RequeueBackfill(ctx, admin, orgID); err == nil {
		t.Fatalf("org_admin 重新回填应拒(仅运营方)")
	}
	if err := svc.RequeueBackfill(ctx, op, orgID); err != nil {
		t.Fatalf("运营方重新回填失败: %v", err)
	}
	v2, _ := svc.GetBackfillStatus(ctx, op, orgID)
	if v2.Status != "pending" {
		t.Fatalf("重新回填后应回 pending,实 %s", v2.Status)
	}
	t.Logf("回填状态/重跑接口 ok: 运营方读 done(起点=%d/行数=%d);org_admin 读本组织可、越组织拒;重跑仅运营方、后回 pending", now-10000, v.RowsIngested)
}
