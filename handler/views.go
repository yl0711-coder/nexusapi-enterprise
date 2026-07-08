package handler

import (
	"net/http"
	"strconv"
	"time"

	"github.com/nexusapi-platform/enterprise/model"
	"github.com/nexusapi-platform/enterprise/pkg/apperr"
	"github.com/nexusapi-platform/enterprise/repo"
	"github.com/nexusapi-platform/enterprise/service"
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
	// v1 正交属性(0022,只暴露非敏感位):门A/门B(前端"重新导入"按钮判断)+ 计费口径(订阅显示)。
	CreatedByPlatform bool   `json:"newapi_created_by_platform"`
	BillingKind       string `json:"billing_kind"`
	CreatedAt         string `json:"created_at"`
}

func toOrgView(o *model.Organization) orgView {
	return orgView{
		ID: o.ID, Name: o.Name, Slug: o.Slug, Status: o.Status, Timezone: o.Timezone,
		DefaultTierID: o.DefaultTierID, BillingMode: o.BillingMode, Archived: o.ArchivedAt != nil,
		CreatedByPlatform: o.CreatedByPlatform, BillingKind: o.BillingKind,
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

// teamCountView 团队 + active 成员数(F4 列表/详情;映射在 handler 里填,views 不引 service)。
type teamCountView struct {
	teamView
	MemberCount int `json:"member_count"`
}

type tierView struct {
	ID           int64            `json:"id"`
	OrgID        int64            `json:"org_id"`
	Name         string           `json:"name"`
	ModelSet     []string         `json:"model_set"`
	ModelCap     map[string]int64 `json:"model_cap,omitempty"`
	QuotaType    string           `json:"quota_type"`             // 架构B:fixed | subscription
	AmountRaw    *int64           `json:"amount_raw"`             // 架构B:额度值(raw;FE 用 quota_per_unit 换算美元)
	ResetPeriod  *string          `json:"reset_period,omitempty"` // 架构B:daily|weekly|monthly
	Visibility   string           `json:"visibility"`             // 架构B:all | assigned
	MonthlyLimit *int64           `json:"monthly_limit_quota"`    // Deprecated: 架构A 遗留
	NewapiGroup  *string          `json:"newapi_group"`           // 计费分组(T17-1;nil=回落组织默认/default)
	IsDefault    bool             `json:"is_default"`
	Status       string           `json:"status"`
	Grants       []repo.TierGrant `json:"grants"` // 档位授权(33 §12-④;all|member|team)
}

func toTierView(t *model.Tier) tierView {
	return tierView{ID: t.ID, OrgID: t.OrgID, Name: t.Name, ModelSet: t.ModelSet, ModelCap: t.ModelCap,
		QuotaType: t.QuotaType, AmountRaw: t.AmountRaw, ResetPeriod: t.ResetPeriod, Visibility: t.Visibility,
		MonthlyLimit: t.MonthlyLimit, NewapiGroup: t.NewapiGroup, IsDefault: t.IsDefault, Status: t.Status}
}

// memberView 脱敏成员视图:key 只回显 key_masked,绝不含 access_token/password。
type memberView struct {
	ID             int64   `json:"id"`
	OrgID          int64   `json:"org_id"`
	TeamID         *int64  `json:"team_id"`
	LoginEmail     string  `json:"login_email"`
	DisplayName    *string `json:"display_name"`
	Role           string  `json:"role"`
	TierID         *int64  `json:"tier_id"`
	NewapiGroup    *string `json:"newapi_group"` // 令牌计价分组快照(T17-1)
	Status         string  `json:"status"`
	KeyMasked      *string `json:"key_masked"`
	BootstrapState string  `json:"bootstrap_state"`
	CreatedAt      string  `json:"created_at"`
	// 列表富化(39号复验:契约"含额度/已用"):tier_name + 三额度,详情/单独端点不带(nil 则 omit)。
	*service.MemberRowExtra
}

func toMemberView(m *model.Member) memberView {
	return memberView{
		ID: m.ID, OrgID: m.OrgID, TeamID: m.TeamID,
		LoginEmail: m.LoginEmail, DisplayName: m.DisplayName, Role: m.Role, TierID: m.TierID, NewapiGroup: m.NewapiGroup,
		Status: m.Status, KeyMasked: m.KeyMasked, BootstrapState: m.BootstrapState,
		CreatedAt: m.CreatedAt.Format(time.RFC3339),
	}
}

// displayCurrency 对客展示币种(A3:应与主站 QuotaDisplayType 一致;MVP 默认 USD,上线读主站 option)。
const displayCurrency = "USD"

// 换算锚定值不再硬编码(39号 P2-6):统一走 service.QuotaPerUnitSetting(platform_setting
// 单一真相源,与启动自检/FE 同口径),由调用方传入。

// balanceView v1 M5(20-§3):客户余额=读求和合计+billing_kind;只回 available,不漏 window/holding 内部拆分。
type balanceView struct {
	AvailableQuota   int64   `json:"available_quota"`
	AvailableDisplay float64 `json:"available_display"` // 按币种换算(quota/QuotaPerUnit)
	BillingKind      string  `json:"billing_kind"`      // wallet / subscription(订阅组织前端显示"订阅计费")
	Currency         string  `json:"currency"`
}

func toBalanceView(b *service.CustomerBalance, qpu int64) balanceView {
	return balanceView{
		AvailableQuota:   b.AvailableQuota,
		AvailableDisplay: float64(b.AvailableQuota) / float64(qpu),
		BillingKind:      b.BillingKind,
		Currency:         displayCurrency,
	}
}

type rechargeView struct {
	ID           int64  `json:"id"`
	AmountQuota  int64  `json:"amount_quota"`
	TransferNo   string `json:"transfer_no"`
	Operator     string `json:"operator"`
	OperatorName string `json:"operator_name,omitempty"` // 可读操作者名(T14)
	Note         string `json:"note,omitempty"`
	RechargedAt  string `json:"recharged_at"`
}

func toRechargeView(r *model.Recharge) rechargeView {
	v := rechargeView{ID: r.ID, AmountQuota: r.Amount, TransferNo: r.TransferNo, Operator: r.Operator, OperatorName: r.OperatorName, RechargedAt: r.RechargedAt.Format(time.RFC3339)}
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

// optInt64 解析可选 query 参数为 *int64;缺省或非法 → nil(过滤器不生效,不报错)。
func optInt64(r *http.Request, name string) *int64 {
	v := r.URL.Query().Get(name)
	if v == "" {
		return nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return nil
	}
	return &n
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
