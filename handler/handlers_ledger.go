// 架构B 阶段1 BE③:读求和余额 + 分配账本可见性端点(全部只读 GET)。
// 字段规范名(组长契约裁定):balance=treasury_raw/members_total_raw/total_raw/low;
// ledger 行显式 direction(credit/debit)+ amount_raw + status。
package handler

import "net/http"

// GET /organizations/{id}/balance — 组织读求和余额(架构B:金库 + Σ成员,O/A)。
// 取代旧 company_balance/escrow 口径视图(第二账退役,33 §5)。
func (h *Handler) handleOrgBalance(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	orgID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	v, err := h.svc.OrgBalance(r.Context(), c, orgID)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, v)
}

// GET /me/balance — 成员本人额度/已用/剩余(29-PRD §4.7;额度光时带对客文案)。
func (h *Handler) handleMyBalance(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	v, err := h.svc.MyBalance(r.Context(), c)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, v)
}

// GET /organizations/{id}/ledger — 本组织划账流水(O/A,分页)。
func (h *Handler) handleOrgLedger(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	orgID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	page, size, offset := parsePaging(r, 50)
	items, total, err := h.svc.ListOrgLedger(r.Context(), c, orgID, size, offset)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, listResp{List: items, Pagination: makePageMeta(page, size, total)})
}

// GET /me/ledger — 成员看给自己的到账(仅 to_user=本人,33 §3.5)。
func (h *Handler) handleMyLedger(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	page, size, offset := parsePaging(r, 50)
	items, total, err := h.svc.ListMyLedger(r.Context(), c, size, offset)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, listResp{List: items, Pagination: makePageMeta(page, size, total)})
}

// GET /ledger — 全平台流水(仅运营方,可筛 org_id)。
func (h *Handler) handleAllLedger(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	page, size, offset := parsePaging(r, 50)
	var orgID int64
	if p := optInt64(r, "org_id"); p != nil {
		orgID = *p
	}
	items, total, err := h.svc.ListAllLedger(r.Context(), c, orgID, size, offset)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, listResp{List: items, Pagination: makePageMeta(page, size, total)})
}
