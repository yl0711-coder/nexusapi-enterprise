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

func granularityParam(r *http.Request) string { return r.URL.Query().Get("granularity") }

// GET /organizations/{id}/usage/timeseries — 组织用量时间序列(折线图,O/A;granularity=day|week|month)。
func (h *Handler) handleOrgUsageTimeSeries(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	orgID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	pts, err := h.svc.OrgUsageTimeSeries(r.Context(), c, orgID, sinceHours(r), granularityParam(r))
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, map[string]any{"series": pts, "since_hours": sinceHours(r)})
}

// GET /members/{id}/usage/timeseries — 成员用量时间序列(本人/上级/管理员)。
func (h *Handler) handleMemberUsageTimeSeries(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	memberID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	pts, err := h.svc.MemberUsageTimeSeries(r.Context(), c, c.OrgID, memberID, sinceHours(r), granularityParam(r))
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, map[string]any{"series": pts, "since_hours": sinceHours(r)})
}

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

// GET /organizations/{id}/usage/detail — 组织下钻明细(O/A;读 usage_detail 逐条;member_id/key_id/model/page 过滤)。
func (h *Handler) handleOrgUsageDetail(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	orgID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	q := r.URL.Query()
	pg, err := h.svc.OrgUsageDetail(r.Context(), c, orgID, sinceHours(r),
		optInt64(r, "member_id"), optInt64(r, "key_id"), q.Get("model"),
		atoiDefault(q.Get("page"), 1), atoiDefault(q.Get("page_size"), 50))
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, pg)
}

// GET /members/{id}/usage/detail — 成员下钻明细(本人/上级/管理员;锁定该 member)。
func (h *Handler) handleMemberUsageDetail(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	memberID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	q := r.URL.Query()
	pg, err := h.svc.MemberUsageDetail(r.Context(), c, c.OrgID, memberID, sinceHours(r),
		atoiDefault(q.Get("page"), 1), atoiDefault(q.Get("page_size"), 50))
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, pg)
}

// GET /organizations/{id}/escrow-balance — 模型2 读穿余额(窗口=桶1读穿 newapi + 托管之和,O/A)。
func (h *Handler) handleEscrowBalance(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	orgID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	bal, err := h.svc.GetDerivedBalance(r.Context(), c, orgID)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, bal)
}

// POST /organizations/{id}/escrow/refill — 手工续充:把一个托管桶并入桶1 可花窗口(运营方,v1 无自动 worker)。
func (h *Handler) handleEscrowRefill(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	orgID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	bal, err := h.svc.RefillWindow(r.Context(), c, orgID)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, bal)
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
