package handler

import (
	"net/http"
	"strconv"
	"time"

	"github.com/nexusapi-platform/enterprise/model"
	"github.com/nexusapi-platform/enterprise/pkg/apperr"
)

// 视图层 DTO:只暴露可对外字段,绝不含密文凭证 / 哈希 / 明文 key(10 §3.4)。

type orgView struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	Slug          string `json:"slug"`
	Status        string `json:"status"`
	Timezone      string `json:"timezone"`
	DefaultTierID *int64 `json:"default_tier_id"`
	BillingMode   string `json:"billing_mode"`
	Archived      bool   `json:"archived"` // T12:是否已归档
	CreatedAt     string `json:"created_at"`
}

func toOrgView(o *model.Organization) orgView {
	return orgView{
		ID: o.ID, Name: o.Name, Slug: o.Slug, Status: o.Status, Timezone: o.Timezone,
		DefaultTierID: o.DefaultTierID, BillingMode: o.BillingMode, Archived: o.ArchivedAt != nil,
		CreatedAt: o.CreatedAt.Format(time.RFC3339),
	}
}

type teamView struct {
	ID            int64  `json:"id"`
	OrgID         int64  `json:"org_id"`
	Name          string `json:"name"`
	DefaultTierID *int64 `json:"default_tier_id"`
	Status        string `json:"status"`
}

func toTeamView(t *model.Team) teamView {
	return teamView{ID: t.ID, OrgID: t.OrgID, Name: t.Name, DefaultTierID: t.DefaultTierID, Status: t.Status}
}

type tierView struct {
	ID           int64            `json:"id"`
	OrgID        int64            `json:"org_id"`
	Name         string           `json:"name"`
	ModelSet     []string         `json:"model_set"`
	ModelCap     map[string]int64 `json:"model_cap,omitempty"`
	MonthlyLimit *int64           `json:"monthly_limit_quota"`
	NewapiGroup  *string          `json:"newapi_group"` // 计费分组(T17-1;nil=回落组织默认/default)
	IsDefault    bool             `json:"is_default"`
	Status       string           `json:"status"`
}

func toTierView(t *model.Tier) tierView {
	return tierView{ID: t.ID, OrgID: t.OrgID, Name: t.Name, ModelSet: t.ModelSet, ModelCap: t.ModelCap,
		MonthlyLimit: t.MonthlyLimit, NewapiGroup: t.NewapiGroup, IsDefault: t.IsDefault, Status: t.Status}
}

// memberView 脱敏成员视图:key 只回显 key_masked,绝不含 access_token/password。
type memberView struct {
	ID             int64   `json:"id"`
	OrgID          int64   `json:"org_id"`
	TeamID         *int64  `json:"team_id"`
	NewapiUserID   int64   `json:"newapi_user_id"`
	LoginEmail     string  `json:"login_email"`
	DisplayName    *string `json:"display_name"`
	Role           string  `json:"role"`
	TierID         *int64  `json:"tier_id"`
	NewapiGroup    *string `json:"newapi_group"` // 令牌计价分组快照(T17-1)
	Status         string  `json:"status"`
	KeyMasked      *string `json:"key_masked"`
	BootstrapState string  `json:"bootstrap_state"`
	CreatedAt      string  `json:"created_at"`
}

func toMemberView(m *model.Member) memberView {
	return memberView{
		ID: m.ID, OrgID: m.OrgID, TeamID: m.TeamID, NewapiUserID: m.NewapiUserID,
		LoginEmail: m.LoginEmail, DisplayName: m.DisplayName, Role: m.Role, TierID: m.TierID, NewapiGroup: m.NewapiGroup,
		Status: m.Status, KeyMasked: m.KeyMasked, BootstrapState: m.BootstrapState,
		CreatedAt: m.CreatedAt.Format(time.RFC3339),
	}
}

type grantView struct {
	ID        int64  `json:"id"`
	MemberID  int64  `json:"member_id"`
	GrantType string `json:"grant_type"`
	Delta     int64  `json:"delta,omitempty"`
	Model     string `json:"model,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Operator  string `json:"operator"`
	ExpireAt  string `json:"expire_at"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
}

func toGrantView(g *model.Grant) grantView {
	v := grantView{
		ID: g.ID, MemberID: g.MemberID, GrantType: g.GrantType,
		Delta: g.Payload.Delta, Model: g.Payload.Model, Operator: g.Operator,
		ExpireAt: g.ExpireAt.Format(time.RFC3339), Status: g.Status,
		CreatedAt: g.CreatedAt.Format(time.RFC3339),
	}
	if g.Reason != nil {
		v.Reason = *g.Reason
	}
	return v
}

// displayCurrency 对客展示币种(A3:应与主站 QuotaDisplayType 一致;MVP 默认 USD,上线读主站 option)。
const displayCurrency = "USD"

// quotaPerUnit 元/美元↔quota 锚定(A4,与主站一致)。
const quotaPerUnit = 500000.0

func toDisplay(quota int64) float64 { return float64(quota) / quotaPerUnit }

type balanceView struct {
	OrgID          int64   `json:"org_id"`
	TotalRecharged int64   `json:"total_recharged_quota"`
	TotalConsumed  int64   `json:"total_consumed_quota"`
	TotalRefunded  int64   `json:"total_refunded_quota"`
	Balance        int64   `json:"balance_quota"`
	LowWatermark   int64   `json:"low_watermark_quota"`
	Currency       string  `json:"currency"`        // 对外币种(口径固化在后端,M2)
	BalanceDisplay float64 `json:"balance_display"` // 按币种换算的金额(quota/QuotaPerUnit)
	RechargedDisplay float64 `json:"total_recharged_display"`
	ConsumedDisplay  float64 `json:"total_consumed_display"`
	RefundedDisplay  float64 `json:"total_refunded_display"`
}

func toBalanceView(b *model.Balance) balanceView {
	return balanceView{
		OrgID: b.OrgID, TotalRecharged: b.TotalRecharged, TotalConsumed: b.TotalConsumed, TotalRefunded: b.TotalRefunded,
		Balance: b.Balance, LowWatermark: b.LowWatermark,
		Currency:       displayCurrency,
		BalanceDisplay: toDisplay(b.Balance), RechargedDisplay: toDisplay(b.TotalRecharged),
		ConsumedDisplay: toDisplay(b.TotalConsumed), RefundedDisplay: toDisplay(b.TotalRefunded),
	}
}

type rechargeView struct {
	ID          int64  `json:"id"`
	AmountQuota int64  `json:"amount_quota"`
	TransferNo  string `json:"transfer_no"`
	Operator    string `json:"operator"`
	Note        string `json:"note,omitempty"`
	RechargedAt string `json:"recharged_at"`
}

func toRechargeView(r *model.Recharge) rechargeView {
	v := rechargeView{ID: r.ID, AmountQuota: r.Amount, TransferNo: r.TransferNo, Operator: r.Operator, RechargedAt: r.RechargedAt.Format(time.RFC3339)}
	if r.Note != nil {
		v.Note = *r.Note
	}
	return v
}

type rechargeReqView struct {
	ID          int64  `json:"id"`
	RequestType string `json:"request_type"`
	AmountQuota int64  `json:"amount_quota"`
	Applicant   string `json:"applicant"`
	Status      string `json:"status"`
	Note        string `json:"note,omitempty"`
	CreatedAt   string `json:"created_at"`
}

func toRechargeReqView(rq *model.RechargeRequest) rechargeReqView {
	v := rechargeReqView{ID: rq.ID, RequestType: rq.RequestType, AmountQuota: rq.Amount, Applicant: rq.Applicant, Status: rq.Status, CreatedAt: rq.CreatedAt.Format(time.RFC3339)}
	if rq.Note != nil {
		v.Note = *rq.Note
	}
	return v
}

type approvalView struct {
	ID            int64  `json:"id"`
	ApplicantID   int64  `json:"applicant_id"`
	ApplicantName string `json:"applicant_name,omitempty"`
	TeamID      *int64 `json:"team_id"`
	RequestType string `json:"request_type"`
	Model       string `json:"model,omitempty"`
	Amount      int64  `json:"amount_quota"`
	Duration    string `json:"duration"`
	Reason      string `json:"reason,omitempty"`
	State       string `json:"state"`
	IsLevel2    bool   `json:"is_level2"`
	CreatedAt   string `json:"created_at"`
}

func toApprovalView(a *model.Approval) approvalView {
	return approvalView{
		ID: a.ID, ApplicantID: a.ApplicantID, ApplicantName: a.ApplicantName, TeamID: a.TeamID, RequestType: a.RequestType,
		Model: a.Payload.Model, Amount: a.Payload.Amount, Duration: a.Payload.Duration, Reason: a.Payload.Reason,
		State: a.State, IsLevel2: a.IsLevel2, CreatedAt: a.CreatedAt.Format(time.RFC3339),
	}
}

type notificationView struct {
	ID        int64  `json:"id"`
	Type      string `json:"type"`
	Title     string `json:"title"`
	Body      string `json:"body,omitempty"`
	IsRead    bool   `json:"is_read"`
	CreatedAt string `json:"created_at"`
}

func toNotificationView(n *model.Notification) notificationView {
	v := notificationView{ID: n.ID, Type: n.Type, Title: n.Title, IsRead: n.IsRead, CreatedAt: n.CreatedAt.Format(time.RFC3339)}
	if n.Body != nil {
		v.Body = *n.Body
	}
	return v
}

// pageMeta 是列表分页元信息(10 §1.5)。
type pageMeta struct {
	Page       int `json:"page"`
	PageSize   int `json:"page_size"`
	Total      int `json:"total"`
	TotalPages int `json:"total_pages"`
}

type listResp struct {
	List       any      `json:"list"`
	Pagination pageMeta `json:"pagination"`
}

func makePageMeta(page, pageSize, total int) pageMeta {
	tp := (total + pageSize - 1) / pageSize
	if tp < 1 {
		tp = 1
	}
	return pageMeta{Page: page, PageSize: pageSize, Total: total, TotalPages: tp}
}

// parsePaging 解析 page/page_size(10 §1.5:page 从 1 起,默认 size,上限 100)。
func parsePaging(r *http.Request, defaultSize int) (page, size, offset int) {
	page = atoiDefault(r.URL.Query().Get("page"), 1)
	if page < 1 {
		page = 1
	}
	size = atoiDefault(r.URL.Query().Get("page_size"), defaultSize)
	if size < 1 {
		size = defaultSize
	}
	if size > 100 {
		size = 100
	}
	return page, size, (page - 1) * size
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}

// pathInt64 解析路径参数为 int64;非法 → 400。
func pathInt64(r *http.Request, name string) (int64, error) {
	v := r.PathValue(name)
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, apperr.BadRequest("路径参数 " + name + " 非法")
	}
	return n, nil
}
