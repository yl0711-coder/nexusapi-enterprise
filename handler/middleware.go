package handler

import (
	"context"
	"net/http"
	"os"
	"strings"

	"github.com/nexusapi-platform/enterprise/pkg/apperr"
	"github.com/nexusapi-platform/enterprise/pkg/session"
)

// mvpWriteAllow 是 MVP 灰度模式下放行的写操作白名单(method + 路径模式,`*` 匹配一个路径段)。
// 改动⑥-1:MVP 下只放"看 + 必要 setup(建组织/团队/层级/开通/自助建key/停用/角色/轮换)"的写;
// 所有钱/控端点(审批/配额策略/批量/调额/grants/充值/退款冲正/计费开关/折扣)不在此列 → 404。
// GET 只读一律放行(在 mvpGate 里短路),故此处只列非 GET。
var mvpWriteAllow = []string{
	"POST /api/v1/auth/login",
	"POST /api/v1/me/password", // 个人设置·自助改密(自助账号操作,非钱非控,MVP 放行)
	"PATCH /api/v1/me",         // 个人设置·改显示名(只改本人)
	"POST /api/v1/organizations",
	"PATCH /api/v1/organizations/*",
	"POST /api/v1/organizations/*/archive",
	"POST /api/v1/organizations/*/unarchive",
	"POST /api/v1/organizations/*/teams",
	"PATCH /api/v1/organizations/*/teams/*",          // F1 团队改名(按段匹配,不误命中 /teams)
	"POST /api/v1/organizations/*/teams/*/archive",   // F1 团队归档
	"POST /api/v1/organizations/*/teams/*/unarchive", // F1 撤归档
	"POST /api/v1/organizations/*/recharges",    // R5后:池子 funding 不被 observe 闸挡(observe 只挡给员工写停人额度);运营方入账,members 才能消费=被观测
	"POST /api/v1/organizations/*/escrow/refill", // 手工续充(运营方应急;自动续充 worker 亦不受 observe 闸)
	"POST /api/v1/organizations/*/tiers",
	"PUT /api/v1/tiers/*",
	"DELETE /api/v1/tiers/*",
	"POST /api/v1/tiers/*/default",
	"POST /api/v1/organizations/*/members", // 单个开通(:bulk / :bulk-status 不在白名单 → 挡)
	"PATCH /api/v1/members/*",
	"POST /api/v1/members/*/role",
	"POST /api/v1/members/*/key:rotate",
	"POST /api/v1/members/*/key:ip-whitelist",
	"POST /api/v1/members/*/tokens", // 改动③ 员工自助建 key
	"POST /api/v1/members/*/status", // 停用/恢复
	"POST /api/v1/notifications/*/read",
	"POST /api/v1/organizations/*/support-sessions", // 运营方支持(写受 CheckSupportGuard 约束)
	"POST /api/v1/support-sessions/*/close",
}

// matchMVPPattern 按"段"匹配:`*` 匹配任意一个路径段(故 members:bulk 不会命中 members)。
func matchMVPPattern(pattern, method, path string) bool {
	sp := strings.SplitN(pattern, " ", 2)
	if len(sp) != 2 || sp[0] != method {
		return false
	}
	pp := strings.Split(strings.Trim(sp[1], "/"), "/")
	rp := strings.Split(strings.Trim(path, "/"), "/")
	if len(pp) != len(rp) {
		return false
	}
	for i := range pp {
		if pp[i] != "*" && pp[i] != rp[i] {
			return false
		}
	}
	return true
}

// mvpGate 是 MVP 灰度部署级封锁(改动⑥-1):MVP_MODE 关 → 放行;开 → GET 只读放行,
// /api/v1/* 的写操作只放行 mvpWriteAllow 白名单,其余一律 404(藏存在性,不暴露端点存在)。
// 真正的拦截在后端这一层,前端藏菜单只是体验。鉴权在 mux 内 requireAuth,本闸在其外,故未鉴权的写也直接 404。
func (h *Handler) mvpGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !h.mvpMode || r.Method == http.MethodGet || r.Method == http.MethodHead ||
			!strings.HasPrefix(r.URL.Path, "/api/v1/") {
			next.ServeHTTP(w, r)
			return
		}
		for _, p := range mvpWriteAllow {
			if matchMVPPattern(p, r.Method, r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
		}
		writeErr(w, r, apperr.NotFound("")) // 非白名单写:404
	})
}

// maxRequestBodyBytes 是单请求体上限(P2 健壮性加固):外部面 + 2G 无 swap 节点,
// 超大 body 在字段校验前就要读进内存 → 内存压力/被打崩面。1MB 远大于本平台任何合法 JSON
// (批量导入也就几十 KB),封顶后超限由 decodeJSON 收敛成 400(http.MaxBytesReader 触发解码错)。
const maxRequestBodyBytes = 1 << 20 // 1 MiB

// maxBodyBytes 给每个请求体套 http.MaxBytesReader 上限,防超大 body 占内存。
func maxBodyBytes(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}

// securityHeaders 给所有响应加基础安全头(GZ-05 修复D):防点击劫持(X-Frame-Options)、
// MIME 嗅探放大 XSS(X-Content-Type-Options)、敏感路径经 Referer 外泄(Referrer-Policy)。
// 严格 CSP 暂不上:当前页面有大量内联 onclick/script,上 script-src 'self' 会打死页面,
// 待内联事件迁移(GZ-05 §五修复A 第4步)完成后再上,列 G1。
// HSTS 仅在对外走 HTTPS 后开启:由 NEXUS_ENABLE_HSTS=true 控制,默认关(灰度纯内网 HTTP 不发 HSTS)。
// CORS 不动:保持当前「无 CORS、同源」(前端由 web.FS 同源内嵌),不要为联调加 Access-Control-Allow-Origin: *。
func securityHeaders(next http.Handler) http.Handler {
	hsts := os.Getenv("NEXUS_ENABLE_HSTS") == "true"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		if hsts {
			h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

// withRequestID 为每个请求生成/透传 request_id(10 §4.2),放进 context 并回写响应头。
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := r.Header.Get("X-Request-Id")
		if rid == "" {
			rid = newRequestID()
		}
		w.Header().Set("X-Request-Id", rid)
		ctx := context.WithValue(r.Context(), ctxKeyRequestID, rid)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// recoverPanic 兜住 handler panic,返回 50000(绝不把堆栈泄露给前端)。
func (h *Handler) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				h.log.Error("handler panic", "panic", v, "path", r.URL.Path, "request_id", requestIDFrom(r.Context()))
				writeErr(w, r, apperr.Internal(""))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// requireAuth 校验 Authorization: Bearer <token>,解出会话 claims 放进 context。
// 无 token / 格式错 → 401(10001);过期 → 401(10003)。
func (h *Handler) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(auth, prefix) {
			writeErr(w, r, apperr.Unauthenticated("缺少会话凭证"))
			return
		}
		claims, err := h.signer.Verify(strings.TrimSpace(auth[len(prefix):]))
		switch err {
		case nil:
			// ok
		case session.ErrExpired:
			writeErr(w, r, apperr.SessionExpired(""))
			return
		default:
			writeErr(w, r, apperr.Unauthenticated("会话凭证非法"))
			return
		}
		// 支持态后端闸(08 §2.2):只读态拒所有写、协助态动钱/读 key 红线挡。真正的闸在后端。
		if err := h.svc.CheckSupportGuard(r.Context(), claims, r.Method, r.URL.Path); err != nil {
			ctx := context.WithValue(r.Context(), ctxKeyClaims, claims)
			writeErr(w, r.WithContext(ctx), err)
			return
		}
		ctx := context.WithValue(r.Context(), ctxKeyClaims, claims)
		next(w, r.WithContext(ctx))
	}
}
