package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/nexusapi-platform/enterprise/pkg/session"
	"github.com/nexusapi-platform/enterprise/service"
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
	mux.HandleFunc("GET /api/v1/members/{id}", h.requireAuth(h.handleGetMember))
	mux.HandleFunc("POST /api/v1/members/{id}/key:rotate", h.requireAuth(h.handleRotateKey))

	// 中间件链:request_id → recover → mux。
	return withRequestID(h.recoverPanic(mux))
}

func (h *Handler) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeRaw(w, http.StatusOK, map[string]string{"status": "ok", "version": h.version})
}

func (h *Handler) handleReadyz(w http.ResponseWriter, r *http.Request) {
	writeRaw(w, http.StatusOK, map[string]string{"status": "ready", "milestone": "1-identity-org-rbac-openmember"})
}

// writeRaw 写裸 JSON(健康检查不用业务信封,沿用里程碑 0 探针格式)。
func writeRaw(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
