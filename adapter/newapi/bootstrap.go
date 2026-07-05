package newapi

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
)

// BootstrapMember 开通成员代发 key 全链路(10 §2.1 / §5.6 / 05 §5.6):
//
//	① CreateUser(AdminAuth)            → 得 newapi_user_id(已存在则接管,§2.5)
//	② POST /api/user/login(成员)      → 得 session cookie
//	③ GET /api/user/token(取一次)     → access_token(一调即旋转,绝不盲目重调,§2.2 step③)
//	④ POST /api/token/(头认证建令牌)  → token_id(确定性 name 幂等,§2.5)
//	⑤ POST /api/token/:id/key          → 明文 key(仅返一次)
//
// 并发互斥:同一成员的 bootstrap 在 `bootstrap:{org}:{member}` 锁内串行化(§2.6),
// 确保步骤③的 access_token "取→存"原子,绝不让两路并发各自旋转互相作废。
//
// 补偿(§2.3):CreateUser 成功后任一步失败 → disable 该用户(宁留可恢复的禁用态,
// 不留可被调用的活跃孤儿);**绝不删用户**(其 logs 关联,删语义重)。
func (a *Adapter) BootstrapMember(ctx context.Context, in BootstrapInput) (BootstrapResult, error) {
	if in.Username == "" || in.Password == "" {
		return BootstrapResult{}, &UpstreamError{Step: stepCreateUser, PlatformCode: CodeInternal, Message: "缺少确定性 username/password", class: classNonRetryable}
	}
	// rc.4 校验约束(源码核实):username<=20、password 8-20 字符。提前挡掉,
	// 避免拿到上游不可读的 50201,并提示 service 层修正确定性派生口径(§2.5)。
	if len(in.Username) > 20 {
		return BootstrapResult{}, &UpstreamError{Step: stepCreateUser, PlatformCode: CodeInternal, Message: "用户名超过 new-api 上限(<=20 字符)", class: classNonRetryable}
	}
	if len(in.Password) < 8 || len(in.Password) > 20 {
		return BootstrapResult{}, &UpstreamError{Step: stepCreateUser, PlatformCode: CodeInternal, Message: "密码不满足 new-api 约束(8-20 字符)", class: classNonRetryable}
	}

	release, err := a.locker.Acquire(ctx, bootstrapLockKey(in.OrgID, in.MemberID))
	if err != nil {
		return BootstrapResult{}, &UpstreamError{Step: "lock", PlatformCode: CodeInternal, Message: "获取 bootstrap 锁失败", class: classRetryable, cause: err}
	}
	defer release()

	// 归属校验闸(v1.1 项B):AllowAdopt=false(首次 provision,Username 是刚生成的随机名)时,**建之前先确定性预检**——
	// 名若已存在 = 撞了外部用户(我们的随机名库里还没有)→ 立即返 UsernameConflict,**绝不 CreateUser/接管/disable**,
	// 由调用方重生成随机名重试。不靠解析上游"已存在"错误消息(不稳),用 getUserByUsername 直接判,确定性。
	if !in.AllowAdopt {
		if _, found, ferr := a.getUserByUsername(ctx, in.Username); ferr == nil && found {
			a.c.log("WARN", "bootstrap.username_conflict_external", map[string]any{"org_id": in.OrgID, "username": in.Username})
			return BootstrapResult{}, &UpstreamError{Step: stepCreateUser, PlatformCode: CodeUsernameConflict, Message: "new-api 用户名已被占用(疑外部用户),不接管", class: classNonRetryable}
		}
	}

	// ① CreateUser:username 作幂等键。撞"已存在"的处置分两种(v1.1 项B 归属校验闸):
	//   - AllowAdopt=true(重开/重试,Username 是本组织库里存好的名)→ 接管(确是自己的用户,§2.5)。
	//   - AllowAdopt=false(预检后仍撞,罕见并发抢注)→ **绝不接管**,返 ErrUsernameConflict 让调用方重生成;**绝不 disable**。
	adopted := false
	_, cerr := a.c.do(ctx, stepCreateUser, "POST", "/api/user/", adminAuth(a.c.cfg), map[string]any{
		"username":     in.Username,
		"password":     in.Password,
		"display_name": truncateRunes(in.DisplayName, 20), // rc.4 display_name max=20
		"role":         roleValue(in.Role),
	})
	if cerr != nil {
		if isAlreadyExists(cerr) {
			if !in.AllowAdopt {
				// 首次 provision 撞名 = 撞外部用户。绝不接管、绝不触碰那个用户,直接返冲突让上层重生成。
				a.c.log("WARN", "bootstrap.username_conflict_external", map[string]any{"org_id": in.OrgID, "username": in.Username})
				return BootstrapResult{}, &UpstreamError{Step: stepCreateUser, PlatformCode: CodeUsernameConflict, Message: "new-api 用户名已被占用(疑外部用户),不接管", class: classNonRetryable}
			}
			adopted = true
			a.c.log("INFO", "bootstrap.adopt_existing_user", map[string]any{"org_id": in.OrgID, "member_id": in.MemberID})
		} else {
			// 5xx 已由 client 退避重试过仍失败,或 4xx 语义错;此时用户未确定建成。
			// 但 5xx 可能"已建成"(§2.2 ①):对 AllowAdopt=false 的首次 provision,Username 是刚生成的高熵随机名,
			// 若此时查得到 = 极大概率是我们 5xx-但已建成的那个(外部预先占用同一随机名的概率 ~2^-80,可忽略)→ 接管安全。
			if id, found, ferr := a.getUserByUsername(ctx, in.Username); ferr == nil && found {
				adopted = true
				a.c.log("WARN", "bootstrap.create_user_uncertain_adopted", map[string]any{"member_id": in.MemberID, "user_id": id})
			} else {
				return BootstrapResult{}, asUpstreamError(stepCreateUser, cerr)
			}
		}
	}

	// 取 user_id(新建或接管都靠查重统一定位)。
	userID, found, err := a.getUserByUsername(ctx, in.Username)
	if err != nil {
		return BootstrapResult{}, asUpstreamError(stepGetUser, err)
	}
	if !found {
		return BootstrapResult{}, &UpstreamError{Step: stepGetUser, PlatformCode: CodeUpstreamCreateU, Message: "建用户后未能定位该用户", class: classRetryable}
	}

	// —— 从这里起用户已存在于 new-api,任何失败都要补偿 disable(§2.3)——
	res := BootstrapResult{NewapiUserID: userID, AdoptedExisting: adopted}

	accessToken, err := a.loginAndGetToken(ctx, in.Username, in.Password)
	if err != nil {
		a.compensateDisable(ctx, userID, "loginOrGetToken")
		return BootstrapResult{}, err
	}
	res.AccessToken = accessToken

	// 改动②(MVP):只建用户、不建令牌——令牌由员工自助建(改动③)。access_token 已取并随 res 返回,
	// 供后续自助建 key。此分支不走 ④CreateToken/⑤RevealTokenKey,返回 TokenID=0/PlaintextKey="",调用方据此走"无令牌"分支。
	if in.SkipToken {
		a.c.log("INFO", "bootstrap.user_only_skip_token", map[string]any{"org_id": in.OrgID, "member_id": in.MemberID, "user_id": userID})
		return res, nil
	}

	cred := MemberCred{NewapiUserID: userID, AccessToken: accessToken}
	tokenID, err := a.CreateToken(ctx, cred, defaultBootstrapTokenSpec(in.MemberID))
	if err != nil {
		// 建 token 失败但用户/凭证 OK:§2.3 规定**不 disable 用户**(凭证有效),
		// 仅标 token 缺失、异步重试;但本 adapter 无平台库,统一交由调用方据返回的
		// 失败步骤决定。这里不 disable,直接返错(凭证有效,成员显示"配置中")。
		a.c.log("WARN", "bootstrap.create_token_failed_user_ok", map[string]any{"user_id": userID})
		// GZ-03:带回 res(含 NewapiUserID)以便调用方收口禁用该 active 用户(否则留下活跃孤儿,失管+漏扣)。
		return res, err
	}
	res.TokenID = tokenID

	key, err := a.RevealTokenKey(ctx, cred, tokenID)
	if err == nil && os.Getenv("NEXUS_IT_FAULT_REVEAL_FAIL") == "1" {
		// 仅集成测试故障注入(生产绝不设此 env):令第⑤步 RevealTokenKey 报失败,走收口路径以复测
		// GZ-03 返工(④建 token 成功后⑤失败 → 收口禁用孤儿 + 回写 user_id + 漏扣告警)。此时 token 已真建。
		err = &UpstreamError{Step: stepRevealKey, PlatformCode: CodeInternal, Message: "注入故障:RevealTokenKey 失败(仅测试)", class: classNonRetryable}
	}
	if err != nil {
		// 取 key 失败:token 已建,可独立重试取;不回滚用户/token(§2.2 ⑤)。
		// 返回 masked 占位由调用方标"待补取";此处返错带 token_id 以便后续补取。
		a.c.log("WARN", "bootstrap.reveal_key_failed_token_ok", map[string]any{"user_id": userID, "token_id": tokenID})
		return res, err
	}
	res.PlaintextKey = key
	a.c.log("INFO", "bootstrap.done", map[string]any{
		"org_id": in.OrgID, "member_id": in.MemberID, "user_id": userID, "token_id": tokenID, "adopted": adopted,
	})
	return res, nil
}

// RefreshAccessToken 在 access_token 失效时(ProbeAccessToken 返 false)用留存密码重取一次。
// 自愈路径(10 §2.7):必须在同一把 bootstrap 锁内,确保不与并发各自旋转。
// 注意:此调用会旋转作废任何旧 access_token,调用方拿到新值后须立即加密落库。
func (a *Adapter) RefreshAccessToken(ctx context.Context, in BootstrapInput) (string, error) {
	release, err := a.locker.Acquire(ctx, bootstrapLockKey(in.OrgID, in.MemberID))
	if err != nil {
		return "", &UpstreamError{Step: "lock", PlatformCode: CodeInternal, Message: "获取 bootstrap 锁失败", class: classRetryable, cause: err}
	}
	defer release()
	return a.loginAndGetToken(ctx, in.Username, in.Password)
}

// loginAndGetToken 执行步骤②③:登录拿 cookie → GET token 取 access_token。
// 步骤③特例(§2.2 step③):返回空 access_token = 直接判失败,**绝不再调一次碰运气**
// (再调会旋转作废刚才那个)。client 内部对 5xx 的重试始终在本锁内,安全。
func (a *Adapter) loginAndGetToken(ctx context.Context, username, password string) (string, error) {
	loginRes, err := a.c.do(ctx, stepLogin, "POST", "/api/user/login", noAuth(), map[string]any{
		"username": username,
		"password": password,
	})
	if err != nil {
		return "", err
	}
	cookie := loginRes.setCookie
	if cookie == "" {
		return "", &UpstreamError{Step: stepLogin, PlatformCode: CodeUpstreamAuth, Message: "登录未返回会话", class: classNonRetryable}
	}
	// 登录响应里带成员 user_id;GET token 需要它作 New-Api-User 头(双头,05 §1.2)。
	loginUserID := parseLoginUserID(loginRes.data)
	if loginUserID == 0 {
		return "", &UpstreamError{Step: stepLogin, PlatformCode: CodeUpstreamAuth, Message: "登录未返回用户标识", class: classNonRetryable}
	}

	tokRes, err := a.c.do(ctx, stepGetToken, "GET", "/api/user/token", sessionAuth(cookie, loginUserID), nil)
	if err != nil {
		return "", err
	}
	accessToken := parseAccessToken(tokRes.data)
	if accessToken == "" {
		// 空 token:不可重试,绝不再调(再调旋转作废)。判失败。
		return "", &UpstreamError{Step: stepGetToken, PlatformCode: CodeUpstreamAuth, Message: "上游未返回 access_token", class: classNonRetryable}
	}
	return accessToken, nil
}

// compensateDisable 执行 §2.3 补偿:禁用(不删)该 new-api 用户,留可恢复的禁用态。
// 补偿失败本身只告警,不掩盖原始错误。
func (a *Adapter) compensateDisable(ctx context.Context, userID int, failedStep string) {
	if derr := a.SetUserStatus(ctx, userID, false); derr != nil {
		a.c.log("ERROR", "bootstrap.compensate_disable_failed", map[string]any{"user_id": userID, "failed_step": failedStep})
		return
	}
	a.c.log("WARN", "bootstrap.compensated_disable", map[string]any{"user_id": userID, "failed_step": failedStep})
}

func bootstrapLockKey(orgID, memberID int64) string {
	return fmt.Sprintf("bootstrap:%d:%d", orgID, memberID)
}

// defaultBootstrapTokenSpec 是开通时建的基线令牌:unlimited(令牌层不限),
// 实际当期上限由**用户层 quota** 控制(ManageUserQuota override,见 02 §3)。
// 层级的 group / model_limits 由 service 后续按 tier 应用,bootstrap 只保证拿到可用 key。
func defaultBootstrapTokenSpec(memberID int64) TokenSpec {
	return TokenSpec{
		Name:           deriveTokenName(memberID, 1),
		UnlimitedQuota: true,
		ExpiredTime:    -1, // 永不过期
	}
}

// deriveTokenName 确定性派生 token name(§2.5 建 token 幂等键)。rotation 轮换时递增。
func deriveTokenName(memberID int64, rotation int) string {
	return fmt.Sprintf("nexus_m%d_v%d", memberID, rotation)
}

// truncateRunes 按 rune 截断到 n(rc.4 校验按字符数,中文 display_name 安全)。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func roleValue(role string) any {
	// C16:new-api role 是整型(普通用户=1)。收敛为**显式 int 映射**,绝不把字符串塞进整型字段(原来非空字符串
	// 直接透传给整型 role 字段=埋雷)。平台只建普通用户;任何值(含未知)都归普通用户,绝不误建管理员。
	switch role {
	case "", "common", "user", "1":
		return 1
	default:
		return 1
	}
}

// parseLoginUserID 从 POST /api/user/login 的 data 中取 user id。
func parseLoginUserID(data json.RawMessage) int {
	var obj struct {
		ID int `json:"id"`
	}
	if err := json.Unmarshal(data, &obj); err == nil {
		return obj.ID
	}
	return 0
}

// parseAccessToken 解析 GET /api/user/token 返回:rc.4 实证为裸字符串;兼容 {access_token}/{key}。
func parseAccessToken(data json.RawMessage) string {
	var s string
	if err := json.Unmarshal(data, &s); err == nil && s != "" {
		return s
	}
	var obj struct {
		AccessToken string `json:"access_token"`
		Key         string `json:"key"`
	}
	if err := json.Unmarshal(data, &obj); err == nil {
		if obj.AccessToken != "" {
			return obj.AccessToken
		}
		if obj.Key != "" {
			return obj.Key
		}
	}
	return ""
}
