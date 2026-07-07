package service

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/nexusapi-platform/enterprise/adapter/newapi"
	"github.com/nexusapi-platform/enterprise/model"
	"github.com/nexusapi-platform/enterprise/pkg/apperr"
)

// auditSystem 写一条系统身份(worker)审计。
func (s *Service) auditSystem(ctx context.Context, orgID int64, action, targetType string, targetID *int64, detail map[string]any, result string) {
	var detailJSON []byte
	if detail != nil {
		detailJSON, _ = json.Marshal(detail)
	}
	if result == "" {
		result = "ok"
	}
	e := &model.AuditEntry{
		OrgID: orgID, Actor: "system:quota-worker", Action: action,
		TargetType: &targetType, TargetID: targetID, Detail: detailJSON, Result: result,
	}
	if err := s.store.WriteAudit(ctx, e); err != nil {
		s.log.Error("worker 写审计失败", "action", action, "err", err)
	}
}

// RecordWorkerFailure 把后台任务失败写入系统审计。它不替代业务函数内的精确告警,只兜住
// worker 级持续失败(尤其 new-api 管理员 token 失效、上游不可达)不能只藏在容器日志里。
func (s *Service) RecordWorkerFailure(ctx context.Context, workerName, step string, err error) {
	if err == nil {
		return
	}
	result := "failed"
	detail := map[string]any{"worker": workerName, "step": step, "error": err.Error()}
	var ae *apperr.Error
	if errors.As(err, &ae) {
		detail["code"] = ae.Code
		if ae.Code == newapi.CodeUpstreamAuth {
			result = "auth_failed"
		}
	}
	var ue *newapi.UpstreamError
	if errors.As(err, &ue) {
		detail["code"] = ue.PlatformCode
		if ue.PlatformCode == newapi.CodeUpstreamAuth {
			result = "auth_failed"
		}
	}
	s.auditSystem(ctx, 0, "worker_failed", "worker", nil, detail, result)
}

// 模型2:删除 ReconcileOrphans —— 它是 model1(员工=newapi user)的孤儿"用户"扫描兜底。
// 模型2 member 不映射 newapi user,开通失败只是员工 token 未建成(无孤儿用户;残留 token 靠确定性名
// 在重开时 adopt 自愈),故该机制 obsolete,整体移除(连同 worker 调用),不留死代码。
