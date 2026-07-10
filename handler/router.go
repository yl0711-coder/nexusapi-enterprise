package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/nexusapi-platform/enterprise/pkg/apperr"
	"github.com/nexusapi-platform/enterprise/pkg/session"
	"github.com/nexusapi-platform/enterprise/service"
	"github.com/nexusapi-platform/enterprise/web"
)

// Handler 持有依赖,挂载所有平台 REST 路由(10 §1.7,前缀 /api/v1)。
type Handler struct {
	svc            *service.Service
	signer         *session.Signer
	log            *slog.Logger
	version        string
	authLim        *attemptLimiter  // A2:登录/改密账号级失败退避(应用层纵深)
	critLim        *criticalLimiter // B3:敏感操作限频(镜像 new-api CriticalRateLimit;揭示明文 key 等)
	gatewayBaseURL string          // F3(28):对客户展示的 API 接入地址(纯展示,可选;未配置则 mykey 不显示接入示例)
}

// New 构造 Handler。
func New(svc *service.Service, signer *session.Signer, log *slog.Logger, version string) *Handler {
	if log == nil {
		log = slog.Default()
	}
	markStarted(time.Now().Unix()) // /metrics uptime 起点
	// NEXUS_GATEWAY_BASE_URL 在此直读(非注入):纯展示字段,不影响任何逻辑;避免为它改 New 签名波及调用方。
	return &Handler{svc: svc, signer: signer, log: log, version: version,
		authLim: newAttemptLimiter(), critLim: newCriticalLimiter(), gatewayBaseURL: os.Getenv("NEXUS_GATEWAY_BASE_URL")}
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
	mux.HandleFunc("GET /api/v1/branding", h.handleBranding) // 公开品牌信息(免鉴权,登录页用;42号样式-3)
	mux.HandleFunc("GET /api/v1/me", h.requireAuth(h.handleMe))
	mux.HandleFunc("POST /api/v1/me/password", h.requireAuth(h.handleChangePassword)) // 个人设置·自助改密
	mux.HandleFunc("PATCH /api/v1/me", h.requireAuth(h.handleUpdateMe))               // 个人设置·改显示名

	// 组织(运营方)。
	mux.HandleFunc("GET /api/v1/organizations", h.requireAuth(h.handleListOrgs))
	mux.HandleFunc("GET /api/v1/newapi-logs", h.requireAuth(h.handleAllNewapiLogs)) // 运营方全局 new-api 日志镜像
	mux.HandleFunc("GET /api/v1/members", h.requireAuth(h.handleListAllMembers))    // 运营方全局员工/Key 管理
	mux.HandleFunc("POST /api/v1/organizations", h.requireAuth(h.handleCreateOrg))
	mux.HandleFunc("GET /api/v1/organizations/{id}", h.requireAuth(h.handleGetOrg))
	mux.HandleFunc("PATCH /api/v1/organizations/{id}", h.requireAuth(h.handleUpdateOrg))
	mux.HandleFunc("POST /api/v1/organizations/{id}/archive", h.requireAuth(h.handleArchiveOrg))                // T12 归档
	mux.HandleFunc("POST /api/v1/organizations/{id}/unarchive", h.requireAuth(h.handleUnarchiveOrg))            // T12 取消归档
	mux.HandleFunc("GET /api/v1/organizations/{id}/newapi-logs", h.requireAuth(h.handleOrgNewapiLogs))          // new-api 完整日志镜像(运营排障)
	mux.HandleFunc("GET /api/v1/organizations/{id}/token-mappings", h.requireAuth(h.handleOrgTokenMappings))    // 员工 ↔ new-api token 映射(运营排障)
	mux.HandleFunc("POST /api/v1/organizations/{id}/hard-stop", h.requireAuth(h.handleHardStop(true)))          // 运维硬停(禁用 org 用户)
	mux.HandleFunc("POST /api/v1/organizations/{id}/hard-stop-release", h.requireAuth(h.handleHardStop(false))) // 解除硬停

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
	mux.HandleFunc("POST /api/v1/tiers/{id}/grants", h.requireAuth(h.handleCreateTierGrant))   // 档位授权(33 §12-④)
	mux.HandleFunc("DELETE /api/v1/tiers/{id}/grants", h.requireAuth(h.handleDeleteTierGrant)) // 撤销授权 body {grant_id}

	// 成员(架构B:开通成员=建平台账号+成员服务账号 saga,不再铸 key;成员维度端点统一过 memberSelfGuard)。
	// A 版「org 凭证建 token」路由已退役(key:rotate / key:reveal / key:ip-whitelist / members/{id}/tokens /
	// usable-groups,33 §5):成员令牌全走 /me/tokens*(下方)。
	mux.HandleFunc("GET /api/v1/organizations/{id}/members", h.requireAuth(h.handleListMembers))
	mux.HandleFunc("POST /api/v1/organizations/{id}/members", h.requireAuth(h.handleOpenMember))
	mux.HandleFunc("POST /api/v1/organizations/{id}/members:bulk", h.requireAuth(h.handleBulkOpen))
	mux.HandleFunc("POST /api/v1/organizations/{id}/members:bulk-status", h.requireAuth(h.handleBulkStatus))
	mux.HandleFunc("GET /api/v1/members/{id}", h.requireAuth(h.memberSelfGuard(h.handleGetMember)))
	mux.HandleFunc("PATCH /api/v1/members/{id}", h.requireAuth(h.memberSelfGuard(h.handleUpdateMember)))
	mux.HandleFunc("POST /api/v1/members/{id}/role", h.requireAuth(h.memberSelfGuard(h.handleAssignRole)))

	// 成员自助令牌(架构B,33 §3.5 member 组:member-only,后台用成员服务账号凭证代调 new-api)。
	mux.HandleFunc("GET /api/v1/me/tokens", h.requireAuth(h.handleListMyTokens))
	mux.HandleFunc("POST /api/v1/me/tokens", h.requireAuth(h.handleCreateMyToken))
	mux.HandleFunc("PATCH /api/v1/me/tokens/{id}", h.requireAuth(h.handleUpdateMyToken))
	mux.HandleFunc("DELETE /api/v1/me/tokens/{id}", h.requireAuth(h.handleDeleteMyToken))
	mux.HandleFunc("POST /api/v1/me/tokens/{id}/key:reveal", h.requireAuth(h.handleRevealMyTokenKey))

	// 生命周期(架构B):追加划账(Transfer,红线) / 停用恢复 / 离职。
	mux.HandleFunc("POST /api/v1/members/{id}/quota:grant", h.requireAuth(h.memberSelfGuard(h.handleGrantQuota))) // 架构B 追加划账(红线)
	mux.HandleFunc("POST /api/v1/members/{id}/password:reset", h.requireAuth(h.memberSelfGuard(h.handleResetMemberPassword))) // C22 重置成员登录密码
	mux.HandleFunc("POST /api/v1/members/{id}/status", h.requireAuth(h.memberSelfGuard(h.handleSetMemberStatus)))
	mux.HandleFunc("POST /api/v1/members/{id}/offboard", h.requireAuth(h.memberSelfGuard(h.handleOffboardMember))) // 离职(disable→静默→退额)
	mux.HandleFunc("POST /api/v1/members/{id}/restore", h.requireAuth(h.memberSelfGuard(h.handleRestoreMember)))
	mux.HandleFunc("POST /api/v1/members/{id}/provision:retry", h.requireAuth(h.memberSelfGuard(h.handleRetryProvision))) // 42号 P1:重试开通   // 恢复(enable+如新建重新分配,{tier_id})
	mux.HandleFunc("GET /api/v1/organizations/{id}/members/offboarded", h.requireAuth(h.handleListOffboarded))     // 离职列表

	// 余额/账本(架构B BE③,只读):读求和余额 + 分配账本三级可见(31-ADR §15)。
	mux.HandleFunc("GET /api/v1/organizations/{id}/balance", h.requireAuth(h.handleOrgBalance)) // 读求和:金库+Σ成员(取代 company_balance 视图)
	mux.HandleFunc("GET /api/v1/organizations/{id}/ledger", h.requireAuth(h.handleOrgLedger))   // 本组织划账流水(O/A)

	// ── 33 §3.5 契约 /orgs/{id}/*(org_admin 面)——39号体检阻断-1:契约用 /orgs、实现只有
	// /organizations 致 org_admin/成员核心页整页失败。同 handler 双注册:/organizations/*(运营方面,
	// FE 34 处在用)原样保留;/orgs/*(org_admin 面,FE 25 处按契约调)按契约补齐。RBAC 在 service 层
	// 按 claims 判,与入口路径无关,双注册不放宽任何权限。
	mux.HandleFunc("GET /api/v1/orgs/{id}/members", h.requireAuth(h.handleListMembers))
	mux.HandleFunc("POST /api/v1/orgs/{id}/members", h.requireAuth(h.handleOpenMember))
	mux.HandleFunc("GET /api/v1/orgs/{id}/tiers", h.requireAuth(h.handleListTiers))
	mux.HandleFunc("POST /api/v1/orgs/{id}/tiers", h.requireAuth(h.handleCreateTier))
	mux.HandleFunc("GET /api/v1/orgs/{id}/balance", h.requireAuth(h.handleOrgBalance))
	mux.HandleFunc("GET /api/v1/orgs/{id}/ledger", h.requireAuth(h.handleOrgLedger))
	mux.HandleFunc("GET /api/v1/orgs/{id}/marketplace", h.requireAuth(h.handleOrgMarketplace)) // 模型广场(分组+模型+倍率)

	// 成员模型广场 + 平台设置(33 §3.5;39号阻断-1 两组从未落地的端点)。
	mux.HandleFunc("GET /api/v1/me/marketplace", h.requireAuth(h.handleMyMarketplace))
	mux.HandleFunc("GET /api/v1/platform-settings", h.requireAuth(h.handleGetPlatformSettings))
	mux.HandleFunc("PUT /api/v1/platform-settings", h.requireAuth(h.handlePutPlatformSettings)) // money_freeze 急停在此(operator)
	mux.HandleFunc("GET /api/v1/ledger", h.requireAuth(h.handleAllLedger))                      // 全平台流水(仅运营方)
	mux.HandleFunc("GET /api/v1/me/balance", h.requireAuth(h.handleMyBalance))                  // 成员本人额度/已用/剩余
	mux.HandleFunc("GET /api/v1/me/ledger", h.requireAuth(h.handleMyLedger))                    // 成员到账记录(仅 to_user=本人)
	// 计费(里程碑3a 遗留):入账 / 申请充值(fundingEnabled 恒关时 service 层 404)。
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

	// 通知(US-13)+ 成员自助。
	mux.HandleFunc("GET /api/v1/notifications", h.requireAuth(h.handleListNotifications))
	mux.HandleFunc("POST /api/v1/notifications/{id}/read", h.requireAuth(h.handleMarkNotificationRead))

	// 用量看板(里程碑5)。
	mux.HandleFunc("GET /api/v1/organizations/{id}/usage", h.requireAuth(h.handleOrgUsage))
	mux.HandleFunc("GET /api/v1/organizations/{id}/usage/timeseries", h.requireAuth(h.handleOrgUsageTimeSeries)) // M2 折线图(day/week/month)
	mux.HandleFunc("GET /api/v1/organizations/{id}/usage/detail", h.requireAuth(h.handleOrgUsageDetail))         // M2 下钻逐条明细
	mux.HandleFunc("GET /api/v1/organizations/{id}/budget-ref", h.requireAuth(h.handleBudgetRef))                // #4 额度参考条(已用$/预付$)
	mux.HandleFunc("GET /api/v1/members/{id}/usage", h.requireAuth(h.memberSelfGuard(h.handleMemberUsage)))
	mux.HandleFunc("GET /api/v1/members/{id}/usage/timeseries", h.requireAuth(h.memberSelfGuard(h.handleMemberUsageTimeSeries))) // M2 成员折线图
	mux.HandleFunc("GET /api/v1/members/{id}/usage/detail", h.requireAuth(h.memberSelfGuard(h.handleMemberUsageDetail)))         // M2 成员下钻逐条明细
	mux.HandleFunc("GET /api/v1/organizations/{id}/usage/export", h.requireAuth(h.handleUsageExport))
	mux.HandleFunc("GET /api/v1/service-status", h.requireAuth(h.handleServiceStatus))

	// 运营方三层支持(里程碑5,08 §2.2/§3.2)。
	mux.HandleFunc("POST /api/v1/organizations/{id}/support-sessions", h.requireAuth(h.handleOpenSupport))
	mux.HandleFunc("GET /api/v1/support-sessions/{id}", h.requireAuth(h.handleGetSupport))
	mux.HandleFunc("POST /api/v1/support-sessions/{id}/close", h.requireAuth(h.handleCloseSupport))

	// API 兜底:未注册的 /api/* 路径(含已退役端点)统一 404,不落入下面 SPA 的静态文件语义。
	// 若无此兜底,"GET /"(SPA 子树)会让未注册写路径命中"同路径存在 GET"而返 405——退役端点应 404
	// 藏存在性(33 §3.5 端点白名单;原 mvpGate 的 404 语义随闸拆除后由此路由层兜底承接)。
	// 按 method 逐个注册(不能裸注册 "/api/":全 method+子树会与 "GET /" 互不更精确,ServeMux 判冲突 panic;
	// 分 method 后同 method 间路径更专者赢、异 method 不相交,合法)。已注册 pattern 更精确、优先,不受影响。
	apiNotFound := func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, r, apperr.NotFound(""))
	}
	// 不含 HEAD:ServeMux 的 GET pattern 隐式匹配 HEAD,单独注册 "HEAD /api/" 会与既有 "GET /api/v1/…" 判冲突。
	for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		mux.HandleFunc(m+" /api/", apiNotFound)
	}

	// 前端 SPA(catch-all,最不具体,/api/v1 与 /healthz 等更具体的先匹配):
	// /api/v1/* 之外的路径走内嵌静态前端;/ 返回 index.html。
	mux.Handle("GET /", http.FileServerFS(web.FS))

	// 中间件链:request_id → 安全头 → 访问日志/指标 → recover → body 上限 → mux
	// (GZ-05 安全头挂最外层;P2 body 上限在 recover 内、mux 外:超限读 body 时报错,recover 兜底)。
	return withRequestID(securityHeaders(h.accessLog(h.recoverPanic(maxBodyBytes(mux)))))
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
