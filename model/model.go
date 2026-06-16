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
