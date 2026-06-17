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
	Name          string
	Slug          string
	AdminEmail    string
	AdminPassword string // 留空则平台生成,初始密码在响应里回显一次
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
	if in.Name == "" || in.Slug == "" || in.AdminEmail == "" {
		return nil, apperr.InvalidParam("组织名称 / slug / 管理员邮箱必填")
	}
	if err := firstErr(checkLen("组织名称", in.Name, maxNameLen),
		checkSlug(in.Slug), checkEmail("管理员邮箱", in.AdminEmail)); err != nil {
		return nil, err
	}
	ctx, cancel := withTimeout(ctx, 8*time.Second)
	defer cancel()

	orgID, err := s.store.CreateOrganization(ctx, &model.Organization{Name: in.Name, Slug: in.Slug})
	if errors.Is(err, repo.ErrConflict) {
		return nil, apperr.Conflict("组织 slug 已存在")
	}
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}

	// 首个组织管理员:平台账号(无代发 key)。
	pw := in.AdminPassword
	if pw == "" {
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
	s.audit(ctx, c, orgID, "create_org", "organization", &orgID, map[string]any{"name": in.Name, "slug": in.Slug, "admin_email": in.AdminEmail})

	return &CreateOrgResult{Org: org, AdminMemberID: adminID, AdminEmail: in.AdminEmail, AdminInitialPassword: pw}, nil
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

// ListOrgs 列出全部客户组织(运营方)。
func (s *Service) ListOrgs(ctx context.Context, c session.Claims, limit, offset int) ([]*model.Organization, int, error) {
	if err := assertRole(c, session.RoleOperator); err != nil {
		return nil, 0, err
	}
	return s.store.ListOrganizations(ctx, limit, offset)
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
