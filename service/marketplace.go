// 模型广场(29-PRD §4.6 / 33 §3.5,39号体检阻断-1 补落地):
//   GET /orgs/:id/marketplace — 组织可用范围(分组+模型+倍率),org_admin/operator;
//   GET /me/marketplace       — 成员被授权范围(分组 ∈ 授权档位分组集),建令牌分组下拉同一真值来源。
// 数据源与 /pricing/groups 同一口径(new-api GroupRatio + 分组模型反转);倍率按 new-api
// 普通用户可见口径展示(藏价已废除,33 §12-7/ADR §9)。只读,不涉钱写。
package service

import (
	"context"
	"strconv"

	"github.com/nexusapi-platform/enterprise/pkg/apperr"
	"github.com/nexusapi-platform/enterprise/pkg/session"
)

// MarketplaceView 模型广场响应({groups:[{group,ratio,models}]},FE marketGroups 容错兼容此形状)。
type MarketplaceView struct {
	Groups []BillingGroup `json:"groups"`
}

// OrgMarketplace 组织模型广场(O/A;与建档位的分组选择器同源=系统分组全集+倍率+模型,
// 口径同 new-api 普通用户建令牌时可见的分组列表)。
func (s *Service) OrgMarketplace(ctx context.Context, c session.Claims, orgID int64) (*MarketplaceView, error) {
	if err := assertOrgScope(c, orgID); err != nil {
		return nil, err
	}
	groups, err := s.ListBillingGroups(ctx, c) // 内部已限 operator/org_admin
	if err != nil {
		return nil, err
	}
	return &MarketplaceView{Groups: groups}, nil
}

// MyMarketplace 成员模型广场:只列**被授权档位分组集**内的分组(与建令牌校验 assertGroupAuthorized
// 同一真值来源 memberAuthorizedGroups),附各分组倍率与模型。
func (s *Service) MyMarketplace(ctx context.Context, c session.Claims) (*MarketplaceView, error) {
	m, err := s.requireSelfServiceMember(ctx, c)
	if err != nil {
		return nil, err
	}
	authorized, err := s.memberAuthorizedGroups(ctx, m)
	if err != nil {
		return nil, err
	}
	ratios, rerr := s.upstream.ListGroupRatios(ctx)
	if rerr != nil {
		return nil, mapUpstream(rerr)
	}
	g2m, merr := s.upstream.ListGroupModels(ctx)
	if merr != nil {
		return nil, mapUpstream(merr)
	}
	out := make([]BillingGroup, 0, len(authorized))
	for _, g := range authorized {
		bg := BillingGroup{Group: g, Models: g2m[g]}
		if r, ok := ratios[g]; ok {
			bg.Ratio = r
		}
		out = append(out, bg)
	}
	return &MarketplaceView{Groups: out}, nil
}

// ───────────────────────────── 平台设置(33 §3.5 operator)─────────────────────────────

// PlatformSettingsView GET/PUT /platform-settings(quota_per_unit 只读;其余可改)。
type PlatformSettingsView struct {
	QuotaPerUnit            int64 `json:"quota_per_unit"`             // 只读(启动自检对齐 new-api,33 §3.6)
	MoneyFreeze             bool  `json:"money_freeze"`               // 钱动作急停(读失败 fail-closed,39号阻断-3)
	MemberTokenLimit        int64 `json:"member_token_limit"`         // 成员令牌数上限(默认 2)
	MemberQuotaCapRaw       int64 `json:"member_quota_cap_raw"`       // 成员额度帽(默认 5e8=$1000,int32 护栏)
	TreasuryLowWatermarkRaw int64 `json:"treasury_low_watermark_raw"` // 金库低预警阈值(<=0=未启用)
}

// UpdatePlatformSettingsInput PUT 入参(指针=只改给了的字段,部分更新)。
type UpdatePlatformSettingsInput struct {
	MoneyFreeze             *bool  `json:"money_freeze"`
	MemberTokenLimit        *int64 `json:"member_token_limit"`
	MemberQuotaCapRaw       *int64 `json:"member_quota_cap_raw"`
	TreasuryLowWatermarkRaw *int64 `json:"treasury_low_watermark_raw"`
}

// GetPlatformSettings 平台配置读(operator;33 §3.5 契约列 operator 下,FE 其他角色 403 后回退 /me 的 quota_per_unit)。
func (s *Service) GetPlatformSettings(ctx context.Context, c session.Claims) (*PlatformSettingsView, error) {
	if err := assertRole(c, session.RoleOperator); err != nil {
		return nil, err
	}
	return s.readPlatformSettings(ctx)
}

func (s *Service) readPlatformSettings(ctx context.Context) (*PlatformSettingsView, error) {
	v := &PlatformSettingsView{}
	var err error
	if v.QuotaPerUnit, err = s.store.GetSettingInt64(ctx, "quota_per_unit", 500000); err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	frozen, ferr := s.store.GetSettingBool(ctx, "money_freeze")
	if ferr != nil {
		return nil, apperr.Internal("").WithCause(ferr)
	}
	v.MoneyFreeze = frozen
	if v.MemberTokenLimit, err = s.store.GetSettingInt64(ctx, "member_token_limit", 2); err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	if v.MemberQuotaCapRaw, err = s.store.GetSettingInt64(ctx, "member_quota_cap_raw", 500000000); err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	if v.TreasuryLowWatermarkRaw, err = s.store.GetSettingInt64(ctx, treasuryLowWatermarkKey, 0); err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	return v, nil
}

// UpdatePlatformSettings 平台配置写(operator;部分更新;quota_per_unit 只读不接受)。
// money_freeze 是涉钱红色开关:翻转必审计 + Error 级日志(值班可 grep);其余上限/阈值也逐项审计。
func (s *Service) UpdatePlatformSettings(ctx context.Context, c session.Claims, in UpdatePlatformSettingsInput) (*PlatformSettingsView, error) {
	if err := assertRole(c, session.RoleOperator); err != nil {
		return nil, err
	}
	if in.MoneyFreeze == nil && in.MemberTokenLimit == nil && in.MemberQuotaCapRaw == nil && in.TreasuryLowWatermarkRaw == nil {
		return nil, apperr.InvalidParam("没有可更新的字段(quota_per_unit 为只读)")
	}
	actor := actorOf(c)
	changed := map[string]any{}
	if in.MemberTokenLimit != nil {
		if *in.MemberTokenLimit <= 0 {
			return nil, apperr.InvalidParam("member_token_limit 须为正整数")
		}
		if err := s.store.SetSetting(ctx, "member_token_limit", strconv.FormatInt(*in.MemberTokenLimit, 10), actor); err != nil {
			return nil, apperr.Internal("").WithCause(err)
		}
		changed["member_token_limit"] = *in.MemberTokenLimit
	}
	if in.MemberQuotaCapRaw != nil {
		// int32 护栏(31-ADR §3):成员 user.quota 经 new-api int32 通道,帽绝不超之。
		if *in.MemberQuotaCapRaw <= 0 || *in.MemberQuotaCapRaw > 2147483647 {
			return nil, apperr.InvalidParam("member_quota_cap_raw 须为正且不超 int32 上限(2147483647)")
		}
		if err := s.store.SetSetting(ctx, "member_quota_cap_raw", strconv.FormatInt(*in.MemberQuotaCapRaw, 10), actor); err != nil {
			return nil, apperr.Internal("").WithCause(err)
		}
		changed["member_quota_cap_raw"] = *in.MemberQuotaCapRaw
	}
	if in.TreasuryLowWatermarkRaw != nil {
		if *in.TreasuryLowWatermarkRaw < 0 {
			return nil, apperr.InvalidParam("treasury_low_watermark_raw 须为非负数")
		}
		if err := s.store.SetSetting(ctx, treasuryLowWatermarkKey, strconv.FormatInt(*in.TreasuryLowWatermarkRaw, 10), actor); err != nil {
			return nil, apperr.Internal("").WithCause(err)
		}
		changed["treasury_low_watermark_raw"] = *in.TreasuryLowWatermarkRaw
	}
	if in.MoneyFreeze != nil {
		val := "false"
		if *in.MoneyFreeze {
			val = "true"
		}
		if err := s.store.SetSetting(ctx, "money_freeze", val, actor); err != nil {
			return nil, apperr.Internal("").WithCause(err)
		}
		changed["money_freeze"] = *in.MoneyFreeze
		if *in.MoneyFreeze {
			s.log.Error("🔴money_freeze 急停已开启:全平台划拨/补满/退额/修复写全部冻结(操作者见审计)", "actor", actor)
		} else {
			s.log.Warn("money_freeze 急停已解除,划拨恢复", "actor", actor)
		}
	}
	s.audit(ctx, c, c.OrgID, "update_platform_settings", "platform_setting", nil, changed)
	return s.readPlatformSettings(ctx)
}
