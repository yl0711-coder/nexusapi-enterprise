// 架构B 阶段1(BE①):成员自助令牌纯函数单测(输入校验 / model_limits 解析 / 档位字段校验)。
// 真链路(建/改/删/揭示/上限并发)由容器集成栈覆盖(质量门铁律:本机集成包 ok 可能全 SKIP 假绿)。
package service

import (
	"testing"
)

func TestValidateAllowIPs(t *testing.T) {
	ok := []string{"", "  ", "203.0.113.5", "203.0.113.0/24", "203.0.113.5, 10.0.0.0/8", "::1", "2001:db8::/32"}
	for _, v := range ok {
		if err := validateAllowIPs(v); err != nil {
			t.Errorf("validateAllowIPs(%q) 应通过,实=%v", v, err)
		}
	}
	bad := []string{"abc", "300.1.1.1", "203.0.113.5;10.0.0.1", "10.0.0.0/33"}
	for _, v := range bad {
		if err := validateAllowIPs(v); err == nil {
			t.Errorf("validateAllowIPs(%q) 应拒", v)
		}
	}
}

func TestSplitModelLimits(t *testing.T) {
	if got := splitModelLimits(""); got != nil {
		t.Errorf("空串应返回 nil,实=%v", got)
	}
	got := splitModelLimits(" gpt-4o, claude-3 ,,gemini ")
	want := []string{"gpt-4o", "claude-3", "gemini"}
	if len(got) != len(want) {
		t.Fatalf("解析结果=%v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("解析结果=%v want %v", got, want)
		}
	}
}

func TestValidateTierArchB(t *testing.T) {
	pos := int64(1000)
	zero := int64(0)
	daily := "daily"
	badPeriod := "hourly"
	cases := []struct {
		name        string
		quotaType   string
		amount      *int64
		resetPeriod *string
		visibility  string
		wantErr     bool
	}{
		{"fixed 缺省全空", "", nil, nil, "", false},
		{"fixed 正额度", "fixed", &pos, nil, "assigned", false},
		{"subscription 带周期", "subscription", &pos, &daily, "all", false},
		{"额度 0 拒(0=没有额度不是无限)", "fixed", &zero, nil, "", true},
		{"subscription 缺周期拒", "subscription", &pos, nil, "", true},
		{"subscription 非法周期拒", "subscription", &pos, &badPeriod, "", true},
		{"fixed 带周期拒", "fixed", &pos, &daily, "", true},
		{"非法 quota_type 拒", "unlimited", &pos, nil, "", true},
		{"非法 visibility 拒", "fixed", &pos, nil, "everyone", true},
	}
	for _, tc := range cases {
		err := validateTierArchB(tc.quotaType, tc.amount, tc.resetPeriod, tc.visibility)
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: err=%v wantErr=%v", tc.name, err, tc.wantErr)
		}
	}
}
