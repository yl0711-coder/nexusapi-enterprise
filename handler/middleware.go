package handler

import (
	"context"
	"net/http"
	"strings"

	"github.com/nexusapi-platform/enterprise/pkg/apperr"
	"github.com/nexusapi-platform/enterprise/pkg/session"
)

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
