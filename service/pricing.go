package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/nexusapi-platform/enterprise/pkg/apperr"
	"github.com/nexusapi-platform/enterprise/pkg/session"
	"github.com/nexusapi-platform/enterprise/repo"
)

// 折扣模式(A1 拍板:整体 / 按分组,不做逐模型)。
const (
	DiscountNone     = "none"
	DiscountTotal    = "total"     // 整体:对客户用到的每个令牌分组都打折
	DiscountPerGroup = "per_group" // 按分组:只对指定令牌分组打折
)

// orgUserGroup 取组织专属 new-api 用户分组名(改动①:读存储字段 organization.newapi_group)。
// nil/读不到回落派生值 org_%d(迁移 0014 已把存量回填为该派生值,新组织由 CreateOrg 写运营手填值)。
// 隔离边界真源,折扣等只读不写(repo.UpdateOrgDiscount 已解耦,红线④)。
func (s *Service) orgUserGroup(ctx context.Context, orgID int64) string {
	if o, err := s.store.GetOrganization(ctx, orgID); err == nil && o.NewapiUserGroup != nil && *o.NewapiUserGroup != "" {
		return *o.NewapiUserGroup
	}
	return fmt.Sprintf("org_%d", orgID)
}

// discountEntry 是平台存的折扣镜像(每令牌分组一条):折扣% + 写入时基础倍率快照 + 算出的绝对特殊倍率。
type discountEntry struct {
	Pct  float64 `json:"pct"`  // 折扣率(0.9 = 9 折)
	Base float64 `json:"base"` // 写入时目标分组基础倍率快照
	Abs  float64 `json:"abs"`  // 绝对特殊倍率 = base × pct(实际写进 new-api 的值)
}

// ConfigureDiscountInput 配置折扣入参(仅运营方;A1/A2)。
type ConfigureDiscountInput struct {
	Mode        string   // none/total/per_group
	DiscountPct float64  // 折扣率,如 0.9
	TokenGroups []string // 目标令牌分组;total 留空默认 ["default"];per_group 指定
}

// PricingView 折扣回显。
type PricingView struct {
	Mode        string                   `json:"mode"`
	UserGroup   string                   `json:"user_group"`
	Entries     map[string]discountEntry `json:"entries"`               // 平台镜像(每令牌分组)
	Upstream    map[string]float64       `json:"upstream_special_ratio"` // new-api 当前实际特殊倍率(只读回显)
}

// ConfigureDiscount 配置组织折扣(仅运营方;A1):写 new-api 分组特殊倍率
// GroupGroupRatio[org_{id}][令牌分组] = 该分组基础倍率 × 折扣%(绝对值、覆盖式);
// 平台存 折扣% + 基础倍率快照(基础变了由对账重算,G)。客户对价格只读。
func (s *Service) ConfigureDiscount(ctx context.Context, c session.Claims, orgID int64, in ConfigureDiscountInput) (*PricingView, error) {
	if err := assertRole(c, session.RoleOperator); err != nil {
		return nil, err
	}
	if _, err := s.store.GetOrganization(ctx, orgID); errors.Is(err, repo.ErrNotFound) {
		return nil, apperr.NotFound("组织不存在")
	} else if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	userGroup := s.orgUserGroup(ctx, orgID)
	groups := in.TokenGroups
	if len(groups) == 0 {
		// total(整体折扣)默认覆盖「该 org 在用的全部令牌分组集」(T17-3/Q1),而非写死 default;
		// per_group 缺省仍回落 default。
		if in.Mode == DiscountTotal {
			groups = s.orgInUseTokenGroups(ctx, orgID)
		}
		if len(groups) == 0 {
			groups = []string{"default"}
		}
	}

	// 平台镜像是该 org 全部己方令牌分组折扣的权威集合(T1/T6)。基于既有镜像演进:
	//   none      → 清空;total → 整体重置为本次分组集;per_group → 把本次分组并入既有镜像(累积)。
	entries := map[string]discountEntry{}
	switch in.Mode {
	case DiscountNone:
		// 清空镜像;下面统一把空集权威下发(删该 org 用户分组)。
	case DiscountTotal, DiscountPerGroup:
		if in.DiscountPct <= 0 || in.DiscountPct > 1 {
			return nil, apperr.InvalidParam("折扣率须在 (0,1](如 0.9 = 9 折)")
		}
		if in.Mode == DiscountPerGroup {
			entries = s.loadDiscountEntries(ctx, orgID) // 累积:保留既有已配分组(T6 修发散)
		}
		for _, g := range groups {
			base, ok, err := s.upstream.GetGroupRatio(ctx, g)
			if err != nil {
				return nil, mapUpstream(err)
			}
			if !ok || base <= 0 {
				base = 1 // 该分组未配基础倍率,按 1 处理
			}
			entries[g] = discountEntry{Pct: in.DiscountPct, Base: base, Abs: base * in.DiscountPct}
		}
	default:
		return nil, apperr.InvalidParam("折扣模式须为 none/total/per_group")
	}

	// 以镜像为权威源,一次性把该 org 用户分组下全部特殊倍率覆盖下发(己方键不取上游旧值,
	// 写后读校验+退避重试;空集→删该用户分组)。镜像与上游因此始终一致。
	desired := make(map[string]float64, len(entries))
	for g, e := range entries {
		desired[g] = e.Abs
	}
	if err := s.upstream.SetOrgGroupRatios(ctx, userGroup, desired); err != nil {
		return nil, mapUpstream(err)
	}

	entriesJSON, _ := json.Marshal(entries)
	// 改动①解耦:折扣不再写组织用户分组列。
	if err := s.store.UpdateOrgDiscount(ctx, orgID, in.Mode, nil, entriesJSON); err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	s.audit(ctx, c, orgID, "configure_discount", "organization", &orgID, map[string]any{"mode": in.Mode, "pct": in.DiscountPct, "groups": groups})
	return s.GetPricing(ctx, c, orgID)
}

// BillingGroup 计费分组信息(层级配置页选择器用,T17-6/D4)。
type BillingGroup struct {
	Group  string   `json:"group"`
	Ratio  float64  `json:"ratio"`  // 基础倍率
	Models []string `json:"models"` // 该分组可用模型(/api/pricing 反转)
}

// ListBillingGroups 列系统计费分组 + 基础倍率 + 各分组可用模型(供层级配分组选择器 + 预检,O/A)。
func (s *Service) ListBillingGroups(ctx context.Context, c session.Claims) ([]BillingGroup, error) {
	if err := assertRole(c, session.RoleOperator, session.RoleOrgAdmin); err != nil {
		return nil, err
	}
	ratios, err := s.upstream.ListGroupRatios(ctx)
	if err != nil {
		return nil, mapUpstream(err)
	}
	g2m, err := s.upstream.ListGroupModels(ctx)
	if err != nil {
		return nil, mapUpstream(err)
	}
	out := make([]BillingGroup, 0, len(ratios))
	for g, r := range ratios {
		out = append(out, BillingGroup{Group: g, Ratio: r, Models: g2m[g]})
	}
	return out, nil
}

// GetPricing 读折扣镜像 + new-api 当前实际特殊倍率回显(O/A;客户只读)。
func (s *Service) GetPricing(ctx context.Context, c session.Claims, orgID int64) (*PricingView, error) {
	if err := assertOrgScope(c, orgID); err != nil {
		return nil, err
	}
	if err := assertRole(c, session.RoleOperator, session.RoleOrgAdmin); err != nil {
		return nil, err
	}
	d, err := s.store.GetOrgDiscount(ctx, orgID)
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	userGroup := s.orgUserGroup(ctx, orgID)
	v := &PricingView{Mode: d.Mode, UserGroup: userGroup, Entries: map[string]discountEntry{}, Upstream: map[string]float64{}}
	if len(d.SpecialRatios) > 0 {
		_ = json.Unmarshal(d.SpecialRatios, &v.Entries)
	}
	for g := range v.Entries {
		if r, ok, gerr := s.upstream.GetGroupGroupRatio(ctx, userGroup, g); gerr == nil && ok {
			v.Upstream[g] = r
		}
	}
	return v, nil
}

// orgInUseTokenGroups 枚举该 org 在用的令牌计价分组集(去重):各 tier 的 newapi_group 经
// resolveTokenGroup(tier??org默认??default)解析。total 折扣按此逐格配(T17-3/Q1)。
func (s *Service) orgInUseTokenGroups(ctx context.Context, orgID int64) []string {
	org, err := s.store.GetOrganization(ctx, orgID)
	if err != nil {
		return nil
	}
	tiers, err := s.store.ListTiers(ctx, orgID)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(g string) {
		if g != "" && !seen[g] {
			seen[g] = true
			out = append(out, g)
		}
	}
	add(resolveTokenGroup(nil, org)) // 组织默认(无 tier 时成员落到的分组)
	for _, t := range tiers {
		add(resolveTokenGroup(t, org))
	}
	return out
}

// ensureTotalDiscountCoversGroup 该 org 后续新用到一个分组时,把已有的 total 折扣自动补到该分组
// (T17-3/Q1:否则新分组回原价、客户被多收)。仅 total 模式、仅"补"(价格只降不升,安全方向)。
// best-effort:开通成员落到新分组时调。
func (s *Service) ensureTotalDiscountCoversGroup(ctx context.Context, orgID int64, group string) {
	if group == "" || group == "default" {
		return
	}
	d, err := s.store.GetOrgDiscount(ctx, orgID)
	if err != nil || d.Mode != DiscountTotal {
		return // 非 total 不自动补(per_group 是显式逐格,none 无折扣)
	}
	entries := s.loadDiscountEntries(ctx, orgID)
	if _, ok := entries[group]; ok {
		return // 已覆盖
	}
	if len(entries) == 0 {
		return // total 但无镜像条目(异常),不猜折扣率,交对账告警
	}
	var pct float64
	for _, e := range entries { // total 各分组同折扣率,取其一
		pct = e.Pct
		break
	}
	base, ok, gerr := s.upstream.GetGroupRatio(ctx, group)
	if gerr != nil {
		s.log.Warn("自动补 total 折扣读基础倍率失败(交对账)", "org_id", orgID, "group", group, "err", gerr)
		return
	}
	if !ok || base <= 0 {
		base = 1
	}
	entries[group] = discountEntry{Pct: pct, Base: base, Abs: base * pct}
	desired := make(map[string]float64, len(entries))
	for g, e := range entries {
		desired[g] = e.Abs
	}
	if err := s.upstream.SetOrgGroupRatios(ctx, s.orgUserGroup(ctx, orgID), desired); err != nil {
		s.log.Warn("自动补 total 折扣下发失败(交对账)", "org_id", orgID, "group", group, "err", err)
		return
	}
	entriesJSON, _ := json.Marshal(entries)
	// 改动①解耦:折扣不再写组织用户分组列。
	if err := s.store.UpdateOrgDiscount(ctx, orgID, DiscountTotal, nil, entriesJSON); err != nil {
		s.log.Warn("自动补 total 折扣持久化失败(交对账)", "org_id", orgID, "group", group, "err", err)
		return
	}
	s.auditSystem(ctx, orgID, "discount_total_autoextend", "organization", &orgID, map[string]any{"group": group, "pct": pct}, "ok")
}

// loadDiscountEntries 读平台存的折扣镜像(map[令牌分组]entry)。
func (s *Service) loadDiscountEntries(ctx context.Context, orgID int64) map[string]discountEntry {
	out := map[string]discountEntry{}
	d, err := s.store.GetOrgDiscount(ctx, orgID)
	if err == nil && len(d.SpecialRatios) > 0 {
		_ = json.Unmarshal(d.SpecialRatios, &out)
	}
	return out
}
