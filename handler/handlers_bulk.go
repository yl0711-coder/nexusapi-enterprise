package handler

import (
	"fmt"
	"net/http"
	"strings"
)

// POST /organizations/{id}/members:bulk — 批量开通(US-02)。
type bulkOpenReq struct {
	Names  []string `json:"names"`
	TeamID *int64   `json:"team_id"`
	TierID *int64   `json:"tier_id"`
}

func (h *Handler) handleBulkOpen(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	orgID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	var in bulkOpenReq
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	res, err := h.svc.BulkOpenMembers(r.Context(), c, orgID, in.Names, in.TeamID, in.TierID)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	ok := 0
	for _, x := range res {
		if x.OK {
			ok++
		}
	}
	writeOK(w, r, http.StatusOK, map[string]any{"results": res, "success": ok, "failed": len(res) - ok})
}

// POST /organizations/{id}/members:bulk-status — 批量停用/恢复。
type bulkStatusReq struct {
	MemberIDs []int64 `json:"member_ids"`
	Enabled   bool    `json:"enabled"`
}

func (h *Handler) handleBulkStatus(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	orgID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	var in bulkStatusReq
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	res, err := h.svc.BulkSetStatus(r.Context(), c, orgID, in.MemberIDs, in.Enabled)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, map[string]any{"results": res})
}

// GET /organizations/{id}/usage/export — 导出用量对账(CSV)。
func (h *Handler) handleUsageExport(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	orgID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	u, err := h.svc.OrgUsage(r.Context(), c, orgID, sinceHours(r))
	if err != nil {
		writeErr(w, r, err)
		return
	}
	var b strings.Builder
	b.WriteString("scope,key,consumed_quota,count\n")
	for _, m := range u.ByModel {
		b.WriteString(fmt.Sprintf("model,%s,%d,%d\n", csvEsc(m.Key), m.ConsumedQuota, m.Count))
	}
	for _, m := range u.ByMember {
		b.WriteString(fmt.Sprintf("member_newapi_uid,%s,%d,%d\n", csvEsc(m.Key), m.ConsumedQuota, m.Count))
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="usage_org%d.csv"`, orgID))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(b.String()))
}
func csvEsc(s string) string {
	if strings.ContainsAny(s, ",\"\n") {
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return s
}

// GET /service-status — 服务状态(全角色)。
func (h *Handler) handleServiceStatus(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	st := h.svc.GetServiceStatus(r.Context(), c)
	writeOK(w, r, http.StatusOK, map[string]any{"overall": st.Overall, "note": st.Note, "models": st.Models})
}

// POST /members/{id}/key:ip-whitelist — 设自己 key 的 IP 白名单(E22)。
type ipWhitelistReq struct {
	AllowIPs string `json:"allow_ips"`
}

func (h *Handler) handleSetKeyIP(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	mid, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	var in ipWhitelistReq
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	if err := h.svc.SetKeyIPWhitelist(r.Context(), c, c.OrgID, mid, in.AllowIPs); err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, map[string]any{"member_id": mid, "allow_ips": in.AllowIPs})
}
