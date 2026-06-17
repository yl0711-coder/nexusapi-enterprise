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

// orgUserGroup 客户专属用户分组名(统一前缀,便于人工辨识"归企业后台管",G.4)。
func orgUserGroup(orgID int64) string { return fmt.Sprintf("org_%d", orgID) }

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
	userGroup := orgUserGroup(orgID)
	groups := in.TokenGroups
	if len(groups) == 0 {
		groups = []string{"default"}
	}
	entries := map[string]discountEntry{}

	switch in.Mode {
	case DiscountNone:
		// 取消折扣:删掉已配令牌分组的特殊倍率条目,回落到主站基础倍率(= 无折扣)。
		// R2-轻微:删键而非写回 base=1×base,避免 GroupGroupRatio 堆死键(两者价格等效)。
		prev := s.loadDiscountEntries(ctx, orgID)
		for g := range prev {
			if err := s.upstream.DeleteGroupGroupRatio(ctx, userGroup, g); err != nil {
				return nil, mapUpstream(err)
			}
		}
	case DiscountTotal, DiscountPerGroup:
		if in.DiscountPct <= 0 || in.DiscountPct > 1 {
			return nil, apperr.InvalidParam("折扣率须在 (0,1](如 0.9 = 9 折)")
		}
		for _, g := range groups {
			base, ok, err := s.upstream.GetGroupRatio(ctx, g)
			if err != nil {
				return nil, mapUpstream(err)
			}
			if !ok || base <= 0 {
				base = 1 // 该分组未配基础倍率,按 1 处理
			}
			abs := base * in.DiscountPct
			if err := s.upstream.SetGroupGroupRatio(ctx, userGroup, g, abs); err != nil {
				return nil, mapUpstream(err)
			}
			entries[g] = discountEntry{Pct: in.DiscountPct, Base: base, Abs: abs}
		}
	default:
		return nil, apperr.InvalidParam("折扣模式须为 none/total/per_group")
	}

	entriesJSON, _ := json.Marshal(entries)
	ug := userGroup
	if err := s.store.UpdateOrgDiscount(ctx, orgID, in.Mode, &ug, nil, entriesJSON); err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	s.audit(ctx, c, orgID, "configure_discount", "organization", &orgID, map[string]any{"mode": in.Mode, "pct": in.DiscountPct, "groups": groups})
	return s.GetPricing(ctx, c, orgID)
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
	userGroup := orgUserGroup(orgID)
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

// loadDiscountEntries 读平台存的折扣镜像(map[令牌分组]entry)。
func (s *Service) loadDiscountEntries(ctx context.Context, orgID int64) map[string]discountEntry {
	out := map[string]discountEntry{}
	d, err := s.store.GetOrgDiscount(ctx, orgID)
	if err == nil && len(d.SpecialRatios) > 0 {
		_ = json.Unmarshal(d.SpecialRatios, &out)
	}
	return out
}
