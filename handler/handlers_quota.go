package handler

import (
	"net/http"
	"time"

	"github.com/nexusapi-platform/enterprise/pkg/apperr"
	"github.com/nexusapi-platform/enterprise/service"
)

// POST /members/{id}/quota:adjust — 临时调额(US-03)。
type adjustQuotaReq struct {
	DeltaQuota int64  `json:"delta_quota"`
	Duration   string `json:"duration"`
	Reason     string `json:"reason"`
}

func (h *Handler) handleAdjustQuota(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	memberID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	var in adjustQuotaReq
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	res, err := h.svc.AdjustQuota(r.Context(), c, c.OrgID, memberID, service.AdjustQuotaInput{
		DeltaQuota: in.DeltaQuota, Duration: in.Duration, Reason: in.Reason,
	})
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, map[string]any{
		"member_id":     memberID,
		"new_cap_quota": res.NewCapQuota,
		"grant_id":      res.GrantID,
		"expire_at":     res.ExpireAt.Format(time.RFC3339),
	})
}

// POST /members/{id}/grants — 设临时权限(US-04:account_ttl / model_add)。
type setGrantReq struct {
	Type     string `json:"type"`
	Model    string `json:"model"`
	ExpireAt string `json:"expire_at"` // RFC3339
	Reason   string `json:"reason"`
}

func (h *Handler) handleSetGrant(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	memberID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	var in setGrantReq
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	exp, perr := time.Parse(time.RFC3339, in.ExpireAt)
	if perr != nil {
		writeErr(w, r, apperr.InvalidParam("expire_at 须为 RFC3339 时间"))
		return
	}
	g, err := h.svc.SetTempPermission(r.Context(), c, c.OrgID, memberID, service.TempPermissionInput{
		Type: in.Type, Model: in.Model, ExpireAt: exp, Reason: in.Reason,
	})
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusCreated, toGrantView(g))
}

// GET /members/{id}/grants — 列临时权限。
func (h *Handler) handleListGrants(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	memberID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	page, size, offset := parsePaging(r, 20)
	grants, total, err := h.svc.ListGrants(r.Context(), c, c.OrgID, memberID, size, offset)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	views := make([]grantView, 0, len(grants))
	for _, g := range grants {
		views = append(views, toGrantView(g))
	}
	writeOK(w, r, http.StatusOK, listResp{List: views, Pagination: makePageMeta(page, size, total)})
}

// DELETE /grants/{id} — 提前撤销临时权限。
func (h *Handler) handleRevokeGrant(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	grantID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	if err := h.svc.RevokeGrant(r.Context(), c, c.OrgID, grantID); err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, map[string]any{"grant_id": grantID, "revoked": true})
}

// POST /members/{id}/status — 停用/恢复成员(US-05)。
type setStatusReq struct {
	Enabled bool `json:"enabled"`
}

func (h *Handler) handleSetMemberStatus(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	memberID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	var in setStatusReq
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	if err := h.svc.SetMemberStatus(r.Context(), c, c.OrgID, memberID, in.Enabled); err != nil {
		writeErr(w, r, err)
		return
	}
	status := "disabled"
	if in.Enabled {
		status = "active"
	}
	writeOK(w, r, http.StatusOK, map[string]any{"member_id": memberID, "status": status})
}

// POST /members/{id}/offboard — 离职(危险):删 token + 软删转离职列表。
func (h *Handler) handleOffboardMember(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	memberID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	if err := h.svc.OffboardMember(r.Context(), c, c.OrgID, memberID); err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, map[string]any{"member_id": memberID, "status": "offboarded"})
}

// POST /members/{id}/restore — 恢复入职:清软删置 active(员工自助重建 key)。
func (h *Handler) handleRestoreMember(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	memberID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	if err := h.svc.RestoreOffboardedMember(r.Context(), c, c.OrgID, memberID); err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, map[string]any{"member_id": memberID, "status": "active"})
}

// GET /organizations/{id}/members/offboarded — 离职列表(可恢复)。
func (h *Handler) handleListOffboarded(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	orgID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	page, size, offset := parsePaging(r, 20)
	members, total, err := h.svc.ListOffboardedMembers(r.Context(), c, orgID, size, offset)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	views := make([]memberView, 0, len(members))
	for _, m := range members {
		views = append(views, toMemberView(m))
	}
	writeOK(w, r, http.StatusOK, listResp{List: views, Pagination: makePageMeta(page, size, total)})
}
