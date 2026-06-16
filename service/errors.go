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
		return apperr.FromUpstream(ue.PlatformCode, ue.Message, ue)
	}
	return apperr.Coerce(err)
}
