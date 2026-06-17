package handler

import (
	"net/http"

	"github.com/nexusapi-platform/enterprise/service"
)

func pricingView(v *service.PricingView) map[string]any {
	return map[string]any{
		"mode":                  v.Mode,
		"user_group":            v.UserGroup,
		"entries":               v.Entries,   // 平台镜像:每令牌分组 {pct, base, abs}
		"upstream_special_ratio": v.Upstream, // new-api 当前实际特殊倍率(只读回显)
	}
}

// GET /organizations/{id}/pricing — 折扣/计价回显(O/A;客户只读)。
func (h *Handler) handleGetPricing(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	orgID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	v, err := h.svc.GetPricing(r.Context(), c, orgID)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, pricingView(v))
}

// POST /pricing/reconcile — 手动触发折扣对账(仅运营方,只读告警,G)。
func (h *Handler) handleReconcileDiscounts(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	drifts, err := h.svc.ReconcileDiscountsForOperator(r.Context(), c)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	if drifts == nil {
		drifts = []service.DiscountDrift{}
	}
	writeOK(w, r, http.StatusOK, map[string]any{"drift_count": len(drifts), "drifts": drifts})
}

// PUT /organizations/{id}/pricing — 配置折扣(仅运营方,写 new-api 分组特殊倍率)。
type pricingReq struct {
	Mode        string   `json:"mode"`         // none/total/per_group
	DiscountPct float64  `json:"discount_pct"` // 0.9 = 9 折
	TokenGroups []string `json:"token_groups"` // total 留空默认 default;per_group 指定
}

func (h *Handler) handleConfigureDiscount(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	orgID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	var in pricingReq
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	v, err := h.svc.ConfigureDiscount(r.Context(), c, orgID, service.ConfigureDiscountInput{
		Mode: in.Mode, DiscountPct: in.DiscountPct, TokenGroups: in.TokenGroups,
	})
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, pricingView(v))
}
