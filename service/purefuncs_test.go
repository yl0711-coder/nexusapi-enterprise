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

// 日志脱敏纯函数测试已随实现下沉 repo 层(31-ADR §8):见 repo/log_redact_test.go
// (含 [v3 反转]:成员保留 token_name/group_name)。

// TestUnit_DecideAttribution 锁死架构B归因决策(33 §3.3 AttributeLog):user_id 主键 +
// token→member 与 member.newapi_user_id→org 一致性断言,不一致落未归因桶(member=nil)+ mismatch。
func TestUnit_DecideAttribution(t *testing.T) {
	m1 := &model.Member{ID: 11, OrgID: 1}
	m2 := &model.Member{ID: 22, OrgID: 1}
	mOther := &model.Member{ID: 33, OrgID: 2}

	// A: user=成员 且 token=同一成员 → 归成员 + key。
	if a := decideAttribution(m1, 0, tokenAttr{found: true, member: m1, keyID: 7}); a.member != m1 || a.keyID != 7 || a.mismatch || a.skip {
		t.Fatalf("A: user与token同成员应归该成员+key, 实 %+v", a)
	}
	// B: user=成员、无/未登记 token → 归成员,key=0。
	if a := decideAttribution(m1, 0, tokenAttr{}); a.member != m1 || a.keyID != 0 || a.mismatch {
		t.Fatalf("B: user=成员无token应归成员, 实 %+v", a)
	}
	// C: user=成员 但 token=别的成员 → 未归因桶(user 侧组织)+ mismatch。
	if a := decideAttribution(m1, 0, tokenAttr{found: true, member: m2, keyID: 9}); a.member != nil || !a.mismatch || a.orgID != 1 || a.keyID != 0 {
		t.Fatalf("C: token串成员应落未归因桶+mismatch, 实 %+v", a)
	}
	// D: user=金库 且 token=同组织成员(A 版遗留)→ 按 token 归成员。
	if a := decideAttribution(nil, 1, tokenAttr{found: true, member: m2, keyID: 9}); a.member != m2 || a.keyID != 9 || a.mismatch {
		t.Fatalf("D: 金库user+同组织token应按token归因, 实 %+v", a)
	}
	// E: user=金库 但 token=他组织成员 → 未归因桶(金库组织)+ mismatch(防跨组织串台)。
	if a := decideAttribution(nil, 1, tokenAttr{found: true, member: mOther, keyID: 9}); a.member != nil || !a.mismatch || a.orgID != 1 {
		t.Fatalf("E: 跨组织token必须落未归因桶+mismatch, 实 %+v", a)
	}
	// F: user=金库、token 未登记 → 未归因桶,无 mismatch(门B/金库自身日志)。
	if a := decideAttribution(nil, 1, tokenAttr{}); a.member != nil || a.mismatch || a.orgID != 1 || a.skip {
		t.Fatalf("F: 金库未登记token应落未归因桶, 实 %+v", a)
	}
	// G: user 非平台 → skip(主站客户)。
	if a := decideAttribution(nil, 0, tokenAttr{}); !a.skip {
		t.Fatalf("G: 非平台user必须skip, 实 %+v", a)
	}
	// 跨组织攻击面:user=成员(org1) token=成员(org2) → 绝不能归到任何成员。
	if a := decideAttribution(m1, 0, tokenAttr{found: true, member: mOther, keyID: 5}); a.member != nil || !a.mismatch {
		t.Fatalf("跨组织串台必须拦下, 实 %+v", a)
	}
}

// TestUnit_LedgerEntryView 锁死账本可见性脱敏(31-ADR §15 + 组长裁定):direction 相对金库派生;
// from/to_user_id/idempotency_key 仅超管;created_by 超管+组织管理员;成员只见方向/金额/状态。
func TestUnit_LedgerEntryView(t *testing.T) {
	tr := &repo.LedgerTransfer{
		ID: 1, OrgID: 3, FromUserID: 100, ToUserID: 200, MemberID: 9,
		AmountRaw: 50_000_000, IdempotencyKey: "grant:9:x", Status: repo.LedgerApplied,
		Reason: "grant", CreatedBy: "org_admin:2",
	}
	// 金库=100 → from=金库 → credit。
	op := ledgerEntryView(tr, 100, session.RoleOperator)
	if op.Direction != LedgerDirCredit || op.FromUserID != 100 || op.IdempotencyKey == "" || op.CreatedBy == "" {
		t.Fatalf("超管应全量+credit, 实 %+v", op)
	}
	oa := ledgerEntryView(tr, 100, session.RoleOrgAdmin)
	if oa.FromUserID != 0 || oa.ToUserID != 0 || oa.IdempotencyKey != "" {
		t.Fatalf("组织管理员不得见内部 user id/幂等键, 实 %+v", oa)
	}
	if oa.CreatedBy == "" || oa.Direction != LedgerDirCredit || oa.AmountRaw != 50_000_000 {
		t.Fatalf("组织管理员应见 created_by/direction/amount, 实 %+v", oa)
	}
	mb := ledgerEntryView(tr, 100, session.RoleMember)
	if mb.FromUserID != 0 || mb.ToUserID != 0 || mb.IdempotencyKey != "" || mb.CreatedBy != "" {
		t.Fatalf("成员视图泄露内部字段: %+v", mb)
	}
	// 退额方向:to=金库 → debit。
	back := &repo.LedgerTransfer{ID: 2, OrgID: 3, FromUserID: 200, ToUserID: 100, MemberID: 9, AmountRaw: 1, Status: repo.LedgerApplied}
	if v := ledgerEntryView(back, 100, session.RoleOrgAdmin); v.Direction != LedgerDirDebit {
		t.Fatalf("成员→金库应为 debit, 实 %+v", v)
	}
	// 金库未知 → direction 空串(不瞎猜)。
	if v := ledgerEntryView(tr, 0, session.RoleOperator); v.Direction != "" {
		t.Fatalf("金库未知不得派生方向, 实 %+v", v)
	}
}

// TestUnit_TreasuryAlertDecision 锁死金库低预警跨越去抖:只在跌破那一拍告警一次,回升复位。
func TestUnit_TreasuryAlertDecision(t *testing.T) {
	if alert, below := treasuryAlertDecision(false, 10, 100); !alert || !below {
		t.Fatal("首次跌破必须告警")
	}
	if alert, below := treasuryAlertDecision(true, 10, 100); alert || !below {
		t.Fatal("持续低位不得重复告警")
	}
	if alert, below := treasuryAlertDecision(true, 200, 100); alert || below {
		t.Fatal("回升应复位且不告警")
	}
	if alert, _ := treasuryAlertDecision(false, 100, 100); alert {
		t.Fatal("恰等于阈值不算跌破(< 语义)")
	}
}

// TestUnit_MemberExhaustedState 锁死成员额度光的对客文案映射(29-PRD §4.9)。
func TestUnit_MemberExhaustedState(t *testing.T) {
	if ex, notice := memberExhaustedState(0); !ex || notice != memberExhaustedNotice {
		t.Fatalf("剩余 0 应判用尽+文案, 实 %v %q", ex, notice)
	}
	if ex, _ := memberExhaustedState(-5); !ex {
		t.Fatal("略负(在途请求)同样判用尽")
	}
	if ex, notice := memberExhaustedState(1); ex || notice != "" {
		t.Fatal("有剩余不得报用尽")
	}
}
