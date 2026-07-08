package handler

import (
	"context"
	"net/http"
	"os"
	"strings"

	"github.com/nexusapi-platform/enterprise/pkg/apperr"
	"github.com/nexusapi-platform/enterprise/pkg/session"
)


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
		// A3 会话有效性回查:非支持态 token 校验成员 status==active 且 session_epoch 匹配(禁用/降级/改密/硬停即刻失效)。
		// 支持态 token 内部跳过(交下面的 CheckSupportGuard)。
		if err := h.svc.ValidateSession(r.Context(), claims); err != nil {
			writeErr(w, r, err)
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
