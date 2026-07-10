// 45号-20:UI 形 body 用例——用**前端实际提交的字段名**走 HTTP 打后端,根治
// "e2e 用对名刷绿 CI、UI 用错名生产炸"(P1-1/P1-2 两次 400 都是这个缝漏的)。
// body 键与 web/app.js 的 readTierForm/doDeleteGrant/doAddGrant 逐字段同步;
// 前端若再改名,契约测(TestContract_FrontendBodyFieldsSubsetOfBackendTags)与本测双保险。
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/nexusapi-platform/enterprise/handler"
	"github.com/nexusapi-platform/enterprise/pkg/session"
)

func TestIntegration_UIShapeTierAndGrantBodies(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	svc, store, _, _ := escrowSvc(t, ctx, "nexus_uishape")
	const orgID = int64(701)
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO organization (id, name, slug) VALUES (?, 'ui-org', 'ui-slug')`, orgID); err != nil {
		t.Fatalf("建组织失败: %v", err)
	}
	// org_admin 会话(平台侧签发,走真 HTTP 鉴权)。
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO member (id, org_id, login_email, role, status, bootstrap_state) VALUES (9, ?, 'uiadmin@t.local', 'org_admin', 'active', 'done')`, orgID); err != nil {
		t.Fatalf("建管理员失败: %v", err)
	}
	// requireAuth 用 handler 的 signer 验 token(svc 不参与验签),故 escrowSvc 的 svc 可直接复用。
	signer, _ := session.NewSigner([]byte("integration-test-session-key-32b!!"), time.Hour)
	tok, _ := signer.Issue(session.Claims{Role: session.RoleOrgAdmin, OrgID: orgID, MemberID: 9})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(handler.New(svc, signer, log, "it").Routes())
	defer ts.Close()

	do := func(method, path string, body map[string]any) (int, map[string]any) {
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, ts.URL+"/api/v1"+path, rd)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		var env struct {
			Code int             `json:"code"`
			Data json.RawMessage `json:"data"`
			Msg  string          `json:"message"`
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(raw, &env)
		var data map[string]any
		_ = json.Unmarshal(env.Data, &data)
		if env.Code != 0 {
			t.Logf("%s %s -> HTTP %d code=%d msg=%s", method, path, resp.StatusCode, env.Code, env.Msg)
		}
		return env.Code, data
	}

	// ① 建档位:body 键 = readTierForm 实际形状(P1-1 曾提交 group/model_limits 必 400)。
	code, tier := do("POST", fmt.Sprintf("/orgs/%d/tiers", orgID), map[string]any{
		"name": "UI形档位", "quota_type": "fixed", "amount_raw": int64(2_000_000),
		"newapi_group": "default", "model_set": []string{},
	})
	if code != 0 {
		t.Fatalf("🔴UI 形 body 建档位应成功,实 code=%d(P1-1 类错名回归)", code)
	}
	tierID := int64(tier["id"].(float64))

	// ② 改档位:同形状 PUT。
	if code, _ := do("PUT", fmt.Sprintf("/tiers/%d", tierID), map[string]any{
		"name": "UI形档位改", "amount_raw": int64(3_000_000),
	}); code != 0 {
		t.Fatalf("🔴UI 形 body 改档位应成功,实 code=%d", code)
	}

	// ③ 授权(doAddGrant 形状 {target_type})→ ④ 取消授权(doDeleteGrant 形状 {grant_id},
	//    P1-2 曾读 g.id 恒 0 落 {target_type,target_id} 兜底必 400)。
	if code, _ := do("POST", fmt.Sprintf("/tiers/%d/grants", tierID), map[string]any{
		"target_type": "all",
	}); code != 0 {
		t.Fatalf("🔴UI 形 body 档位授权应成功,实 code=%d", code)
	}
	// 取 grant_id(前端从 tier.grants[].grant_id 读——响应字段名同步验证)。
	var tiersResp struct {
		List []struct {
			ID     int64 `json:"id"`
			Grants []struct {
				GrantID int64 `json:"grant_id"`
			} `json:"grants"`
		} `json:"list"`
	}
	req, _ := http.NewRequest("GET", ts.URL+fmt.Sprintf("/api/v1/orgs/%d/tiers", orgID), nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, _ := http.DefaultClient.Do(req)
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&env)
	resp.Body.Close()
	_ = json.Unmarshal(env.Data, &tiersResp)
	var gid int64
	for _, tt := range tiersResp.List {
		if tt.ID == tierID && len(tt.Grants) > 0 {
			gid = tt.Grants[0].GrantID
		}
	}
	if gid == 0 {
		t.Fatalf("🔴tier.grants[].grant_id 应非 0(前端取消授权靠它;P1-2 曾读不到)")
	}
	if code, _ := do("DELETE", fmt.Sprintf("/tiers/%d/grants", tierID), map[string]any{
		"grant_id": gid,
	}); code != 0 {
		t.Fatalf("🔴UI 形 body 取消授权应成功,实 code=%d", code)
	}
	t.Logf("45号-20 UI形body ok: 建档(newapi_group/model_set)/改档/授权/取消授权(grant_id=%d)全通", gid)
}
