package newapi

import "context"

// new-api token 状态值域(common/constants.go:217 源码核实)。
const (
	tokenStatusEnabled  = 1 // 启用
	tokenStatusDisabled = 2 // 禁用(非 Enabled 即被 relay 拦,model/token.go:196)
)

// UpdateToken 就地更新令牌(PUT /api/token/,不旋转 key)。用于改 IP 白名单等(E22)。
// 复用成员 access_token 头认证;重申已知字段(name/unlimited/expired)避免被清空。
func (a *Adapter) UpdateToken(ctx context.Context, cred MemberCred, tokenID int, spec TokenSpec) error {
	body := tokenPayload(spec)
	body["id"] = tokenID
	if _, err := a.c.do(ctx, stepCreateToken, "PUT", "/api/token/", userAuth(cred), body); err != nil {
		return err
	}
	return nil
}

// SetTokenStatus 启停令牌(禁用不删,R5后步骤4)。走 **PUT /api/token/?status_only=1**:new-api 控制器(controller/token.go:250)
// 先 GetTokenByIds 再**只覆盖 status**(保留 name/group/quota),且 Token.Update() 会 cacheSetToken 刷 Redis 缓存
// → 近实时生效(非等缓存 TTL,源码核实 token.go:255 GetTokenByKey+token_cache.go)。key 保留、启用即通。
func (a *Adapter) SetTokenStatus(ctx context.Context, cred MemberCred, tokenID int, enabled bool) error {
	status := tokenStatusDisabled
	if enabled {
		status = tokenStatusEnabled
	}
	body := map[string]any{"id": tokenID, "status": status}
	if _, err := a.c.do(ctx, stepCreateToken, "PUT", "/api/token/?status_only=1", userAuth(cred), body); err != nil {
		return err
	}
	return nil
}
