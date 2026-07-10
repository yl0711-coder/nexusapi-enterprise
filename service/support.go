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
	Scope      string // readonly / assist
	GrantType  string // authorized / break_glass(assist 必填)
	TTLSeconds int
	Reason     string
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
	// GZ-05 修复B:此前零鉴权,任意登录用户可遍历 sid 关闭他人进行中的支持会话(DoS)。
	// 收紧为:运营方角色 + 仅发起该会话的运营方本人可关闭(否则 NotFound,不泄露会话存在性)。
	if err := assertRole(c, session.RoleOperator); err != nil {
		return err
	}
	ss, err := s.store.GetSupportSession(ctx, sid)
	if errors.Is(err, repo.ErrNotFound) {
		return apperr.NotFound("会话不存在")
	}
	if err != nil {
		return apperr.Internal("").WithCause(err)
	}
	if ss.Actor != fmt.Sprintf("operator:%d", c.MemberID) {
		return apperr.NotFound("会话不存在")
	}
	if err := s.store.RevokeSupportSession(ctx, sid); err != nil {
		return apperr.Internal("").WithCause(err)
	}
	s.auditSupport(ctx, c, ss.OrgID, sid, "support_exit", "organization", &ss.OrgID, nil)
	return nil
}

// GetSupportSession 查会话状态。
func (s *Service) GetSupportSession(ctx context.Context, c session.Claims, sid int64) (*model.SupportSession, error) {
	// GZ-05 修复B:此前零鉴权,任意登录用户可遍历 sid 读他人支持会话详情(信息泄露)。收紧为仅运营方可读。
	if err := assertRole(c, session.RoleOperator); err != nil {
		return nil, err
	}
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
	// 40号 P2-4:支持态曾是 epoch 踢线的唯一缺口——回查运营方本人(c.MemberID=运营方真实 id,全局查)
	// status==active 且 epoch 匹配;运营方被禁用/改密/离职后,其在途支持 token 读写全部即刻失效,不等 TTL。
	st, ep, aerr := s.store.GetMemberStatusEpochByID(ctx, c.MemberID)
	if aerr != nil || st != model.MemberStatusActive || ep != int64(c.Epoch) {
		return apperr.Forbidden("支持会话已失效(运营方账号状态已变更),请重新进入")
	}
	if method == http.MethodGet || method == http.MethodHead {
		return nil
	}
	if c.SupportScope == model.SupportReadonly {
		return apperr.ReadonlyBlocked("只读支持态下不可写")
	}
	// 协助态:动钱 / 铸或读明文 key 红线后端硬挡(08 §2.2)。
	if isMoneyOrKeyRedline(method, path) {
		return apperr.MoneyRedline("协助态下动钱 / 铸或读明文 key 触发红线,已拦截(需客户本人或破玻璃专项)")
	}
	return nil
}

// assistWriteAllow 协助态可执行的**普通写能力白名单**(架构B,31-ADR §10 / 33 §3.5 改造):
// 从「路径子串黑名单」倒转为显式端点能力枚举——凡不在列的写端点**默认落红线**,新端点默认受保护。
// 匹配按路径段(`*` = 恰好一个段),与 method 精确比对。
//
// 显式不列(=红线,10403 硬挡)举例:开通成员(POST .../members,建号+首笔划账+登录凭证回显)、
// members:bulk、offboard/restore(动钱:退额/重新分配)、quota:grant|quota:adjust|grants(划账/调额)、
// /me/tokens 全部写与 key:reveal(铸/读 key)、password:reset(可借改密接管成员会话→绕 key 红线)、
// recharges/recharge-requests/debits/pricing/billing-settings/default-token-group(钱与计价)、
// hard-stop(风控大动作)、approvals(审批通过会触发额度下发)。
var assistWriteAllow = []string{
	"POST /api/v1/auth/login",
	"POST /api/v1/me/password", // 改的是操作者自己的平台密码
	"PATCH /api/v1/me",
	"PATCH /api/v1/organizations/*",
	"POST /api/v1/organizations/*/archive",
	"POST /api/v1/organizations/*/unarchive",
	"POST /api/v1/organizations/*/teams",
	"PATCH /api/v1/organizations/*/teams/*",
	"POST /api/v1/organizations/*/teams/*/archive",
	"POST /api/v1/organizations/*/teams/*/unarchive",
	"POST /api/v1/organizations/*/tiers", // 档位=模板配置;真动钱在划账端点(那些全在红线)
	"PUT /api/v1/tiers/*",
	"DELETE /api/v1/tiers/*",
	"POST /api/v1/tiers/*/default",
	"PATCH /api/v1/members/*", // 改名/团队/档位指针(不划钱)
	"POST /api/v1/members/*/role",
	"POST /api/v1/members/*/status", // 停用/启用(disable 非钱非 key)
	"POST /api/v1/notifications/*/read",
	"POST /api/v1/support-sessions/*/close",
}

// isMoneyOrKeyRedline 判定 (method, path) 是否属「动钱 / 铸或读明文 key」红线(协助态硬挡)。
// 架构B:**能力白名单制**——写请求不在 assistWriteAllow 内即红线(默认拒,防新端点漏挡);
// 读(GET/HEAD)恒非红线(只读态另有整体闸)。
func isMoneyOrKeyRedline(method, path string) bool {
	if method == http.MethodGet || method == http.MethodHead {
		return false
	}
	for _, pat := range assistWriteAllow {
		if matchEndpointPattern(pat, method, path) {
			return false
		}
	}
	return true
}

// matchEndpointPattern 按段匹配 "METHOD /a/*/b" 形式的端点模式(`*` 恰好匹配一个路径段;
// 段数不等即不匹配,故 members:bulk 不会命中 members)。
func matchEndpointPattern(pattern, method, path string) bool {
	sp := strings.SplitN(pattern, " ", 2)
	if len(sp) != 2 || sp[0] != method {
		return false
	}
	pp := strings.Split(strings.Trim(sp[1], "/"), "/")
	rp := strings.Split(strings.Trim(path, "/"), "/")
	if len(pp) != len(rp) {
		return false
	}
	for i := range pp {
		if pp[i] != "*" && pp[i] != rp[i] {
			return false
		}
	}
	return true
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
