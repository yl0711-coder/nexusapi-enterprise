package service

import (
	"errors"

	"github.com/nexusapi-platform/enterprise/adapter/newapi"
	"github.com/nexusapi-platform/enterprise/pkg/apperr"
)

// mapUpstream 把 adapter 的上游错误(newapi.UpstreamError)归一成平台 *apperr.Error:
// 取其已脱敏的 PlatformCode + Message(绝不透传上游原始报文,10 §1.1 / §4.1),
// HTTP 码按段映射(apperr.FromUpstream)。非上游错误归 50000 内部错误。
func mapUpstream(err error) *apperr.Error {
	if err == nil {
		return nil
	}
	var ue *newapi.UpstreamError
	if errors.As(err, &ue) {
		if ue == nil { // typed-nil 防御(接口非 nil 包着 nil 指针):按"无错误"处理,绝不解引用 panic
			return nil
		}
		return apperr.FromUpstream(ue.PlatformCode, ue.Message, ue)
	}
	return apperr.Coerce(err)
}
