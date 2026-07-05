package handler

import (
	"net/http"

	"github.com/nexusapi-platform/enterprise/service"
)

// POST /api/v1/approvals — 成员提交增额/开模型申请(US-06,自助)。
type submitApprovalReq struct {
	Model    string `json:"model"`
	Amount   int64  `json:"amount_quota"`
	Duration string `json:"duration"`
	Reason   string `json:"reason"`
}

func (h *Handler) handleSubmitApproval(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	var in submitApprovalReq
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	a, err := h.svc.SubmitApproval(r.Context(), c, service.SubmitApprovalInput{
		Model: in.Model, Amount: in.Amount, Duration: in.Duration, Reason: in.Reason,
	})
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusCreated, toApprovalView(a))
}

// GET /api/v1/organizations/{id}/approvals?state= — 审批队列(O/A 全 org / L 本团队 / M 本人)。
func (h *Handler) handleListApprovals(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	// C13:路径 {id} 不用于取数——ListApprovals 以会话 c.OrgID 为权威 org(防 IDOR,路径 org 不信任,仅 URL 语义)。
	page, size, offset := parsePaging(r, 20)
	as, total, err := h.svc.ListApprovals(r.Context(), c, r.URL.Query().Get("state"), size, offset)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	views := make([]approvalView, 0, len(as))
	for _, a := range as {
		views = append(views, toApprovalView(a))
	}
	writeOK(w, r, http.StatusOK, listResp{List: views, Pagination: makePageMeta(page, size, total)})
}

// POST /api/v1/approvals/{id}/decide — 批准/驳回(US-06)。
type decideReq struct {
	Approved bool   `json:"approved"`
	Comment  string `json:"comment"`
}

func (h *Handler) handleDecideApproval(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	id, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	var in decideReq
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	a, err := h.svc.DecideApproval(r.Context(), c, id, in.Approved, in.Comment)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, toApprovalView(a))
}

// GET /api/v1/notifications — 本人站内通知(US-13)。
func (h *Handler) handleListNotifications(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	page, size, offset := parsePaging(r, 20)
	ns, total, unread, err := h.svc.ListNotifications(r.Context(), c, size, offset)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	views := make([]notificationView, 0, len(ns))
	for _, n := range ns {
		views = append(views, toNotificationView(n))
	}
	writeOK(w, r, http.StatusOK, map[string]any{
		"list":       views,
		"unread":     unread,
		"pagination": makePageMeta(page, size, total),
	})
}

// POST /api/v1/notifications/{id}/read — 标已读(id=all 标全部)。
func (h *Handler) handleMarkNotificationRead(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	var id int64
	if r.PathValue("id") == "all" {
		id = 0
	} else {
		v, err := pathInt64(r, "id")
		if err != nil {
			writeErr(w, r, err)
			return
		}
		id = v
	}
	if err := h.svc.MarkNotificationRead(r.Context(), c, id); err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, map[string]any{"ok": true})
}
