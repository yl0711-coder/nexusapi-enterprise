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

// 折扣模式(09 §14)。
const (
	DiscountNone     = "none"
	DiscountTotal    = "total"
	DiscountPerModel = "per_model"
	DiscountFixed    = "fixed"
)

// ConfigureDiscountInput 配置折扣入参(03 §3.5.1;配置权在运营方)。
type ConfigureDiscountInput struct {
	Mode          string             // none/total/per_model/fixed
	NewapiGroup   string             // 应用的分组(留空用组织已配/默认 org{id})
	GroupRatio    *float64           // total 模式:分组倍率(如 0.8=八折)
	SpecialRatios map[string]float64 // per_model/fixed 模式:{model: 倍率}(覆盖,非相乘)
}

// PricingView 折扣回显(只读,读 new-api 当前值反显)。
type PricingView struct {
	Mode               string
	NewapiGroup        string
	GroupRatioConfig   *float64           // 平台镜像值
	GroupRatioUpstream *float64           // new-api 当前实际值(只读回显)
	SpecialRatios      map[string]float64 // 平台镜像
}

// ConfigureDiscount 配置组织折扣(仅运营方;客户只读)。总折扣单向写入 new-api GroupRatio;
// 单模型/直接定价本期持久化镜像 + 标注(分组特殊倍率 GroupGroupRatio 逐调用分组语义待接,03 §3.5.1 坑)。
func (s *Service) ConfigureDiscount(ctx context.Context, c session.Claims, orgID int64, in ConfigureDiscountInput) (*PricingView, error) {
	if err := assertRole(c, session.RoleOperator); err != nil {
		return nil, err
	}
	org, err := s.store.GetOrganization(ctx, orgID)
	if errors.Is(err, repo.ErrNotFound) {
		return nil, apperr.NotFound("组织不存在")
	} else if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	switch in.Mode {
	case DiscountNone, DiscountTotal, DiscountPerModel, DiscountFixed:
	default:
		return nil, apperr.InvalidParam("折扣模式须为 none/total/per_model/fixed")
	}

	// 解析应用分组:入参 > 组织已配 > 默认 org{id}。
	group := in.NewapiGroup
	if group == "" && org.NewapiGroup != nil {
		group = *org.NewapiGroup
	}
	if group == "" {
		group = fmt.Sprintf("org%d", orgID)
	}

	var specialJSON []byte
	if len(in.SpecialRatios) > 0 {
		specialJSON, _ = json.Marshal(in.SpecialRatios)
	}

	// total:单向写入 new-api GroupRatio。
	if in.Mode == DiscountTotal {
		if in.GroupRatio == nil || *in.GroupRatio <= 0 {
			return nil, apperr.InvalidParam("总折扣须给正的分组倍率")
		}
		if err := s.upstream.SetGroupRatio(ctx, group, *in.GroupRatio); err != nil {
			return nil, mapUpstream(err)
		}
	} else if in.Mode == DiscountPerModel || in.Mode == DiscountFixed {
		// 镜像持久化;new-api 分组特殊倍率(GroupGroupRatio)写入待后续接(逐调用分组,03 §3.5.1)。
		s.log.Warn("per_model/fixed 折扣本期仅持久化镜像,new-api 特殊倍率写入待接 GroupGroupRatio", "org_id", orgID)
	}

	if err := s.store.UpdateOrgDiscount(ctx, orgID, in.Mode, &group, in.GroupRatio, specialJSON); err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	s.audit(ctx, c, orgID, "configure_discount", "organization", &orgID, map[string]any{
		"mode": in.Mode, "group": group, "group_ratio": in.GroupRatio,
	})
	return s.GetPricing(ctx, c, orgID)
}

// GetPricing 读折扣配置 + new-api 当前分组倍率回显(O/A;客户只读)。
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
	v := &PricingView{Mode: d.Mode, GroupRatioConfig: d.GroupRatio}
	if d.NewapiGroup != nil {
		v.NewapiGroup = *d.NewapiGroup
	}
	if len(d.SpecialRatios) > 0 {
		_ = json.Unmarshal(d.SpecialRatios, &v.SpecialRatios)
	}
	// 只读回显:读 new-api 当前实际分组倍率(计费真相源)。
	if v.NewapiGroup != "" {
		if r, ok, gerr := s.upstream.GetGroupRatio(ctx, v.NewapiGroup); gerr == nil && ok {
			rr := r
			v.GroupRatioUpstream = &rr
		}
	}
	return v, nil
}
