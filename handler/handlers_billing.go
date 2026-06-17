package handler

import (
	"net/http"
	"strconv"

	"github.com/nexusapi-platform/enterprise/pkg/session"
	"github.com/nexusapi-platform/enterprise/service"
)

// actorString 给响应展示用的操作者标识(脱敏,只到角色+成员 id)。
func actorString(c session.Claims) string {
	return string(c.Role) + ":" + strconv.FormatInt(c.MemberID, 10)
}

// GET /organizations/{id}/balance — 公司余额(O/A)。
func (h *Handler) handleGetBalance(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	orgID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	b, err := h.svc.GetBalance(r.Context(), c, orgID)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, toBalanceView(b))
}

// GET /organizations/{id}/recharges — 入账记录(O/A)。
func (h *Handler) handleListRecharges(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	orgID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	page, size, offset := parsePaging(r, 20)
	rs, total, err := h.svc.ListRecharges(r.Context(), c, orgID, size, offset)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	views := make([]rechargeView, 0, len(rs))
	for _, rc := range rs {
		views = append(views, toRechargeView(rc))
	}
	writeOK(w, r, http.StatusOK, listResp{List: views, Pagination: makePageMeta(page, size, total)})
}

// POST /organizations/{id}/recharges — 入账(运营方,US-08)。
type rechargeReq struct {
	AmountQuota int64  `json:"amount_quota"`
	TransferNo  string `json:"transfer_no"`
	AmountCNY   *int64 `json:"amount_cny"`
	Note        string `json:"note"`
}

func (h *Handler) handleRecharge(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	orgID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	var in rechargeReq
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	b, err := h.svc.Recharge(r.Context(), c, orgID, service.RechargeInput{
		AmountQuota: in.AmountQuota, TransferNo: in.TransferNo, AmountCNY: in.AmountCNY, Note: in.Note,
	})
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusCreated, map[string]any{
		"org_id":              orgID,
		"balance_quota_after": b.Balance,
		"operator":            actorString(c),
	})
}

// GET /organizations/{id}/recharge-requests — 申请列表(O/A)。
func (h *Handler) handleListRechargeRequests(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	orgID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	page, size, offset := parsePaging(r, 20)
	rqs, total, err := h.svc.ListRechargeRequests(r.Context(), c, orgID, size, offset)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	views := make([]rechargeReqView, 0, len(rqs))
	for _, rq := range rqs {
		views = append(views, toRechargeReqView(rq))
	}
	writeOK(w, r, http.StatusOK, listResp{List: views, Pagination: makePageMeta(page, size, total)})
}

// POST /organizations/{id}/recharge-requests — 申请充值/退款(组织管理员,US-09/US-12)。
type rechargeRequestReq struct {
	Type   string `json:"type"`
	Amount int64  `json:"amount_quota"`
	Note   string `json:"note"`
}

// GET /organizations/{id}/billing-settings — 计费开关(O/A)。
func (h *Handler) handleGetBillingSettings(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	orgID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	bs, err := h.svc.GetBillingSettings(r.Context(), c, orgID)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, map[string]any{
		"billing_enabled": bs.BillingEnabled, "hard_stop_enabled": bs.HardStopEnabled, "low_watermark_quota": bs.LowWatermark,
	})
}

// PATCH /organizations/{id}/billing-settings — 设计费灰度开关(仅运营方)。
type billingSettingsReq struct {
	BillingEnabled  *bool  `json:"billing_enabled"`
	HardStopEnabled *bool  `json:"hard_stop_enabled"`
	LowWatermark    *int64 `json:"low_watermark_quota"`
}

func (h *Handler) handleSetBillingSettings(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	orgID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	var in billingSettingsReq
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	bs, err := h.svc.SetBillingSettings(r.Context(), c, orgID, service.BillingSettingsInput{
		BillingEnabled: in.BillingEnabled, HardStopEnabled: in.HardStopEnabled, LowWatermark: in.LowWatermark,
	})
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, map[string]any{
		"billing_enabled": bs.BillingEnabled, "hard_stop_enabled": bs.HardStopEnabled, "low_watermark_quota": bs.LowWatermark,
	})
}

func (h *Handler) handleRequestRecharge(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	orgID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	var in rechargeRequestReq
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	rq, err := h.svc.RequestRecharge(r.Context(), c, orgID, service.RequestRechargeInput{
		Type: in.Type, Amount: in.Amount, Note: in.Note,
	})
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusCreated, toRechargeReqView(rq))
}
