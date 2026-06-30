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
	ID                int64
	Name              string
	Slug              string
	Status            string
	Timezone          string
	NewapiUserGroup   *string // 改动①:组织专属 new-api 用户分组(列 newapi_group;隔离边界+唯一;nil 回落 org_%d)
	DefaultTierID     *int64
	BillingMode       string
	DefaultTokenGroup *string    // 组织级默认令牌计价分组(D1 两级;nil=回落 default)
	ArchivedAt        *time.Time // 归档时间(NULL=未归档,T12)
	NewapiUserID      *int64     // 模型2(0020):组织=一个 new-api user,此为池子锚(user.quota=预付池子);nil=尚未开通
	AccessTokenEnc    []byte     // 模型2(0020):该组织 new-api user 的 access_token,应用层加密存(建员工 token 用);列 newapi_access_token_enc
	PasswordEnc       []byte     // 模型2(0020):该组织 new-api user 的密码,加密存(access_token 失效时重登录自愈);列 newapi_password_enc
	CreatedAt         time.Time
	UpdatedAt         time.Time
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

// ===== 模型2(0020)新增实体:组织树 / 托管多桶 / 开通 outbox =====

// org_unit 状态/类型(模型2)。
const (
	OrgUnitActive   = "active"
	OrgUnitArchived = "archived"
)

// escrow_bucket 状态(模型2 托管多桶)。
const (
	EscrowActive  = "active"  // 桶1:镜像进 org user.quota 的可花窗口
	EscrowHolding = "holding" // 平台库托管,未进窗口
	EscrowMerged  = "merged"  // 已并入窗口(续充消耗)
)

// outbox 状态(模型2 开通意图)。
const (
	OutboxPending = "pending"
	OutboxDone    = "done"
	OutboxFailed  = "failed"
)

// OrgUnit 对应 org_unit 表(模型2,0020)。组织内部任意深度树;Path 物化路径含首尾斜杠 "/1/7/22/"。
type OrgUnit struct {
	ID        int64
	OrgID     int64
	ParentID  *int64 // 根节点 nil
	Path      string
	Name      string
	Type      string
	Status    string
	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time
}

// EscrowBucket 对应 escrow_bucket 表(模型2,0020)。Seq=1/Active=镜像进 user.quota 的可花窗口,其余托管。
type EscrowBucket struct {
	ID        int64
	OrgID     int64
	Seq       int
	Amount    int64
	Status    string
	Threshold int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Outbox 对应 outbox 表(模型2,0020)。开通意图;IdempotencyKey 唯一=幂等键。
type Outbox struct {
	ID             int64
	AggregateType  string // organization / member
	AggregateID    int64
	Payload        []byte // JSON
	Status         string
	IdempotencyKey string
	Attempts       int
	LastError      *string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Tier 对应 tier 表(09 §5)。ModelSet/ModelCap 以 JSON 字符串透传(本期不解析)。
type Tier struct {
	ID           int64
	OrgID        int64
	Name         string
	ModelSet     []string         // 允许的模型集合;空 = 继承组织默认
	ModelCap     map[string]int64 // 单模型日上限(quota),如 {"claude-opus":50000};软限额(E4)
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
// 模型2:member 不持 NewapiUserID(那是 organization 的池子锚)、不持 AccessTokenEnc/MemberPasswordEnc
// (员工 token 在 org user 下建,凭证归 org)。归因走 member_key(token_id→key_id→member)。
type Member struct {
	ID                   int64
	OrgID                int64
	TeamID               *int64 // 兼容层(R2 切 org_unit)
	LoginEmail           string
	DisplayName          *string
	Role                 string
	TierID               *int64
	NewapiGroup          *string // 令牌计价分组快照(开通时解析,T17-1;nil=default)
	Status               string
	ExpireAt             *time.Time
	PlatformPasswordHash *string
	BootstrappedAt       *time.Time
	NewapiTokenID        *int64 // 该成员当前令牌(挂 org user 下)的 token id;全历史在 member_key_token
	KeyMasked            *string
	KeyRotation          int
	BootstrapState       string
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// key 槽 / 物理令牌状态(v2 0015)。
const (
	KeySlotActive   = "active"
	KeySlotRevoked  = "revoked"
	KeyTokenActive     = "active"
	KeyTokenRevoked    = "revoked"
	KeyTokenSuperseded = "superseded" // 轮换后被取代的旧令牌(留作历史归因)
)

// 周期类型(v2 0017,单一周期三选一)。
const (
	PeriodDay   = "day"
	PeriodWeek  = "week"
	PeriodMonth = "month"
)

// MemberKeySlot 对应 member_key_slot 表(v2 0015)。id = 平台稳定 key_id,1:N 挂成员,轮换不变。
type MemberKeySlot struct {
	ID        int64 // = 平台稳定 key_id
	OrgID     int64
	MemberID  int64
	IsPrimary bool
	Status    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// MemberKeyToken 对应 member_key_token 表(v2 0015,append-only)。
// 物理 new-api 令牌;轮换=插新行 + 旧行 IsCurrent=false,绝不删 → 历史日志按旧 token_id 仍映射回同一 KeyID。
type MemberKeyToken struct {
	ID            int64
	KeyID         int64 // 所属稳定 key 槽(MemberKeySlot.ID)
	OrgID         int64
	MemberID      int64
	NewapiTokenID *int64 // 物理 new-api token id(归因主键;nil=尚未建成)
	TokenName     string // 确定性令牌名 nexus_m{member}_v{rotation}(归因兜底键)
	IsCurrent     bool
	KeyMasked     *string
	Rotation      int
	Status        string
	CreatedAt     time.Time
}

// MemberBudget 对应 member_budget 表(v2 0017)。每成员一行,单一周期。
// 本期(观测)只配置不执行:cap/period 可设,granted 恒 0(不发放),spend_cache 由结算派生。
type MemberBudget struct {
	ID                int64
	OrgID             int64
	MemberID          int64
	PeriodType        string // day/week/month
	Cap               int64  // 周期发放上限(quota)
	Granted           int64  // 本期已发放(quota;占用池子)
	PeriodAnchor      *time.Time
	PendingCap        *int64  // 待下周期生效的新 cap
	PendingPeriodType *string // 待下周期生效的新周期类型
	SpendCache        int64   // 派生消费缓存(可重算)
	LastResetAt       *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
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
// Committed(v2 0018):组织池"已发放占用"= Σ成员 granted,不超卖与"可分配"派生用;
// 本期(观测)恒 0(不发放)。可分配 = TotalRecharged - Committed。
type Balance struct {
	OrgID          int64
	TotalRecharged int64
	TotalConsumed  int64
	TotalRefunded  int64
	Committed      int64
	Balance        int64
	LowWatermark   int64
	Version        int64
}

// Recharge 对应 recharge 表(09 §8)。transfer_no 唯一 = 入账幂等。
type Recharge struct {
	ID           int64
	OrgID        int64
	Amount       int64
	AmountCNY    *int64
	TransferNo   string
	Operator     string
	OperatorName string // 操作者显示名(瞬态,T14;由 operator:id 解析,非表列)
	Note         *string
	RechargedAt  time.Time
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
	ApprovalPending     = "pending"       // 待一审(团队负责人)
	ApprovalL1Approved  = "l1_approved"   // 一审过,待二审(组织管理员)
	ApprovalApproved    = "approved"      // 终批通过
	ApprovalRejected    = "rejected"      // 驳回
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

	// ApplicantName 申请人显示名(瞬态,列表 join 填充,非 approval 表列;T9)。
	ApplicantName string
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

// 支持态(09 §14)。
const (
	SupportReadonly   = "readonly"
	SupportAssist     = "assist"
	SupportAuthorized = "authorized"
	SupportBreakGlass = "break_glass"
	SupportActive     = "active"
	SupportRevoked    = "revoked"
	SupportExpired    = "expired"
)

// SupportSession 对应 support_session 表(09 §14)。
type SupportSession struct {
	ID         int64
	OrgID      int64
	Actor      string
	OnBehalfOf string
	Scope      string
	GrantType  *string
	State      string
	StartedAt  time.Time
	ExpireAt   time.Time
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
