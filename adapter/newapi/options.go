package newapi

import (
	"context"
	"encoding/json"
	"fmt"
)

// GroupRatio 是 new-api 的分组倍率选项 {group: ratio}(计价/折扣,03 §3.5.1)。
// 平台单向写入(配置入口)、只读回显;new-api 始终是计费唯一真相源。

// GetGroupRatio 读某分组当前的分组倍率(只读回显)。bool=该分组是否已配。
func (a *Adapter) GetGroupRatio(ctx context.Context, group string) (float64, bool, error) {
	m, err := a.getGroupRatioMap(ctx)
	if err != nil {
		return 0, false, err
	}
	r, ok := m[group]
	return r, ok, nil
}

// SetGroupRatio 设某分组的分组倍率(读-改-写 GroupRatio 选项,单向写入 new-api)。
// 如八折=0.8,对该分组下全部模型一致生效(总折扣)。
func (a *Adapter) SetGroupRatio(ctx context.Context, group string, ratio float64) error {
	m, err := a.getGroupRatioMap(ctx)
	if err != nil {
		return err
	}
	m[group] = ratio
	val, merr := json.Marshal(m)
	if merr != nil {
		return &UpstreamError{Step: stepSetOption, PlatformCode: CodeInternal, Message: "序列化分组倍率失败", class: classNonRetryable, cause: merr}
	}
	// new-api option 端点须带尾斜杠;value 为 JSON 字符串(实证)。
	// 注意:do 返回 *UpstreamError;直接 `return uerr`(error 型)会把 nil 指针装箱成非 nil 接口(typed-nil 坑),
	// 故显式判 nil 后 return nil。
	if _, uerr := a.c.do(ctx, stepSetOption, "PUT", "/api/option/", adminAuth(a.c.cfg),
		map[string]any{"key": "GroupRatio", "value": string(val)}); uerr != nil {
		return uerr
	}
	return nil
}

// getGroupRatioMap 读当前 GroupRatio 选项并解析成 map。
func (a *Adapter) getGroupRatioMap(ctx context.Context) (map[string]float64, error) {
	res, err := a.c.do(ctx, stepGetOption, "GET", "/api/option/", adminAuth(a.c.cfg), nil)
	if err != nil {
		return nil, err
	}
	var opts []struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal(res.data, &opts); err != nil {
		return nil, &UpstreamError{Step: stepGetOption, PlatformCode: CodeInternal, Message: "解析选项失败", class: classNonRetryable, cause: err}
	}
	m := map[string]float64{}
	for _, o := range opts {
		if o.Key == "GroupRatio" && o.Value != "" {
			if err := json.Unmarshal([]byte(o.Value), &m); err != nil {
				return nil, &UpstreamError{Step: stepGetOption, PlatformCode: CodeInternal, Message: "解析分组倍率失败", class: classNonRetryable, cause: fmt.Errorf("%w", err)}
			}
			break
		}
	}
	return m, nil
}
