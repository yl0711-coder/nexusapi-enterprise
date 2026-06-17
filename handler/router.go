package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/nexusapi-platform/enterprise/pkg/session"
	"github.com/nexusapi-platform/enterprise/service"
	"github.com/nexusapi-platform/enterprise/web"
)

// Handler 持有依赖,挂载所有平台 REST 路由(10 §1.7,前缀 /api/v1)。
type Handler struct {
	svc     *service.Service
	signer  *session.Signer
	log     *slog.Logger
	version string
}

// New 构造 Handler。
func New(svc *service.Service, signer *session.Signer, log *slog.Logger, version string) *Handler {
	if log == nil {
		log = slog.Default()
	}
	return &Handler{svc: svc, signer: signer, log: log, version: version}
}

// Routes 返回挂好中间件的根 http.Handler。
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()

	// 健康检查(无需鉴权;部署探针,沿用里程碑 0 契约)。
	mux.HandleFunc("GET /healthz", h.handleHealthz)
	mux.HandleFunc("GET /readyz", h.handleReadyz)

	// 认证。
	mux.HandleFunc("POST /api/v1/auth/login", h.handleLogin)
	mux.HandleFunc("GET /api/v1/me", h.requireAuth(h.handleMe))

	// 组织(运营方)。
	mux.HandleFunc("GET /api/v1/organizations", h.requireAuth(h.handleListOrgs))
	mux.HandleFunc("POST /api/v1/organizations", h.requireAuth(h.handleCreateOrg))
	mux.HandleFunc("GET /api/v1/organizations/{id}", h.requireAuth(h.handleGetOrg))
	mux.HandleFunc("PATCH /api/v1/organizations/{id}", h.requireAuth(h.handleUpdateOrg))
	mux.HandleFunc("GET /api/v1/organizations/{id}/approval-rules", h.requireAuth(h.handleGetApprovalRules))
	mux.HandleFunc("PUT /api/v1/organizations/{id}/approval-rules", h.requireAuth(h.handleSetApprovalRules))
	mux.HandleFunc("GET /api/v1/organizations/{id}/quota-policies", h.requireAuth(h.handleListPolicies))
	mux.HandleFunc("PUT /api/v1/organizations/{id}/quota-policies", h.requireAuth(h.handleSetPolicy))

	// 团队。
	mux.HandleFunc("GET /api/v1/organizations/{id}/teams", h.requireAuth(h.handleListTeams))
	mux.HandleFunc("POST /api/v1/organizations/{id}/teams", h.requireAuth(h.handleCreateTeam))

	// 层级。
	mux.HandleFunc("GET /api/v1/organizations/{id}/tiers", h.requireAuth(h.handleListTiers))
	mux.HandleFunc("POST /api/v1/organizations/{id}/tiers", h.requireAuth(h.handleCreateTier))
	mux.HandleFunc("POST /api/v1/tiers/{id}/default", h.requireAuth(h.handleSetDefaultTier))

	// 成员(开通成员 = 代发 key,US-01)。
	mux.HandleFunc("GET /api/v1/organizations/{id}/members", h.requireAuth(h.handleListMembers))
	mux.HandleFunc("POST /api/v1/organizations/{id}/members", h.requireAuth(h.handleOpenMember))
	mux.HandleFunc("POST /api/v1/organizations/{id}/members:bulk", h.requireAuth(h.handleBulkOpen))
	mux.HandleFunc("POST /api/v1/organizations/{id}/members:bulk-status", h.requireAuth(h.handleBulkStatus))
	mux.HandleFunc("GET /api/v1/members/{id}", h.requireAuth(h.handleGetMember))
	mux.HandleFunc("PATCH /api/v1/members/{id}", h.requireAuth(h.handleUpdateMember))
	mux.HandleFunc("POST /api/v1/members/{id}/role", h.requireAuth(h.handleAssignRole))
	mux.HandleFunc("POST /api/v1/members/{id}/key:rotate", h.requireAuth(h.handleRotateKey))
	mux.HandleFunc("POST /api/v1/members/{id}/key:ip-whitelist", h.requireAuth(h.handleSetKeyIP))

	// 额度执行(里程碑2):调额 / 临时权限 / 停用恢复。
	mux.HandleFunc("POST /api/v1/members/{id}/quota:adjust", h.requireAuth(h.handleAdjustQuota))
	mux.HandleFunc("POST /api/v1/members/{id}/grants", h.requireAuth(h.handleSetGrant))
	mux.HandleFunc("GET /api/v1/members/{id}/grants", h.requireAuth(h.handleListGrants))
	mux.HandleFunc("DELETE /api/v1/grants/{id}", h.requireAuth(h.handleRevokeGrant))
	mux.HandleFunc("POST /api/v1/members/{id}/status", h.requireAuth(h.handleSetMemberStatus))

	// 计费(里程碑3a):余额 / 入账 / 申请充值(钱进 + 只读 + 告警;扣费 3b 下一轮)。
	mux.HandleFunc("GET /api/v1/organizations/{id}/balance", h.requireAuth(h.handleGetBalance))
	mux.HandleFunc("GET /api/v1/organizations/{id}/recharges", h.requireAuth(h.handleListRecharges))
	mux.HandleFunc("POST /api/v1/organizations/{id}/recharges", h.requireAuth(h.handleRecharge))
	mux.HandleFunc("GET /api/v1/organizations/{id}/recharge-requests", h.requireAuth(h.handleListRechargeRequests))
	mux.HandleFunc("POST /api/v1/organizations/{id}/recharge-requests", h.requireAuth(h.handleRequestRecharge))
	mux.HandleFunc("POST /api/v1/organizations/{id}/debits", h.requireAuth(h.handleDebit))
	// 计费灰度开关(里程碑3b,逐组织,默认关;运营方控)。
	mux.HandleFunc("GET /api/v1/organizations/{id}/billing-settings", h.requireAuth(h.handleGetBillingSettings))
	mux.HandleFunc("PATCH /api/v1/organizations/{id}/billing-settings", h.requireAuth(h.handleSetBillingSettings))
	// 计价/折扣联动(里程碑3c,单向写 new-api 分组倍率,客户只读)。
	mux.HandleFunc("GET /api/v1/organizations/{id}/pricing", h.requireAuth(h.handleGetPricing))
	mux.HandleFunc("PUT /api/v1/organizations/{id}/pricing", h.requireAuth(h.handleConfigureDiscount))

	// 申请-审批(里程碑4,US-06)+ 通知(US-13)+ 成员自助。
	mux.HandleFunc("POST /api/v1/approvals", h.requireAuth(h.handleSubmitApproval))
	mux.HandleFunc("GET /api/v1/organizations/{id}/approvals", h.requireAuth(h.handleListApprovals))
	mux.HandleFunc("POST /api/v1/approvals/{id}/decide", h.requireAuth(h.handleDecideApproval))
	mux.HandleFunc("GET /api/v1/notifications", h.requireAuth(h.handleListNotifications))
	mux.HandleFunc("POST /api/v1/notifications/{id}/read", h.requireAuth(h.handleMarkNotificationRead))

	// 用量看板(里程碑5)。
	mux.HandleFunc("GET /api/v1/organizations/{id}/usage", h.requireAuth(h.handleOrgUsage))
	mux.HandleFunc("GET /api/v1/members/{id}/usage", h.requireAuth(h.handleMemberUsage))
	mux.HandleFunc("GET /api/v1/organizations/{id}/usage/export", h.requireAuth(h.handleUsageExport))
	mux.HandleFunc("GET /api/v1/service-status", h.requireAuth(h.handleServiceStatus))

	// 运营方三层支持(里程碑5,08 §2.2/§3.2)。
	mux.HandleFunc("POST /api/v1/organizations/{id}/support-sessions", h.requireAuth(h.handleOpenSupport))
	mux.HandleFunc("GET /api/v1/support-sessions/{id}", h.requireAuth(h.handleGetSupport))
	mux.HandleFunc("POST /api/v1/support-sessions/{id}/close", h.requireAuth(h.handleCloseSupport))

	// 前端 SPA(catch-all,最不具体,/api/v1 与 /healthz 等更具体的先匹配):
	// /api/v1/* 之外的路径走内嵌静态前端;/ 返回 index.html。
	mux.Handle("GET /", http.FileServerFS(web.FS))

	// 中间件链:request_id → recover → mux。
	return withRequestID(h.recoverPanic(mux))
}

func (h *Handler) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeRaw(w, http.StatusOK, map[string]string{"status": "ok", "version": h.version})
}

func (h *Handler) handleReadyz(w http.ResponseWriter, r *http.Request) {
	// R2-S5:真探一次 DB;库不可达则 503,LB/编排据此停打流量。
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := h.svc.Ping(ctx); err != nil {
		writeRaw(w, http.StatusServiceUnavailable, map[string]string{"status": "not_ready", "reason": "db_unreachable"})
		return
	}
	writeRaw(w, http.StatusOK, map[string]string{"status": "ready", "milestone": "6-feature-complete"})
}

// writeRaw 写裸 JSON(健康检查不用业务信封,沿用里程碑 0 探针格式)。
func writeRaw(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
