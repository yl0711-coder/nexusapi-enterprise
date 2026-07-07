// 架构B 阶段0(33 §3.1/§4):划账适配层护栏。
// new-api 引擎零保护(31-ADR §12-1/2,rc.4 源码核实):IncreaseUserQuota 无上界(写超 int32 → MySQL 报错
// 但 Redis INCRBY 照涨 = cache/DB 分叉);DecreaseUserQuota 无下溢地板(能减成负)。
// 平台是唯一防线:每次写 quota 前强制 0 ≤ raw ≤ int32 上限;减前按实时余额 clamp≥0。
package newapi

import (
	"context"
	"encoding/json"
	"fmt"
)

// Int32QuotaMax 为 new-api user.Quota 的 MySQL INT32 上限(≈$4294 @QuotaPerUnit=500000)。
const Int32QuotaMax = int64(2147483647)

// IncreaseUserQuota 给用户加额度(划账入账侧;走 ManageUser add,同步刷新 Redis 缓存)。
// 【护栏】amountRaw 必须为正;写后余额不得超 int32(先读实时值预检,防 cache/DB 分叉)。
func (a *Adapter) IncreaseUserQuota(ctx context.Context, userID int, amountRaw int64) error {
	if amountRaw <= 0 {
		return &UpstreamError{Step: stepManageUser, PlatformCode: CodeInternal, Message: fmt.Sprintf("加额度金额非法(必须>0,得 %d)", amountRaw), class: classNonRetryable}
	}
	cur, err := a.GetUserQuota(ctx, userID)
	if err != nil {
		return err
	}
	if cur+amountRaw > Int32QuotaMax {
		return &UpstreamError{Step: stepManageUser, PlatformCode: CodeInternal,
			Message: fmt.Sprintf("写后额度将超 int32 上限(当前 %d + %d > %d),拒绝(防 cache/DB 分叉)", cur, amountRaw, Int32QuotaMax), class: classNonRetryable}
	}
	return a.ManageUserQuota(ctx, userID, QuotaAdd, amountRaw)
}

// DecreaseUserQuota 给用户减额度(划账出账侧;走 ManageUser subtract,同步刷新 Redis 缓存)。
// 【护栏】amountRaw 必须为正;减前按**实时余额** clamp——实际扣减 min(amountRaw, 当前余额),
// 绝不把 quota 减成负(引擎无地板)。返回实际扣减值(离职退额按实时余额结转时调用方需要)。
func (a *Adapter) DecreaseUserQuota(ctx context.Context, userID int, amountRaw int64) (int64, error) {
	if amountRaw <= 0 {
		return 0, &UpstreamError{Step: stepManageUser, PlatformCode: CodeInternal, Message: fmt.Sprintf("减额度金额非法(必须>0,得 %d)", amountRaw), class: classNonRetryable}
	}
	cur, err := a.GetUserQuota(ctx, userID)
	if err != nil {
		return 0, err
	}
	actual := amountRaw
	if cur < actual {
		actual = cur // clamp:最多减到 0
	}
	if actual <= 0 {
		return 0, nil // 余额已 0,无可减(幂等安全)
	}
	if err := a.ManageUserQuota(ctx, userID, QuotaSubtract, actual); err != nil {
		return 0, err
	}
	return actual, nil
}

// GetQuotaPerUnit 读所连 new-api 实例的实际 QuotaPerUnit(GET /api/status,misc.go:74 透出)。
// 启动自检 VerifyQuotaPerUnit(33 §3.6)用:与 platform_setting.quota_per_unit 比对,不一致拒启动。
func (a *Adapter) GetQuotaPerUnit(ctx context.Context) (float64, error) {
	res, err := a.c.do(ctx, stepStatus, "GET", "/api/status", noAuth(), nil)
	if err != nil {
		return 0, err
	}
	var env struct {
		QuotaPerUnit float64 `json:"quota_per_unit"`
	}
	if e := json.Unmarshal(res.data, &env); e != nil {
		return 0, &UpstreamError{Step: stepStatus, PlatformCode: CodeInternal, Message: "解析 /api/status 失败", class: classNonRetryable, cause: e}
	}
	if env.QuotaPerUnit <= 0 {
		return 0, &UpstreamError{Step: stepStatus, PlatformCode: CodeInternal, Message: "new-api 未透出 quota_per_unit", class: classNonRetryable}
	}
	return env.QuotaPerUnit, nil
}
