// 架构B 阶段1(BE①):money/key 红线「能力白名单」单测(31-ADR §10 / 33 §3.5)。
// 铁律:写请求不在白名单内即红线(默认拒)——新端点默认受保护;读恒非红线。
package service

import (
	"net/http"
	"testing"
)

func TestIsMoneyOrKeyRedline_Whitelist(t *testing.T) {
	cases := []struct {
		method, path string
		redline      bool
		why          string
	}{
		// 读恒非红线(只读态另有整体闸)。
		{http.MethodGet, "/api/v1/organizations/1/members", false, "GET 非红线"},
		{http.MethodHead, "/api/v1/me/tokens", false, "HEAD 非红线"},

		// 白名单内普通写:放行。
		{http.MethodPatch, "/api/v1/organizations/12", false, "改组织资料"},
		{http.MethodPost, "/api/v1/organizations/12/teams", false, "建团队"},
		{http.MethodPatch, "/api/v1/organizations/12/teams/3", false, "团队改名"},
		{http.MethodPost, "/api/v1/organizations/12/tiers", false, "建档位(模板)"},
		{http.MethodPut, "/api/v1/tiers/5", false, "改档位"},
		{http.MethodPost, "/api/v1/tiers/5/default", false, "设默认档"},
		{http.MethodPatch, "/api/v1/members/7", false, "改成员资料"},
		{http.MethodPost, "/api/v1/members/7/status", false, "停用/启用"},
		{http.MethodPost, "/api/v1/notifications/9/read", false, "通知已读"},

		// 动钱:全红线。
		{http.MethodPost, "/api/v1/organizations/12/members", true, "开通成员=建号+首笔划账+凭证回显"},
		{http.MethodPost, "/api/v1/organizations/12/members:bulk", true, "批量开通"},
		{http.MethodPost, "/api/v1/members/7/offboard", true, "离职退额动钱"},
		{http.MethodPost, "/api/v1/members/7/restore", true, "恢复重新分配动钱"},
		{http.MethodPost, "/api/v1/members/7/quota:grant", true, "追加划账"},
		{http.MethodPost, "/api/v1/members/7/quota:adjust", true, "调额"},
		{http.MethodPost, "/api/v1/members/7/grants", true, "临时权限含额度"},
		{http.MethodPost, "/api/v1/organizations/12/recharges", true, "充值"},
		{http.MethodPost, "/api/v1/organizations/12/debits", true, "扣款"},
		{http.MethodPut, "/api/v1/organizations/12/pricing", true, "计价"},
		{http.MethodPatch, "/api/v1/organizations/12/billing-settings", true, "计费开关"},
		{http.MethodPut, "/api/v1/organizations/12/default-token-group", true, "默认计价分组"},
		{http.MethodPost, "/api/v1/organizations/12/hard-stop", true, "硬停"},
		{http.MethodPost, "/api/v1/tiers/5/grants", true, "档位授权=计价档访问控制"},

		// 铸/读/改 key:全红线(/me/tokens 写、key:reveal)。
		{http.MethodPost, "/api/v1/me/tokens", true, "建令牌"},
		{http.MethodPatch, "/api/v1/me/tokens/3", true, "改令牌"},
		{http.MethodDelete, "/api/v1/me/tokens/3", true, "删令牌"},
		{http.MethodPost, "/api/v1/me/tokens/3/key:reveal", true, "揭示明文"},

		// 会话接管口:password:reset 可借改密登成成员再揭 key → 必须红线(白名单显式不列)。
		{http.MethodPost, "/api/v1/members/7/password:reset", true, "改密接管绕 key 红线"},

		// 审批通过会触发额度下发 → 红线(默认拒)。
		{http.MethodPost, "/api/v1/approvals/3/decide", true, "审批放钱"},

		// 未来新增端点:默认落红线(倒转黑名单的意义所在)。
		{http.MethodPost, "/api/v1/some/new/endpoint", true, "新端点默认红线"},
	}
	for _, tc := range cases {
		if got := isMoneyOrKeyRedline(tc.method, tc.path); got != tc.redline {
			t.Errorf("isMoneyOrKeyRedline(%s %s)=%v, want %v(%s)", tc.method, tc.path, got, tc.redline, tc.why)
		}
	}
}

func TestMatchEndpointPattern(t *testing.T) {
	cases := []struct {
		pattern, method, path string
		want                  bool
	}{
		{"POST /api/v1/organizations/*/teams", "POST", "/api/v1/organizations/12/teams", true},
		{"POST /api/v1/organizations/*/teams", "POST", "/api/v1/organizations/12/teams/3", false}, // 段数不等
		{"POST /api/v1/organizations/*/teams", "PUT", "/api/v1/organizations/12/teams", false},    // method 不符
		{"PATCH /api/v1/members/*", "PATCH", "/api/v1/members/7", true},
		// `*` 恰好一个段:members:bulk 是独立段值,不被 /members 模式误吞。
		{"POST /api/v1/organizations/*/members", "POST", "/api/v1/organizations/12/members:bulk", false},
	}
	for _, tc := range cases {
		if got := matchEndpointPattern(tc.pattern, tc.method, tc.path); got != tc.want {
			t.Errorf("matchEndpointPattern(%q,%s,%s)=%v want %v", tc.pattern, tc.method, tc.path, got, tc.want)
		}
	}
}
