// 40号 P2-6:前后端字段契约测试——"前端读的字段名 == 后端 json tag" 的编译期外守护。
// 背景:成员额度概念曾在三个 struct 用三套字段名+两种嵌套(P2-1 根因),前端靠 rawOf 多名兜底,
// 写漏一名(P2-2)或遇嵌套(P1-2)就整块恒"-"。统一为 remaining_raw/used_raw/granted_raw 扁平后,
// 本测锁死:后端任一响应字段改名/改嵌套、或前端读回旧名 → 立即红(纯单测,无需 DB,CI 恒跑)。
package handler

import (
	"encoding/json"
	"os"
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
