package service

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/nexusapi-platform/enterprise/model"
	"github.com/nexusapi-platform/enterprise/repo"
)

// ReverseExpiredGrants 是 quota-worker 的单次扫描:把已到期仍 active 的 grant 按类型反向应用
// (03 §3.4)。leader 单写者、跨 org;返回本次反向条数。每条带乐观标记防与人工撤销重复反向。
//
//   - quota_add/quota_sub → 标 expired,重算该成员 override 下发(扣回临时额)
//   - account_ttl         → 标 expired,disable new-api 用户、member→expired(US-04a)
//   - model_add           → 标 expired(令牌当前不限模型,无 enforcement 可收;仅记录)
func (s *Service) ReverseExpiredGrants(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = 100
	}
	grants, err := s.store.ListExpiredActiveGrants(ctx, s.now(), limit)
	if err != nil {
		return 0, err
	}
	reversed := 0
	for _, g := range grants {
		marked, err := s.store.MarkGrantReverted(ctx, g.ID, model.GrantStatusExpired)
		if err != nil {
			s.log.Error("worker 标记 grant 到期失败", "grant_id", g.ID, "err", err)
			continue
		}
		if !marked {
			continue // 已被人工撤销/他人处理
		}
		if err := s.reverseGrant(ctx, g); err != nil {
			s.log.Error("worker 反向 grant 失败(已标 expired,待重试/人工)", "grant_id", g.ID, "type", g.GrantType, "err", err)
			// 不回滚标记:避免反复反向;留日志+审计 failed,符合 03 §3.4「回退失败告警」。
			s.auditSystem(ctx, g.OrgID, "grant_expire_revert", "member", &g.MemberID, map[string]any{
				"grant_id": g.ID, "type": g.GrantType, "result": "failed",
			}, "failed")
			continue
		}
		s.auditSystem(ctx, g.OrgID, "grant_expire_revert", "member", &g.MemberID, map[string]any{
			"grant_id": g.ID, "type": g.GrantType,
		}, "ok")
		reversed++
	}
	return reversed, nil
}

// reverseGrant 按类型对单条到期 grant 做反向动作。member 不存在/未就绪则跳过(记日志)。
func (s *Service) reverseGrant(ctx context.Context, g *model.Grant) error {
	m, err := s.store.GetMember(ctx, g.OrgID, g.MemberID)
	if errors.Is(err, repo.ErrNotFound) {
		return nil // 成员已删,无需反向
	}
	if err != nil {
		return err
	}
	switch g.GrantType {
	case model.GrantQuotaAdd, model.GrantQuotaSub:
		if m.NewapiUserID == 0 {
			return nil
		}
		_, err := s.applyMemberOverride(ctx, m) // grant 已 expired,合成时自动不含它
		return err
	case model.GrantAccountTTL:
		if m.NewapiUserID == 0 {
			return nil
		}
		if err := s.upstream.SetUserStatus(ctx, int(m.NewapiUserID), false); err != nil {
			return mapUpstream(err)
		}
		return s.store.UpdateMemberStatus(ctx, g.OrgID, g.MemberID, model.MemberStatusExpired)
	case model.GrantModelAdd:
		return nil // 仅记录,无 enforcement
	default:
		return nil
	}
}

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

// ReconcileOrphans 后台 orphan 扫描兜底(GZ-03 返工·治洞1):对"开通失败收口、已回写 newapi_user_id"的成员,
// 幂等再禁用其 new-api 用户——消除"收口时 SetUserStatus 重试仍失败、留下活跃孤儿"。SetUserStatus(false) 幂等,
// 已禁用的再调无副作用。LIMIT 控批量;返回本次再禁用条数。quota-worker 每 tick 调一次。
func (s *Service) ReconcileOrphans(ctx context.Context) (int, error) {
	members, err := s.store.ListFailedOrphanMembers(ctx, 100)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, m := range members {
		if m.NewapiUserID == 0 {
			continue
		}
		if derr := s.upstream.SetUserStatus(ctx, int(m.NewapiUserID), false); derr != nil {
			s.log.Error("后台 orphan 扫描:再禁用孤儿用户失败(下轮重试)", "member_id", m.ID, "newapi_user_id", m.NewapiUserID, "err", derr)
			continue
		}
		n++
	}
	if n > 0 {
		s.log.Info("后台 orphan 扫描:幂等再禁用孤儿用户", "count", n)
	}
	return n, nil
}
