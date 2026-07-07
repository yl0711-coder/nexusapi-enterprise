// 架构B 阶段1 · BE② 契约增补(33 §12 增补,组长广播 2026-07-07)——**占位实现,集成时以 BE② 版本为准**。
//
// 本文件只为 BE①(身份层)对增补契约编程 + 自验编译/联调:两方法的正式实现归 BE②(钱核心),
// 组长阶段2 集成时若 BE② 已交付同名方法,**删除本文件**(方法签名冻结,调用方无需改动)。
// 占位实现严格走 Transfer 唯一写钱面(34 §1.1),不引入任何绕过。
package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/nexusapi-platform/enterprise/model"
	"github.com/nexusapi-platform/enterprise/pkg/apperr"
	"github.com/nexusapi-platform/enterprise/pkg/session"
	"github.com/nexusapi-platform/enterprise/repo"
)

// GrantMemberQuota 追加划账 / 恢复分配(BE② 契约增补①):金库→成员,经 Transfer(守恒/幂等/急停全继承)。
// 校验:调用者本组织 org_admin(团队负责人限本团队);金额正数且 ≤ 成员帽;成员须 active 且有服务账号。
// note 记审计;idemKey 由调用方给定(同键重放=幂等成功)。
func (s *Service) GrantMemberQuota(ctx context.Context, c session.Claims, orgID, memberID int64, amountRaw int64, note, idemKey string) error {
	if err := assertOrgScope(c, orgID); err != nil {
		return err
	}
	if err := assertRole(c, session.RoleOperator, session.RoleOrgAdmin, session.RoleTeamLeader); err != nil {
		return err
	}
	if idemKey == "" {
		return apperr.InvalidParam("缺少幂等键")
	}
	cap64, _ := s.store.GetSettingInt64(ctx, "member_quota_cap_raw", 500000000)
	if amountRaw <= 0 || amountRaw > cap64 {
		return apperr.InvalidParam(fmt.Sprintf("划账金额必须为正且不超过成员上限(%d raw)", cap64))
	}
	org, err := s.store.GetOrganization(ctx, orgID)
	if err != nil {
		return apperr.Internal("").WithCause(err)
	}
	if org.NewapiUserID == nil {
		return apperr.New(apperr.CodeInvalidParam, 409, "组织金库尚未开通")
	}
	m, err := s.store.GetMember(ctx, orgID, memberID)
	if errors.Is(err, repo.ErrNotFound) {
		return apperr.NotFound("成员不存在")
	}
	if err != nil {
		return apperr.Internal("").WithCause(err)
	}
	if c.Role == session.RoleTeamLeader {
		if err := assertTeamScope(c, m.TeamID); err != nil {
			return err
		}
	}
	if m.Status != model.MemberStatusActive || m.NewapiUserID == nil {
		return apperr.New(apperr.CodeInvalidParam, 409, "成员非 active 或尚无服务账号,不可划账")
	}
	if terr := s.Transfer(ctx, orgID, int(*org.NewapiUserID), int(*m.NewapiUserID), memberID, amountRaw, ReasonTopup, idemKey, actorOf(c)); terr != nil {
		return terr
	}
	s.audit(ctx, c, orgID, "grant_member_quota", "member", &memberID, map[string]any{"amount_raw": amountRaw, "note": note, "idem_key": idemKey})
	return nil
}

// RefundMemberBalance 离职退额(BE② 契约增补②):读成员实时余额 → 反向 Transfer(成员→金库,
// reason=offboard_refund)。**前置断言成员非 active**(调用方必须 disable-first,31-ADR §4.5)。
// 收敛式幂等:余额 ≤0 直接返 0(重试天然跳过,不双退);Transfer 中断留 pending 交对账环。
func (s *Service) RefundMemberBalance(ctx context.Context, orgID, memberID int64, actor string) (int64, error) {
	m, err := s.store.GetMemberAny(ctx, orgID, memberID)
	if errors.Is(err, repo.ErrNotFound) {
		return 0, apperr.NotFound("成员不存在")
	}
	if err != nil {
		return 0, apperr.Internal("").WithCause(err)
	}
	if m.Status == model.MemberStatusActive {
		return 0, apperr.New(apperr.CodeInvalidParam, 409, "成员仍为 active:退额前必须先 disable(离职流程序:disable→静默→退额)")
	}
	if m.NewapiUserID == nil {
		return 0, nil // 无服务账号,无可退
	}
	org, err := s.store.GetOrganization(ctx, orgID)
	if err != nil {
		return 0, apperr.Internal("").WithCause(err)
	}
	if org.NewapiUserID == nil {
		return 0, apperr.New(apperr.CodeInvalidParam, 409, "组织金库尚未开通,无法退额")
	}
	bal, gerr := s.upstream.GetUserQuota(ctx, int(*m.NewapiUserID))
	if gerr != nil {
		return 0, mapUpstream(gerr)
	}
	if bal <= 0 {
		return 0, nil // 已退净/在途冲负:无可退(负残余按 31-ADR §4.4「最多超一个在途请求」口径)
	}
	idem := fmt.Sprintf("offboard:%d:%d", memberID, s.now().Unix())
	if terr := s.Transfer(ctx, orgID, int(*m.NewapiUserID), int(*org.NewapiUserID), memberID, bal, ReasonOffboardRefund, idem, actor); terr != nil {
		return 0, terr
	}
	return bal, nil
}
