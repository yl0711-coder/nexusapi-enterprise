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
	mvpMode bool // 改动⑥:MVP 灰度封锁(mvpGate 路由白名单 + /me 透出 mvp_mode 给前端藏菜单)
	authLim *attemptLimiter // A2:登录/改密账号级失败退避(应用层纵深)
}

// New 构造 Handler。
func New(svc *service.Service, signer *session.Signer, log *slog.Logger, version string, mvpMode bool) *Handler {
	if log == nil {
		log = slog.Default()
	}
	markStarted(time.Now().Unix()) // /metrics uptime 起点
	return &Handler{svc: svc, signer: signer, log: log, version: version, mvpMode: mvpMode, authLim: newAttemptLimiter()}
}

// Routes 返回挂好中间件的根 http.Handler。
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()

	// 健康检查(无需鉴权;部署探针,沿用里程碑 0 契约)。
	mux.HandleFunc("GET /healthz", h.handleHealthz)
	mux.HandleFunc("GET /readyz", h.handleReadyz)
	mux.HandleFunc("GET /metrics", h.handleMetrics) // Prometheus 抓取(R2-运维)

	// 认证。
	mux.HandleFunc("POST /api/v1/auth/login", h.handleLogin)
	mux.HandleFunc("GET /api/v1/me", h.requireAuth(h.handleMe))
	mux.HandleFunc("POST /api/v1/me/password", h.requireAuth(h.handleChangePassword)) // 个人设置·自助改密
	mux.HandleFunc("PATCH /api/v1/me", h.requireAuth(h.handleUpdateMe))               // 个人设置·改显示名

	// 组织(运营方)。
	mux.HandleFunc("GET /api/v1/organizations", h.requireAuth(h.handleListOrgs))
	mux.HandleFunc("POST /api/v1/organizations", h.requireAuth(h.handleCreateOrg))
	mux.HandleFunc("GET /api/v1/organizations/{id}", h.requireAuth(h.handleGetOrg))
	mux.HandleFunc("PATCH /api/v1/organizations/{id}", h.requireAuth(h.handleUpdateOrg))
	mux.HandleFunc("POST /api/v1/organizations/{id}/archive", h.requireAuth(h.handleArchiveOrg))           // T12 归档
	mux.HandleFunc("POST /api/v1/organizations/{id}/unarchive", h.requireAuth(h.handleUnarchiveOrg))       // T12 取消归档
	mux.HandleFunc("POST /api/v1/organizations/{id}/import-tokens", h.requireAuth(h.handleReimportTokens))      // 门B 重新导入(运营方,幂等)
	mux.HandleFunc("GET /api/v1/organizations/{id}/backfill", h.requireAuth(h.handleGetBackfill))                // 历史回填状态(24-§9)
	mux.HandleFunc("POST /api/v1/organizations/{id}/backfill/requeue", h.requireAuth(h.handleRequeueBackfill))   // 重新回填(运营方,幂等)
	mux.HandleFunc("POST /api/v1/organizations/{id}/hard-stop", h.requireAuth(h.handleHardStop(true)))          // 运维硬停(禁用 org 用户)
	mux.HandleFunc("POST /api/v1/organizations/{id}/hard-stop-release", h.requireAuth(h.handleHardStop(false))) // 解除硬停
	mux.HandleFunc("GET /api/v1/organizations/{id}/approval-rules", h.requireAuth(h.handleGetApprovalRules))
	mux.HandleFunc("PUT /api/v1/organizations/{id}/approval-rules", h.requireAuth(h.handleSetApprovalRules))
	mux.HandleFunc("GET /api/v1/organizations/{id}/quota-policies", h.requireAuth(h.handleListPolicies))
	mux.HandleFunc("PUT /api/v1/organizations/{id}/quota-policies", h.requireAuth(h.handleSetPolicy))

	// 团队。
	mux.HandleFunc("GET /api/v1/organizations/{id}/teams", h.requireAuth(h.handleListTeams))
	mux.HandleFunc("POST /api/v1/organizations/{id}/teams", h.requireAuth(h.handleCreateTeam))
	mux.HandleFunc("GET /api/v1/organizations/{id}/teams/{tid}", h.requireAuth(h.handleGetTeam))                  // F1 详情
	mux.HandleFunc("PATCH /api/v1/organizations/{id}/teams/{tid}", h.requireAuth(h.handleUpdateTeam))             // F1 改名
	mux.HandleFunc("POST /api/v1/organizations/{id}/teams/{tid}/archive", h.requireAuth(h.handleArchiveTeam))     // F1 归档
	mux.HandleFunc("POST /api/v1/organizations/{id}/teams/{tid}/unarchive", h.requireAuth(h.handleUnarchiveTeam)) // F1 撤归档
	mux.HandleFunc("GET /api/v1/organizations/{id}/teams/{tid}/usage", h.requireAuth(h.handleTeamUsage))          // F3 团队下钻用量

	// 层级。
	mux.HandleFunc("GET /api/v1/organizations/{id}/tiers", h.requireAuth(h.handleListTiers))
	mux.HandleFunc("POST /api/v1/organizations/{id}/tiers", h.requireAuth(h.handleCreateTier))
	mux.HandleFunc("PUT /api/v1/tiers/{id}", h.requireAuth(h.handleUpdateTier))
	mux.HandleFunc("DELETE /api/v1/tiers/{id}", h.requireAuth(h.handleDeleteTier))
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
	// 改动③:员工自助建 key(选模型分组)+ 列本企业可用模型分组(分组选择器)。MVP 白名单已含。
	mux.HandleFunc("POST /api/v1/members/{id}/tokens", h.requireAuth(h.handleCreateMemberToken))
	mux.HandleFunc("GET /api/v1/members/{id}/usable-groups", h.requireAuth(h.handleMemberUsableGroups))

	// 额度执行(里程碑2):调额 / 临时权限 / 停用恢复。
	mux.HandleFunc("POST /api/v1/members/{id}/quota:adjust", h.requireAuth(h.handleAdjustQuota))
	mux.HandleFunc("POST /api/v1/members/{id}/grants", h.requireAuth(h.handleSetGrant))
	mux.HandleFunc("GET /api/v1/members/{id}/grants", h.requireAuth(h.handleListGrants))
	mux.HandleFunc("DELETE /api/v1/grants/{id}", h.requireAuth(h.handleRevokeGrant))
	mux.HandleFunc("POST /api/v1/members/{id}/status", h.requireAuth(h.handleSetMemberStatus))
	mux.HandleFunc("POST /api/v1/members/{id}/offboard", h.requireAuth(h.handleOffboardMember))                    // 离职(删token+软删)
	mux.HandleFunc("POST /api/v1/members/{id}/restore", h.requireAuth(h.handleRestoreMember))                      // 恢复入职
	mux.HandleFunc("GET /api/v1/organizations/{id}/members/offboarded", h.requireAuth(h.handleListOffboarded))     // 离职列表

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
	mux.HandleFunc("POST /api/v1/pricing/reconcile", h.requireAuth(h.handleReconcileDiscounts)) // 手动折扣对账(运营方,G)
	mux.HandleFunc("GET /api/v1/pricing/groups", h.requireAuth(h.handleListBillingGroups))      // 计费分组选择器(T17-6)
	mux.HandleFunc("PUT /api/v1/organizations/{id}/default-token-group", h.requireAuth(h.handleSetOrgDefaultTokenGroup))

	// 申请-审批(里程碑4,US-06)+ 通知(US-13)+ 成员自助。
	mux.HandleFunc("POST /api/v1/approvals", h.requireAuth(h.handleSubmitApproval))
	mux.HandleFunc("GET /api/v1/organizations/{id}/approvals", h.requireAuth(h.handleListApprovals))
	mux.HandleFunc("POST /api/v1/approvals/{id}/decide", h.requireAuth(h.handleDecideApproval))
	mux.HandleFunc("GET /api/v1/notifications", h.requireAuth(h.handleListNotifications))
	mux.HandleFunc("POST /api/v1/notifications/{id}/read", h.requireAuth(h.handleMarkNotificationRead))

	// 用量看板(里程碑5)。
	mux.HandleFunc("GET /api/v1/organizations/{id}/usage", h.requireAuth(h.handleOrgUsage))
	mux.HandleFunc("GET /api/v1/organizations/{id}/usage/timeseries", h.requireAuth(h.handleOrgUsageTimeSeries)) // M2 折线图(day/week/month)
	mux.HandleFunc("GET /api/v1/organizations/{id}/usage/detail", h.requireAuth(h.handleOrgUsageDetail))         // M2 下钻逐条明细
	mux.HandleFunc("GET /api/v1/organizations/{id}/budget-ref", h.requireAuth(h.handleBudgetRef))                // #4 额度参考条(已用$/预付$)
	mux.HandleFunc("GET /api/v1/organizations/{id}/escrow-balance", h.requireAuth(h.handleEscrowBalance))        // R3 读穿余额(窗口+托管)
	mux.HandleFunc("POST /api/v1/organizations/{id}/escrow/refill", h.requireAuth(h.handleEscrowRefill))         // R3 手工续充
	mux.HandleFunc("GET /api/v1/members/{id}/usage", h.requireAuth(h.handleMemberUsage))
	mux.HandleFunc("GET /api/v1/members/{id}/usage/timeseries", h.requireAuth(h.handleMemberUsageTimeSeries)) // M2 成员折线图
	mux.HandleFunc("GET /api/v1/members/{id}/usage/detail", h.requireAuth(h.handleMemberUsageDetail))         // M2 成员下钻逐条明细
	mux.HandleFunc("GET /api/v1/organizations/{id}/usage/export", h.requireAuth(h.handleUsageExport))
	mux.HandleFunc("GET /api/v1/service-status", h.requireAuth(h.handleServiceStatus))

	// 运营方三层支持(里程碑5,08 §2.2/§3.2)。
	mux.HandleFunc("POST /api/v1/organizations/{id}/support-sessions", h.requireAuth(h.handleOpenSupport))
	mux.HandleFunc("GET /api/v1/support-sessions/{id}", h.requireAuth(h.handleGetSupport))
	mux.HandleFunc("POST /api/v1/support-sessions/{id}/close", h.requireAuth(h.handleCloseSupport))

	// 前端 SPA(catch-all,最不具体,/api/v1 与 /healthz 等更具体的先匹配):
	// /api/v1/* 之外的路径走内嵌静态前端;/ 返回 index.html。
	mux.Handle("GET /", http.FileServerFS(web.FS))

	// 中间件链:request_id → 安全头 → 访问日志/指标 → recover → body 上限 → MVP 封锁闸 → mux
	// (GZ-05 安全头挂最外层;P2 body 上限在 recover 内、闸/mux 外:超限读 body 时报错,recover 兜底;
	// 改动⑥ mvpGate 紧贴 mux:非白名单写操作 404)。
	return withRequestID(securityHeaders(h.accessLog(h.recoverPanic(maxBodyBytes(h.mvpGate(mux))))))
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
