package newapi

import (
	"context"
	"encoding/json"
)

// 订阅口径(v1 H1,20-§7):new-api 默认 subscription_first——org 用户一旦有 active 订阅,消费走订阅
// **不扣 user.quota**(service/quota.go:411-425),读求和余额虚高、原生停服失效。两个自助端点(UserAuth,
// 用该用户自己的 access token 调,零改 new-api)用于检测与堵死:
//   GET /api/subscription/self            → {billing_preference, subscriptions:[active...]}
//   PUT /api/subscription/self/preference → 设 billing_preference(wallet_only 永不回退订阅,billing_session.go:404)

const stepSubscription = "Subscription"

// SelfSubscription 是 GET /api/subscription/self 的关注字段。
type SelfSubscription struct {
	BillingPreference string // 当前计费偏好(空/非法在 new-api 侧归一为 subscription_first)
	HasActive         bool   // 是否有 active 订阅(门B 关联检测:有 → billing_kind=subscription)
}

// GetSelfSubscription 用该用户 access token 读自身订阅状态(门A 幂等补设校验 + 门B 关联检测用)。
func (a *Adapter) GetSelfSubscription(ctx context.Context, cred MemberCred) (*SelfSubscription, error) {
	res, err := a.c.do(ctx, stepSubscription, "GET", "/api/subscription/self", userAuth(cred), nil)
	if err != nil {
		return nil, err
	}
	var env struct {
		BillingPreference string            `json:"billing_preference"`
		Subscriptions     []json.RawMessage `json:"subscriptions"` // active 订阅列表(只关心非空)
	}
	if e := json.Unmarshal(res.data, &env); e != nil {
		return nil, &UpstreamError{Step: stepSubscription, PlatformCode: CodeInternal, Message: "解析订阅状态失败", class: classNonRetryable, cause: e}
	}
	return &SelfSubscription{BillingPreference: env.BillingPreference, HasActive: len(env.Subscriptions) > 0}, nil
}

// SetBillingPreference 设该用户计费偏好(自助端点)。门A 开通/翻转时设 "wallet_only" 堵订阅旁路(H1);
// 幂等(重复设同值无害),失败可重试——调用方放进 EnsureOrgProvisioned 幂等路径,不靠一次性动作(20-§7 兜底)。
func (a *Adapter) SetBillingPreference(ctx context.Context, cred MemberCred, pref string) error {
	body := map[string]string{"billing_preference": pref}
	_, err := a.c.do(ctx, stepSubscription, "PUT", "/api/subscription/self/preference", userAuth(cred), body)
	return err
}
