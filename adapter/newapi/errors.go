package newapi

import (
	"errors"
	"fmt"
	"net/http"
)

// errClass 是上游调用结果的分类,驱动"可重试 / 不可重试 / 触发自愈"决策(10 §2.2 / §2.4)。
type errClass int

const (
	classNonRetryable errClass = iota // 4xx 语义失败:已存在 / 入参错 / 密码错
	classRetryable                    // 5xx / 超时 / 连接错:可退避重试
	classAuthExpired                  // 401 / token invalid:触发 §2.7 自愈复核
)

// 平台错误码(10 §4.1 的 5xxxx 段)。上游 4xx/5xx/超时一律翻译为这些码,绝不透传上游原始报文。
const (
	CodeOK                = 0
	CodeInternal          = 50000 // 平台未捕获异常(bug)
	CodeUpstreamCreateU   = 50201 // 上游建用户失败(CreateUser 5xx/超时)
	CodeUpstreamCreateTok = 50202 // 上游建 token 失败
	CodeUpstreamRevealKey = 50203 // 上游取 key 失败
	CodeUpstreamAuth      = 50301 // 上游鉴权失效(凭证问题;自愈失败才返此)
	CodeUpstreamBizReject = 50400 // 上游业务拒绝(如模型未配价拒发)
	CodeUpstreamDown      = 50500 // 上游不可用(连接失败/熔断打开)
	CodeUpstreamTimeout   = 50504 // 上游超时
)

// UpstreamError 是 adapter 对外暴露的统一上游错误。
// Message 已脱敏、面向人可读;绝不包含上游原始 body / 堆栈 / 敏感字段(10 §1.1 / §4.2)。
type UpstreamError struct {
	Step         string   // 失败步骤(如 "CreateUser"、"GetToken"),仅入日志,不返前端
	HTTPStatus   int      // 上游 HTTP 状态(0 = 连接/超时层错误)
	PlatformCode int      // 平台 5xxxx 码
	Message      string   // 脱敏的人类可读描述(可返前端)
	class        errClass // 内部分类
	cause        error    // 原始错误(连接/超时),仅日志
	// upstreamMsg 是上游原始 message,**仅 adapter 内部用**(如判别"用户已存在"以接管)
	// 与入日志;**绝不**经平台信封透传给前端(10 §1.1)。service 层只读 PlatformCode + Message。
	upstreamMsg string
}

func (e *UpstreamError) Error() string {
	if e.HTTPStatus > 0 {
		return fmt.Sprintf("newapi upstream %s failed: code=%d http=%d: %s", e.Step, e.PlatformCode, e.HTTPStatus, e.Message)
	}
	return fmt.Sprintf("newapi upstream %s failed: code=%d: %s", e.Step, e.PlatformCode, e.Message)
}

func (e *UpstreamError) Unwrap() error { return e.cause }

// Retryable 报告该错误是否可退避重试。供 Executor 与上层补偿逻辑判断。
func (e *UpstreamError) Retryable() bool { return e.class == classRetryable }

// AuthExpired 报告是否为上游鉴权失效(401),用以触发 §2.7 自愈复核。
func (e *UpstreamError) AuthExpired() bool { return e.class == classAuthExpired }

// classifyHTTP 把上游 HTTP 状态码映射成 errClass + 平台码。
// step 用于选对 5xxxx 码;timeout/连接错由 newTransportError 单独构造。
func classifyHTTP(step string, status int, sanitizedMsg string) *UpstreamError {
	e := &UpstreamError{Step: step, HTTPStatus: status, Message: sanitizedMsg}
	switch {
	case status == http.StatusUnauthorized:
		e.class = classAuthExpired
		e.PlatformCode = CodeUpstreamAuth
		if e.Message == "" {
			e.Message = "上游鉴权失效"
		}
	case status >= 400 && status < 500:
		// 4xx = 业务/语义失败,不可重试。
		e.class = classNonRetryable
		e.PlatformCode = platformCodeForBiz(step)
		if e.Message == "" {
			e.Message = "上游拒绝该请求"
		}
	case status >= 500:
		// 5xx = 上游内部错,可重试。
		e.class = classRetryable
		e.PlatformCode = CodeUpstreamDown
		if e.Message == "" {
			e.Message = "上游暂时不可用,请稍后重试"
		}
	default:
		// 2xx/3xx 不该走到这里;保守按不可重试。
		e.class = classNonRetryable
		e.PlatformCode = CodeInternal
		if e.Message == "" {
			e.Message = "上游返回了预期外的状态"
		}
	}
	return e
}

// platformCodeForBiz 为 4xx 业务拒绝按步骤选码;无特定步骤归入通用 50400。
func platformCodeForBiz(step string) int {
	switch step {
	case stepCreateUser:
		return CodeUpstreamCreateU
	case stepCreateToken:
		return CodeUpstreamCreateTok
	case stepRevealKey:
		return CodeUpstreamRevealKey
	default:
		return CodeUpstreamBizReject
	}
}

// newTransportError 构造连接失败/超时类错误(HTTPStatus=0),按是否超时选码。
func newTransportError(step string, timeout bool, cause error) *UpstreamError {
	e := &UpstreamError{Step: step, HTTPStatus: 0, class: classRetryable, cause: cause}
	if timeout {
		e.PlatformCode = CodeUpstreamTimeout
		e.Message = "上游响应超时,请稍后重试"
	} else {
		e.PlatformCode = CodeUpstreamDown
		e.Message = "上游连接失败,请稍后重试"
	}
	return e
}

// ErrCircuitOpen 在熔断打开时由 Executor 直接返回(不打上游)。
var ErrCircuitOpen = &UpstreamError{
	Step:         "executor",
	PlatformCode: CodeUpstreamDown,
	Message:      "上游连续失败,已临时熔断,请稍后重试",
	class:        classRetryable,
}

// asUpstreamError 尽力把任意 error 归一成 *UpstreamError(便于上层统一拿平台码)。
func asUpstreamError(step string, err error) *UpstreamError {
	if err == nil {
		return nil
	}
	var ue *UpstreamError
	if errors.As(err, &ue) {
		return ue
	}
	return &UpstreamError{
		Step:         step,
		PlatformCode: CodeInternal,
		Message:      "平台内部错误",
		class:        classNonRetryable,
		cause:        err,
	}
}
