package newapi

import (
	"context"
	"encoding/json"
	"fmt"
)

// SetUserGroup 把 new-api 用户的分组设为 group(读-改-写,保留 quota 等其它字段,A2)。
// 折扣按"用户分组"归属:开通成员时设成 org_{id},GroupGroupRatio 即按此查折扣。
func (a *Adapter) SetUserGroup(ctx context.Context, userID int, group string) error {
	// 读当前用户对象。
	res, err := a.c.do(ctx, stepGetUser, "GET", fmt.Sprintf("/api/user/%d", userID), adminAuth(a.c.cfg), nil)
	if err != nil {
		return err
	}
	var u map[string]any
	if uerr := json.Unmarshal(res.data, &u); uerr != nil {
		return &UpstreamError{Step: stepGetUser, PlatformCode: CodeInternal, Message: "解析用户失败", class: classNonRetryable, cause: uerr}
	}
	if u == nil {
		return &UpstreamError{Step: stepGetUser, PlatformCode: CodeUpstreamBizReject, Message: "用户不存在", class: classNonRetryable}
	}
	if cur, _ := u["group"].(string); cur == group {
		return nil // 已是目标分组,免写
	}
	u["group"] = group
	// 读-改-写回(保留 quota 等)。
	if _, werr := a.c.do(ctx, stepManageUser, "PUT", "/api/user/", adminAuth(a.c.cfg), u); werr != nil {
		return werr
	}
	return nil
}
