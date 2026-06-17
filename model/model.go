// Package model 是平台领域实体(repo 与 service 共用)。
// 字段定义对齐 09-数据字典;枚举取值见 09 §14。本包不含业务逻辑、不依赖其他内部包。
package model

import "time"

// 组织状态(09 §14)。
const (
	OrgStatusActive  = "active"
	OrgStatusLow     = "low"
	OrgStatusStopped = "stopped"
)

// 成员状态(09 §14 + provisioning 中间态,08 US-01 失败分支)。
const (
	MemberStatusActive       = "active"
	MemberStatusDisabled     = "disabled"
	MemberStatusExpired      = "expired"
	MemberStatusPending      = "pending"
	MemberStatusProvisioning = "provisioning"
)

// bootstrap_state(10 §2.5)。
const (
	BootstrapPending = "pending"
	BootstrapDone    = "done"
	BootstrapFailed  = "failed"
)

// 通用状态(team/tier)。
const (
	StatusActive   = "active"
	StatusArchived = "archived"
)

// Organization 对应 organization 表(09 §1)。本期只用治理相关字段,计费字段后续里程碑接。
type Organization struct {
	ID            int64
	Name          string
	Slug          string
	Status        string
	Timezone      string
	NewapiGroup   *string
	DefaultTierID *int64
	BillingMode   string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Team 对应 team 表(09 §2)。
type Team struct {
	ID             int64
	OrgID          int64
	Name           string
	LeaderMemberID *int64
	DefaultTierID  *int64
	Status         string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Tier 对应 tier 表(09 §5)。ModelSet/ModelCap 以 JSON 字符串透传(本期不解析)。
type Tier struct {
	ID           int64
	OrgID        int64
	Name         string
	ModelSet     []string // 允许的模型集合;空 = 继承组织默认
	DailyLimit   *int64
	WeeklyLimit  *int64
	MonthlyLimit *int64
	NewapiGroup  *string
	IsDefault    bool
	Status       string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Member 对应 member 表(09 §4 + 平台补列)。
// 密文/哈希字段不出 repo 边界给前端;service 负责加解密与脱敏。
type Member struct {
	ID                   int64
	OrgID                int64
	TeamID               *int64
	NewapiUserID         int64
	LoginEmail           string
	DisplayName          *string
	Role                 string
	TierID               *int64
	Status               string
	ExpireAt             *time.Time
	PlatformPasswordHash *string
	AccessTokenEnc       []byte
	MemberPasswordEnc    []byte
	BootstrappedAt       *time.Time
	NewapiTokenID        *int64
	KeyMasked            *string
	KeyRotation          int
	BootstrapState       string
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// grant_type(09 §14 + 08 §3.4)。
const (
	GrantQuotaAdd   = "quota_add"   // 临时增额(payload.delta>0)
	GrantQuotaSub   = "quota_sub"   // 临时减额(payload.delta<0)
	GrantModelAdd   = "model_add"   // 临时放开模型(payload.model)
	GrantAccountTTL = "account_ttl" // 临时账号有效期(到期停号)
)

// grant status(09 §14)。
const (
	GrantStatusActive  = "active"
	GrantStatusExpired = "expired"
	GrantStatusRevoked = "revoked"
)

// Grant 对应 member_grant 表(09 §11,落地改名避保留字)。
type Grant struct {
	ID          int64
	OrgID       int64
	MemberID    int64
	GrantType   string
	Payload     GrantPayload
	Reason      *string
	Operator    string
	EffectiveAt time.Time
	ExpireAt    time.Time
	Status      string
	RevertedAt  *time.Time
	CreatedAt   time.Time
}

// GrantPayload 是 grant 的载荷(按 grant_type 取用其中字段)。
type GrantPayload struct {
	Delta    int64  `json:"delta,omitempty"`    // quota_add/sub:带符号的额度增减(quota)
	Duration string `json:"duration,omitempty"` // 时长标识:today/3d/week 等
	Model    string `json:"model,omitempty"`    // model_add:放开的模型名
}

// Balance 对应 company_balance 表(09 §7)。balance = total_recharged - total_consumed。
type Balance struct {
	OrgID          int64
	TotalRecharged int64
	TotalConsumed  int64
	Balance        int64
	LowWatermark   int64
	Version        int64
}

// Recharge 对应 recharge 表(09 §8)。transfer_no 唯一 = 入账幂等。
type Recharge struct {
	ID          int64
	OrgID       int64
	Amount      int64
	AmountCNY   *int64
	TransferNo  string
	Operator    string
	Note        *string
	RechargedAt time.Time
}

// 申请类型 / 状态。
const (
	RechargeReqTopup     = "topup"
	RechargeReqRefund    = "refund"
	RechargeReqPending   = "pending"
	RechargeReqProcessed = "processed"
	RechargeReqRejected  = "rejected"
)

// RechargeRequest 对应 recharge_request 表(补 09)。只发起申请、不改余额。
type RechargeRequest struct {
	ID          int64
	OrgID       int64
	RequestType string
	Amount      int64
	Note        *string
	Applicant   string
	Status      string
	ProcessedBy *string
	ProcessedAt *time.Time
	CreatedAt   time.Time
}

// approval 状态(09 §14)+ 请求类型。
const (
	ApprovalPending     = "pending"      // 待一审(团队负责人)
	ApprovalL1Approved  = "l1_approved"  // 一审过,待二审(组织管理员)
	ApprovalApproved    = "approved"     // 终批通过
	ApprovalRejected    = "rejected"     // 驳回
	ApprovalAutoApprove = "auto_approved" // 自动通过
	ApprovalCancelled   = "cancelled"

	ReqQuotaRaise = "quota_raise"
	ReqModelOpen  = "model_open"
)

// Approval 对应 approval 表(09 §12)。
type Approval struct {
	ID           int64
	OrgID        int64
	ApplicantID  int64
	TeamID       *int64
	RequestType  string
	Payload      ApprovalPayload
	State        string
	IsLevel2     bool
	L1ReviewerID *int64
	L2ReviewerID *int64
	RejectReason *string
	CreatedAt    time.Time
}

// ApprovalPayload 是申请载荷。
type ApprovalPayload struct {
	Model    string `json:"model,omitempty"`
	Amount   int64  `json:"amount,omitempty"`   // 申请额度(quota)
	Duration string `json:"duration,omitempty"` // today/3d/week
	Reason   string `json:"reason,omitempty"`
}

// Notification 对应 notification 表(站内通知,US-13)。
type Notification struct {
	ID        int64
	OrgID     int64
	MemberID  int64
	Type      string
	Title     string
	Body      *string
	IsRead    bool
	CreatedAt time.Time
}

// AuditEntry 对应 audit_log 表(09 §13)。Detail 为已脱敏 JSON(绝不含明文 key/密文)。
type AuditEntry struct {
	OrgID            int64
	Actor            string
	OnBehalfOf       *string
	SupportSessionID *int64
	Action           string
	TargetType       *string
	TargetID         *int64
	Detail           []byte // JSON
	Result           string
	RequestID        *string
}
