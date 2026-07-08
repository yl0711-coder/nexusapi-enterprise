// Package model 是平台领域实体(repo 与 service 共用)。
// 字段定义对齐 09-数据字典;枚举取值见 09 §14。本包不含业务逻辑、不依赖其他内部包。
package model

import "time"

// 组织状态(09 §14)。
const (
	OrgStatusActive  = "active"
	OrgStatusLow     = "low"
	OrgStatusStopped = "stopped"
	// OrgStatusHardStopped v1 运维硬停(20-§4):禁用该组织的 new-api 用户(近实时 403 全令牌,欠费/风控用),
	// 与余额驱动的 stopped 正交。硬停期间平台对该 org 的管理写操作同被屏蔽(org access token 也 403)。
	// v2 注意:recomputeOrgStatus(billing 开后)不得覆盖此状态——解除只走 ReleaseHardStop。
	OrgStatusHardStopped = "hard_stopped"
)

// 成员状态(09 §14 + provisioning 中间态,08 US-01 失败分支)。
const (
	MemberStatusActive       = "active"
	MemberStatusDisabled     = "disabled"   // 禁用(临时停,token 置禁用不删,key 保留,启用即通)
	MemberStatusOffboarded   = "offboarded" // 离职(token 删除+软删 deleted_at,转离职列表可恢复,恢复需重建 key)
	MemberStatusExpired      = "expired"
	MemberStatusPending      = "pending"
	MemberStatusProvisioning = "provisioning"
)

// bootstrap_state(10 §2.5 + 架构B 0030)。
const (
	BootstrapPending = "pending"
	BootstrapDone    = "done"
	BootstrapFailed  = "failed"
	// BootstrapQuarantined 架构B 孤儿隔离(34 §3-③):CreateUser 成功后续 saga 步骤失败,
	// new-api 无干净删 user 能力 → disable + 本标记;worker/统计一律跳过;可重试(重入新名重建)。
	BootstrapQuarantined = "quarantined"
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
	NewapiUsername    *string    // v1.1 项B(0024):门A 组织 new-api 用户名(随机名存库,取代可猜的 org<id>);门B/未开通=nil
	AccessTokenEnc    []byte     // 模型2(0020):该组织 new-api user 的 access_token,应用层加密存(建员工 token 用);列 newapi_access_token_enc
	PasswordEnc       []byte     // 模型2(0020):该组织 new-api user 的密码,加密存(access_token 失效时重登录自愈);列 newapi_password_enc
	// v1 正交属性(0022,20-§2):一种组织按属性工作,不按场景分支;场景(门A/门B)仅决定初值。
	FundingMode       string // self_funded(v1 恒定,钱在 new-api)/platform_funded(v2 托管)
	CreatedByPlatform bool   // provenance:门A=true(可清资产/可自愈)/门B=false(绝不删企业资产/运维重粘);列 newapi_user_created_by_platform
	MemberCapMode     string // shared(v1 恒定,全员共享池子)/quota(v2 按人硬分)
	BillingKind       string // wallet(读求和余额)/subscription(订阅计费,余额页显示"订阅计费")
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// v1 组织正交属性取值(0022,20-§2)。
const (
	FundingSelfFunded     = "self_funded"     // 钱在 new-api,平台不经手(v1 全部)
	FundingPlatformFunded = "platform_funded" // 平台经手钱/托管桶(v2)
	CapModeShared         = "shared"          // 全员 unlimited 共享池子(v1 全部)
	CapModeQuota          = "quota"           // 按人硬分 remain_quota(v2)
	BillingKindWallet     = "wallet"          // 钱包计费:余额=读求和
	BillingKindSub        = "subscription"    // 订阅计费:池子不反映消费,余额页显示"订阅计费"
)

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

// ===== 模型2(0020)新增实体:组织树 / 托管多桶 =====

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

// OrgEscrowConfig 对应 org_escrow_config 表(0021)。生效阈值=COALESCE(ThresholdManualOverride, ThresholdAuto)。
type OrgEscrowConfig struct {
	OrgID                   int64
	ThresholdAuto           int64  // 每天按近7天补货点重算
	ThresholdManualOverride *int64 // 运维手动定(优先);nil=用 auto
	ConsumedBaseline        *int64 // B1:funding 激活时快照的 SUM(ledger);窗口纠偏/对账只算此后增量。nil=未快照
	UpdatedAt               time.Time
}

// EffectiveThreshold 生效续充阈值:手动覆盖优先,否则自动值。
func (c *OrgEscrowConfig) EffectiveThreshold() int64 {
	if c.ThresholdManualOverride != nil {
		return *c.ThresholdManualOverride
	}
	return c.ThresholdAuto
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

// 档位额度型 / 可见性(架构B 0032,31-ADR §5):fixed=固定/单次(不重置)| subscription=订阅/周期(worker 补满)。
// 无「无上限」档。visibility:all=全组织成员可用(无需 grant 行)/ assigned=须经 tier_grant 授权到成员或团队。
const (
	TierQuotaFixed         = "fixed"
	TierQuotaSubscription  = "subscription"
	TierVisibilityAll      = "all"
	TierVisibilityAssigned = "assigned"
)

// Tier 对应 tier 表(09 §5 + 架构B 0032)。ModelSet/ModelCap 以 JSON 字符串透传(本期不解析)。
// 架构B:额度落成员 user.quota(AmountRaw 为唯一额度值);旧三档 limit 保留废弃(0032,代码停引用)。
type Tier struct {
	ID           int64
	OrgID        int64
	Name         string
	ModelSet     []string         // 允许的模型集合;空 = 继承组织默认
	ModelCap     map[string]int64 // 单模型日上限(quota),如 {"claude-opus":50000};软限额(E4)
	QuotaType    string           // 架构B(0032):fixed | subscription
	AmountRaw    *int64           // 架构B(0032):额度值(raw quota;建成员必设、正数、≤成员帽、不可 0)
	ResetPeriod  *string          // 架构B(0032):subscription 的周期 daily|weekly|monthly;fixed=nil
	Visibility   string           // 架构B(0032):all | assigned(经 tier_grant 授权)
	DailyLimit   *int64           // Deprecated: 架构A 遗留(0032 废弃,读兼容保留)
	WeeklyLimit  *int64           // Deprecated: 架构A 遗留(0032 废弃,读兼容保留)
	MonthlyLimit *int64           // Deprecated: 架构A 遗留(0032 废弃,读兼容保留)
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
	SessionEpoch         int // A3:会话代次;禁用/降级/改密/硬停自增,令旧平台 token 立即失效
	// 架构B(0030):成员=各自 new-api user(平台托管服务账号)。凭证密文不进本结构(单独 repo 方法取,防密文到处传)。
	NewapiUserID   *int64  // 成员自己的 new-api user id(nil=未开通/旧A版数据)
	NewapiUsername *string // 成员 new-api 用户名(日志按 username 查、401 自愈重登用)
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// key 槽 / 物理令牌状态(v2 0015)。
const (
	KeySlotActive      = "active"
	KeySlotRevoked     = "revoked"
	KeyTokenActive     = "active"
	KeyTokenRevoked    = "revoked"
	KeyTokenSuperseded = "superseded" // 轮换后被取代的旧令牌(留作历史归因)
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

// Grant/GrantPayload 及其常量(member_grant 表模型)已随临时 grant 机器退役删除(33 §12-4;表留档)。

// Balance 对应 company_balance 表(09 §7)。balance = total_recharged - total_consumed。
// 模型2:company_balance 降为派生影子/对账用(真相=工单+日志,余额读穿 escrow);
// model1 的 committed 字段已删(不超卖靠原生闸门,未来划拨用 unit_budget,14 §74)。
type Balance struct {
	OrgID          int64
	TotalRecharged int64
	TotalConsumed  int64
	TotalRefunded  int64
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

// Approval/ApprovalPayload 及其常量(approval 表模型)已随审批子系统退役删除(33 §12-4;表留档)。

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
