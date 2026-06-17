// Package handler 是 HTTP 入口层(10 §4.3):解析/校验请求、组统一信封、定 HTTP 状态码。
// 不含业务逻辑(全在 service);RBAC/幂等/加密均不在此层。
package handler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"

	"github.com/nexusapi-platform/enterprise/pkg/apperr"
	"github.com/nexusapi-platform/enterprise/pkg/session"
)

// Envelope 是所有平台 API 的统一响应信封(10 §1.1)。无论成功失败都返回它。
type Envelope struct {
	Code      int    `json:"code"`
	Message   string `json:"message"`
	Data      any    `json:"data"`
	RequestID string `json:"request_id"`
}

type ctxKey int

const (
	ctxKeyRequestID ctxKey = iota
	ctxKeyClaims
)

// newRequestID 生成全链路追踪 id(10 §4.2:贯穿日志与信封)。
func newRequestID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "req_" + hex.EncodeToString(b)
}

func requestIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyRequestID).(string); ok {
		return v
	}
	return ""
}

func claimsFrom(ctx context.Context) (session.Claims, bool) {
	v, ok := ctx.Value(ctxKeyClaims).(session.Claims)
	return v, ok
}

// writeOK 写成功信封(code=0),自动带 request_id。status 通常 200/201。
func writeOK(w http.ResponseWriter, r *http.Request, status int, data any) {
	writeEnvelope(w, status, Envelope{
		Code:      apperr.CodeOK,
		Message:   "ok",
		Data:      data,
		RequestID: requestIDFrom(r.Context()),
	})
}

// writeErr 把错误归一成 *apperr.Error,按其 HTTPStatus + Code 写信封。
// 业务可预期失败走 200+code,只有协议/鉴权/系统级异常才用 4xx/5xx(10 §1.2)。
func writeErr(w http.ResponseWriter, r *http.Request, err error) {
	e := apperr.Coerce(err)
	writeEnvelope(w, e.HTTPStatus, Envelope{
		Code:      e.Code,
		Message:   e.Message,
		Data:      nil,
		RequestID: requestIDFrom(r.Context()),
	})
}

func writeEnvelope(w http.ResponseWriter, status int, env Envelope) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(env)
}

// decodeJSON 解析请求体到 v;解析失败属协议级错误,返回 400(10 §1.2;与业务校验 422 区分)。
func decodeJSON(r *http.Request, v any) error {
	if r.Body == nil {
		return apperr.BadRequest("请求体为空")
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return apperr.BadRequest("请求体格式非法")
	}
	return nil
}
