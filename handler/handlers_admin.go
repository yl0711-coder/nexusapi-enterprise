package handler

import (
	"net/http"

	"github.com/nexusapi-platform/enterprise/repo"
)

// PATCH /organizations/{id} — 组织设置(E19)。
type orgSettingsReq struct {
	Name          *string `json:"name"`
	Timezone      *string `json:"timezone"`
	DefaultTierID *int64  `json:"default_tier_id"`
}

func (h *Handler) handleUpdateOrg(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	orgID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	var in orgSettingsReq
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	o, err := h.svc.UpdateOrgSettings(r.Context(), c, orgID, in.Name, in.Timezone, in.DefaultTierID)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, toOrgView(o))
}

// GET/PUT /organizations/{id}/approval-rules — 审批阈值(E13)。
type approvalRulesReq struct {
	AutoMaxQuota *int64 `json:"auto_max_quota"`
	AutoMaxDays  *int   `json:"auto_max_days"`
	L1MaxQuota   *int64 `json:"l1_max_quota"`
}

func rulesView(r *repo.ApprovalRules) map[string]any {
	return map[string]any{"auto_max_quota": r.AutoMaxQuota, "auto_max_days": r.AutoMaxDays, "l1_max_quota": r.L1MaxQuota}
}
func (h *Handler) handleGetApprovalRules(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	orgID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	rr, err := h.svc.GetApprovalRules(r.Context(), c, orgID)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, rulesView(rr))
}
func (h *Handler) handleSetApprovalRules(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	orgID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	var in approvalRulesReq
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	rr, err := h.svc.SetApprovalRules(r.Context(), c, orgID, in.AutoMaxQuota, in.L1MaxQuota, in.AutoMaxDays)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, rulesView(rr))
}

// PATCH /members/{id} — 改团队/层级。
type updateMemberReq struct {
	TeamID *int64 `json:"team_id"`
	TierID *int64 `json:"tier_id"`
}

func (h *Handler) handleUpdateMember(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	mid, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	var in updateMemberReq
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	m, err := h.svc.UpdateMember(r.Context(), c, c.OrgID, mid, in.TeamID, in.TierID)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, toMemberView(m))
}

// POST /members/{id}/role — 任命/变更角色(E17)。
type roleReq struct {
	Role string `json:"role"`
}

func (h *Handler) handleAssignRole(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	mid, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	var in roleReq
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	if err := h.svc.AssignRole(r.Context(), c, c.OrgID, mid, in.Role); err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, map[string]any{"member_id": mid, "role": in.Role})
}

// GET/PUT /organizations/{id}/quota-policies — 配额策略。
func policyView(p *repo.QuotaPolicy) map[string]any {
	return map[string]any{"id": p.ID, "scope": p.Scope, "scope_id": p.ScopeID, "period": p.Period, "limit_quota": p.LimitQuota, "reset_anchor": p.ResetAnchor, "status": p.Status}
}
func (h *Handler) handleListPolicies(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	orgID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	ps, err := h.svc.ListQuotaPolicies(r.Context(), c, orgID)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(ps))
	for _, p := range ps {
		out = append(out, policyView(p))
	}
	writeOK(w, r, http.StatusOK, out)
}

type policyReq struct {
	Scope       string `json:"scope"`
	ScopeID     int64  `json:"scope_id"`
	Period      string `json:"period"`
	LimitQuota  int64  `json:"limit_quota"`
	ResetAnchor string `json:"reset_anchor"`
}

func (h *Handler) handleSetPolicy(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	orgID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	var in policyReq
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	if err := h.svc.SetQuotaPolicy(r.Context(), c, orgID, &repo.QuotaPolicy{
		Scope: in.Scope, ScopeID: in.ScopeID, Period: in.Period, LimitQuota: in.LimitQuota, ResetAnchor: in.ResetAnchor,
	}); err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, map[string]any{"ok": true})
}
