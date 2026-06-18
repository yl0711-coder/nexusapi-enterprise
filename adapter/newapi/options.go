package newapi

import (
	"context"
	"encoding/json"
	"math"
	"time"
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
	return a.mutateGroupGroupRatio(ctx, func(m map[string]map[string]float64) {
		if m[userGroup] == nil {
			m[userGroup] = map[string]float64{}
		}
		m[userGroup][tokenGroup] = ratio
	})
}

// DeleteGroupGroupRatio 删掉某条特殊倍率(取消折扣回落基础倍率,不堆死键)。
// 同一单写者锁 + merge-preserve:只删自己这条,空了的用户分组顺手清掉,绝不动别人条目。
func (a *Adapter) DeleteGroupGroupRatio(ctx context.Context, userGroup, tokenGroup string) error {
	return a.mutateGroupGroupRatio(ctx, func(m map[string]map[string]float64) {
		if inner, ok := m[userGroup]; ok {
			delete(inner, tokenGroup)
			if len(inner) == 0 {
				delete(m, userGroup)
			}
		}
	})
}

// mutateGroupGroupRatio 在单写者锁下做 GroupGroupRatio 的读-改-写(merge-preserve)。
// 单写者锁(R2-S2/G):option 是全局 JSON、读-改-写非原子,并发会丢更新(动钱)。
// 用 KeyedLocker 串行化所有 GroupGroupRatio 写(MVP 单实例;多实例换分布式锁同栈)。
func (a *Adapter) mutateGroupGroupRatio(ctx context.Context, apply func(m map[string]map[string]float64)) error {
	release, lerr := a.locker.Acquire(ctx, "option:GroupGroupRatio")
	if lerr != nil {
		return &UpstreamError{Step: stepSetOption, PlatformCode: CodeInternal, Message: "获取折扣写锁失败", class: classRetryable, cause: lerr}
	}
	defer release()

	m, err := a.getGroupGroupRatioMap(ctx)
	if err != nil {
		return err
	}
	if m == nil {
		m = map[string]map[string]float64{}
	}
	apply(m)
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

// ratioEps 倍率比较容差(浮点 + JSON 往返)。
const ratioEps = 1e-9

// ggrVerifyBackoffs 写后读校验的退避序列(应对 new-api option 读缓存滞后,T1 第②层兜底)。
var ggrVerifyBackoffs = []time.Duration{120 * time.Millisecond, 360 * time.Millisecond, 800 * time.Millisecond}

// SetOrgGroupRatios 以平台为权威源设某用户分组(org_{id})下全部令牌分组特殊倍率,详见接口注释。
// 单写者锁内"读其它分组 → 整体覆盖己方分组 → 写回",再做写后读校验+退避重试(T1 两层):
//   - 第①层(根治):己方用户分组的内层 map 整体由 desired 覆盖(desired 是平台镜像聚合出的全集),
//     绝不把上游读回的己方旧值 merge 回去 —— 消除 read-after-write 把刚写的己方键回退掉的问题。
//     其它用户分组(vip 等手工键)原样保留(merge-preserve)。
//   - 第②层(兜底):PUT 后读回校验己方键是否=desired;不一致则退避后幂等重写(重写仍是"整体覆盖
//     己方分组",对己方键幂等、不会回滚)。退避用尽仍不一致 → 不报错,交 reconcile 检出告警。
func (a *Adapter) SetOrgGroupRatios(ctx context.Context, userGroup string, desired map[string]float64) error {
	release, lerr := a.locker.Acquire(ctx, "option:GroupGroupRatio")
	if lerr != nil {
		return &UpstreamError{Step: stepSetOption, PlatformCode: CodeInternal, Message: "获取折扣写锁失败", class: classRetryable, cause: lerr}
	}
	defer release()

	// 一次"读其它分组 → 整体覆盖己方分组 → 写回"。己方键恒由 desired 覆盖,对己方键幂等。
	writeOnce := func() error {
		m, err := a.getGroupGroupRatioMap(ctx)
		if err != nil {
			return err
		}
		if m == nil {
			m = map[string]map[string]float64{}
		}
		if len(desired) == 0 {
			delete(m, userGroup) // 取消折扣:删整个 org 用户分组(回落基础倍率,不堆死键)
		} else {
			inner := make(map[string]float64, len(desired))
			for g, r := range desired {
				inner[g] = r
			}
			m[userGroup] = inner // 权威覆盖:不采信上游读回的己方键值
		}
		val, merr := json.Marshal(m)
		if merr != nil {
			return &UpstreamError{Step: stepSetOption, PlatformCode: CodeInternal, Message: "序列化分组特殊倍率失败", class: classNonRetryable, cause: merr}
		}
		if _, uerr := a.c.do(ctx, stepSetOption, "PUT", "/api/option/", adminAuth(a.c.cfg),
			map[string]any{"key": "GroupGroupRatio", "value": string(val)}); uerr != nil {
			return uerr // 必须显式判空,否则 nil *UpstreamError 装箱成非 nil error(typed-nil 坑)
		}
		return nil
	}

	if err := writeOnce(); err != nil {
		return err
	}
	// 第②层兜底:写后读校验 + 退避幂等重写(应对上游 option 读缓存滞后)。
	for _, b := range ggrVerifyBackoffs {
		if a.orgRatiosMatch(ctx, userGroup, desired) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(b):
		}
		if err := writeOnce(); err != nil {
			return err
		}
	}
	// 退避用尽仍不一致:写已下发且对己方键幂等,不报错,交 reconcile 检出告警。
	return nil
}

// orgRatiosMatch 读上游校验某用户分组的内层 map 是否与 desired 完全一致(空 desired ⇔ 该分组不存在/空)。
func (a *Adapter) orgRatiosMatch(ctx context.Context, userGroup string, desired map[string]float64) bool {
	m, err := a.getGroupGroupRatioMap(ctx)
	if err != nil {
		return false
	}
	inner := m[userGroup]
	if len(inner) != len(desired) {
		return false
	}
	for g, want := range desired {
		got, ok := inner[g]
		if !ok || math.Abs(got-want) > ratioEps {
			return false
		}
	}
	return true
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
