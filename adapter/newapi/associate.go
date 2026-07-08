package newapi

import (
	"context"
	"encoding/json"
	"fmt"
)

// 门B 关联现有 new-api 用户(v1,20-§8 / 16-方案):录入企业 user + access token → 校验(身份一致/role 普通)
// → 导入其名下全部令牌为平台成员。全部用**该用户自己的 access token**(UserAuth 端点),不需要企业密码。

const (
	stepSelfInfo   = "GetSelfInfo"
	stepListTokens = "ListUserTokens"

	// RoleCommonUser new-api 普通用户 role 值(common/constants:1;admin=10/root=100)。
	// 门B role 闸:录入的 token 必须属普通用户,拒管理员/超管(最小权限,防越权凭证放大爆炸半径)。
	RoleCommonUser = 1
)

// SelfInfo 是 GET /api/user/self 的关注字段(门B 身份/role 校验)。
type SelfInfo struct {
	ID       int    `json:"id"`
	Username string `json:"username"`
	Role     int    `json:"role"`
	Status   int    `json:"status"`
	Group    string `json:"group"` // 该用户的 new-api 用户分组(录入分组须与之一致,防呆)
}

// GetSelfInfo 用 access token 取该用户自身信息(门B 校验:解出的 user 必须==录入的 user,role 必须普通)。
func (a *Adapter) GetSelfInfo(ctx context.Context, cred MemberCred) (*SelfInfo, error) {
	res, err := a.c.do(ctx, stepSelfInfo, "GET", "/api/user/self", userAuth(cred), nil)
	if err != nil {
		return nil, err
	}
	var info SelfInfo
	if e := json.Unmarshal(res.data, &info); e != nil {
		return nil, &UpstreamError{Step: stepSelfInfo, PlatformCode: CodeInternal, Message: "解析用户自身信息失败", class: classNonRetryable, cause: e}
	}
	return &info, nil
}

// UserToken 是令牌快照(门B 导入 F4 + 架构B 成员自助令牌列表复用;只读展示,明文拿不到)。
// 架构B(阶段1)加性补齐:remain_quota/used_quota/allow_ips(成员「我的令牌」页显示额度/剩余/IP/用量)。
type UserToken struct {
	ID             int    `json:"id"`
	Name           string `json:"name"`
	Group          string `json:"group"`
	Status         int    `json:"status"` // 1=enabled 2=disabled
	UnlimitedQuota bool   `json:"unlimited_quota"`
	RemainQuota    int64  `json:"remain_quota"` // 令牌剩余额度(raw;finite 令牌的子上限)
	UsedQuota      int64  `json:"used_quota"`   // 令牌已用(raw,new-api 原生计数)
	ExpiredTime    int64  `json:"expired_time"`
	ModelLimits    string `json:"model_limits"`
	AllowIPs       string `json:"allow_ips"` // IP 白名单(单 IP/CIDR,逗号分隔;空=不限)
	KeyMasked      string `json:"key"`       // 列表接口已脱敏(buildMaskedTokenResponse)
}

// ListUserTokens 列该用户名下**全部**令牌(门B 导入/漂移同步用)。
// 必须翻页拉全(p+page_size,pageInfo{items,total}):企业令牌可能几十上百,只拉第一页会漏导入成员、
// 漂移同步还会把漏的误判成"已删"标离职(20 审计新-D)。
func (a *Adapter) ListUserTokens(ctx context.Context, cred MemberCred) ([]UserToken, error) {
	var out []UserToken
	const pageSize = 100
	for page := 1; page <= 100; page++ { // 上限 1 万枚,防御死循环
		res, err := a.c.do(ctx, stepListTokens, "GET",
			q("/api/token/", map[string]string{"p": fmt.Sprintf("%d", page), "page_size": fmt.Sprintf("%d", pageSize)}), userAuth(cred), nil)
		if err != nil {
			return nil, err
		}
		var paged struct {
			Items []UserToken `json:"items"`
			Total int         `json:"total"`
		}
		if e := json.Unmarshal(res.data, &paged); e != nil {
			return nil, &UpstreamError{Step: stepListTokens, PlatformCode: CodeInternal, Message: "解析令牌列表失败", class: classNonRetryable, cause: e}
		}
		out = append(out, paged.Items...)
		if page*pageSize >= paged.Total || len(paged.Items) == 0 {
			break
		}
	}
	return out, nil
}
