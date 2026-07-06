package handler

import (
	"net/http"

	"github.com/nexusapi-platform/enterprise/service"
)

func (h *Handler) handleOrgNewapiLogs(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	orgID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	page, size, offset := parsePaging(r, 50)
	var logType *int
	if raw := r.URL.Query().Get("type"); raw != "" {
		v := atoiDefault(raw, 0)
		logType = &v
	}
	items, total, err := h.svc.ListOrgNewapiLogs(r.Context(), c, orgID, service.OrgNewapiLogFilter{
		LogType: logType, MemberID: optInt64(r, "member_id"), RequestID: r.URL.Query().Get("request_id"),
		Limit: size, Offset: offset,
	})
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, map[string]any{"items": items, "page": makePageMeta(page, size, total)})
}

func (h *Handler) handleOrgTokenMappings(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	orgID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	items, err := h.svc.ListMemberTokenMappings(r.Context(), c, orgID)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, map[string]any{"items": items})
}
