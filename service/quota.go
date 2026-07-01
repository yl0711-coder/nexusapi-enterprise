package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nexusapi-platform/enterprise/adapter/newapi"
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

// computeOverride 合成成员当期 override 额度 = 基线 + 活跃额度类 grant(clamp 0)。
// 基线(B1 已拍板模型):显式覆盖(member>team>org 的 quota_policy.limit)?? 解析 tier 链
// (member.tier ?? team.tier ?? org.tier)取当前 period 的 limit。tier 是可复用额度模板,
// 运行时解析、不为每成员物化 policy;组织建时必有默认档,**无任何隐藏兜底常量**。
func (s *Service) computeOverride(ctx context.Context, m *model.Member) (int64, error) {
	base, err := s.resolveBaseQuota(ctx, m)
	if err != nil {
		return 0, err
	}
	grants, err := s.store.ListActiveQuotaGrants(ctx, m.OrgID, m.ID)
	if err != nil {
		return 0, err
	}
	override := base
	for _, g := range grants {
		override += g.Payload.Delta
	}
	if override < 0 {
		override = 0
	}
	return override, nil
}

// resolveBaseQuota 解析成员当期基线额度(quota)。
func (s *Service) resolveBaseQuota(ctx context.Context, m *model.Member) (int64, error) {
	policies, err := s.store.ListQuotaPolicies(ctx, m.OrgID)
	if err != nil {
		return 0, err
	}
	period := orgPeriod(policies) // org 级 reset 规则的 period,默认 monthly
	// 显式覆盖:member 级 > team 级(quota_policy.limit_quota)。
	if lim, ok := explicitOverride(policies, m.ID, m.TeamID); ok {
		return lim, nil
	}
	// tier 链:member.tier ?? team.tier ?? org.tier。
	tierID := m.TierID
	if tierID == nil && m.TeamID != nil {
		if t, err := s.store.GetTeam(ctx, m.OrgID, *m.TeamID); err == nil {
			tierID = t.DefaultTierID
		}
	}
	if tierID == nil {
		org, err := s.store.GetOrganization(ctx, m.OrgID)
		if err != nil {
			return 0, err
		}
		tierID = org.DefaultTierID
	}
	if tierID == nil {
		return 0, apperr.Internal("组织未配默认档,无法确定额度基线(请建组织时设默认档)")
	}
	tier, err := s.store.GetTier(ctx, m.OrgID, *tierID)
	if err != nil {
		return 0, err
	}
	lim, ok := tierPeriodLimit(tier, period)
	if !ok {
		return 0, apperr.Internal("默认档未配该周期额度上限")
	}
	return lim, nil
}

// orgPeriod 取组织级重置规则的 period(scope=org 的 quota_policy);缺省 monthly。
func orgPeriod(policies []*repo.QuotaPolicy) string {
	for _, p := range policies {
		if p.Scope == "org" && p.Period != "" {
			return p.Period
		}
	}
	return "monthly"
}

// explicitOverride 找显式覆盖额度:member 级优先于 team 级(scope 的 quota_policy.limit_quota)。
func explicitOverride(policies []*repo.QuotaPolicy, memberID int64, teamID *int64) (int64, bool) {
	for _, p := range policies {
		if p.Scope == "member" && p.ScopeID == memberID && p.Status == "active" {
			return p.LimitQuota, true
		}
	}
	if teamID != nil {
		for _, p := range policies {
			if p.Scope == "team" && p.ScopeID == *teamID && p.Status == "active" {
				return p.LimitQuota, true
			}
		}
	}
	return 0, false
}

// tierPeriodLimit 取层级在该 period 的额度上限;该 period 未配则回退 monthly→weekly→daily 第一个非空。
func tierPeriodLimit(t *model.Tier, period string) (int64, bool) {
	pick := func() *int64 {
		switch period {
		case "daily":
			return t.DailyLimit
		case "weekly":
			return t.WeeklyLimit
		default:
			return t.MonthlyLimit
		}
	}
	if v := pick(); v != nil {
		return *v, true
	}
	for _, v := range []*int64{t.MonthlyLimit, t.WeeklyLimit, t.DailyLimit} {
		if v != nil {
			return *v, true
		}
	}
	return 0, false
}

// applyMemberOverride 重算并经 adapter(限速出口)把成员 override quota 下发到 new-api。
// 返回下发的 override 值。new-api 用户必须已 bootstrap(有 newapi_user_id)。
//
// GZ-04 方案②:这是配额下发的**唯一出口**(重置/到期反向/恢复/人工调额/开通/审批/层级变更全经此)。
// 在此按组织计费状态统一决策最终值(gateByOrgStatus:应硬停则 0),使进程内只有一个逻辑写者、
// 按 DB 状态收敛——消除"settlement 写 0 vs quota-worker 写正常额"的并发无序覆盖(GZ-04 D1/D2)。
func (s *Service) applyMemberOverride(ctx context.Context, m *model.Member) (int64, error) {
	// GZ-04 返工·方案①:per-org 串行化"读状态→决策→下发"三步,**消除两 goroutine(settlement converge /
	// quota-worker)对同成员 override 的下发侧互覆盖**(旧值覆盖新值)。同组织串行、跨组织并发不受影响;
	// computeOverride 也纳入锁内,使"基线+grant 读取→下发"对同成员一致。
	// 据实更正(五总监复核):翻 status 旗标的 UpdateOrgStatus(billing.go)在本锁之外,故"翻转拍"那一瞬
	// 并非靠本锁消除——而是 recomputeOrgStatus 翻 stopped 后紧接着 convergeOrgQuotas 下发 0 来**收敛兜底**
	// (最终落 0、不停在错值)。另:本锁持锁跨 ManageUserQuota 的 HTTP,故同组织下发串行有延迟。
	// 多节点:进程内锁失效,需换分布式锁;"翻旗标在锁外靠 converge 收敛 + 持锁跨 HTTP 串行延迟"已记入
	// 多节点改造已知项(见 ADR-多节点worker选主-租约fencing.md)。
	release, lerr := s.quotaLocker.Acquire(ctx, fmt.Sprintf("quota-override:org:%d", m.OrgID))
	if lerr != nil {
		return 0, apperr.Internal("").WithCause(lerr)
	}
	defer release()
	override, err := s.computeOverride(ctx, m)
	if err != nil {
		return 0, err
	}
	final, err := s.gateByOrgStatus(ctx, m.OrgID, override)
	if err != nil {
		return 0, err
	}
	// 观测闸(R5 OBS-1 必修):observe 下绝不写员工 token 限额——写 spec{RemainQuota:final, UnlimitedQuota:false}
	// 会把员工从"无限额"翻成"限额"=消费到顶即被网关切断=停人,违反观测铁律。上次 b35d2dc 只在 reset/orphan
	// 调用方修了闸,而 applyMemberOverride 本身漏闸,且 UpdateTier/UpdateMember(HTTP 白名单放行的可达路径)
	// 会触达这里。把闸下沉到此处=一处覆盖全部调用方(reset/converge/grant/tier/member/approval)。
	// gateByOrgStatus 的 observe 分支只防"0-clamp",不够——"写有限额度"这个动作本身就是停人。
	if s.observeMode {
		return final, nil
	}
	// 模型2:员工额度落到其 token.remain_quota(不再写 user.quota=那是组织池子,由 escrow/续充 管)。
	// 这是 R4 额度执行;开 flag(R4)才真下发(observe 已在上面短路)。
	if m.NewapiTokenID == nil {
		return final, nil // 员工尚无令牌(观测自助前),无可执行额度
	}
	spec := newapi.TokenSpec{Name: deriveTokenName(m.ID, m.KeyRotation), RemainQuota: final, UnlimitedQuota: false, ExpiredTime: -1, Group: memberTokenGroup(m)}
	if err := s.withOrgCred(ctx, m.OrgID, func(cred newapi.MemberCred) error {
		return s.upstream.UpdateToken(ctx, cred, int(*m.NewapiTokenID), spec)
	}); err != nil {
		return 0, mapUpstream(err)
	}
	return final, nil
}

// gateByOrgStatus 按组织计费状态把 override clamp 到硬停值(GZ-04 方案②的"按状态决策"):
// 组织 status==stopped 且 hard_stop_enabled → 0;否则原值。每次下发前重读 DB 状态——
// 这是单写者收敛的唯一依据(任何写者读到的都是同一份最新状态,故不会互相覆盖出错)。
func (s *Service) gateByOrgStatus(ctx context.Context, orgID, override int64) (int64, error) {
	// MVP 观测模式(改动⑥-3):不碰钱/不停服,绝不把"应硬停"下发成 0;直接下发算出的额度。
	// observe 下组织本就不会进 stopped(结算跳过扣钱),此处为显式防御,语义清晰。
	if s.observeMode {
		return override, nil
	}
	org, err := s.store.GetOrganization(ctx, orgID)
	if err != nil {
		return 0, err
	}
	if org.Status != model.OrgStatusStopped {
		return override, nil
	}
	flags, err := s.store.GetOrgBillingFlags(ctx, orgID)
	if err != nil {
		return 0, err
	}
	if flags.HardStopEnabled {
		return 0, nil // 应硬停:即使算出来是正常额度也下发 0
	}
	return override, nil
}

// AdjustQuotaInput 是临时调额入参(US-03 / 10 §1.8.2)。
type AdjustQuotaInput struct {
	DeltaQuota int64  // 可正可负(收回)
	Duration   string // today / 3d / week
	Reason     string
}

// AdjustQuotaResult 调额产物。
type AdjustQuotaResult struct {
	NewCapQuota int64
	GrantID     int64
	ExpireAt    time.Time
}

// AdjustQuota 临时调额(US-03,E07):建 grant → 重算 override → 经 adapter 下发 → 审计。
// 到期由 quota-worker 反向(03 §3.4)。RBAC:组织管理员(本 org)/ 团队负责人(本 team)。
func (s *Service) AdjustQuota(ctx context.Context, c session.Claims, orgID, memberID int64, in AdjustQuotaInput) (*AdjustQuotaResult, error) {
	// R2-M4:调额是动钱面——拒空值(delta=0 无意义)+ 限绝对值上限(防误填天量)。
	if in.DeltaQuota == 0 {
		return nil, apperr.InvalidParam("调整量不能为 0")
	}
	if in.DeltaQuota > maxAdjustQuota || in.DeltaQuota < -maxAdjustQuota {
		return nil, apperr.InvalidParam("单次调整量超出上限")
	}
	if err := checkLen("原因", in.Reason, maxNoteLen); err != nil {
		return nil, err
	}
	m, err := s.loadManageableMember(ctx, c, orgID, memberID)
	if err != nil {
		return nil, err
	}
	expireAt, err := durationToExpiry(in.Duration, s.now())
	if err != nil {
		return nil, err
	}
	grantType := model.GrantQuotaAdd
	if in.DeltaQuota < 0 {
		grantType = model.GrantQuotaSub
	}
	var reason *string
	if in.Reason != "" {
		reason = &in.Reason
	}
	grantID, err := s.store.CreateGrant(ctx, &model.Grant{
		OrgID: orgID, MemberID: memberID, GrantType: grantType,
		Payload: model.GrantPayload{Delta: in.DeltaQuota, Duration: in.Duration},
		Reason:  reason, Operator: actorOf(c), ExpireAt: expireAt,
	})
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	newCap, err := s.applyMemberOverride(ctx, m)
	if err != nil {
		return nil, err
	}
	s.audit(ctx, c, orgID, "adjust_quota", "member", &memberID, map[string]any{
		"delta": in.DeltaQuota, "duration": in.Duration, "new_cap": newCap, "grant_id": grantID,
	})
	return &AdjustQuotaResult{NewCapQuota: newCap, GrantID: grantID, ExpireAt: expireAt}, nil
}

// TempPermissionInput 是临时权限入参(US-04)。
type TempPermissionInput struct {
	Type     string    // account_ttl / model_add
	Model    string    // model_add 时:放开的模型
	ExpireAt time.Time // 到期时间(account_ttl=账号有效期止;model_add=收回时点)
	Reason   string
}

// SetTempPermission 设临时权限(US-04,E08):account_ttl(到期停号)/ model_add(临时放开模型)。
// model_add 本期仅记录 grant(令牌当前不限模型,模型硬隔离见 03 §3.4.1,enforcement 待后续)。
func (s *Service) SetTempPermission(ctx context.Context, c session.Claims, orgID, memberID int64, in TempPermissionInput) (*model.Grant, error) {
	m, err := s.loadManageableMember(ctx, c, orgID, memberID)
	if err != nil {
		return nil, err
	}
	_ = m
	if in.Type != model.GrantAccountTTL && in.Type != model.GrantModelAdd {
		return nil, apperr.InvalidParam("临时权限类型须为 account_ttl 或 model_add")
	}
	if in.ExpireAt.Before(s.now()) {
		return nil, apperr.InvalidParam("到期时间须晚于当前")
	}
	if in.Type == model.GrantModelAdd && in.Model == "" {
		return nil, apperr.InvalidParam("model_add 须指定模型")
	}
	var reason *string
	if in.Reason != "" {
		reason = &in.Reason
	}
	grantID, err := s.store.CreateGrant(ctx, &model.Grant{
		OrgID: orgID, MemberID: memberID, GrantType: in.Type,
		Payload: model.GrantPayload{Model: in.Model},
		Reason:  reason, Operator: actorOf(c), ExpireAt: in.ExpireAt,
	})
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	s.audit(ctx, c, orgID, "set_temp_permission", "member", &memberID, map[string]any{
		"type": in.Type, "model": in.Model, "expire_at": in.ExpireAt.Format(time.RFC3339),
	})
	return s.store.GetGrant(ctx, orgID, grantID)
}

// ListGrants 列成员临时权限(分页)。
func (s *Service) ListGrants(ctx context.Context, c session.Claims, orgID, memberID int64, limit, offset int) ([]*model.Grant, int, error) {
	if _, err := s.loadManageableMember(ctx, c, orgID, memberID); err != nil {
		return nil, 0, err
	}
	return s.store.ListGrantsByMember(ctx, orgID, memberID, limit, offset)
}

// RevokeGrant 提前撤销临时权限(立即反向,不等到期,03 §3.4)。
func (s *Service) RevokeGrant(ctx context.Context, c session.Claims, orgID, grantID int64) error {
	g, err := s.store.GetGrant(ctx, orgID, grantID)
	if errors.Is(err, repo.ErrNotFound) {
		return apperr.NotFound("临时权限不存在")
	}
	if err != nil {
		return apperr.Internal("").WithCause(err)
	}
	m, err := s.loadManageableMember(ctx, c, orgID, g.MemberID)
	if err != nil {
		return err
	}
	if g.Status != model.GrantStatusActive {
		return apperr.New(apperr.CodeInvalidParam, 409, "该临时权限已结束,无需撤销")
	}
	marked, err := s.store.MarkGrantReverted(ctx, grantID, model.GrantStatusRevoked)
	if err != nil {
		return apperr.Internal("").WithCause(err)
	}
	if !marked {
		return apperr.New(apperr.CodeOptimisticLock, 409, "该临时权限已被处理")
	}
	// 额度类撤销 → 重算 override 下发(扣回临时额)。
	if g.GrantType == model.GrantQuotaAdd || g.GrantType == model.GrantQuotaSub {
		if _, err := s.applyMemberOverride(ctx, m); err != nil {
			return err
		}
	}
	s.audit(ctx, c, orgID, "revoke_grant", "member", &g.MemberID, map[string]any{"grant_id": grantID, "type": g.GrantType})
	return nil
}

// SetMemberStatus 停用/恢复成员(US-05,E09):enable/disable new-api 用户 + 改 member.status。
// disable:key 立即失效;恢复:enable,key 复用不重建(03 §3.3)。
func (s *Service) SetMemberStatus(ctx context.Context, c session.Claims, orgID, memberID int64, enabled bool) error {
	m, err := s.loadManageableMember(ctx, c, orgID, memberID)
	if err != nil {
		return err
	}
	// 模型2 R5后裁定:**禁用不删**——禁用=把该成员令牌置**禁用状态**(status_only,key 保留、启用即通、近实时生效);
	// 恢复=启用**同一** token。额度预留不回=R4(v1 成员 unlimited,无 per-member 分配可退)。
	// 删除/离职是**单独危险操作**(OffboardMember:删 token + 软删转离职列表),不走这里。
	if m.NewapiTokenID != nil {
		if serr := s.withOrgCred(ctx, orgID, func(cred newapi.MemberCred) error {
			return s.upstream.SetTokenStatus(ctx, cred, int(*m.NewapiTokenID), enabled)
		}); serr != nil {
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
	s.audit(ctx, c, orgID, "set_member_status", "member", &memberID, map[string]any{"enabled": enabled})
	return nil
}

// OffboardMember 离职(危险操作,org_admin/team_leader):删该成员令牌(key 失效)+ 软删转离职列表(资料/历史保留可恢复)。
// 额度退回可分配=R4(v1 成员 unlimited,无 per-member 分配)。恢复入职需重建 key(新 key,旧不可恢复)。
func (s *Service) OffboardMember(ctx context.Context, c session.Claims, orgID, memberID int64) error {
	m, err := s.loadManageableMember(ctx, c, orgID, memberID)
	if err != nil {
		return err
	}
	if m.NewapiTokenID != nil {
		if derr := s.withOrgCred(ctx, orgID, func(cred newapi.MemberCred) error {
			return s.upstream.DeleteToken(ctx, cred, int(*m.NewapiTokenID))
		}); derr != nil {
			return mapUpstream(derr)
		}
	}
	if err := s.store.OffboardMember(ctx, orgID, memberID); err != nil {
		return apperr.Internal("").WithCause(err)
	}
	s.audit(ctx, c, orgID, "offboard_member", "member", &memberID, nil)
	return nil
}

// RestoreOffboardedMember 恢复入职(org_admin):清软删 + 置 active;无 token,员工登录后自助重建 key(新 key)。
func (s *Service) RestoreOffboardedMember(ctx context.Context, c session.Claims, orgID, memberID int64) error {
	if err := assertOrgScope(c, orgID); err != nil {
		return err
	}
	if err := assertRole(c, session.RoleOperator, session.RoleOrgAdmin); err != nil {
		return err
	}
	if err := s.store.RestoreOffboardedMember(ctx, orgID, memberID); err != nil {
		return apperr.Internal("").WithCause(err)
	}
	s.audit(ctx, c, orgID, "restore_member", "member", &memberID, nil)
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

// durationToExpiry 把时长标识换成到期时间(UTC;时区精化见后续)。
// today=次日 0 点;3d=+72h;week=+7 天。也接受 RFC3339 绝对时间。
func durationToExpiry(duration string, now time.Time) (time.Time, error) {
	switch duration {
	case "today", "":
		y, mo, d := now.Date()
		return time.Date(y, mo, d, 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1), nil
	case "3d":
		return now.Add(72 * time.Hour), nil
	case "week":
		return now.AddDate(0, 0, 7), nil
	default:
		if t, err := time.Parse(time.RFC3339, duration); err == nil && t.After(now) {
			return t, nil
		}
		return time.Time{}, apperr.InvalidParam("时长须为 today/3d/week 或未来的 RFC3339 时间")
	}
}
