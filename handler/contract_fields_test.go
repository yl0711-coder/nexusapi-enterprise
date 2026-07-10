// 40号 P2-6:前后端字段契约测试——"前端读的字段名 == 后端 json tag" 的编译期外守护。
// 背景:成员额度概念曾在三个 struct 用三套字段名+两种嵌套(P2-1 根因),前端靠 rawOf 多名兜底,
// 写漏一名(P2-2)或遇嵌套(P1-2)就整块恒"-"。统一为 remaining_raw/used_raw/granted_raw 扁平后,
// 本测锁死:后端任一响应字段改名/改嵌套、或前端读回旧名 → 立即红(纯单测,无需 DB,CI 恒跑)。
package handler

import (
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/nexusapi-platform/enterprise/service"
)

// 统一额度契约名(40号 P2-1 裁定)。
var quotaFieldNames = []string{"remaining_raw", "used_raw", "granted_raw"}

// 令牌维度契约名(MyTokenView 既有 tag,与成员额度维度分开)。
var tokenFieldNames = []string{"quota_raw", "remain_raw", "period_used_raw"}

// 已废弃的旧名:后端响应与前端源码都不得再出现(令牌维度合法名 quota_raw/remain_raw/period_used_raw 不在此列)。
var retiredFieldNames = []string{"consumed_raw", "granted_net_raw", "used_quota_raw", "total_granted_raw", "limit_raw", "remain_quota_raw"}

func keysOf(t *testing.T, v any) map[string]bool {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out := map[string]bool{}
	for k := range m {
		out[k] = true
	}
	return out
}

func i64p(v int64) *int64 { return &v }

// TestContract_MemberQuotaFields 锁死三个额度 DTO:统一名齐、旧名绝迹、详情扁平(无 quota 嵌套)。
func TestContract_MemberQuotaFields(t *testing.T) {
	cases := []struct {
		name string
		v    any
	}{
		{"列表富化 memberView+RowExtra", memberView{MemberRowExtra: &service.MemberRowExtra{
			RemainingRaw: i64p(1), UsedRaw: i64p(2), GrantedRaw: i64p(3)}}},
		{"详情拍平 memberView+Snapshot", struct {
			memberView
			*service.MemberQuotaSnapshot
		}{memberView{}, &service.MemberQuotaSnapshot{RemainingRaw: 1, UsedRaw: 2, GrantedRaw: 3, TokenCount: 4}}},
		{"自余额 MemberBalanceView", service.MemberBalanceView{Provisioned: true, RemainingRaw: 1, UsedRaw: 2, GrantedRaw: 3}},
	}
	// 令牌维度(MyTokenView)单独断言:令牌契约名齐 + 旧名绝迹。
	tokKeys := keysOf(t, service.MyTokenView{})
	for _, want := range tokenFieldNames {
		if !tokKeys[want] {
			t.Errorf("[令牌 MyTokenView] 缺契约字段 %q", want)
		}
	}
	for _, old := range retiredFieldNames {
		if tokKeys[old] {
			t.Errorf("[令牌 MyTokenView] 出现已废弃字段 %q", old)
		}
	}
	for _, c := range cases {
		keys := keysOf(t, c.v)
		for _, want := range quotaFieldNames {
			if !keys[want] {
				t.Errorf("[%s] 缺统一契约字段 %q(P2-1:改名即红)", c.name, want)
			}
		}
		for _, old := range retiredFieldNames {
			if keys[old] {
				t.Errorf("[%s] 出现已废弃字段 %q(P2-1:旧名回流即红)", c.name, old)
			}
		}
		if keys["quota"] {
			t.Errorf("[%s] 额度被嵌进 quota 键(P1-2:必须拍平到顶层)", c.name)
		}
	}
}

// TestContract_FrontendFieldNames 锁死前端:app.js 内成员额度取值只用统一名,旧名绝迹。
// (quota_raw/period_used_raw 属令牌维度白名单;此处只封成员额度旧名。)
func TestContract_FrontendFieldNames(t *testing.T) {
	src, err := os.ReadFile("../web/app.js")
	if err != nil {
		t.Fatalf("读 web/app.js: %v", err)
	}
	js := string(src)
	for _, old := range retiredFieldNames {
		if idx := strings.Index(js, `"`+old+`"`); idx >= 0 {
			line := 1 + strings.Count(js[:idx], "\n")
			t.Errorf("前端仍引用已废弃字段 %q(app.js:%d)——统一契约名后旧名必须清净", old, line)
		}
	}
	for _, want := range quotaFieldNames {
		if !strings.Contains(js, `"`+want+`"`) {
			t.Errorf("前端未引用统一契约字段 %q(疑似前端又换了名,与后端漂移)", want)
		}
	}
}

// ── 45号-19:契约测扩射程 ──────────────────────────────────────────────

// TestContract_FrontendForbiddenMarkers 封杀 app.js 里已踩过雷的错名与退役标记:
// tier 错名("group":/model_limits 曾致建档必 400,45号 P1-1)、门B/回填标记(曾致建组织 400)。
func TestContract_FrontendForbiddenMarkers(t *testing.T) {
	src, err := os.ReadFile("../web/app.js")
	if err != nil {
		t.Fatalf("读 web/app.js: %v", err)
	}
	js := string(src)
	// 禁串(出现即红)。"group":合法场景=令牌维度(createMyTokenReq/PATCH me/tokens 有 group tag),
	// 但 tier body 语境的 group 已在 P1-1 改 newapi_group——这里封杀曾出事的组合精确串。
	forbidden := []string{
		`model_limits:`, `"model_limits"`, // tier 错名(后端只认 model_set)
		`co_mode`, `body.associate`, `"associate"`, // 门B 标记
		`/backfill`, `doReimport`, `import-tokens`, // 回填/导入退役端点
	}
	for _, f := range forbidden {
		if idx := strings.Index(js, f); idx >= 0 {
			line := 1 + strings.Count(js[:idx], "\n")
			t.Errorf("前端出现已封杀标记 %q(app.js:%d)——tier 错名/门B/回填残留回流即红(45号-19)", f, line)
		}
	}
	// direction 枚举:前端必须认后端权威 credit/debit(45号 P2-3;曾不认 debit 把退额画成入账)。
	for _, must := range []string{`d === "credit"`, `d === "debit"`} {
		if !strings.Contains(js, must) {
			t.Errorf("前端 ledgerDir 缺权威枚举判断 %q(direction 只认 credit/debit)", must)
		}
	}
}

// TestContract_FrontendBodyFieldsSubsetOfBackendTags 45号-19 核心:前端提交 body 的字段名
// 必须 ⊆ 后端写端点 input struct 的 json tag 并集——根治"前端起个后端不认识的名,严格解析必 400"
// (P1-1/P1-2/门B associate 三次同根事故)。提取规则:app.js 中 `const body = {`/`body = {` 起
// 到首个 `};` 段内的 `key:` 形态键。后端并集=handler 包内全部写端点 req structs 反射(含嵌套指针)。
func TestContract_FrontendBodyFieldsSubsetOfBackendTags(t *testing.T) {
	// 后端合法键并集(写端点 req structs;handler 包内可反射未导出类型)。
	legal := map[string]bool{}
	collect := func(v any) {
		rt := reflect.TypeOf(v)
		for i := 0; i < rt.NumField(); i++ {
			tag := strings.Split(rt.Field(i).Tag.Get("json"), ",")[0]
			if tag != "" && tag != "-" {
				legal[tag] = true
			}
		}
	}
	collect(loginReq{})
	collect(changePasswordReq{})
	collect(updateMeReq{})
	collect(createOrgReq{})
	collect(createTierReq{})
	collect(updateTierReq{})
	collect(createMyTokenReq{})
	collect(updateMyTokenReq{})
	collect(service.UpdatePlatformSettingsInput{})
	// 零散写端点的 inline 键(grep handler 各 decodeJSON 匿名/小结构;漂移时此表跟着后端改)。
	for _, k := range []string{"name", "team_id", "tier_id", "email", "enabled", "role", "amount_raw", "reason",
		"idempotency_key", "grant_id", "target_type", "target_id", "scope", "grant_type", "ttl_seconds",
		"note", "group", "mode", "default_token_group", "display_name", "timezone", "default_tier_id",
		"emails", "member_ids", "quota_raw", "allow_ips", "reset_period", "model_set", "model_cap",
		"quota_type", "visibility", "newapi_user_group", "newapi_group", "admin_email", "admin_password", "slug"} {
		legal[k] = true
	}
	src, err := os.ReadFile("../web/app.js")
	if err != nil {
		t.Fatalf("读 web/app.js: %v", err)
	}
	lines := strings.Split(string(src), "\n")
	keyRe := regexp.MustCompile(`^\s*([a-z][a-z0-9_]*)\s*:`)
	inBody := false
	for n, ln := range lines {
		if strings.Contains(ln, "body = {") || strings.Contains(ln, "const body = {") {
			inBody = true
			// 同行 inline 键
			for _, m := range regexp.MustCompile(`[{,]\s*([a-z][a-z0-9_]*)\s*:`).FindAllStringSubmatch(ln, -1) {
				if !legal[m[1]] {
					t.Errorf("前端 body 字段 %q(app.js:%d)不在后端任何写端点 json tag 里——提交必 400(45号-19)", m[1], n+1)
				}
			}
			if strings.Contains(ln, "};") || (strings.Contains(ln, "}") && strings.Count(ln, "{") <= strings.Count(ln, "}")) {
				inBody = false
			}
			continue
		}
		if inBody {
			if strings.Contains(ln, "};") {
				inBody = false
				continue
			}
			if m := keyRe.FindStringSubmatch(ln); m != nil {
				if !legal[m[1]] {
					t.Errorf("前端 body 字段 %q(app.js:%d)不在后端任何写端点 json tag 里——提交必 400(45号-19)", m[1], n+1)
				}
			}
		}
	}
}
