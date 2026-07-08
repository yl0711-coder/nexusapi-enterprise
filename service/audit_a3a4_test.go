package service

import (
	"context"
	"testing"

	"github.com/nexusapi-platform/enterprise/pkg/session"
)

// TestUnit_MoneyOrKeyRedline 锁死红线枚举(A4 → 架构B 能力白名单):协助态须挡住"铸/读明文 key"与"动钱"端点;
// 架构B 改白名单制后,已退役路径(key:rotate 等)与新端点天然落红线(默认拒);离职因含退额划账改判红线。
func TestUnit_MoneyOrKeyRedline(t *testing.T) {
	cases := []struct {
		method, path string
		want         bool
	}{
		{"POST", "/api/v1/organizations/5/members", true},    // 开通成员 = 铸 key(核心补堵)
		{"POST", "/api/v1/members/9/tokens", true},           // 员工建 key
		{"POST", "/api/v1/members/9/key:rotate", true},       // 轮换 = 出新 key
		{"POST", "/api/v1/members/9/key:reveal", true},       // 揭示明文 key(收口 A:支持态硬挡运营方读明文)
		{"POST", "/api/v1/members/9/key:ip-whitelist", true}, // 动 key
		{"POST", "/api/v1/organizations/5/recharges", true},  // 动钱
		{"POST", "/api/v1/organizations/5/recharge-requests", true},
		{"PATCH", "/api/v1/organizations/5/billing-settings", true},
		{"PUT", "/api/v1/organizations/5/pricing", true},
		{"POST", "/api/v1/members/9/status", false},                  // 禁用成员 ≠ 铸 key,协助态可
		{"PATCH", "/api/v1/members/9", false},                        // 改名 ≠ 红线
		{"POST", "/api/v1/organizations/5/members/9/offboard", true}, // 架构B:离职=disable→退额,动钱入红线(33 §3.5)
	}
	for _, c := range cases {
		if got := isMoneyOrKeyRedline(c.method, c.path); got != c.want {
			t.Errorf("isMoneyOrKeyRedline(%s, %s)=%v 期望 %v", c.method, c.path, got, c.want)
		}
	}
}

// TestUnit_MeSupportSummary 锁死 A4:支持态 Me() 返回摘要、不再死查目标 org 成员 401(支持会话现在能进得去)。
func TestUnit_MeSupportSummary(t *testing.T) {
	s := New(Deps{}) // 支持态路径不查库,nil store 无妨
	m, err := s.Me(context.Background(), session.Claims{SupportSessionID: 7, OrgID: 42, MemberID: 3, Role: session.RoleOrgAdmin})
	if err != nil {
		t.Fatalf("支持态 Me 不应报错(A4:不再死查目标 org 的运营方成员): %v", err)
	}
	if m == nil || m.OrgID != 42 || m.Role != string(session.RoleOrgAdmin) {
		t.Fatalf("支持态摘要应带目标 org + 角色: %+v", m)
	}
}
