package handler

import (
	"net/http"

	"github.com/nexusapi-platform/enterprise/service"
)

func pricingView(v *service.PricingView) map[string]any {
	return map[string]any{
		"mode":                 v.Mode,
		"newapi_group":         v.NewapiGroup,
		"group_ratio_config":   v.GroupRatioConfig,
		"group_ratio_upstream": v.GroupRatioUpstream, // new-api 当前实际值(只读回显)
		"special_ratios":       v.SpecialRatios,
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

// PUT /organizations/{id}/pricing — 配置折扣(仅运营方,单向写入 new-api)。
type pricingReq struct {
	Mode          string             `json:"mode"`
	NewapiGroup   string             `json:"newapi_group"`
	GroupRatio    *float64           `json:"group_ratio"`
	SpecialRatios map[string]float64 `json:"special_ratios"`
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
		Mode: in.Mode, NewapiGroup: in.NewapiGroup, GroupRatio: in.GroupRatio, SpecialRatios: in.SpecialRatios,
	})
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, pricingView(v))
}
