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
	if s.observeMode {
		return 0, nil // MVP(观测)下 quota-worker 不碰 new-api 写(与 reconcile/settlement 同口径,放行前必做2)
	}
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
		if m.NewapiTokenID == nil {
			return nil // 模型2:无令牌无可执行额度
		}
		_, err := s.applyMemberOverride(ctx, m) // grant 已 expired,合成时自动不含它(落 token.remain_quota)
		return err
	case model.GrantAccountTTL:
		// 模型2:账号到期 = 停该成员令牌(member 无自己的 newapi user)。无令牌则只置 expired。
		if m.NewapiTokenID != nil {
			cred, cerr := s.orgCred(ctx, g.OrgID)
			if cerr != nil {
				return cerr
			}
			if derr := s.upstream.DeleteToken(ctx, cred, int(*m.NewapiTokenID)); derr != nil {
				return mapUpstream(derr)
			}
			if cerr := s.store.ClearMemberToken(ctx, g.OrgID, g.MemberID); cerr != nil {
				return cerr
			}
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

// 模型2:删除 ReconcileOrphans —— 它是 model1(员工=newapi user)的孤儿"用户"扫描兜底。
// 模型2 member 不映射 newapi user,开通失败只是员工 token 未建成(无孤儿用户;残留 token 靠确定性名
// 在重开时 adopt 自愈),故该机制 obsolete,整体移除(连同 worker 调用),不留死代码。
