package newapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

// LogEntry 是 new-api 消费日志的一条(/api/log/ type=2,05 §1)。
// quota = 该条结算消耗(平台扣公司余额的依据,03 §3.1「以 logs 为准」)。
// TokenID/TokenName(v2 M0-S2):该次调用所用令牌,结算按 TokenID 映射回平台稳定 key_id 做 key 维度归因
// (new-api Log 两字段都有,model/log.go:26/35 已核实)。
type LogEntry struct {
	ID               int64
	UserID           int
	CreatedAt        int64 // unix 秒
	ModelName        string
	Quota            int64
	PromptTokens     int64
	CompletionTokens int64
	TokenID          int64
	TokenName        string
}

// AllLogEntry 是 new-api /api/log/ 的完整日志镜像口径。它不参与计费,只用于运营排障。
// 与 LogEntry 分开,避免误把错误/系统日志喂进 settlement。
type AllLogEntry struct {
	ID               int64
	UserID           int
	CreatedAt        int64 // unix 秒
	Type             int
	Content          string
	Username         string
	TokenName        string
	ModelName        string
	Quota            int64
	PromptTokens     int64
	CompletionTokens int64
	UseTime          int
	IsStream         bool
	ChannelID        int
	ChannelName      string
	TokenID          int64
	Group            string
	IP               string
	RequestID        string
	Other            json.RawMessage
}

// ReadConsumptionLogs 读 [sinceUnix, untilUnix] 窗口内的**消费日志**(type=2),分页。
// 小窗口 + 分页,绝不全表(05 §1.1 / 红线)。管理员身份。返回本页条目 + 该窗口总数。
func (a *Adapter) ReadConsumptionLogs(ctx context.Context, sinceUnix, untilUnix int64, page, pageSize int) ([]LogEntry, int, error) {
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 || pageSize > 100 {
		pageSize = 100
	}
	// C17:new-api /api/log/ 时间过滤为**双闭区间** [since,until];相邻窗口在边界秒会重叠 → 调用方须按 log.id 去重
	// (forward 靠 id 水位、回填靠 usage_detail 幂等)。
	path := fmt.Sprintf("/api/log/?type=2&start_timestamp=%d&end_timestamp=%d&p=%d&page_size=%d",
		sinceUnix, untilUnix, page, pageSize)
	res, err := a.c.do(ctx, stepReadLogs, "GET", path, adminAuth(a.c.cfg), nil)
	if err != nil {
		return nil, 0, err
	}
	return parseLogPage(res.data)
}

// ReadConsumptionLogsByUsername 同 ReadConsumptionLogs,但额外按 new-api username **精确等值**过滤
// (model/log.go:310 `logs.username = ?`,非 LIKE)——历史回填只拉这一个企业用户的日志,不扫全站(24-§7.2)。
// forward 结算路径不使用此方法,其签名与行为不受影响。
func (a *Adapter) ReadConsumptionLogsByUsername(ctx context.Context, username string, sinceUnix, untilUnix int64, page, pageSize int) ([]LogEntry, int, error) {
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 || pageSize > 100 {
		pageSize = 100
	}
	path := fmt.Sprintf("/api/log/?type=2&start_timestamp=%d&end_timestamp=%d&p=%d&page_size=%d&username=%s",
		sinceUnix, untilUnix, page, pageSize, url.QueryEscape(username))
	res, err := a.c.do(ctx, stepReadLogs, "GET", path, adminAuth(a.c.cfg), nil)
	if err != nil {
		return nil, 0, err
	}
	return parseLogPage(res.data)
}

// ReadAllLogsByUsername 读 [sinceUnix, untilUnix] 窗口内该 username 的全部日志(type=0=不筛类型)。
// 只供企业后台排障镜像使用,不参与计费结算。
func (a *Adapter) ReadAllLogsByUsername(ctx context.Context, username string, sinceUnix, untilUnix int64, page, pageSize int) ([]AllLogEntry, int, error) {
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 || pageSize > 100 {
		pageSize = 100
	}
	path := fmt.Sprintf("/api/log/?type=0&start_timestamp=%d&end_timestamp=%d&p=%d&page_size=%d&username=%s",
		sinceUnix, untilUnix, page, pageSize, url.QueryEscape(username))
	res, err := a.c.do(ctx, stepReadLogs, "GET", path, adminAuth(a.c.cfg), nil)
	if err != nil {
		return nil, 0, err
	}
	return parseAllLogPage(res.data)
}

// SumConsumedQuotaByUsername 用 new-api 官方 GET /api/log/stat 取该 username 截至 untilUnix 的**权威总消耗**
// (SumUsedQuota,data.quota)。供回填-台账对账安全网(24-§9):比对 SUM(usage_ledger) 与此权威值,漂移即告警。
// 管理员身份;窗口 [0, untilUnix](untilUnix 取 forward 水位,避开未结算的近期尾巴造成假漂移)。
func (a *Adapter) SumConsumedQuotaByUsername(ctx context.Context, username string, untilUnix int64) (int64, error) {
	path := fmt.Sprintf("/api/log/stat?type=2&username=%s&start_timestamp=0&end_timestamp=%d",
		url.QueryEscape(username), untilUnix)
	res, err := a.c.do(ctx, stepReadLogs, "GET", path, adminAuth(a.c.cfg), nil)
	if err != nil {
		return 0, err
	}
	var env struct {
		Quota int64 `json:"quota"`
	}
	if err := json.Unmarshal(res.data, &env); err != nil {
		return 0, &UpstreamError{Step: stepReadLogs, PlatformCode: CodeInternal, Message: "解析日志统计失败", class: classNonRetryable, cause: err}
	}
	return env.Quota, nil
}

// parseLogPage 解析 /api/log/ 一页返回(data = {items,total});ReadConsumptionLogs 与
// ReadConsumptionLogsByUsername 共用,保证两条读取路径解析口径逐字节一致。
func parseLogPage(data json.RawMessage) ([]LogEntry, int, error) {
	var env struct {
		Total int `json:"total"`
		Items []struct {
			ID               int64  `json:"id"`
			UserID           int    `json:"user_id"`
			CreatedAt        int64  `json:"created_at"`
			ModelName        string `json:"model_name"`
			Quota            int64  `json:"quota"`
			PromptTokens     int64  `json:"prompt_tokens"`
			CompletionTokens int64  `json:"completion_tokens"`
			TokenID          int64  `json:"token_id"`
			TokenName        string `json:"token_name"`
		} `json:"items"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, 0, &UpstreamError{Step: stepReadLogs, PlatformCode: CodeInternal, Message: "解析消费日志失败", class: classNonRetryable, cause: err}
	}
	out := make([]LogEntry, 0, len(env.Items))
	for _, it := range env.Items {
		out = append(out, LogEntry{
			ID: it.ID, UserID: it.UserID, CreatedAt: it.CreatedAt, ModelName: it.ModelName,
			Quota: it.Quota, PromptTokens: it.PromptTokens, CompletionTokens: it.CompletionTokens,
			TokenID: it.TokenID, TokenName: it.TokenName,
		})
	}
	return out, env.Total, nil
}

func parseAllLogPage(data json.RawMessage) ([]AllLogEntry, int, error) {
	var env struct {
		Total int `json:"total"`
		Items []struct {
			ID               int64           `json:"id"`
			UserID           int             `json:"user_id"`
			CreatedAt        int64           `json:"created_at"`
			Type             int             `json:"type"`
			Content          string          `json:"content"`
			Username         string          `json:"username"`
			TokenName        string          `json:"token_name"`
			ModelName        string          `json:"model_name"`
			Quota            int64           `json:"quota"`
			PromptTokens     int64           `json:"prompt_tokens"`
			CompletionTokens int64           `json:"completion_tokens"`
			UseTime          int             `json:"use_time"`
			IsStream         bool            `json:"is_stream"`
			ChannelID        int             `json:"channel_id"`
			ChannelName      string          `json:"channel_name"`
			TokenID          int64           `json:"token_id"`
			Group            string          `json:"group"`
			IP               string          `json:"ip"`
			RequestID        string          `json:"request_id"`
			Other            json.RawMessage `json:"other"`
		} `json:"items"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, 0, &UpstreamError{Step: stepReadLogs, PlatformCode: CodeInternal, Message: "解析完整日志失败", class: classNonRetryable, cause: err}
	}
	out := make([]AllLogEntry, 0, len(env.Items))
	for _, it := range env.Items {
		out = append(out, AllLogEntry{
			ID: it.ID, UserID: it.UserID, CreatedAt: it.CreatedAt, Type: it.Type,
			Content: it.Content, Username: it.Username, TokenName: it.TokenName, ModelName: it.ModelName,
			Quota: it.Quota, PromptTokens: it.PromptTokens, CompletionTokens: it.CompletionTokens,
			UseTime: it.UseTime, IsStream: it.IsStream, ChannelID: it.ChannelID, ChannelName: it.ChannelName,
			TokenID: it.TokenID, Group: it.Group, IP: it.IP, RequestID: it.RequestID, Other: it.Other,
		})
	}
	return out, env.Total, nil
}
