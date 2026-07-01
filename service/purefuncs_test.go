package service

// B档#2:涉钱/涉时/校验纯函数单测(无 DB/网络,service 包内 internal test)。跑法见 test 容器命令加 ./service/。
// 覆盖代码审查测试总监点名的现成纯函数:补货点 clamp(涉钱)、周期/时长边界(涉时)、脱敏、二道闸、幂等键派生、令牌分组(涉价)。

import (
	"strings"
	"testing"
	"time"

	"github.com/nexusapi-platform/enterprise/model"
)

func TestUnit_ComputeAutoThreshold(t *testing.T) {
	// 历史<7天 → DEFAULT_NEW,不 overCeil。
	if got, over := computeAutoThreshold(999, 999, false); got != escrowDefaultNew || over {
		t.Fatalf("无历史应=DEFAULT_NEW(%d,false),得(%d,%v)", escrowDefaultNew, got, over)
	}
	// 有历史、raw 低于地板 → FLOOR。
	if got, over := computeAutoThreshold(0, 0, true); got != escrowFloor || over {
		t.Fatalf("raw 低于地板应 clamp 到 FLOOR(%d,false),得(%d,%v)", escrowFloor, got, over)
	}
	// 有历史、raw 在区间 → raw(=peak×0.75 + maxSingle)。
	// peak=100_000_000 → int64(1e8×0.5×1.5)=75_000_000;+ maxSingle 50_000_000 = 125_000_000。
	if got, over := computeAutoThreshold(100_000_000, 50_000_000, true); got != 125_000_000 || over {
		t.Fatalf("区间内应=raw(125000000,false),得(%d,%v)", got, over)
	}
	// 有历史、raw 超上限 → CEIL + overCeil=true(超重度组织告警)。
	if got, over := computeAutoThreshold(10_000_000_000, 0, true); got != escrowCeil || !over {
		t.Fatalf("raw 超上限应 clamp 到 CEIL(%d,true),得(%d,%v)", escrowCeil, got, over)
	}
}

func TestUnit_PeriodBoundary(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 6, 17, 13, 45, 30, 0, loc) // 2026-06-17
	if got := periodBoundary("daily", now, loc); !got.Equal(time.Date(2026, 6, 17, 0, 0, 0, 0, loc)) {
		t.Fatalf("daily 应=当日 00:00,得 %v", got)
	}
	if got := periodBoundary("monthly", now, loc); !got.Equal(time.Date(2026, 6, 1, 0, 0, 0, 0, loc)) {
		t.Fatalf("monthly 应=当月 1 日 00:00,得 %v", got)
	}
	wk := periodBoundary("weekly", now, loc) // 周边界=本周一 00:00
	if wk.Weekday() != time.Monday || wk.Hour() != 0 || wk.After(now) {
		t.Fatalf("weekly 应=本周一 00:00 且不晚于 now,得 %v(%s)", wk, wk.Weekday())
	}
	if got := periodBoundary("bad", now, loc); !got.IsZero() {
		t.Fatalf("未知周期应返回零值,得 %v", got)
	}
}

func TestUnit_DurationToExpiry(t *testing.T) {
	now := time.Date(2026, 6, 17, 10, 0, 0, 0, time.UTC)
	if got, err := durationToExpiry("3d", now); err != nil || !got.Equal(now.Add(72*time.Hour)) {
		t.Fatalf("3d 应=now+72h,得 %v err=%v", got, err)
	}
	if got, err := durationToExpiry("week", now); err != nil || !got.Equal(now.AddDate(0, 0, 7)) {
		t.Fatalf("week 应=now+7d,得 %v err=%v", got, err)
	}
	if got, err := durationToExpiry("", now); err != nil || !got.Equal(time.Date(2026, 6, 18, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("空(today)应=次日 00:00 UTC,得 %v err=%v", got, err)
	}
	if _, err := durationToExpiry("garbage", now); err == nil {
		t.Fatalf("非法时长应报错")
	}
	if _, err := durationToExpiry(now.Add(-time.Hour).Format(time.RFC3339), now); err == nil {
		t.Fatalf("过去的 RFC3339 应报错(须未来)")
	}
}

func TestUnit_MaskKey(t *testing.T) {
	if got := maskKey("sk-nexus-abcdef1234"); got != "sk-nexus••••1234" {
		t.Fatalf("脱敏应留头8+末4,得 %q", got)
	}
	if got := maskKey("abc"); got != "••••" {
		t.Fatalf("过短应全打码,得 %q", got)
	}
}

func TestUnit_CheckText(t *testing.T) {
	ok := []string{"正常备注", `客户说"要加额度"`, "含反斜杠 a\\b", "多行\n第二行\t制表"}
	for _, s := range ok {
		if err := checkText("备注", s, maxNoteLen); err != nil {
			t.Fatalf("自由文本应放行 %q,却报错: %v", s, err)
		}
	}
	bad := []string{"<script>alert(1)</script>", "含标签 <b>", "控制字符 a\x00b"}
	for _, s := range bad {
		if err := checkText("备注", s, maxNoteLen); err == nil {
			t.Fatalf("应挡住 XSS 向量/控制字符 %q", s)
		}
	}
	if err := checkText("备注", strings.Repeat("a", maxNoteLen+1), maxNoteLen); err == nil {
		t.Fatalf("超长应报错")
	}
}

func TestUnit_CheckName(t *testing.T) {
	if err := checkName("姓名", "张三 John", maxNameLen); err != nil {
		t.Fatalf("正常名应放行: %v", err)
	}
	for _, s := range []string{`a"b`, "a'b", "a`b", "a<b", "a\\b", "a\x01b"} {
		if err := checkName("姓名", s, maxNameLen); err == nil {
			t.Fatalf("name 黑名单应挡住 %q", s)
		}
	}
}

func TestUnit_DeriveKeys(t *testing.T) {
	if got := deriveOrgUsername(123456); got != "org123456" || len(got) > 20 {
		t.Fatalf("org username 应=org{id} 且<=20,得 %q", got)
	}
	if got := deriveTokenName(45, 2); got != "nexus_m45_v2" {
		t.Fatalf("token name 应=nexus_m{id}_v{rot},得 %q", got)
	}
}

func TestUnit_ResolveTokenGroup(t *testing.T) {
	g := "vip"
	tier := &model.Tier{NewapiGroup: &g}
	if got := resolveTokenGroup(tier, nil); got != "vip" {
		t.Fatalf("有档分组应用档,得 %q", got)
	}
	og := "enterprise"
	org := &model.Organization{DefaultTokenGroup: &og}
	if got := resolveTokenGroup(nil, org); got != "enterprise" {
		t.Fatalf("无档有 org 默认应用 org 默认,得 %q", got)
	}
	if got := resolveTokenGroup(nil, nil); got != "default" {
		t.Fatalf("都无应回退 default,得 %q", got)
	}
}
