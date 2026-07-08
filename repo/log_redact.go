// 架构B 阶段1 BE③(31-ADR §8 护栏):日志脱敏**下沉 repo 层按角色出列**——
// 任何读端点从 repo 拿到的行已按视角清洗,不靠各读端点各自记得脱敏。
// 零值 = LogViewRestricted(最严),fail-closed:调用方必须显式声明 LogViewFull 才拿全量。
package repo

import (
	"encoding/json"
	"strings"
)

// LogView 是镜像日志的读取视角(26-§4.2 + 31-ADR §8/§9 镜像原则)。
type LogView int

const (
	// LogViewRestricted 组织管理员/成员视角(默认,零值即最严):
	// 清顶层渠道系(channel_id/channel_name)与 new-api 内部归因 id(newapi_token_id/newapi_user_id/key_id);
	// other 剥 admin_info/stream_status;content 截断;IP 打码。
	// 【v3 反转,26-§1】成员保留本人 token_name/group_name(成员=多令牌,须按令牌名筛选/展示)。
	LogViewRestricted LogView = iota
	// LogViewFull 超管(operator)视角:照抄 new-api 管理员,原样返回,不做任何剥离/截断/打码。
	LogViewFull
)

const logContentMaxRunes = 240

// redactOrgNewapiLogs 按视角就地清洗镜像日志行(唯一脱敏出口;List* 返回前统一过此函数)。
func redactOrgNewapiLogs(items []OrgNewapiLog, v LogView) {
	if v == LogViewFull {
		return
	}
	for i := range items {
		// 顶层渠道系:低权一律不可见(渠道对客户侧透明,仅超管)。
		items[i].ChannelID = 0
		items[i].ChannelName = ""
		// 顶层 new-api 内部归因 id:低权不暴露(排障靠成员名/令牌名,不给内部 id)。
		items[i].NewapiTokenID = 0
		items[i].NewapiUserID = 0
		items[i].KeyID = 0
		// token_name/group_name 保留([v3 反转]:成员多令牌要按令牌名看/筛;分组随页面需要保留)。
		// other:剥 admin_info/stream_status;解析失败返回空串,绝不返半截 JSON。
		items[i].Other = sanitizeLogOther(items[i].Other)
		// content 自由文本(可能含 prompt 片段/敏感串):低权截断。IP 打码。
		items[i].Content = truncateLogRunes(items[i].Content, logContentMaxRunes)
		items[i].IP = maskLogIP(items[i].IP)
	}
}

// sanitizeLogOther 对低权视角净化 other JSON:删 admin_info(渠道信息)与 stream_status
// (与 new-api formatUserLogs 低权口径一致,model/log.go:53)。空串原样;解析失败返回空串。
func sanitizeLogOther(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return ""
	}
	delete(m, "admin_info")
	delete(m, "stream_status")
	out, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(out)
}

func truncateLogRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	rs := []rune(s)
	if len(rs) <= max {
		return s
	}
	return string(rs[:max]) + "..."
}

func maskLogIP(ip string) string {
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return ""
	}
	if strings.Contains(ip, ",") {
		parts := strings.Split(ip, ",")
		for i := range parts {
			parts[i] = maskLogIP(parts[i])
		}
		return strings.Join(parts, ", ")
	}
	if strings.Count(ip, ".") == 3 {
		parts := strings.Split(ip, ".")
		parts[3] = "*"
		return strings.Join(parts, ".")
	}
	if strings.Contains(ip, ":") {
		parts := strings.Split(ip, ":")
		if len(parts) > 2 {
			return strings.Join(parts[:2], ":") + ":***"
		}
	}
	if len([]rune(ip)) > 6 {
		return truncateLogRunes(ip, 6) + "*"
	}
	return "*"
}
