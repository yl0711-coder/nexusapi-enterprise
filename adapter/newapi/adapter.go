package newapi

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/nexusapi-platform/enterprise/pkg/lock"
)

// Adapter 是 NewapiAdapter 的实现。对 new-api 的所有调用收口于此 + 走 client 单出口。
type Adapter struct {
	c      *client
	locker lock.KeyedLocker
}

// New 创建 adapter。locker 用于同成员 bootstrap 串行化(10 §2.6);传 nil 则用进程内锁(MVP 单实例)。
func New(cfg Config, locker lock.KeyedLocker) *Adapter {
	if locker == nil {
		locker = lock.NewInProcessLocker()
	}
	return &Adapter{c: newClient(cfg), locker: locker}
}

var _ NewapiAdapter = (*Adapter)(nil)

// --- 令牌 CRUD:复用已存 access_token 纯头认证(永不再登录,05 §5.3)---

// CreateToken 以成员身份建令牌,返回 token_id。
// 幂等:按确定性 spec.Name 先查重(命中复用)再建(10 §2.5)。
func (a *Adapter) CreateToken(ctx context.Context, cred MemberCred, spec TokenSpec) (int, error) {
	// 先查是否已存在同名 token(重试/并发安全):命中直接复用,不重建。
	if id, found, err := a.findTokenByName(ctx, cred, spec.Name); err != nil {
		return 0, err
	} else if found {
		a.c.log("INFO", "token.adopt_existing", map[string]any{"user_id": cred.NewapiUserID, "token_id": id})
		return id, nil
	}
	if _, err := a.c.do(ctx, stepCreateToken, "POST", "/api/token/", userAuth(cred), tokenPayload(spec)); err != nil {
		return 0, err
	}
	// 建成功后按 name 取回 id(new-api 建 token 不一定回 id,统一靠列表定位)。
	id, found, err := a.findTokenByName(ctx, cred, spec.Name)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, &UpstreamError{Step: stepCreateToken, PlatformCode: CodeUpstreamCreateTok, Message: "建令牌后未能定位该令牌", class: classRetryable}
	}
	return id, nil
}

// RevealTokenKey 取明文 key(读操作,幂等安全,可重试)。
func (a *Adapter) RevealTokenKey(ctx context.Context, cred MemberCred, tokenID int) (string, error) {
	res, err := a.c.do(ctx, stepRevealKey, "POST", "/api/token/"+strconv.Itoa(tokenID)+"/key", userAuth(cred), nil)
	if err != nil {
		return "", err
	}
	key := parseTokenKey(res.data)
	if key == "" {
		return "", &UpstreamError{Step: stepRevealKey, PlatformCode: CodeUpstreamRevealKey, Message: "上游未返回明文 key", class: classRetryable}
	}
	return key, nil
}

// RotateToken 轮换令牌:先建新(成员不致无 key)→ 取新 key →(尽力)删旧。
// 返回新 token_id 与明文 key。删旧失败不回滚新 token(成员已有可用 key),仅告警待清理。
func (a *Adapter) RotateToken(ctx context.Context, cred MemberCred, oldTokenID int, spec TokenSpec) (int, string, error) {
	newID, err := a.CreateToken(ctx, cred, spec)
	if err != nil {
		return 0, "", err
	}
	key, err := a.RevealTokenKey(ctx, cred, newID)
	if err != nil {
		return 0, "", err
	}
	if oldTokenID > 0 && oldTokenID != newID {
		if derr := a.DeleteToken(ctx, cred, oldTokenID); derr != nil {
			// 不阻断:新 key 已可用。落 WARN 待异步清理孤儿旧 token。
			a.c.log("WARN", "token.rotate_delete_old_failed", map[string]any{
				"user_id": cred.NewapiUserID, "old_token_id": oldTokenID, "new_token_id": newID,
			})
		}
	}
	return newID, key, nil
}

// DeleteToken 吊销令牌(幂等:已不存在视为成功)。
func (a *Adapter) DeleteToken(ctx context.Context, cred MemberCred, tokenID int) error {
	_, err := a.c.do(ctx, stepDeleteToken, "DELETE", "/api/token/"+strconv.Itoa(tokenID), userAuth(cred), nil)
	if err != nil {
		// 删一个不存在的 token:按目标态幂等视为成功(10 §4.5)。new-api 对"不存在"
		// 多以 200 + success:false 返回(非 4xx),故按 not-found 语义判别而非 HTTP 码。
		if isNotFound(err) {
			a.c.log("INFO", "token.delete_idempotent_miss", map[string]any{"token_id": tokenID})
			return nil
		}
		return err
	}
	return nil
}

// --- 额度 / 状态:管理员身份按 user_id(不冒充)---

// ManageUserQuota 调成员额度。override 写绝对值(到 0 即硬停),天然幂等。
//
// 线格式(rc.4 源码核实 + 真机验证 2026-06-16):POST /api/user/manage
// {id, action:"add_quota", mode:"override"|"add"|"subtract", value:N}。
// 关键:额度字段名是 **value**(不是 quota);override 写绝对值(到 0 即硬停)。
// ManageRequest.Value 在 rc.4 是 Go int(64 位平台即 int64),大额度安全。
func (a *Adapter) ManageUserQuota(ctx context.Context, userID int, mode QuotaMode, quota int64) error {
	body := map[string]any{"id": userID, "action": "add_quota", "mode": string(mode), "value": quota}
	// 注意:do 返回的是 *UpstreamError;直接 `return err` 会把 nil 指针装进非 nil 的
	// error 接口(Go typed-nil 陷阱),故先做指针判空再返回。
	if _, err := a.c.do(ctx, stepManageUser, "POST", "/api/user/manage", adminAuth(a.c.cfg), body); err != nil {
		return err
	}
	return nil
}

// GetUserQuota 读 new-api 用户当前剩余额度(quota 列;模型2 读穿余额=桶1 实际剩余,GetUserQuota user.go:782 源码核实)。
// GET /api/user/{id}(管理员);res.data 即用户对象。
func (a *Adapter) GetUserQuota(ctx context.Context, userID int) (int64, error) {
	res, err := a.c.do(ctx, stepGetUser, "GET", fmt.Sprintf("/api/user/%d", userID), adminAuth(a.c.cfg), nil)
	if err != nil {
		return 0, err
	}
	var env struct {
		Quota int64 `json:"quota"`
	}
	if e := json.Unmarshal(res.data, &env); e != nil {
		return 0, &UpstreamError{Step: stepGetUser, PlatformCode: CodeInternal, Message: "解析用户额度失败", class: classNonRetryable, cause: e}
	}
	return env.Quota, nil
}

// SetUserStatus 启停成员(enable/disable)。停用即断、恢复即通,无需重 bootstrap(05 §5.3)。
// 目标态幂等:重复 enable/disable 无害(10 §4.5)。
func (a *Adapter) SetUserStatus(ctx context.Context, userID int, enabled bool) error {
	action := "disable"
	if enabled {
		action = "enable"
	}
	body := map[string]any{"id": userID, "action": action}
	if _, err := a.c.do(ctx, stepSetStatus, "POST", "/api/user/manage", adminAuth(a.c.cfg), body); err != nil {
		return err
	}
	return nil
}

// ProbeAccessToken 探测 access_token 是否仍有效(10 §2.7)。
// 用一个轻量 UserAuth 端点(列自己的 token,size=1):
//   - 鉴权失效(401/access token invalid)→ valid=false, err=nil
//   - 成功 → valid=true
//   - 其它错误(5xx/超时)→ 无法判定,返回 err 让上层决定
func (a *Adapter) ProbeAccessToken(ctx context.Context, cred MemberCred) (bool, error) {
	_, err := a.c.do(ctx, stepProbe, "GET", q("/api/token/", map[string]string{"p": "0", "size": "1"}), userAuth(cred), nil)
	if err == nil {
		return true, nil
	}
	ue := asUpstreamError(stepProbe, err)
	if ue.AuthExpired() {
		return false, nil
	}
	return false, err
}

// --- 内部辅助 ---

// findTokenByName 列出成员的 token,按确定性 name 精确匹配,返回其 id。
func (a *Adapter) findTokenByName(ctx context.Context, cred MemberCred, name string) (int, bool, error) {
	res, err := a.c.do(ctx, stepCreateToken, "GET", q("/api/token/", map[string]string{"p": "0", "size": "100"}), userAuth(cred), nil)
	if err != nil {
		return 0, false, err
	}
	for _, t := range parseTokenList(res.data) {
		if t.Name == name {
			return t.ID, true, nil
		}
	}
	return 0, false, nil
}

// getUserByUsername 按确定性 username 查重(10 §2.5),返回 user_id。用于 bootstrap 接管已存在用户。
func (a *Adapter) getUserByUsername(ctx context.Context, username string) (int, bool, error) {
	res, err := a.c.do(ctx, stepGetUser, "GET", q("/api/user/search", map[string]string{"keyword": username}), adminAuth(a.c.cfg), nil)
	if err != nil {
		return 0, false, err
	}
	for _, u := range parseUserList(res.data) {
		if u.Username == username {
			return u.ID, true, nil
		}
	}
	return 0, false, nil
}

// tokenPayload 构造建 token 的请求体(05 §1.1 字段)。
func tokenPayload(spec TokenSpec) map[string]any {
	body := map[string]any{
		"name":            spec.Name,
		"remain_quota":    spec.RemainQuota,
		"unlimited_quota": spec.UnlimitedQuota,
		"expired_time":    spec.ExpiredTime,
	}
	if len(spec.ModelLimits) > 0 {
		body["model_limits_enabled"] = true
		body["model_limits"] = strings.Join(spec.ModelLimits, ",")
	} else {
		body["model_limits_enabled"] = false
	}
	if spec.Group != "" {
		body["group"] = spec.Group
	}
	if spec.AllowIPs != "" {
		body["allow_ips"] = spec.AllowIPs // R3:IP 白名单(单 IP + CIDR),网关侧校验
	}
	return body
}

// --- new-api 响应解析(对返回结构做宽松解析,容忍数组 / 分页两种形态)---

type userObj struct {
	ID       int    `json:"id"`
	Username string `json:"username"`
}

type tokenObj struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// parseUserList 解析用户搜索返回。容忍 data 为数组或 {items:[],...} 分页包装。
func parseUserList(data json.RawMessage) []userObj {
	if list := tryArray[userObj](data); list != nil {
		return list
	}
	var paged struct {
		Items []userObj `json:"items"`
		Data  []userObj `json:"data"`
		List  []userObj `json:"list"`
	}
	if err := json.Unmarshal(data, &paged); err == nil {
		switch {
		case paged.Items != nil:
			return paged.Items
		case paged.Data != nil:
			return paged.Data
		case paged.List != nil:
			return paged.List
		}
	}
	return nil
}

func parseTokenList(data json.RawMessage) []tokenObj {
	if list := tryArray[tokenObj](data); list != nil {
		return list
	}
	var paged struct {
		Items []tokenObj `json:"items"`
		Data  []tokenObj `json:"data"`
		List  []tokenObj `json:"list"`
	}
	if err := json.Unmarshal(data, &paged); err == nil {
		switch {
		case paged.Items != nil:
			return paged.Items
		case paged.Data != nil:
			return paged.Data
		case paged.List != nil:
			return paged.List
		}
	}
	return nil
}

// parseTokenKey 解析 GetTokenKey 返回:可能是裸字符串、{key:""}、或 {data:{key:""}}。
func parseTokenKey(data json.RawMessage) string {
	var s string
	if err := json.Unmarshal(data, &s); err == nil && s != "" {
		return s
	}
	var obj struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(data, &obj); err == nil && obj.Key != "" {
		return obj.Key
	}
	return ""
}

func tryArray[T any](data json.RawMessage) []T {
	var list []T
	if err := json.Unmarshal(data, &list); err == nil && list != nil {
		return list
	}
	return nil
}

// isAlreadyExists 判别上游 4xx/biz 错误是否为"用户名已存在"(可接管,而非真失败)。
// 只读 UpstreamError 的内部 upstreamMsg(绝不外泄前端)。
func isAlreadyExists(err error) bool {
	ue := asUpstreamError("", err)
	m := strings.ToLower(ue.upstreamMsg)
	return strings.Contains(m, "exist") || strings.Contains(ue.upstreamMsg, "已存在") || strings.Contains(ue.upstreamMsg, "已被")
}

// isNotFound 判别上游错误是否为"资源不存在"(删除幂等用)。只读内部 upstreamMsg。
func isNotFound(err error) bool {
	ue := asUpstreamError("", err)
	m := strings.ToLower(ue.upstreamMsg)
	return strings.Contains(m, "not found") || strings.Contains(m, "not exist") ||
		strings.Contains(ue.upstreamMsg, "不存在") || strings.Contains(ue.upstreamMsg, "无效")
}
