package handler

import (
	"net/http"
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

// PATCH /members/{id} — 改团队/层级。
type updateMemberReq struct {
	TeamID      *int64  `json:"team_id"`
	TierID      *int64  `json:"tier_id"`
	DisplayName *string `json:"display_name"` // v1 19-F2:管理员改成员显示名(导入名/随机名可改)
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
	m, err := h.svc.UpdateMember(r.Context(), c, c.OrgID, mid, in.TeamID, in.TierID, in.DisplayName)
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

