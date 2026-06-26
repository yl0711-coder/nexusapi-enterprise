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
	// #3:导出客户财务可直接分摊的口径——员工名 + 美元(不再是 new-api 数字 id + quota 裸数)。
	// 成员名复用 by_member 已解析的 Label(display_name||login_email);空(非平台成员)回落标注。
	var b strings.Builder
	b.WriteString("维度,名称,费用(美元),调用次数\n")
	for _, m := range u.ByModel {
		b.WriteString(fmt.Sprintf("模型,%s,%s,%d\n", csvEsc(m.Key), usdStr(m.ConsumedQuota), m.Count))
	}
	for _, m := range u.ByMember {
		name := m.Label
		if name == "" {
			name = "未知成员(uid " + m.Key + ")"
		}
		b.WriteString(fmt.Sprintf("员工,%s,%s,%d\n", csvEsc(name), usdStr(m.ConsumedQuota), m.Count))
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="usage_org%d.csv"`, orgID))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(b.String()))
}
func csvEsc(s string) string {
	// CSV 公式注入防护(P2):成员名/模型名可被恶意构造成 = + - @ tab CR 开头,Excel/Sheets 打开会当公式执行。
	// 外发客户财务表,前缀单引号中和(Excel 视为文本)。多字节中文名首字节 >127,不会误触。
	if s != "" && strings.IndexByte("=+-@\t\r", s[0]) >= 0 {
		s = "'" + s
	}
	if strings.ContainsAny(s, ",\"\n") {
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return s
}

// usdStr 把 quota 换算成美元串(锚定 500000 quota=1 美元,与看板 money() 同口径)。
func usdStr(quota int64) string { return fmt.Sprintf("%.6f", float64(quota)/500000.0) }

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
