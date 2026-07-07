package repo

// 日志脱敏纯函数单测(无 DB;脱敏下沉 repo 层后测试随实现搬家,31-ADR §8)。
// 锁死点:零值视角=最严(fail-closed);低权清渠道系/内部 id 但保留 token_name/group_name([v3 反转]);
// other 剥 admin_info/stream_status、坏 JSON 返空串;content 截断;IP 打码;超管原样。

import (
	"strings"
	"testing"
)

func mkLogRows() []OrgNewapiLog {
	return []OrgNewapiLog{{
		ChannelID: 9, ChannelName: "ch-a", NewapiUserID: 100, NewapiTokenID: 200, KeyID: 7,
		TokenName: "nexus_m1_v1", GroupName: "vip", IP: "1.2.3.4",
		Content: "x", Other: `{"frt":1,"admin_info":{"channel_id":9},"stream_status":"ok"}`,
	}}
}

func TestUnit_RedactOrgNewapiLogs_Full(t *testing.T) {
	rows := mkLogRows()
	redactOrgNewapiLogs(rows, LogViewFull)
	if rows[0].ChannelName == "" || rows[0].NewapiTokenID == 0 || !strings.Contains(rows[0].Other, "admin_info") ||
		rows[0].IP != "1.2.3.4" {
		t.Fatalf("超管(Full)必须原样返回,不得剥离: %+v", rows[0])
	}
}

func TestUnit_RedactOrgNewapiLogs_Restricted(t *testing.T) {
	rows := mkLogRows()
	redactOrgNewapiLogs(rows, LogViewRestricted)
	r := rows[0]
	if r.ChannelID != 0 || r.ChannelName != "" || r.NewapiTokenID != 0 || r.NewapiUserID != 0 || r.KeyID != 0 {
		t.Fatalf("受限视角响应体仍含渠道系/内部归因 id: %+v", r)
	}
	// [v3 反转,26-§1]:成员=多令牌,token_name/group_name 必须保留(按令牌名筛选/展示)。
	if r.TokenName != "nexus_m1_v1" || r.GroupName != "vip" {
		t.Fatalf("受限视角应保留令牌名/分组(v3 反转): %+v", r)
	}
	if strings.Contains(r.Other, "admin_info") || strings.Contains(r.Other, "stream_status") {
		t.Fatalf("受限视角 other 仍含 admin_info/stream_status: %s", r.Other)
	}
	if !strings.Contains(r.Other, "frt") {
		t.Fatalf("受限视角误删展开所需字段 frt: %s", r.Other)
	}
	if r.IP == "1.2.3.4" {
		t.Fatalf("受限视角 IP 未打码: %s", r.IP)
	}
}

// 零值视角必须等于最严(fail-closed):调用方忘传 View 也不会泄露。
func TestUnit_RedactZeroValueIsRestricted(t *testing.T) {
	if LogViewRestricted != 0 {
		t.Fatal("LogViewRestricted 必须是零值(fail-closed 地基,别改枚举顺序)")
	}
	rows := mkLogRows()
	var zero LogView
	redactOrgNewapiLogs(rows, zero)
	if rows[0].ChannelName != "" || rows[0].NewapiTokenID != 0 {
		t.Fatalf("零值视角必须按最严清洗: %+v", rows[0])
	}
}

func TestUnit_SanitizeLogOther(t *testing.T) {
	raw := `{"frt":123,"cache_tokens":50,"reasoning_effort":"high","admin_info":{"channel_id":9},"stream_status":"ok","po":["a"]}`
	got := sanitizeLogOther(raw)
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
	if sanitizeLogOther("") != "" {
		t.Fatal("空串应原样返回空串")
	}
	if got := sanitizeLogOther(`{"frt":1,"admin_info":{trunc`); got != "" {
		t.Fatalf("坏/半截 JSON 必须返回空串,不得返半截,得 %q", got)
	}
}

func TestUnit_MaskLogIP(t *testing.T) {
	if got := maskLogIP("1.2.3.4"); got != "1.2.3.*" {
		t.Fatalf("IPv4 应打码末段, 得 %q", got)
	}
	if got := maskLogIP("2001:db8::1"); !strings.HasSuffix(got, ":***") {
		t.Fatalf("IPv6 应打码, 得 %q", got)
	}
	if maskLogIP("") != "" {
		t.Fatal("空 IP 应返回空")
	}
}
