package handler

import (
	"net/http"
)

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
