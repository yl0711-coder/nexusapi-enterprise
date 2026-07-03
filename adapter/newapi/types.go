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
	SkipToken   bool   // 改动②(MVP):只建用户、不建令牌(令牌由员工自助建,改动③);仍取 access_token 供自助建 key
	// AllowAdopt v1.1 项B 归属校验闸:
	//   true  = 重开/重试同组织,Username 是本组织库里存好的名 → CreateUser 撞"已存在"即接管(确是自己的用户)。
	//   false = 首次 provision,Username 是刚生成的随机名(库里还没有)→ 撞"已存在"= 撞了外部用户 → **绝不接管**,
	//           返 ErrUsernameConflict(IsUsernameConflict 可判),由调用方重生成随机名重试;绝不 disable 那个外部用户。
	AllowAdopt bool
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
	SetTokenStatus(ctx context.Context, cred MemberCred, tokenID int, enabled bool) error // 禁用不删(status_only,近实时,保留key)
	DeleteToken(ctx context.Context, cred MemberCred, tokenID int) error

	// 额度/状态走 user_id(管理员身份,不冒充):
	ManageUserQuota(ctx context.Context, userID int, mode QuotaMode, quota int64) error // override/add/subtract
	GetUserQuota(ctx context.Context, userID int) (quota int64, err error)              // 读 org user 当前剩余额度(模型2 读穿余额=桶1)
	SetUserStatus(ctx context.Context, userID int, enabled bool) error                  // enable/disable

	// ProbeAccessToken 运行期探测 access_token 是否仍有效(10 §2.7)。
	ProbeAccessToken(ctx context.Context, cred MemberCred) (valid bool, err error)
	// RefreshAccessToken 用 username+password 重登派生新 access_token(401 自愈:失效才重登)。
	RefreshAccessToken(ctx context.Context, in BootstrapInput) (accessToken string, err error)

	// 订阅口径(v1 H1,20-§7):检测 active 订阅/当前偏好 + 设 wallet_only 堵订阅旁路(自助端点,零改 new-api)。
	GetSelfSubscription(ctx context.Context, cred MemberCred) (*SelfSubscription, error)
	SetBillingPreference(ctx context.Context, cred MemberCred, pref string) error

	// 门B 关联(v1,20-§8):身份/role 校验 + 导入现有令牌(翻页拉全)。
	GetSelfInfo(ctx context.Context, cred MemberCred) (*SelfInfo, error)
	ListUserTokens(ctx context.Context, cred MemberCred) ([]UserToken, error)

	// ReadConsumptionLogs 读消费日志窗口(type=2,小窗口分页,绝不全表),供计费结算(03 §3.1)。
	ReadConsumptionLogs(ctx context.Context, sinceUnix, untilUnix int64, page, pageSize int) ([]LogEntry, int, error)

	// 计价/折扣联动(03 §3.5.1,单向写入 new-api、只读回显):
	// 平台只写 GroupGroupRatio(分组特殊倍率,覆盖式);GroupRatio 只读(取基础倍率快照)。
	GetGroupRatio(ctx context.Context, group string) (ratio float64, configured bool, err error)
	GetGroupGroupRatio(ctx context.Context, userGroup, tokenGroup string) (ratio float64, configured bool, err error)
	SetGroupGroupRatio(ctx context.Context, userGroup, tokenGroup string, ratio float64) error
	// DeleteGroupGroupRatio 删掉某「用户分组×令牌分组」特殊倍率条目(取消折扣回落基础倍率;
	// merge-preserve + 单写者锁,不堆死键。R2-轻微:mode=none 应删键而非写 base)。
	DeleteGroupGroupRatio(ctx context.Context, userGroup, tokenGroup string) error
	// SetOrgGroupRatios 以平台镜像为权威源,一次性把某用户分组(org_{id})下的全部令牌分组特殊倍率
	// 设为 desired(authoritative replace 该用户分组,绝不采信上游读回的己方旧值),同时 merge-preserve
	// 其它用户分组(vip 等手工键)。desired 为空 → 删除该用户分组。写后读校验 + 退避重试,
	// 应对 new-api option 读缓存滞后(read-after-write,T1 动钱)。折扣写路径专用。
	SetOrgGroupRatios(ctx context.Context, userGroup string, desired map[string]float64) error

	// SetUserGroup 设 new-api 用户分组(读-改-写,quota 不丢);折扣按用户分组归属(A2)。
	SetUserGroup(ctx context.Context, userID int, group string) error

	// 计费分组能力(T17-4):
	// ListGroupRatios 读「分组 → 基础倍率」(系统现有计费分组);
	// ListGroupModels 读「分组 → 可用模型」(走 /api/pricing 反转,配置期预检 D4);
	// AddOrgUsableGroup 把业务分组加进某 org 用户分组的可用分组(§3 硬约束,不补则 403)。
	ListGroupRatios(ctx context.Context) (map[string]float64, error)
	ListGroupModels(ctx context.Context) (map[string][]string, error)
	AddOrgUsableGroup(ctx context.Context, userGroup, group string) error
	// GetOrgUsableGroups 读某 org 用户分组的可用模型分组列表(改动③a;建组织校验①用)。
	GetOrgUsableGroups(ctx context.Context, userGroup string) ([]string, error)
}
