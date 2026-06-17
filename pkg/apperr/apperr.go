// Package apperr 是平台统一错误类型与错误码体系(10 §4.1 / §1.2)。
//
// 平台 code 分段,0=成功;前端按首位段决定行为(跳登录/提示/重试)。
// 错误既携带平台业务 code(进响应信封),也携带建议的 HTTP 状态码(10 §1.2:
// 业务可预期失败走 200+code,只有协议/鉴权/系统级异常才用 4xx/5xx)。
//
// 本包是最底层横切,不依赖 service/handler/adapter;adapter 的上游错误
// (newapi.UpstreamError)在 service 层用 FromUpstream 归一成 *Error。
package apperr

import (
	"errors"
	"fmt"
	"net/http"
)

// 错误码分段(10 §4.1)。
const (
	CodeOK = 0

	// 1xxxx 鉴权 / 会话 / 权限(跳登录/提权,不可重试)。
	CodeUnauthenticated = 10001 // 未登录
	CodeSessionExpired  = 10003 // 会话过期
	CodeForbidden       = 10401 // 无权限(RBAC 拒)
	CodeReadonlyBlocked = 10402 // 支持态只读拦截
	CodeMoneyRedline    = 10403 // 资金红线挡

	// 2xxxx 业务校验 / 状态冲突。
	CodeInvalidParam     = 20001 // 参数非法
	CodeQuotaInsufficent = 20101 // 额度不足
	CodeApprovalHandled  = 20902 // 审批已被处理
	CodeIdempotency      = 20903 // 幂等冲突
	CodeOptimisticLock   = 20904 // 乐观锁冲突

	// 3xxxx 平台资源 / 数据。
	CodeNotFound    = 30001 // 资源不存在
	CodeOutOfScope  = 30002 // 不在可见范围(跨 org,对外按 404)
	CodeConflictDup = 30003 // 资源已存在(重名/邮箱冲突)

	// 5xxxx 上游 new-api / 平台系统(adapter 已定义同段码,这里复列平台自身用的)。
	CodeInternal = 50000 // 平台未捕获异常(bug)
)

// Error 是平台统一错误。Message 面向终端用户可直接展示(脱敏、无堆栈)。
type Error struct {
	Code       int    // 平台业务码(进信封)
	HTTPStatus int    // 建议 HTTP 状态码(10 §1.2)
	Message    string // 给人看的脱敏描述
	cause      error  // 原始错误,仅日志,不进信封
}

func (e *Error) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("apperr code=%d http=%d: %s (cause: %v)", e.Code, e.HTTPStatus, e.Message, e.cause)
	}
	return fmt.Sprintf("apperr code=%d http=%d: %s", e.Code, e.HTTPStatus, e.Message)
}

func (e *Error) Unwrap() error { return e.cause }

// WithCause 附加原始错误(仅日志,不进信封),返回自身便于链式。
func (e *Error) WithCause(err error) *Error {
	e.cause = err
	return e
}

// New 构造一个平台错误。
func New(code, httpStatus int, message string) *Error {
	return &Error{Code: code, HTTPStatus: httpStatus, Message: message}
}

// 常用构造器(HTTP 码按 10 §1.2 约定)。
func Unauthenticated(msg string) *Error {
	return New(CodeUnauthenticated, http.StatusUnauthorized, orDefault(msg, "请先登录"))
}
func SessionExpired(msg string) *Error {
	return New(CodeSessionExpired, http.StatusUnauthorized, orDefault(msg, "会话已过期,请重新登录"))
}
func Forbidden(msg string) *Error {
	return New(CodeForbidden, http.StatusForbidden, orDefault(msg, "无权限执行该操作"))
}
func ReadonlyBlocked(msg string) *Error {
	return New(CodeReadonlyBlocked, http.StatusForbidden, orDefault(msg, "只读支持态下不可写"))
}
func MoneyRedline(msg string) *Error {
	return New(CodeMoneyRedline, http.StatusForbidden, orDefault(msg, "涉及资金的操作触发红线,已拦截"))
}
func InvalidParam(msg string) *Error {
	return New(CodeInvalidParam, http.StatusBadRequest, orDefault(msg, "请求参数非法"))
}

// NotFound 用于资源不存在;跨 org 不可见也走这里(不暴露存在性,10 §1.2)。
func NotFound(msg string) *Error {
	return New(CodeNotFound, http.StatusNotFound, orDefault(msg, "资源不存在"))
}

// Conflict 用于重名/邮箱已存在等(US-01 失败分支 409)。
func Conflict(msg string) *Error {
	return New(CodeConflictDup, http.StatusConflict, orDefault(msg, "资源已存在,存在冲突"))
}

// OptimisticLock 用于乐观锁版本冲突(409)。
func OptimisticLock(msg string) *Error {
	return New(CodeOptimisticLock, http.StatusConflict, orDefault(msg, "并发冲突,请重试"))
}

// IdempotencyInProgress 用于幂等键命中进行中(409,10 §1.6)。
func IdempotencyInProgress(msg string) *Error {
	return New(CodeIdempotency, http.StatusConflict, orDefault(msg, "请求处理中,请勿重复提交"))
}

// Internal 用于平台自身未捕获异常(bug);信封 code=50000,HTTP 500。
func Internal(msg string) *Error {
	return New(CodeInternal, http.StatusInternalServerError, orDefault(msg, "平台内部错误"))
}

// upstreamView 是 adapter 上游错误对外暴露的最小视图,避免 apperr 依赖 adapter 包。
// newapi.UpstreamError 通过其导出字段天然满足(在 service 层用 errors.As 取后适配)。
type upstreamView interface {
	error
}

// FromUpstream 把已知平台码 + 脱敏 message 的上游错误归一成 *Error。
// code 取上游 PlatformCode(5xxxx),message 取上游脱敏 Message;HTTP 码按段映射:
// 50500→502、50504→504、50301→200(自愈失败)、其余 5xxxx 业务可恢复→200。
// 调用方(service)负责从 *newapi.UpstreamError 提取 code/message 后传入。
func FromUpstream(code int, message string, cause error) *Error {
	httpStatus := http.StatusOK // 业务可恢复类默认 200+code(10 §1.2 / §4.1)
	switch code {
	case 50500: // 上游不可用
		httpStatus = http.StatusBadGateway
	case 50503:
		httpStatus = http.StatusServiceUnavailable
	case 50504: // 上游超时
		httpStatus = http.StatusGatewayTimeout
	case 50301: // 上游鉴权失效(自愈失败):走 5xx,与其余上游故障一致,便于按 HTTP 状态告警(R2-M9)
		httpStatus = http.StatusBadGateway
	case CodeInternal:
		httpStatus = http.StatusInternalServerError
	}
	if message == "" {
		message = "上游服务异常,请稍后重试"
	}
	return &Error{Code: code, HTTPStatus: httpStatus, Message: message, cause: cause}
}

// As 提取链上的 *Error;没有则返回 nil, false。
func As(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// Coerce 把任意 error 归一成 *Error:已是 *Error 原样返回,否则包成 50000 内部错误
// (原始错误进 cause 仅日志,Message 用通用脱敏文案,绝不透传内部细节给前端)。
func Coerce(err error) *Error {
	if err == nil {
		return nil
	}
	if e, ok := As(err); ok {
		return e
	}
	return Internal("").WithCause(err)
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

var _ upstreamView = (*Error)(nil)
