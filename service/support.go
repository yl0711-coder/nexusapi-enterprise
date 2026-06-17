package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/nexusapi-platform/enterprise/model"
	"github.com/nexusapi-platform/enterprise/pkg/apperr"
	"github.com/nexusapi-platform/enterprise/pkg/session"
	"github.com/nexusapi-platform/enterprise/repo"
)

// OpenSupportInput 开支持会话入参(运营方,08 §2.2 / §3.2)。
type OpenSupportInput struct {
	Scope     string // readonly / assist
	GrantType string // authorized / break_glass(assist 必填)
	TTLSeconds int
	Reason    string
}

// SupportSessionResult 开会话产物:新会话 token(运营方据此以客户管理员身份在该 org 操作)+ 会话信息。
type SupportSessionResult struct {
	Token   string
	Session *model.SupportSession
}

// OpenSupportSession 运营方对某客户组织开支持会话(只读/协助)。返回一个新会话 token:
// 运营方持它以"组织管理员"身份在该 org 操作,但受 CheckSupportGuard 闸约束
// (只读态拒所有写;协助态可写但动钱/读 key 红线挡)。每写一步双身份写客户 audit_log。
func (s *Service) OpenSupportSession(ctx context.Context, c session.Claims, orgID int64, in OpenSupportInput) (*SupportSessionResult, error) {
	if err := assertRole(c, session.RoleOperator); err != nil {
		return nil, err
	}
	if in.Scope != model.SupportReadonly && in.Scope != model.SupportAssist {
		return nil, apperr.InvalidParam("scope 须为 readonly 或 assist")
	}
	var grantType *string
	if in.Scope == model.SupportAssist {
		// 破玻璃需我方二级审批前置,整套运营方支持是二期(E1);一期只放客户授权协助。
		if in.GrantType == model.SupportBreakGlass {
			return nil, apperr.Forbidden("破玻璃(需二级审批前置)为二期能力,本期不开放")
		}
		if in.GrantType != model.SupportAuthorized {
			return nil, apperr.InvalidParam("协助态须给 grant_type=authorized(破玻璃二期)")
		}
		gt := in.GrantType
		grantType = &gt
	}
	if _, err := s.store.GetOrganization(ctx, orgID); errors.Is(err, repo.ErrNotFound) {
		return nil, apperr.NotFound("组织不存在")
	} else if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	ttl := in.TTLSeconds
	if ttl <= 0 || ttl > 24*3600 {
		ttl = 7200 // 默认 2h(08 演示值)
	}
	expireAt := s.now().Add(time.Duration(ttl) * time.Second)
	actor := fmt.Sprintf("operator:%d", c.MemberID)
	onBehalf := fmt.Sprintf("org_admin@org%d", orgID)

	sid, err := s.store.CreateSupportSession(ctx, &model.SupportSession{
		OrgID: orgID, Actor: actor, OnBehalfOf: onBehalf, Scope: in.Scope, GrantType: grantType, ExpireAt: expireAt,
	})
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	// 签发会话 token:以组织管理员身份在该 org 操作,带支持态标记。
	tok, err := s.signer.Issue(session.Claims{
		MemberID: c.MemberID, OrgID: orgID, Role: session.RoleOrgAdmin,
		SupportSessionID: sid, SupportScope: in.Scope,
	})
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	// 进入留痕(双身份写客户 audit_log)。
	s.auditSupport(ctx, c, orgID, sid, "support_enter", "organization", &orgID, map[string]any{
		"scope": in.Scope, "grant_type": in.GrantType, "expire_at": expireAt.Format(time.RFC3339),
	})
	ss, _ := s.store.GetSupportSession(ctx, sid)
	return &SupportSessionResult{Token: tok, Session: ss}, nil
}

// CloseSupportSession 结束支持会话。
func (s *Service) CloseSupportSession(ctx context.Context, c session.Claims, sid int64) error {
	ss, err := s.store.GetSupportSession(ctx, sid)
	if errors.Is(err, repo.ErrNotFound) {
		return apperr.NotFound("会话不存在")
	}
	if err != nil {
		return apperr.Internal("").WithCause(err)
	}
	if err := s.store.RevokeSupportSession(ctx, sid); err != nil {
		return apperr.Internal("").WithCause(err)
	}
	s.auditSupport(ctx, c, ss.OrgID, sid, "support_exit", "organization", &ss.OrgID, nil)
	return nil
}

// GetSupportSession 查会话状态。
func (s *Service) GetSupportSession(ctx context.Context, c session.Claims, sid int64) (*model.SupportSession, error) {
	ss, err := s.store.GetSupportSession(ctx, sid)
	if errors.Is(err, repo.ErrNotFound) {
		return nil, apperr.NotFound("会话不存在")
	}
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	return ss, nil
}

// CheckSupportGuard 是支持态后端闸(08 §2.2,真正的闸在后端、不靠前端隐藏):
//   - 非支持态 → 放行
//   - 会话失效/过期 → 拒
//   - 读(GET/HEAD)→ 放行
//   - 只读态任何写 → 403(10402)
//   - 协助态动钱/读 key 红线 → 403(10403);其余普通写放行(由 service 双身份留痕)
func (s *Service) CheckSupportGuard(ctx context.Context, c session.Claims, method, path string) error {
	if c.SupportSessionID == 0 {
		return nil
	}
	ss, err := s.store.GetSupportSession(ctx, c.SupportSessionID)
	if err != nil || ss.State != model.SupportActive || !s.now().Before(ss.ExpireAt) {
		return apperr.Forbidden("支持会话已结束或失效,请重新进入")
	}
	if ss.OrgID != c.OrgID {
		return apperr.NotFound("资源不存在") // 跨 org 隔离
	}
	if method == http.MethodGet || method == http.MethodHead {
		return nil
	}
	if c.SupportScope == model.SupportReadonly {
		return apperr.ReadonlyBlocked("只读支持态下不可写")
	}
	// 协助态:动钱 / 读明文 key 红线后端硬挡(08 §2.2)。
	if isMoneyOrKeyRedline(path) {
		return apperr.MoneyRedline("协助态下动钱 / 读明文 key 触发红线,已拦截(需客户本人或破玻璃专项)")
	}
	return nil
}

// isMoneyOrKeyRedline 判定路径是否属"动钱 / 读明文 key"红线(协助态硬挡)。
func isMoneyOrKeyRedline(path string) bool {
	for _, p := range []string{"/recharges", "/recharge-requests", "/pricing", "/billing-settings", "/key:"} {
		if strings.Contains(path, p) {
			return true
		}
	}
	return false
}

// auditSupport 写支持态双身份审计(actor=运营方真实 + on_behalf_of=客户管理员),写客户 audit_log。
func (s *Service) auditSupport(ctx context.Context, c session.Claims, orgID, sid int64, action, targetType string, targetID *int64, detail map[string]any) {
	var detailJSON []byte
	if detail != nil {
		detailJSON, _ = json.Marshal(detail)
	}
	onBehalf := fmt.Sprintf("org_admin@org%d", orgID)
	actor := fmt.Sprintf("operator:%d", c.MemberID)
	sidv := sid
	e := &model.AuditEntry{
		OrgID: orgID, Actor: actor, OnBehalfOf: &onBehalf, SupportSessionID: &sidv,
		Action: action, TargetType: &targetType, TargetID: targetID, Detail: detailJSON, Result: "ok",
	}
	if err := s.store.WriteAudit(ctx, e); err != nil {
		s.log.Error("写支持审计失败", "action", action, "err", err)
	}
}
