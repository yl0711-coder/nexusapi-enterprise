package newapi

import (
	"context"
	"encoding/json"
)

// 计费分组能力(T17-4):平台读「系统有哪些分组 + 基础倍率」「某分组可用模型」,并把业务分组
// 加进某 org 用户分组的可用分组(§3 硬约束,不补则令牌用业务分组时 403)。

// ListGroupRatios 读 GroupRatio option:{分组: 基础倍率}。系统现有计费分组 + 各自基础倍率。
func (a *Adapter) ListGroupRatios(ctx context.Context) (map[string]float64, error) {
	var m map[string]float64
	if err := a.getOptionJSON(ctx, "GroupRatio", &m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]float64{}
	}
	return m, nil
}

// ListGroupModels 返回「分组 → 可用模型」,走公开 GET /api/pricing 的 enable_groups 反转(D6 实证)。
// enable_groups 反映实际渠道 abilities(可路由集),用于配置期 model_set ⊆ 分组可用模型 预检(T17-5/D4)。
func (a *Adapter) ListGroupModels(ctx context.Context) (map[string][]string, error) {
	res, err := a.c.do(ctx, stepGetOption, "GET", "/api/pricing", adminAuth(a.c.cfg), nil)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		ModelName    string   `json:"model_name"`
		EnableGroups []string `json:"enable_groups"`
	}
	if jerr := json.Unmarshal(res.data, &rows); jerr != nil {
		return nil, &UpstreamError{Step: stepGetOption, PlatformCode: CodeInternal, Message: "解析计价表失败", class: classNonRetryable, cause: jerr}
	}
	out := map[string][]string{}
	for _, r := range rows {
		for _, g := range r.EnableGroups {
			out[g] = append(out[g], r.ModelName)
		}
	}
	return out, nil
}

// optGroupSpecialUsable 是 new-api 真正读取的 option key(分层配置 group_ratio_setting 的子字段)。
// 注意:绝不是 top-level "GroupSpecialUsableGroup"(那是 new-api 不读的死键,T17-2 验收 P0 教训);
// new-api 分层配置走 "配置名.子键"(model/option.go: handleConfigUpdate),读回也以此 key 出现。
const optGroupSpecialUsable = "group_ratio_setting.group_special_usable_group"

// GetOrgUsableGroups 读某 org 用户分组的"可用模型分组"列表(改动③a;建组织校验①用)。
// 读 group_special_usable_group(嵌套 map {用户分组:{模型分组:描述}}),取 m[userGroup] 的 key 集。
// 用户分组不存在/为空 → 返回空切片(配合①建组织校验:为空=运营还没在 new-api 把模型分组挂到该用户分组)。
func (a *Adapter) GetOrgUsableGroups(ctx context.Context, userGroup string) ([]string, error) {
	var m map[string]map[string]string
	if err := a.getOptionJSON(ctx, optGroupSpecialUsable, &m); err != nil {
		return nil, err
	}
	sub := m[userGroup]
	out := make([]string, 0, len(sub))
	for g := range sub {
		out = append(out, g)
	}
	return out, nil
}

// AddOrgUsableGroup 把业务令牌分组加进某 org 用户分组的"可用分组"(§3 硬约束,不补则真调用 403)。
// 写 new-api 真读的 group_ratio_setting.group_special_usable_group,值为嵌套 map
// {用户分组: {业务分组: 描述}};单写者锁 + merge-preserve(保留 vip 等其它用户分组条目)+ 去重幂等。
// group 为空 / default / 等于 userGroup 自身时免操作(这些天然可用)。
func (a *Adapter) AddOrgUsableGroup(ctx context.Context, userGroup, group string) error {
	if group == "" || group == "default" || group == userGroup {
		return nil
	}
	release, lerr := a.locker.Acquire(ctx, "option:"+optGroupSpecialUsable)
	if lerr != nil {
		return &UpstreamError{Step: stepSetOption, PlatformCode: CodeInternal, Message: "获取可用分组写锁失败", class: classRetryable, cause: lerr}
	}
	defer release()

	var m map[string]map[string]string
	if err := a.getOptionJSON(ctx, optGroupSpecialUsable, &m); err != nil {
		return err
	}
	if m == nil {
		m = map[string]map[string]string{}
	}
	if m[userGroup] == nil {
		m[userGroup] = map[string]string{}
	}
	if _, ok := m[userGroup][group]; ok {
		return nil // 幂等:已含
	}
	m[userGroup][group] = "platform" // 描述(可读标识,仅展示用)
	val, merr := json.Marshal(m)
	if merr != nil {
		return &UpstreamError{Step: stepSetOption, PlatformCode: CodeInternal, Message: "序列化可用分组失败", class: classNonRetryable, cause: merr}
	}
	if _, uerr := a.c.do(ctx, stepSetOption, "PUT", "/api/option/", adminAuth(a.c.cfg),
		map[string]any{"key": optGroupSpecialUsable, "value": string(val)}); uerr != nil {
		return uerr
	}
	return nil
}
