package service

import (
	"context"
	"fmt"
	"math"

	"github.com/nexusapi-platform/enterprise/pkg/session"
)

// 折扣对账(G,只读检测,绝不自动改价):平台落折扣后,new-api 侧的特殊倍率可能被
// 外部(人工/其它工具)改动,或主站基础倍率(GroupRatio)变动导致折扣率失真。对账只
// 比对「平台镜像」与「主站实际」,发现漂移即告警(日志 + 审计 + 通知运营),由人决定是否
// 重配,绝不自动回写——动钱面不做无人值守的自动纠偏(feedback:涉钱先讲风险)。

// ratioEps 倍率比较容差(浮点 + 主站存字符串往返)。
const ratioEps = 1e-9

// DriftKind 折扣漂移类型。
type DriftKind string

const (
	DriftTampered    DriftKind = "tampered"     // 己方特殊倍率被外部改(主站实际 != 平台镜像 Abs)
	DriftBaseShifted DriftKind = "base_shifted" // 主站基础倍率变动,折扣率已失真(实际折扣 != 配置折扣)
	DriftMissing     DriftKind = "missing"      // 平台镜像有折扣但主站特殊倍率已消失
)

// DiscountDrift 一条对账发现(只读)。
type DiscountDrift struct {
	OrgID      int64     `json:"org_id"`
	UserGroup  string    `json:"user_group"`
	TokenGroup string    `json:"token_group"`
	Kind       DriftKind `json:"kind"`
	Expected   float64   `json:"expected"` // 平台镜像应有值(Abs 或 Base)
	Actual     float64   `json:"actual"`   // 主站实际值
	Detail     string    `json:"detail"`
}

// ReconcileDiscounts 扫描所有已配折扣的组织,检测特殊倍率漂移并告警(G)。
// 返回本轮所有漂移项;无副作用(不改价)。worker 周期调用,运营也可手动触发。
func (s *Service) ReconcileDiscounts(ctx context.Context) ([]DiscountDrift, error) {
	// MVP 观测模式(改动⑥-3):折扣本期闲置(不写特殊倍率),折扣对账整段跳过,reconcile worker 只跑 ReconcileBilling。
	if s.observeMode {
		return nil, nil
	}
	ids, err := s.store.ListDiscountedOrgIDs(ctx)
	if err != nil {
		return nil, err
	}
	var all []DiscountDrift
	for _, orgID := range ids {
		drifts := s.reconcileOrgDiscount(ctx, orgID)
		all = append(all, drifts...)
	}
	if len(all) > 0 {
		s.log.Warn("折扣对账发现漂移(只告警不自动改价)", "count", len(all))
		for _, d := range all {
			s.log.Warn("折扣漂移",
				"org_id", d.OrgID, "token_group", d.TokenGroup, "kind", string(d.Kind),
				"expected", d.Expected, "actual", d.Actual, "detail", d.Detail)
			// 通知该组织管理员:价格异常需运营介入(不暴露内部细节)。
			for _, adminID := range s.orgAdminIDs(ctx, d.OrgID) {
				s.notify(ctx, d.OrgID, adminID, "pricing_drift", "价格配置异常",
					"检测到折扣配置与上游不一致,运营方将核对处理,期间计费以上游实际为准")
			}
			tg := d.TokenGroup
			s.auditSystem(ctx, d.OrgID, "discount_reconcile", "organization", &d.OrgID, map[string]any{
				"token_group": tg, "kind": string(d.Kind), "expected": d.Expected, "actual": d.Actual,
			}, "drift")
		}
	}
	return all, nil
}

// reconcileOrgDiscount 对单个组织的折扣镜像逐令牌分组对账。上游读失败的条目跳过(下轮再对)。
func (s *Service) reconcileOrgDiscount(ctx context.Context, orgID int64) []DiscountDrift {
	entries := s.loadDiscountEntries(ctx, orgID)
	if len(entries) == 0 {
		return nil
	}
	userGroup := s.orgUserGroup(ctx, orgID)
	var out []DiscountDrift
	for g, e := range entries {
		actual, ok, err := s.upstream.GetGroupGroupRatio(ctx, userGroup, g)
		if err != nil {
			continue // 上游暂不可达,本轮跳过
		}
		if !ok {
			out = append(out, DiscountDrift{OrgID: orgID, UserGroup: userGroup, TokenGroup: g, Kind: DriftMissing,
				Expected: e.Abs, Actual: 0, Detail: "平台镜像有折扣,但主站特殊倍率已消失"})
			continue
		}
		if math.Abs(actual-e.Abs) > ratioEps {
			out = append(out, DiscountDrift{OrgID: orgID, UserGroup: userGroup, TokenGroup: g, Kind: DriftTampered,
				Expected: e.Abs, Actual: actual,
				Detail: fmt.Sprintf("主站特殊倍率被外部改:应 %.6g 实 %.6g", e.Abs, actual)})
		}
		// 基础倍率漂移:当前 GroupRatio 变了 → 同样折扣率算出的绝对倍率应已不同。
		base, bok, berr := s.upstream.GetGroupRatio(ctx, g)
		if berr != nil {
			continue
		}
		if !bok || base <= 0 {
			base = 1
		}
		if math.Abs(base-e.Base) > ratioEps {
			out = append(out, DiscountDrift{OrgID: orgID, UserGroup: userGroup, TokenGroup: g, Kind: DriftBaseShifted,
				Expected: e.Base, Actual: base,
				Detail: fmt.Sprintf("主站基础倍率变动(快照 %.6g → 当前 %.6g),折扣率已失真,需重配", e.Base, base)})
		}
	}
	return out
}

// orgAdminIDs 取组织管理员成员 id(对账告警通知用);失败返回空(降级为只记日志)。
func (s *Service) orgAdminIDs(ctx context.Context, orgID int64) []int64 {
	ids, err := s.store.ListOrgAdminIDs(ctx, orgID)
	if err != nil {
		return nil
	}
	return ids
}

// ReconcileDiscountsForOperator 运营方手动触发对账(仅运营方,O)。
func (s *Service) ReconcileDiscountsForOperator(ctx context.Context, c session.Claims) ([]DiscountDrift, error) {
	if err := assertRole(c, session.RoleOperator); err != nil {
		return nil, err
	}
	return s.ReconcileDiscounts(ctx)
}
