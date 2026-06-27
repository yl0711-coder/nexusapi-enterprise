package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nexusapi-platform/enterprise/pkg/session"
	"github.com/nexusapi-platform/enterprise/service"
)

// TestMVPGate_BlocksMoneyAndControl 验证改动⑥-1 路由封锁:MVP_MODE 开时,钱/控写端点被 mvpGate 在鉴权前 404,
// 白名单写端点过闸(无 token → requireAuth 401)。不带 Authorization:
//   - 被封锁端点 → mvpGate 先于 requireAuth 返回 404(若没封锁,无 token 会是 401);
//   - 白名单端点 → 过闸到 requireAuth → 401(若被错封,会是 404)。
//
// 401 vs 404 干净区分"是否被封锁",无需真 token / MySQL / rc.4。
func TestMVPGate_BlocksMoneyAndControl(t *testing.T) {
	signer, err := session.NewSigner([]byte("mvp-gate-test-session-key-32bytes!"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	svc := service.New(service.Deps{Signer: signer, ObserveMode: true})
	ts := httptest.NewServer(New(svc, signer, nil, "mvp-test", true).Routes())
	defer ts.Close()

	do := func(method, path string) int {
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("req %s %s: %v", method, path, err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	// 钱/控端点必须 404(含回执显式点名的 billing-settings / debits)。
	blocked := [][2]string{
		{"POST", "/api/v1/approvals"},
		{"POST", "/api/v1/approvals/1/decide"},
		{"PUT", "/api/v1/organizations/1/quota-policies"},
		{"PUT", "/api/v1/organizations/1/approval-rules"},
		{"POST", "/api/v1/organizations/1/members:bulk"},
		{"POST", "/api/v1/organizations/1/members:bulk-status"},
		{"PATCH", "/api/v1/organizations/1/billing-settings"}, // 涉钱·显式断言
		{"POST", "/api/v1/organizations/1/debits"},            // 退款冲正·涉钱·显式断言
		{"POST", "/api/v1/members/1/quota:adjust"},
		{"POST", "/api/v1/members/1/grants"},
		{"DELETE", "/api/v1/grants/1"},
		{"POST", "/api/v1/organizations/1/recharges"},
		{"POST", "/api/v1/organizations/1/recharge-requests"},
		{"PUT", "/api/v1/organizations/1/pricing"},
		{"POST", "/api/v1/pricing/reconcile"},
		{"PUT", "/api/v1/organizations/1/default-token-group"},
	}
	for _, b := range blocked {
		if st := do(b[0], b[1]); st != http.StatusNotFound {
			t.Errorf("MVP 封锁:%s %s 应被 mvpGate 挡 404,实 %d", b[0], b[1], st)
		}
	}

	// 白名单写端点:过闸 → requireAuth 401(证明没被错误封锁),不是 404。
	allowed := [][2]string{
		{"POST", "/api/v1/organizations"},
		{"PATCH", "/api/v1/organizations/1"},
		{"POST", "/api/v1/organizations/1/teams"},
		{"POST", "/api/v1/organizations/1/tiers"},
		{"PUT", "/api/v1/tiers/1"},
		{"POST", "/api/v1/organizations/1/members"},
		{"POST", "/api/v1/members/1/status"},
		{"POST", "/api/v1/members/1/tokens"}, // 改动③ 自助建 key
		{"POST", "/api/v1/members/1/key:rotate"},
		{"POST", "/api/v1/me/password"}, // 个人设置·自助改密(MVP 放行)
	}
	for _, a := range allowed {
		if st := do(a[0], a[1]); st == http.StatusNotFound {
			t.Errorf("MVP 白名单:%s %s 不应被封锁(应过闸到 401),实 404", a[0], a[1])
		}
	}

	// GET 只读一律放行(过闸 → 401,不 404)。
	if st := do("GET", "/api/v1/organizations/1/usage"); st == http.StatusNotFound {
		t.Errorf("MVP:GET 只读不应被封锁,实 404")
	}
	// 注:POST /auth/login 也在白名单且无需鉴权,但本测试用极简 svc(store=nil),命中它会在 handler 内
	// nil 解引用(被 recover 成 500),不影响"是否封锁"的判定;故不在此断言,登录放行由 mvpWriteAllow 显式列出保证。
}

// TestMaxBodyBytes_OversizedRejected 验证 P2 健壮性加固:超过 1MB 的请求体被 http.MaxBytesReader
// 在 decode 时收敛成干净 400(结构化 4xx,非 500/非 panic/非 hang),保护 2G 无 swap 节点内存。
// 打公开且读 body 的登录端点(decodeJSON 在 svc.Login 之前,超限先于业务触发,故 store=nil 也不进业务)。
func TestMaxBodyBytes_OversizedRejected(t *testing.T) {
	signer, _ := session.NewSigner([]byte("mvp-gate-test-session-key-32bytes!"), time.Hour)
	svc := service.New(service.Deps{Signer: signer, ObserveMode: true})
	ts := httptest.NewServer(New(svc, signer, nil, "mvp-test", true).Routes())
	defer ts.Close()

	big := `{"email":"` + strings.Repeat("a", maxRequestBodyBytes+1024) + `","password":"x"}`
	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/auth/login", strings.NewReader(big))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("超大 body 请求不应 hang/断连: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("超大 body(>%dB)应被收敛成 400(非 500/panic),实 %d", maxRequestBodyBytes, resp.StatusCode)
	}
}

// TestMVPGate_OffPassesAll 验证 MVP_MODE 关时 mvpGate 不拦任何端点(钱端点无 token → 401,非 404)。
func TestMVPGate_OffPassesAll(t *testing.T) {
	signer, _ := session.NewSigner([]byte("mvp-gate-test-session-key-32bytes!"), time.Hour)
	svc := service.New(service.Deps{Signer: signer})
	ts := httptest.NewServer(New(svc, signer, nil, "mvp-test", false).Routes()) // mvpMode=false
	defer ts.Close()

	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/organizations/1/debits", strings.NewReader("{}"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		t.Errorf("MVP 关时 debits 不应被封锁 404(应过闸到 401),实 %d", resp.StatusCode)
	}
}
