package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nexusapi-platform/enterprise/model"
	"github.com/nexusapi-platform/enterprise/pkg/apperr"
	"github.com/nexusapi-platform/enterprise/pkg/session"
	"github.com/nexusapi-platform/enterprise/repo"
)

// DefaultBaseTierMonthlyQuota 是建组织时自动建的"基础档"月度额度(quota,可见可改;= $50/月,锚定 500000=$1)。
// 这不是隐藏兜底常量,而是落在可见 tier 模板上的初始保守值,运营/商务按客户调整。
const DefaultBaseTierMonthlyQuota int64 = 25_000_000

// QuotaPerUnit 锚定:1 元/美元 = 500000 quota(与主站一致;元↔quota 换算的唯一常量)。
const QuotaPerUnit int64 = 500000


// SetMemberStatus 停用/恢复成员(架构B,31-ADR §4.5/33 §3.5):enable/disable **成员自己的 new-api user**
// (SetUserStatus,连带停其全部令牌、双缓存失效近实时生效;禁 override-to-0)。恢复=enable,令牌原样复通。
func (s *Service) SetMemberStatus(ctx context.Context, c session.Claims, orgID, memberID int64, enabled bool) error {
	m, err := s.loadManageableMember(ctx, c, orgID, memberID)
	if err != nil {
		return err
	}
	// 硬停期管理写屏蔽(20-§4 语义沿用;fail-closed:读不到组织即拒)。
	if org, gerr := s.store.GetOrganization(ctx, orgID); gerr != nil {
		return apperr.Internal("").WithCause(gerr)
	} else if org.Status == model.OrgStatusHardStopped {
		return apperr.New(apperr.CodeForbidden, 403, "组织已被运维硬停,管理操作暂不可用(解除后恢复)")
	}
	// 离职是单独危险操作(OffboardMember:disable→静默→退额),不走这里。
	if m.Status == model.MemberStatusOffboarded {
		return apperr.New(apperr.CodeInvalidParam, 409, "成员已离职,如需恢复请走恢复入职")
	}
	if m.NewapiUserID != nil {
		if serr := s.upstream.SetUserStatus(ctx, int(*m.NewapiUserID), enabled); serr != nil {
			return mapUpstream(serr)
		}
	}
	status := model.MemberStatusDisabled
	if enabled {
		status = model.MemberStatusActive
	}
	if err := s.store.UpdateMemberStatus(ctx, orgID, memberID, status); err != nil {
		return apperr.Internal("").WithCause(err)
	}
	// A3:改状态自增 epoch,作废该成员旧平台会话——禁用者旧 token 立即失效;再启用亦须重新登录。
	if err := s.store.BumpMemberSessionEpoch(ctx, orgID, memberID); err != nil {
		return apperr.Internal("").WithCause(err)
	}
	// 40号 P2-4:被停用者若是运营方,主动吊销其全部在途支持会话(epoch 踢线不覆盖支持态 token,
	// guard 回查是第一道,这里是显式清场;fail-open 只告警——guard 回查兜底)。
	if !enabled && m.Role == string(session.RoleOperator) {
		if n, rerr := s.store.RevokeSupportSessionsByActor(ctx, fmt.Sprintf("operator:%d", memberID)); rerr != nil {
			s.log.Error("停用运营方:吊销其支持会话失败(guard 回查仍会拦)", "member_id", memberID, "err", rerr)
		} else if n > 0 {
			s.log.Warn("停用运营方:已吊销其在途支持会话", "member_id", memberID, "count", n)
		}
	}
	s.audit(ctx, c, orgID, "set_member_status", "member", &memberID, map[string]any{"enabled": enabled})
	return nil
}

// OffboardMember 离职(架构B,31-ADR §4.5/30-§6,危险操作,org_admin/team_leader):
//
//	① disable 成员 new-api user(SetUserStatus,令牌立即全停;disable-first,陈旧窗口不再危险)
//	② 平台侧软删转离职列表 + session_epoch 踢线
//	③ 确认静默(有界等待余额稳定;disable→彻底静默 ~60s TTL,窗口内至多再落一个在途请求的后补)
//	④ 反向退额:RefundMemberBalance(BE② 契约,读实时余额 → Transfer 成员→金库,reason=offboard_refund)
//
// 幂等收敛:已离职成员重调本方法 = 重跑 ③④(退额读实时余额,余额 0 自然跳过),
// 覆盖「①②成功、④失败」的重试路径,不会双退。
func (s *Service) OffboardMember(ctx context.Context, c session.Claims, orgID, memberID int64) error {
	if err := assertOrgScope(c, orgID); err != nil {
		return err
	}
	if err := assertRole(c, session.RoleOrgAdmin, session.RoleTeamLeader); err != nil {
		return err
	}
	// 硬停期管理写屏蔽(20-§4 语义沿用;fail-closed)。
	if org, gerr := s.store.GetOrganization(ctx, orgID); gerr != nil {
		return apperr.Internal("").WithCause(gerr)
	} else if org.Status == model.OrgStatusHardStopped {
		return apperr.New(apperr.CodeForbidden, 403, "组织已被运维硬停,管理操作暂不可用(解除后恢复)")
	}
	m, err := s.store.GetMemberAny(ctx, orgID, memberID)
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

	// ① disable-first(有服务账号才有 new-api 侧动作;quarantined 孤儿本就 disabled,幂等)。
	if m.NewapiUserID != nil {
		if derr := s.upstream.SetUserStatus(ctx, int(*m.NewapiUserID), false); derr != nil {
			return mapUpstream(derr)
		}
	}
	// ② 软删 + 踢线(重调时已软删,UPDATE ... deleted_at IS NULL 不命中,天然幂等)。
	if m.Status != model.MemberStatusOffboarded {
		if err := s.store.OffboardMember(ctx, orgID, memberID); err != nil {
			return apperr.Internal("").WithCause(err)
		}
		if err := s.store.BumpMemberSessionEpoch(ctx, orgID, memberID); err != nil {
			s.log.Error("离职:踢线失败(旧会话最长 12h 自然失效)", "member_id", memberID, "err", err)
		}
		// 40号 P2-4:离职者若是运营方,吊销其全部在途支持会话(与 SetMemberStatus 停用同口径)。
		if m.Role == string(session.RoleOperator) {
			if n, rerr := s.store.RevokeSupportSessionsByActor(ctx, fmt.Sprintf("operator:%d", memberID)); rerr != nil {
				s.log.Error("离职运营方:吊销其支持会话失败(guard 回查仍会拦)", "member_id", memberID, "err", rerr)
			} else if n > 0 {
				s.log.Warn("离职运营方:已吊销其在途支持会话", "member_id", memberID, "count", n)
			}
		}
	}

	var refunded int64
	if m.NewapiUserID != nil {
		// ③ 确认静默:有界等待成员 quota 连续两次读数一致(在途请求后补落定)。
		s.waitQuotaQuiescence(ctx, int(*m.NewapiUserID))
		// ④ 退额(BE② 契约;前置断言成员非 active——②已置 offboarded)。
		var rerr error
		refunded, rerr = s.RefundMemberBalance(ctx, orgID, memberID, actorOf(c))
		if rerr != nil {
			// 成员已停(令牌全停,无继续消费风险),仅退额未完成:如实报错,重调本端点幂等补退。
			s.log.Error("离职:退额失败(成员已 disable,可重调离职端点补退)", "member_id", memberID, "err", rerr)
			s.audit(ctx, c, orgID, "offboard_member", "member", &memberID, map[string]any{"result": "refund_failed"})
			return rerr
		}
	}
	s.audit(ctx, c, orgID, "offboard_member", "member", &memberID, map[string]any{"refunded_raw": refunded})
	return nil
}

// waitQuotaQuiescence 离职「确认静默」的有界实现:连续两次读到相同实时余额即认为在途请求已落定
// (最多重读 3 次,间隔 s.offboardQuiesceWait)。disable→彻底静默的 ~60s TTL 不在请求内硬等——
// 残余风险按 31-ADR §4.4 口径 =「最多超一个在途请求的成本」,由退额读实时余额 + 对账环兜底。
func (s *Service) waitQuotaQuiescence(ctx context.Context, newapiUserID int) {
	prev, err := s.upstream.GetUserQuota(ctx, newapiUserID)
	if err != nil {
		return // 读不到就不等(退额自己会再读实时值)
	}
	for i := 0; i < 3; i++ {
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.offboardQuiesceWait):
		}
		cur, err := s.upstream.GetUserQuota(ctx, newapiUserID)
		if err != nil {
			return
		}
		if cur == prev {
			return // 稳定:静默确认
		}
		prev = cur
	}
}

// RestoreMember 恢复入职(架构B,33 §3.2:enable + 如新建重新分配额度;离职已退额,不自动恢复)。
// tierID 必填(33 §3.5 契约 {tier_id}):恢复时重选档位 = 新的额度 + 分组。
//   - 有服务账号:SetUserStatus(enable)→ 重设分组/档位 → GrantMemberQuota(BE② 契约,金库→成员划账);
//   - 无服务账号(旧数据/quarantined 孤儿重入):走 ProvisionMemberServiceAccount 如新建(saga 内含首笔划账)。
func (s *Service) RestoreMember(ctx context.Context, c session.Claims, orgID, memberID, tierID int64) error {
	if err := assertOrgScope(c, orgID); err != nil {
		return err
	}
	if err := assertRole(c, session.RoleOperator, session.RoleOrgAdmin); err != nil {
		return err
	}
	org, err := s.store.GetOrganization(ctx, orgID)
	if err != nil {
		return apperr.Internal("").WithCause(err)
	}
	if org.Status == model.OrgStatusHardStopped {
		return apperr.New(apperr.CodeForbidden, 403, "组织已被运维硬停,管理操作暂不可用(解除后恢复)")
	}
	tier, err := s.store.GetTier(ctx, orgID, tierID)
	if errors.Is(err, repo.ErrNotFound) {
		return apperr.InvalidParam("指定档位不存在")
	}
	if err != nil {
		return apperr.Internal("").WithCause(err)
	}
	if tier.AmountRaw == nil || *tier.AmountRaw <= 0 {
		return apperr.InvalidParam("该档位未配置有效额度(amount_raw 须为正数),请先按架构B完善档位")
	}
	m, err := s.store.GetMemberAny(ctx, orgID, memberID)
	if errors.Is(err, repo.ErrNotFound) {
		return apperr.NotFound("成员不存在")
	}
	if err != nil {
		return apperr.Internal("").WithCause(err)
	}
	if m.Status != model.MemberStatusOffboarded {
		return apperr.New(apperr.CodeInvalidParam, 409, "该成员不在离职列表")
	}
	grp := resolveTokenGroup(tier, org)

	if err := s.store.RestoreOffboardedMember(ctx, orgID, memberID); err != nil {
		return apperr.Internal("").WithCause(err)
	}
	if err := s.store.UpdateMemberTierGroup(ctx, orgID, memberID, tierID, grp); err != nil {
		return apperr.Internal("").WithCause(err)
	}

	if m.NewapiUserID == nil {
		// 无服务账号:如新建走 Provision saga(含建号/组/wallet_only/首笔划账;失败=隔离,如实报错可重试)。
		if _, perr := s.ProvisionMemberServiceAccount(ctx, orgID, memberID, grp, *tier.AmountRaw, actorOf(c)); perr != nil {
			return perr
		}
	} else {
		if serr := s.upstream.SetUserStatus(ctx, int(*m.NewapiUserID), true); serr != nil {
			return mapUpstream(serr)
		}
		// 分组按新档位重设(×N 配置护栏,31-ADR §5:漏一环成员发请求当场 403)。
		if gerr := s.upstream.SetUserGroup(ctx, int(*m.NewapiUserID), grp); gerr != nil {
			return mapUpstream(gerr)
		}
		// 如新建重新分配(BE② 契约:GrantMemberQuota 走 Transfer,金库不足即失败;成员已 enable 但额度 0=令牌不可花,可重试)。
		idem := fmt.Sprintf("restore:%d:%d", memberID, s.now().Unix())
		if terr := s.GrantMemberQuota(ctx, c, orgID, memberID, *tier.AmountRaw, "restore 重新分配", idem); terr != nil {
			s.log.Error("恢复入职:重新分配额度失败(成员已 enable、额度未到位,可重试恢复或走追加划账)", "member_id", memberID, "err", terr)
			return terr
		}
	}
	s.audit(ctx, c, orgID, "restore_member", "member", &memberID, map[string]any{"tier_id": tierID, "amount_raw": *tier.AmountRaw})
	return nil
}

// ListOffboardedMembers 列离职成员(org_admin/team_leader;离职列表可恢复)。
func (s *Service) ListOffboardedMembers(ctx context.Context, c session.Claims, orgID int64, limit, offset int) ([]*model.Member, int, error) {
	if err := assertOrgScope(c, orgID); err != nil {
		return nil, 0, err
	}
	if err := assertRole(c, session.RoleOperator, session.RoleOrgAdmin, session.RoleTeamLeader); err != nil {
		return nil, 0, err
	}
	return s.store.ListOffboardedMembers(ctx, orgID, limit, offset)
}

// loadManageableMember 取成员并做"可管理"RBAC 校验:组织管理员(本 org)/ 团队负责人(本 team);
// 成员/运营方拒。校验通过返回成员(且已 bootstrap done,有 newapi_user_id)。
func (s *Service) loadManageableMember(ctx context.Context, c session.Claims, orgID, memberID int64) (*model.Member, error) {
	if err := assertOrgScope(c, orgID); err != nil {
		return nil, err
	}
	if err := assertRole(c, session.RoleOrgAdmin, session.RoleTeamLeader); err != nil {
		return nil, err
	}
	m, err := s.store.GetMember(ctx, orgID, memberID)
	if errors.Is(err, repo.ErrNotFound) {
		return nil, apperr.NotFound("成员不存在")
	}
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	if c.Role == session.RoleTeamLeader {
		if err := assertTeamScope(c, m.TeamID); err != nil {
			return nil, err
		}
	}
	if m.BootstrapState != model.BootstrapDone {
		return nil, apperr.New(apperr.CodeInvalidParam, 409, "该成员尚未就绪")
	}
	return m, nil
}

