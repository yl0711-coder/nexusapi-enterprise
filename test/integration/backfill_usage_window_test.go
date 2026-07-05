// 历史回填 · 用量窗口(24 验收·产品建议的后端印证)。真 rc.4。
// 回填的老历史(20天前)在近7天默认窗看不到、在"全部历史"窗看得到 —— 印证前端加"全部历史"档 +
// 门B 默认切它的必要性(否则客户第一眼以为没数据)。
package integration

import (
	"context"
	"os"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/nexusapi-platform/enterprise/adapter/newapi"
	"github.com/nexusapi-platform/enterprise/service"
)

func TestIntegration_BackfillUsageWideWindow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	svc, store, upstream, _, _ := concSvc(t, ctx, "nexus_bfwin", true)
	nsql := os.Getenv("NEXUS_IT_NEWAPI_SQL_DSN")

	entCred := mkEnterpriseUser(t, ctx, upstream, "winent")
	tokenID, err := upstream.CreateToken(ctx, entCred, newapi.TokenSpec{Name: "win_tok", UnlimitedQuota: true, ExpiredTime: -1})
	if err != nil {
		t.Fatalf("造令牌失败: %v", err)
	}
	if err := upstream.AddOrgUsableGroup(ctx, "default", "vip"); err != nil {
		t.Fatalf("预配分组失败: %v", err)
	}
	uid := int64(entCred.NewapiUserID)
	const q = int64(4200)
	seedLogWithUsername(t, nsql, uid, int64(tokenID), "winent", "gpt-win", q, time.Now().Unix()-20*24*3600) // 20 天前

	res, err := svc.CreateOrg(ctx, opcOperator(), service.CreateOrgInput{
		Name: "窗口企业", Slug: "win-ok", AdminEmail: "win@t.local", NewapiUserGroup: "default",
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
	// 近 7 天窗口:看不到 20 天前的回填历史(这正是客户"第一眼以为没数据"的成因)。
	u7, err := svc.OrgUsage(ctx, op, orgID, 168)
	if err != nil {
		t.Fatalf("近7天用量失败: %v", err)
	}
	if u7.TotalQuota != 0 {
		t.Fatalf("近7天窗口不应含 20 天前回填历史,实 total=%d", u7.TotalQuota)
	}
	// 全部历史(前端 WIN_ALL=200000h):看得到。
	uAll, err := svc.OrgUsage(ctx, op, orgID, 200000)
	if err != nil {
		t.Fatalf("全部历史用量失败: %v", err)
	}
	if uAll.TotalQuota != q {
		t.Fatalf("全部历史窗口应含回填历史 %d,实 total=%d", q, uAll.TotalQuota)
	}
	t.Logf("全部历史窗口 ok: 近7天看不到20天前回填历史(0),全部历史看得到(%d)——印证前端加'全部历史'档 + 门B 默认切它", q)
}
