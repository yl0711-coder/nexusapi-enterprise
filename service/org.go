package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nexusapi-platform/enterprise/model"
	"github.com/nexusapi-platform/enterprise/pkg/apperr"
	"github.com/nexusapi-platform/enterprise/pkg/session"
	"github.com/nexusapi-platform/enterprise/repo"
)

// CreateOrgInput 建客户组织入参(E01,运营方独有)。连带建该组织第一个管理员账号(10 §1.7)。
type CreateOrgInput struct {
	Name            string
	Slug            string
	AdminEmail      string
	AdminPassword   string // 留空则平台生成,初始密码在响应里回显一次
	NewapiUserGroup string // 改动①:运营手填的 new-api 用户分组(必填;隔离边界;须已在 new-api 配好可用模型分组)
}

// CreateOrgResult 建组织产物。AdminInitialPassword 仅本次回显一次(供运营方交付客户管理员)。
type CreateOrgResult struct {
	Org                  *model.Organization
	AdminMemberID        int64
	AdminEmail           string
	AdminInitialPassword string
}

// CreateOrg 新建客户组织 + 首个组织管理员平台账号(E01)。仅运营方可调。
func (s *Service) CreateOrg(ctx context.Context, c session.Claims, in CreateOrgInput) (*CreateOrgResult, error) {
	if err := assertRole(c, session.RoleOperator); err != nil {
		return nil, err
	}
	if in.Name == "" || in.Slug == "" || in.AdminEmail == "" || in.NewapiUserGroup == "" {
		return nil, apperr.InvalidParam("组织名称 / slug / 管理员邮箱 / new-api 用户分组必填")
	}
	if err := firstErr(checkName("组织名称", in.Name, maxNameLen),
		checkSlug(in.Slug), checkEmail("管理员邮箱", in.AdminEmail),
		checkName("new-api 用户分组", in.NewapiUserGroup, 64)); err != nil { // 总监复验:统一到 checkName(长度+危险字符黑名单),与 org/tier/member 一致
		return nil, err
	}
	ctx, cancel := withTimeout(ctx, 8*time.Second)
	defer cancel()

	// 改动①:校验运营已在 new-api 把模型分组挂到该用户分组(group_special_usable_group 非空);
	// 否则建出来的成员令牌调用会被 new-api 分组 403。为空 → 提示先去 new-api 配。
	usable, gerr := s.upstream.GetOrgUsableGroups(ctx, in.NewapiUserGroup)
	if gerr != nil {
		return nil, mapUpstream(gerr)
	}
	if len(usable) == 0 {
		return nil, apperr.InvalidParam("该 new-api 用户分组尚未配可用模型分组(group_special_usable_group),请先在 new-api 配好再建组织")
	}

	// 架构B:组织金库一律门A 新建,不关联已有 new-api 用户当金库(门B 关联/导入整体退役,ADR §11 / doc36 决策A)。
	billingKind := model.BillingKindWallet

	orgID, err := s.store.CreateOrganization(ctx, &model.Organization{
		Name: in.Name, Slug: in.Slug, NewapiUserGroup: &in.NewapiUserGroup,
		CreatedByPlatform: true, // 架构B:组织一律门A 新建(门B 关联已退役,金库=平台新建资产)
		BillingKind:       billingKind,
	})
	if errors.Is(err, repo.ErrConflict) {
		return nil, apperr.Conflict("组织 slug 或 new-api 用户分组已被占用(一个用户分组只绑一个组织)")
	}
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}

	// 首个组织管理员:平台账号(无代发 key)。
	pw := in.AdminPassword
	if pw != "" {
		// C12:显式传入的初始密码走与改密同款强度校验(8–64),防运营方设弱密。
		if n := len(pw); n < 8 || n > 64 {
			return nil, apperr.InvalidParam("管理员初始密码长度须为 8–64 位")
		}
	} else {
		if pw, err = genPlatformPassword(); err != nil {
			return nil, apperr.Internal("").WithCause(err)
		}
	}
	hash, err := hashPassword(pw)
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	adminID, err := s.store.CreateMemberProvisional(ctx, &model.Member{
		OrgID:                orgID,
		LoginEmail:           in.AdminEmail,
		Role:                 string(session.RoleOrgAdmin),
		PlatformPasswordHash: &hash,
	})
	if errors.Is(err, repo.ErrConflict) {
		return nil, apperr.Conflict("管理员邮箱已被占用")
	}
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	if err := s.store.ActivatePlatformAccount(ctx, orgID, adminID); err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}

	// 建组织必有默认档(B1:干掉隐藏兜底)。自动建一个保守"基础档"tier + 设为组织默认 + 建 org 级月度重置策略。
	// 基础档额度为可见、可改的默认值(具体数额由商务/运营按客户调),非隐藏常量。
	baseLimit := DefaultBaseTierMonthlyQuota
	baseTierID, terr := s.store.CreateTier(ctx, &model.Tier{OrgID: orgID, Name: "基础档", MonthlyLimit: &baseLimit})
	if terr == nil {
		_ = s.store.SetDefaultTier(ctx, orgID, baseTierID)
		_ = s.store.UpsertQuotaPolicy(ctx, &repo.QuotaPolicy{OrgID: orgID, Scope: "org", ScopeID: orgID, Period: "monthly", LimitQuota: baseLimit, ResetAnchor: "00:00"})
	} else {
		s.log.Error("建组织默认档失败", "org_id", orgID, "err", terr)
	}

	org, err := s.store.GetOrganization(ctx, orgID)
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	s.audit(ctx, c, orgID, "create_org", "organization", &orgID, map[string]any{
		"name": in.Name, "slug": in.Slug, "admin_email": in.AdminEmail, "mode": "created",
	})

	return &CreateOrgResult{Org: org, AdminMemberID: adminID, AdminEmail: in.AdminEmail, AdminInitialPassword: pw}, nil
}

// HardStopOrg 运维硬停/解除(架构B,31-ADR §4.5/33 §3.2,运营方风控):
// 硬停 = **disable 金库 user + fan-out disable 全部成员 user**(幂等;漏一个=有人还在花)+ 全员会话踢线。
// 解除 = enable 金库 + 只 enable 平台侧 status=active 且 bootstrap=done 的成员(个别停用/离职/quarantined 的不解)。
// 一律 disable(SetUserStatus,双缓存失效近实时 403),禁 override-to-0(不刷缓存)。
// 与余额驱动的停服正交(有钱也能停:欠费纠纷/风控)。硬停期间该 org 的管理写操作被 withOrgCred/OpenMember 闸屏蔽。
func (s *Service) HardStopOrg(ctx context.Context, c session.Claims, orgID int64, stop bool) error {
	if err := assertRole(c, session.RoleOperator); err != nil {
		return err
	}
	org, err := s.store.GetOrganization(ctx, orgID)
	if errors.Is(err, repo.ErrNotFound) {
		return apperr.NotFound("组织不存在")
	}
	if err != nil {
		return apperr.Internal("").WithCause(err)
	}
	if stop && org.Status == model.OrgStatusHardStopped {
		return nil // 幂等
	}
	if !stop && org.Status != model.OrgStatusHardStopped {
		return apperr.InvalidParam("组织不在硬停状态")
	}
	uid, _, ok, err := s.store.GetOrgNewapiCred(ctx, orgID)
	if err != nil {
		return apperr.Internal("").WithCause(err)
	}
	if !ok {
		return apperr.New(apperr.CodeInvalidParam, 409, "组织尚未开通 new-api 金库")
	}
	// ① 金库先行(停:立断管理面 + 金库不再可划出;解除:先恢复金库)。
	if uerr := s.upstream.SetUserStatus(ctx, int(uid), !stop); uerr != nil {
		return mapUpstream(uerr)
	}
	// ② fan-out 全部成员 user(幂等重入:失败即返错,组织状态不翻转,运维重调补齐)。
	creds, lerr := s.store.ListMembersWithServiceAccount(ctx, orgID)
	if lerr != nil {
		return apperr.Internal("").WithCause(lerr)
	}
	var failed int
	for _, mc := range creds {
		if stop {
			// 停:全量 disable(含已停用/离职/quarantined——本就 disabled,upstream 幂等)。
			if derr := s.upstream.SetUserStatus(ctx, int(mc.NewapiUserID), false); derr != nil {
				s.log.Error("硬停 fan-out:disable 成员 user 失败(漏一个=有人还在花,须重试)", "org_id", orgID, "member_id", mc.MemberID, "err", derr)
				failed++
			}
			continue
		}
		// 解除:只 enable 平台侧应为 active 的成员(GetMemberAny:含软删行以便判离职跳过)。
		m, merr := s.store.GetMemberAny(ctx, orgID, mc.MemberID)
		if merr != nil {
			s.log.Error("解除硬停 fan-out:读成员失败(该成员维持 disabled,可重调解除补齐)", "member_id", mc.MemberID, "err", merr)
			failed++
			continue
		}
		if m.Status != model.MemberStatusActive || m.BootstrapState != model.BootstrapDone {
			continue // 停用/离职/quarantined:不解(它们的 disable 语义独立于硬停)
		}
		if eerr := s.upstream.SetUserStatus(ctx, int(mc.NewapiUserID), true); eerr != nil {
			s.log.Error("解除硬停 fan-out:enable 成员 user 失败(可重调解除补齐)", "member_id", mc.MemberID, "err", eerr)
			failed++
		}
	}
	if failed > 0 {
		// 不翻组织状态:幂等重调会重跑 ①②(已到位的 upstream 调用幂等),直至全量收敛。
		return apperr.Internal(fmt.Sprintf("硬停 fan-out 有 %d 个成员未收敛,请重试本操作(幂等)", failed))
	}
	newStatus := model.OrgStatusActive
	action := "hard_stop_release"
	if stop {
		newStatus = model.OrgStatusHardStopped
		action = "hard_stop"
	}
	if serr := s.store.UpdateOrgStatus(ctx, orgID, newStatus); serr != nil {
		// new-api 侧已生效(那是真动作),平台状态没跟上:告警,运维重试补状态。
		s.log.Error("硬停:new-api 已生效但组织状态落库失败(重试补)", "org_id", orgID, "stop", stop, "err", serr)
		return apperr.Internal("").WithCause(serr)
	}
	// A3/WB-4:硬停即刻失效——作废该组织全部成员的平台会话(否则被硬停组织的成员旧 token 仍可操作达 12h)。
	if stop {
		if berr := s.store.BumpOrgMembersSessionEpoch(ctx, orgID); berr != nil {
			s.log.Error("硬停:作废成员会话失败(旧 token 最长 12h 后自然失效)", "org_id", orgID, "err", berr)
		}
	}
	s.audit(ctx, c, orgID, action, "organization", &orgID, map[string]any{"newapi_user_id": uid, "members_fanout": len(creds)})
	return nil
}

// GetOrg 取组织详情(运营方任意 / 组织管理员本组织)。
func (s *Service) GetOrg(ctx context.Context, c session.Claims, orgID int64) (*model.Organization, error) {
	if err := assertOrgScope(c, orgID); err != nil {
		return nil, err
	}
	if err := assertRole(c, session.RoleOperator, session.RoleOrgAdmin); err != nil {
		return nil, err
	}
	org, err := s.store.GetOrganization(ctx, orgID)
	if errors.Is(err, repo.ErrNotFound) {
		return nil, apperr.NotFound("组织不存在")
	}
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	return org, nil
}

// ListOrgs 列出客户组织(运营方)。includeArchived=false 默认隐藏已归档(T12);q=服务端搜索(名称/slug,B1/28)。
func (s *Service) ListOrgs(ctx context.Context, c session.Claims, q string, limit, offset int, includeArchived bool) ([]*model.Organization, int, error) {
	if err := assertRole(c, session.RoleOperator); err != nil {
		return nil, 0, err
	}
	return s.store.ListOrganizations(ctx, q, limit, offset, includeArchived)
}

// SetOrgArchived 归档/取消归档组织(T12:仅运营方,软隐藏不物理删除,留痕)。
func (s *Service) SetOrgArchived(ctx context.Context, c session.Claims, orgID int64, archived bool) error {
	if err := assertRole(c, session.RoleOperator); err != nil {
		return err
	}
	if _, err := s.store.GetOrganization(ctx, orgID); errors.Is(err, repo.ErrNotFound) {
		return apperr.NotFound("组织不存在")
	} else if err != nil {
		return apperr.Internal("").WithCause(err)
	}
	if err := s.store.SetOrgArchived(ctx, orgID, archived); err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return apperr.NotFound("组织不存在")
		}
		return apperr.Internal("").WithCause(err)
	}
	action := "archive_org"
	if !archived {
		action = "unarchive_org"
	}
	s.audit(ctx, c, orgID, action, "organization", &orgID, nil)
	return nil
}

// audit 写一条审计(留痕一等公民,08 §0.4)。detail 已脱敏(绝不含明文 key / 密文)。
func (s *Service) audit(ctx context.Context, c session.Claims, orgID int64, action, targetType string, targetID *int64, detail map[string]any) {
	var detailJSON []byte
	if detail != nil {
		detailJSON, _ = json.Marshal(detail)
	}
	e := &model.AuditEntry{
		OrgID:      orgID,
		Actor:      actorOf(c),
		Action:     action,
		TargetType: &targetType,
		TargetID:   targetID,
		Detail:     detailJSON,
		Result:     "ok",
	}
	// 运营方支持态:双身份(actor=运营方真实 + on_behalf_of=客户管理员),写客户 audit_log(08 §0.4)。
	if c.SupportSessionID != 0 {
		sid := c.SupportSessionID
		e.SupportSessionID = &sid
		e.Actor = fmt.Sprintf("operator:%d", c.MemberID)
		onBehalf := fmt.Sprintf("org_admin@org%d", orgID)
		e.OnBehalfOf = &onBehalf
	}
	if err := s.store.WriteAudit(ctx, e); err != nil {
		s.log.Error("写审计失败", "action", action, "org_id", orgID, "err", err)
	}
}

// actorOf 返回审计中的操作者标识(MVP 用 member_id;后续可换显示名)。
func actorOf(c session.Claims) string {
	switch c.Role {
	case session.RoleOperator:
		return "operator:" + itoa(c.MemberID)
	default:
		return string(c.Role) + ":" + itoa(c.MemberID)
	}
}
