package service

import (
	"context"

	"github.com/nexusapi-platform/enterprise/pkg/session"
)

// ServiceStatus 是对外服务状态(GET /service-status,全角色;脱敏)。
type ServiceStatus struct {
	Overall string   `json:"overall"`
	Note    string   `json:"note"`
	Models  []string `json:"models"`
}

// GetServiceStatus 返回平台/上游可达性概览。MVP 基础版:进程在即视为可服务;
// 模型级状态后续接 new-api 渠道健康。任何登录用户可看(脱敏,无跨组织信息)。
func (s *Service) GetServiceStatus(ctx context.Context, c session.Claims) *ServiceStatus {
	return &ServiceStatus{Overall: "operational", Note: "平台运行中;模型级状态接入 new-api 渠道健康为后续项", Models: []string{}}
}
