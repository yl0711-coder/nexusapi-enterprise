package handler

import (
	"net/http"
	"strconv"
	"strings"

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
	var channelID *int
	if raw := r.URL.Query().Get("channel"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil {
			channelID = &v
		}
	}
	startTimestamp, _ := strconv.ParseInt(r.URL.Query().Get("start_timestamp"), 10, 64)
	endTimestamp, _ := strconv.ParseInt(r.URL.Query().Get("end_timestamp"), 10, 64)
	items, total, err := h.svc.ListOrgNewapiLogs(r.Context(), c, orgID, service.OrgNewapiLogFilter{
		LogType: logType, MemberID: optInt64(r, "member_id"),
		StartTimestamp: startTimestamp, EndTimestamp: endTimestamp,
		TokenName: strings.TrimSpace(r.URL.Query().Get("token_name")),
		ModelName: strings.TrimSpace(r.URL.Query().Get("model_name")),
		ChannelID: channelID,
		GroupName: strings.TrimSpace(r.URL.Query().Get("group")),
		RequestID: strings.TrimSpace(r.URL.Query().Get("request_id")),
		Limit:     size, Offset: offset,
	})
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, map[string]any{"items": items, "page": makePageMeta(page, size, total)})
}

func (h *Handler) handleAllNewapiLogs(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	page, size, offset := parsePaging(r, 50)
	var logType *int
	if raw := r.URL.Query().Get("type"); raw != "" {
		v := atoiDefault(raw, 0)
		logType = &v
	}
	var channelID *int
	if raw := r.URL.Query().Get("channel"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil {
			channelID = &v
		}
	}
	var orgID int64
	if raw := strings.TrimSpace(r.URL.Query().Get("org_id")); raw != "" {
		if v, err := strconv.ParseInt(raw, 10, 64); err == nil {
			orgID = v
		}
	}
	startTimestamp, _ := strconv.ParseInt(r.URL.Query().Get("start_timestamp"), 10, 64)
	endTimestamp, _ := strconv.ParseInt(r.URL.Query().Get("end_timestamp"), 10, 64)
	items, total, err := h.svc.ListAllNewapiLogs(r.Context(), c, service.OrgNewapiLogFilter{
		OrgID: orgID, LogType: logType, MemberID: optInt64(r, "member_id"),
		StartTimestamp: startTimestamp, EndTimestamp: endTimestamp,
		TokenName: strings.TrimSpace(r.URL.Query().Get("token_name")),
		ModelName: strings.TrimSpace(r.URL.Query().Get("model_name")),
		ChannelID: channelID,
		GroupName: strings.TrimSpace(r.URL.Query().Get("group")),
		RequestID: strings.TrimSpace(r.URL.Query().Get("request_id")),
		Limit:     size, Offset: offset,
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
	page, size, offset := parsePaging(r, 50) // B4(28):分页,不再一次拉全量
	items, total, err := h.svc.ListMemberTokenMappings(r.Context(), c, orgID, size, offset)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, map[string]any{"items": items, "page": makePageMeta(page, size, total)})
}
