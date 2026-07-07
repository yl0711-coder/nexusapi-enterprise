package service

// B档#2:涉钱/涉时/校验纯函数单测(无 DB/网络,service 包内 internal test)。跑法见 test 容器命令加 ./service/。
// 覆盖代码审查测试总监点名的现成纯函数:补货点 clamp(涉钱)、周期/时长边界(涉时)、脱敏、二道闸、幂等键派生、令牌分组(涉价)。

import (
	"strings"
	"testing"
	"time"

	"github.com/nexusapi-platform/enterprise/pkg/session"
	"github.com/nexusapi-platform/enterprise/repo"
	"github.com/nexusapi-platform/enterprise/model"
)

// A4(五路复测):门B 导入令牌名清洗——剥危险字符 + rune 安全截断(不切碎多字节)。
func TestUnit_SanitizeExternalName(t *testing.T) {
	if got := sanitizeExternalName(`<img onerror=x>张三`); got != "img onerror=x张三" {
		t.Fatalf("应剥 <>,得 %q", got)
	}
	if got := sanitizeExternalName("'; DROP TABLE users;--"); strings.ContainsAny(got, "<>\"'`\\") {
		t.Fatalf("应剥引号/反斜杠,得 %q", got)
	}
	if got := sanitizeExternalName("a\x00b`c"); got != "abc" {
		t.Fatalf("应剥控制字符与反引号,得 %q", got)
	}
	// rune 安全截断:全中文名超上限,截后仍是合法 UTF-8(不出现半个字符)。
	long := strings.Repeat("超", maxNameLen+5)
	got := sanitizeExternalName(long)
	if r := []rune(got); len(r) != maxNameLen {
		t.Fatalf("应按 rune 截到 %d 字符,得 %d", maxNameLen, len(r))
	}
	if !utf8ValidString(got) {
		t.Fatalf("截断后应仍是合法 UTF-8(不切碎 rune),得 %q", got)
	}
}

func utf8ValidString(s string) bool {
	for _, r := range s {
		if r == 0xFFFD { // RuneError:出现即说明有非法字节序列
			return false
		}
	}
	return true
}

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

// TestUnit_SanitizeOther 锁死 26-§4.2:低权 other 净化必须删 admin_info/stream_status,
// 保留计费过程/缓存/frt 等展开所需字段;空串原样、坏 JSON 返空串(绝不返半截,否则前端展开 parse 失败)。
func TestUnit_SanitizeOther(t *testing.T) {
	raw := `{"frt":123,"cache_tokens":50,"reasoning_effort":"high","admin_info":{"channel_id":9},"stream_status":"ok","po":["a"]}`
	got := sanitizeOther(raw)
	for _, banned := range []string{"admin_info", "stream_status"} {
		if strings.Contains(got, banned) {
			t.Fatalf("低权 other 仍含 %q: %s", banned, got)
		}
	}
	for _, keep := range []string{"frt", "cache_tokens", "reasoning_effort", "po"} {
		if !strings.Contains(got, keep) {
			t.Fatalf("低权 other 误删了展开所需字段 %q: %s", keep, got)
		}
	}
	if sanitizeOther("") != "" {
		t.Fatal("空串应原样返回空串")
	}
	if got := sanitizeOther(`{"frt":1,"admin_info":{trunc`); got != "" {
		t.Fatalf("坏/半截 JSON 必须返回空串,不得返半截,得 %q", got)
	}
}

// TestUnit_SanitizeLogsByRole 锁死隔离洞:低权响应体不得含顶层渠道系/内部归因 id;
// 员工再去令牌名/分组;超管(operator)原样不动。这是"真隔离,不靠前端藏"的服务端保证。
func TestUnit_SanitizeLogsByRole(t *testing.T) {
	mk := func() []repo.OrgNewapiLog {
		return []repo.OrgNewapiLog{{
			ChannelID: 9, ChannelName: "ch-a", NewapiUserID: 100, NewapiTokenID: 200, KeyID: 7,
			TokenName: "nexus_m1_v1", GroupName: "vip", IP: "1.2.3.4",
			Content: "x", Other: `{"frt":1,"admin_info":{"channel_id":9}}`,
		}}
	}

	// 超管:原样,渠道/归因 id/other 全保留。
	op := mk()
	sanitizeLogsByRole(op, session.RoleOperator)
	if op[0].ChannelName == "" || op[0].NewapiTokenID == 0 || !strings.Contains(op[0].Other, "admin_info") {
		t.Fatalf("超管必须原样返回,不得剥离: %+v", op[0])
	}

	// 组织管理员:去顶层渠道系与内部归因 id;令牌名/分组保留(其视角要看成员令牌)。
	oa := mk()
	sanitizeLogsByRole(oa, session.RoleOrgAdmin)
	if oa[0].ChannelID != 0 || oa[0].ChannelName != "" || oa[0].NewapiTokenID != 0 || oa[0].NewapiUserID != 0 || oa[0].KeyID != 0 {
		t.Fatalf("组织管理员响应体仍含渠道系/内部归因 id: %+v", oa[0])
	}
	if oa[0].TokenName == "" {
		t.Fatal("组织管理员应保留令牌名(需看成员令牌维度)")
	}
	if strings.Contains(oa[0].Other, "admin_info") {
		t.Fatal("组织管理员 other 仍含 admin_info")
	}

	// 员工:在组织管理员基础上再去令牌名/分组。
	mb := mk()
	sanitizeLogsByRole(mb, session.RoleMember)
	if mb[0].TokenName != "" || mb[0].GroupName != "" {
		t.Fatalf("员工应去令牌名/分组: %+v", mb[0])
	}
	if mb[0].ChannelName != "" || mb[0].NewapiTokenID != 0 {
		t.Fatalf("员工响应体仍含渠道系/内部归因 id: %+v", mb[0])
	}
}
