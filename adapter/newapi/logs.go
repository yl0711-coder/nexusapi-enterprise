package newapi

import (
	"context"
	"encoding/json"
	"fmt"
)

// LogEntry 是 new-api 消费日志的一条(/api/log/ type=2,05 §1)。
// quota = 该条结算消耗(平台扣公司余额的依据,03 §3.1「以 logs 为准」)。
type LogEntry struct {
	ID               int64
	UserID           int
	CreatedAt        int64 // unix 秒
	ModelName        string
	Quota            int64
	PromptTokens     int64
	CompletionTokens int64
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
	path := fmt.Sprintf("/api/log/?type=2&start_timestamp=%d&end_timestamp=%d&p=%d&page_size=%d",
		sinceUnix, untilUnix, page, pageSize)
	res, err := a.c.do(ctx, stepReadLogs, "GET", path, adminAuth(a.c.cfg), nil)
	if err != nil {
		return nil, 0, err
	}
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
		} `json:"items"`
	}
	if err := json.Unmarshal(res.data, &env); err != nil {
		return nil, 0, &UpstreamError{Step: stepReadLogs, PlatformCode: CodeInternal, Message: "解析消费日志失败", class: classNonRetryable, cause: err}
	}
	out := make([]LogEntry, 0, len(env.Items))
	for _, it := range env.Items {
		out = append(out, LogEntry{
			ID: it.ID, UserID: it.UserID, CreatedAt: it.CreatedAt, ModelName: it.ModelName,
			Quota: it.Quota, PromptTokens: it.PromptTokens, CompletionTokens: it.CompletionTokens,
		})
	}
	return out, env.Total, nil
}
