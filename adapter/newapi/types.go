// Package newapi 是对 new-api(rc.4)官方管理 HTTP API 的唯一封装收口。
//
// 设计依据:
//   - 接口规格 / 失败矩阵 / 补偿 / 幂等 / 并发互斥 / 自愈:10 §2
//   - 上游契约 / rc.4 实证行为:05 §1 / §5
//   - 零数据侵入铁律:全官方 HTTP API,不碰 new-api 库/缓存
//
// 所有方法签名见 10 §2.1。本包对外暴露 NewapiAdapter 接口,内部走 Executor
// 单出口(限速/退避/熔断/短超时),上游错误一律翻译为平台 5xxxx 码、绝不透传。
package newapi

import "context"

// QuotaMode 对应 new-api ManageUser 的 add_quota 模式(05 §1.1)。
type QuotaMode string

const (
	// QuotaOverride 写绝对值(override = 到 0 即硬停),天然幂等。
	QuotaOverride QuotaMode = "override"
	// QuotaAdd 增量。
	QuotaAdd QuotaMode = "add"
	// QuotaSubtract 减量。
	QuotaSubtract QuotaMode = "subtract"
)

// BootstrapInput 是开通成员代发 key 的入参。
//
// Username/Password 采用确定性派生(见 10 §2.5),作为 new-api 侧幂等键:
// 重试/并发只会撞同一 username → CreateUser 返"已存在" → 查重接管,天然防重复建用户。
type BootstrapInput struct {
	OrgID       int64
	MemberID    int64
	Username    string // 确定性派生,如 org{org}_m{member};调用方负责保持稳定
	Password    string // 平台生成并加密留存(重 bootstrap 兜底,见 10 §2.7 / §3.1)
	DisplayName string
	Role        string // new-api role,成员一般为普通用户
}

// BootstrapResult 是代发 key 全链路成功后的产物。
//
// PlaintextKey 明文 API key 仅在此返回一次(供创建响应展示/复制),
// 平台库只存密文 + 脱敏串,后续不再回显(10 §3.1 / §1.8.1)。
// AccessToken 由调用方加密落库(10 §3.1),日常操作复用、纯头认证、永不再登录。
type BootstrapResult struct {
	NewapiUserID int
	AccessToken  string
	TokenID      int
	PlaintextKey string
	// AdoptedExisting 为 true 表示本次是"接管"了 new-api 侧已存在的用户
	// (上次 bootstrap 残留 / 并发撞键),而非全新创建。便于审计与排查。
	AdoptedExisting bool
}

// MemberCred 是复用已存 access_token 的纯头认证凭证(10 §2.1)。
// AccessToken 取自平台库时已解密(10 §3.1)。
type MemberCred struct {
	NewapiUserID int
	AccessToken  string
}

// TokenSpec 描述要建/改的 new-api 令牌(05 §1.1 的字段)。
type TokenSpec struct {
	Name           string   // 确定性派生(如 nexus_m{member}_v{rotation}),作建 token 幂等键
	RemainQuota    int64    // remain_quota
	UnlimitedQuota bool     // unlimited_quota
	ModelLimits    []string // model_limits(空 = 不限模型)
	Group          string   // 分组(层级映射,承载计价/折扣)
	ExpiredTime    int64    // expired_time,Unix 秒;-1 = 永不过期
	AllowIPs       string   // allow_ips:IP 白名单,支持单 IP 与 CIDR(R3,官方原生)
}

// NewapiAdapter 是对 new-api 官方管理 API 的唯一封装(10 §2.1)。
// 所有方法走 Executor 单出口(限速/退避/熔断/短超时)。
type NewapiAdapter interface {
	// BootstrapMember 开通成员全链路:幂等(同成员锁内串行化),返回明文 key(仅此一次)。
	// 失败矩阵见 10 §2.2,补偿见 §2.3,并发互斥见 §2.6。
	BootstrapMember(ctx context.Context, in BootstrapInput) (BootstrapResult, error)

	// 以下复用已存 access_token 的纯头认证操作(永不再登录):
	CreateToken(ctx context.Context, cred MemberCred, spec TokenSpec) (tokenID int, err error)
	RevealTokenKey(ctx context.Context, cred MemberCred, tokenID int) (plaintextKey string, err error)
	RotateToken(ctx context.Context, cred MemberCred, oldTokenID int, spec TokenSpec) (newTokenID int, key string, err error)
	UpdateToken(ctx context.Context, cred MemberCred, tokenID int, spec TokenSpec) error
	DeleteToken(ctx context.Context, cred MemberCred, tokenID int) error

	// 额度/状态走 user_id(管理员身份,不冒充):
	ManageUserQuota(ctx context.Context, userID int, mode QuotaMode, quota int64) error // override/add/subtract
	SetUserStatus(ctx context.Context, userID int, enabled bool) error                  // enable/disable

	// ProbeAccessToken 运行期探测 access_token 是否仍有效(10 §2.7)。
	ProbeAccessToken(ctx context.Context, cred MemberCred) (valid bool, err error)

	// ReadConsumptionLogs 读消费日志窗口(type=2,小窗口分页,绝不全表),供计费结算(03 §3.1)。
	ReadConsumptionLogs(ctx context.Context, sinceUnix, untilUnix int64, page, pageSize int) ([]LogEntry, int, error)

	// 计价/折扣联动(03 §3.5.1,单向写入 new-api、只读回显):
	GetGroupRatio(ctx context.Context, group string) (ratio float64, configured bool, err error)
	SetGroupRatio(ctx context.Context, group string, ratio float64) error
}
