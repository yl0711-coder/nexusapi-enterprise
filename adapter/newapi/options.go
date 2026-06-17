package newapi

import (
	"context"
	"encoding/json"
)

// new-api 计价三旋钮(03 §3.5.1,已源码核实):
//   - ModelRatio / CompletionRatio:按模型全局,主站维护。平台只读、从不写。
//   - GroupRatio {分组: 倍率}:分组基础倍率,主站维护。平台只读(取基础倍率快照用)。
//   - GroupGroupRatio {用户分组: {令牌分组: 倍率}}:分组特殊倍率,覆盖(非相乘)分组基础倍率。
//     **这是平台唯一写的一层(客户折扣)。** 写入限定只动这一个 option key(F1:误操作面最小)。
//
// 平台落折扣:绝对特殊倍率 = 目标令牌分组基础倍率 × 折扣%;写 GroupGroupRatio[org_{id}][令牌分组]。

// GetGroupRatio 读某分组的基础倍率(只读;取折扣快照基数用)。bool=该分组是否已配。
func (a *Adapter) GetGroupRatio(ctx context.Context, group string) (float64, bool, error) {
	var m map[string]float64
	if err := a.getOptionJSON(ctx, "GroupRatio", &m); err != nil {
		return 0, false, err
	}
	r, ok := m[group]
	return r, ok, nil
}

// GetGroupGroupRatio 读某「用户分组×令牌分组」的特殊倍率(只读回显)。
func (a *Adapter) GetGroupGroupRatio(ctx context.Context, userGroup, tokenGroup string) (float64, bool, error) {
	m, err := a.getGroupGroupRatioMap(ctx)
	if err != nil {
		return 0, false, err
	}
	if inner, ok := m[userGroup]; ok {
		r, ok2 := inner[tokenGroup]
		return r, ok2, nil
	}
	return 0, false, nil
}

// SetGroupGroupRatio 设「用户分组×令牌分组」特殊倍率(merge-preserve:读现状 → 只覆盖这一个 key →
// 写回,绝不抹掉别人/手工配的其它条目,G 类字段所有权)。是平台唯一写的 option。
func (a *Adapter) SetGroupGroupRatio(ctx context.Context, userGroup, tokenGroup string, ratio float64) error {
	m, err := a.getGroupGroupRatioMap(ctx)
	if err != nil {
		return err
	}
	if m == nil {
		m = map[string]map[string]float64{}
	}
	if m[userGroup] == nil {
		m[userGroup] = map[string]float64{}
	}
	m[userGroup][tokenGroup] = ratio
	val, merr := json.Marshal(m)
	if merr != nil {
		return &UpstreamError{Step: stepSetOption, PlatformCode: CodeInternal, Message: "序列化分组特殊倍率失败", class: classNonRetryable, cause: merr}
	}
	// 平台只允许写 GroupGroupRatio 这一个 option key(F1 误操作面最小)。
	if _, uerr := a.c.do(ctx, stepSetOption, "PUT", "/api/option/", adminAuth(a.c.cfg),
		map[string]any{"key": "GroupGroupRatio", "value": string(val)}); uerr != nil {
		return uerr
	}
	return nil
}

func (a *Adapter) getGroupGroupRatioMap(ctx context.Context) (map[string]map[string]float64, error) {
	var m map[string]map[string]float64
	if err := a.getOptionJSON(ctx, "GroupGroupRatio", &m); err != nil {
		return nil, err
	}
	return m, nil
}

// getOptionJSON 读某 option 的值(JSON 字符串)并反序列化到 v。option 不存在则 v 保持零值。
func (a *Adapter) getOptionJSON(ctx context.Context, key string, v any) error {
	res, err := a.c.do(ctx, stepGetOption, "GET", "/api/option/", adminAuth(a.c.cfg), nil)
	if err != nil {
		return err
	}
	var opts []struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal(res.data, &opts); err != nil {
		return &UpstreamError{Step: stepGetOption, PlatformCode: CodeInternal, Message: "解析选项失败", class: classNonRetryable, cause: err}
	}
	for _, o := range opts {
		if o.Key == key && o.Value != "" {
			if err := json.Unmarshal([]byte(o.Value), v); err != nil {
				return &UpstreamError{Step: stepGetOption, PlatformCode: CodeInternal, Message: "解析选项值失败", class: classNonRetryable, cause: err}
			}
			return nil
		}
	}
	return nil
}
