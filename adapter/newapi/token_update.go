package newapi

import "context"

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
