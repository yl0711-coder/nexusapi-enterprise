package handler

import (
	"net/http"
	"time"

	"github.com/nexusapi-platform/enterprise/model"
	"github.com/nexusapi-platform/enterprise/service"
)

func supportSessionView(ss *model.SupportSession) map[string]any {
	gt := ""
	if ss.GrantType != nil {
		gt = *ss.GrantType
	}
	return map[string]any{
		"session_id":   ss.ID,
		"org_id":       ss.OrgID,
		"actor":        ss.Actor,
		"on_behalf_of": ss.OnBehalfOf,
		"scope":        ss.Scope,
		"grant_type":   gt,
		"state":        ss.State,
		"expire_at":    ss.ExpireAt.Format(time.RFC3339),
	}
}

// POST /organizations/{id}/support-sessions — 运营方开支持会话(只读/协助)。
type openSupportReq struct {
	Scope      string `json:"scope"`
	GrantType  string `json:"grant_type"`
	TTLSeconds int    `json:"ttl_seconds"`
	Reason     string `json:"reason"`
}

func (h *Handler) handleOpenSupport(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	orgID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	var in openSupportReq
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	res, err := h.svc.OpenSupportSession(r.Context(), c, orgID, service.OpenSupportInput{
		Scope: in.Scope, GrantType: in.GrantType, TTLSeconds: in.TTLSeconds, Reason: in.Reason,
	})
	if err != nil {
		writeErr(w, r, err)
		return
	}
	out := supportSessionView(res.Session)
	out["token"] = res.Token // 运营方据此 token 以支持态在该 org 操作
	writeOK(w, r, http.StatusCreated, out)
}

// GET /support-sessions/{id} — 会话状态。
func (h *Handler) handleGetSupport(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	sid, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	ss, err := h.svc.GetSupportSession(r.Context(), c, sid)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, supportSessionView(ss))
}

// POST /support-sessions/{id}/close — 结束会话。
func (h *Handler) handleCloseSupport(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	sid, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	if err := h.svc.CloseSupportSession(r.Context(), c, sid); err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, map[string]any{"closed": true})
}

// ---- 用量看板 ----

func usageView(u *service.UsageReport) map[string]any {
	return map[string]any{
		"since_hours": u.SinceHours,
		"total_quota": u.TotalQuota,
		"by_model":    u.ByModel,
		"by_member":   u.ByMember,
		"by_team":     u.ByTeam, // F3:按团队(整组织看板填;团队下钻/成员视图为空)
	}
}

func sinceHours(r *http.Request) int { return atoiDefault(r.URL.Query().Get("since_hours"), 24) }

// GET /organizations/{id}/usage — 组织用量分析(O/A)。
func (h *Handler) handleOrgUsage(w http.ResponseWriter, r *http.Request) {
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
	writeOK(w, r, http.StatusOK, usageView(u))
}

// GET /organizations/{id}/budget-ref — 额度参考条(#4·B:已用$/预付$,藏价有意例外,O/A)。
func (h *Handler) handleBudgetRef(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	orgID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	ref, err := h.svc.OrgBudgetRef(r.Context(), c, orgID)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, map[string]any{
		"consumed_quota":  ref.ConsumedQuota,
		"recharged_quota": ref.RechargedQuota,
	})
}

// GET /members/{id}/usage — 成员用量(本人/上级/管理员)。
func (h *Handler) handleMemberUsage(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	memberID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	u, err := h.svc.MemberUsage(r.Context(), c, c.OrgID, memberID, sinceHours(r))
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, usageView(u))
}
