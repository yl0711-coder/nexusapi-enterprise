// 架构B 阶段1(BE①):assertSelf 统一中间件单测——成员传别人 id 一律 403,本人放行;上级角色不受限。
package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nexusapi-platform/enterprise/pkg/session"
)

func TestMemberSelfGuard(t *testing.T) {
	h := &Handler{}
	mux := http.NewServeMux()
	called := false
	mux.HandleFunc("POST /api/v1/members/{id}/status", h.memberSelfGuard(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	do := func(c session.Claims, path string) int {
		called = false
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req = req.WithContext(context.WithValue(req.Context(), ctxKeyClaims, c))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code
	}

	member := session.Claims{Role: session.RoleMember, MemberID: 7, OrgID: 1}
	// 成员操作本人:放行。
	if code := do(member, "/api/v1/members/7/status"); code != http.StatusOK || !called {
		t.Fatalf("成员操作本人应放行,code=%d called=%v", code, called)
	}
	// 成员传别人 id:403,业务 handler 不触达。
	if code := do(member, "/api/v1/members/8/status"); code != http.StatusForbidden || called {
		t.Fatalf("成员传他人 id 应 403 且不触达业务,code=%d called=%v", code, called)
	}
	// 上级角色(org_admin)不受限,由端点内 RBAC 判定。
	admin := session.Claims{Role: session.RoleOrgAdmin, MemberID: 99, OrgID: 1}
	if code := do(admin, "/api/v1/members/8/status"); code != http.StatusOK || !called {
		t.Fatalf("org_admin 应由中间件放行(端点内再判),code=%d called=%v", code, called)
	}
}
