package service

import (
	"context"
	"errors"
	"time"

	"github.com/nexusapi-platform/enterprise/model"
	"github.com/nexusapi-platform/enterprise/pkg/apperr"
	"github.com/nexusapi-platform/enterprise/pkg/session"
	"github.com/nexusapi-platform/enterprise/repo"
)

// OperatorOrgSlug 是平台运营方自身组织的 slug(运营方账号挂在此组织下)。
const OperatorOrgSlug = "_operator"

// LoginResult 是登录成功的返回(会话 token + 身份摘要)。
type LoginResult struct {
	Token     string
	Member    *model.Member
	ExpiresAt time.Time
}

// Login 校验平台账号(邮箱 + 密码)并签发会话 token(10 §1.4;MVP 账号登录,SSO/2FA 见决策 R4)。
// 失败一律返回模糊的 401(不区分"邮箱不存在"与"密码错",防账号枚举)。
//
// 注:2FA(6 位验证码,原型登录页)本里程碑留钩子未启用,后续接入。
func (s *Service) Login(ctx context.Context, email, password string) (*LoginResult, error) {
	ctx, cancel := withTimeout(ctx, 5*time.Second)
	defer cancel()

	m, err := s.store.GetMemberByEmail(ctx, email)
	if errors.Is(err, repo.ErrNotFound) {
		return nil, apperr.Unauthenticated("账号或密码错误")
	}
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	if m.PlatformPasswordHash == nil || !checkPassword(*m.PlatformPasswordHash, password) {
		return nil, apperr.Unauthenticated("账号或密码错误")
	}
	if m.Status == model.MemberStatusDisabled || m.Status == model.MemberStatusExpired {
		return nil, apperr.Forbidden("账号已停用")
	}

	claims := session.Claims{
		MemberID: m.ID,
		OrgID:    m.OrgID,
		Role:     session.Role(m.Role),
	}
	if m.TeamID != nil {
		claims.TeamID = *m.TeamID
	}
	tok, err := s.signer.Issue(claims)
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	return &LoginResult{Token: tok, Member: m}, nil
}

// Me 返回当前登录者身份(GET /me)。
func (s *Service) Me(ctx context.Context, c session.Claims) (*model.Member, error) {
	m, err := s.store.GetMember(ctx, c.OrgID, c.MemberID)
	if errors.Is(err, repo.ErrNotFound) {
		return nil, apperr.Unauthenticated("会话对应的账号不存在")
	}
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	return m, nil
}

// ChangePassword 个人设置·自助改平台登录密码:校验旧密码 → 校验新密码 → bcrypt 重哈希 → 落库。
// 对所有平台账号通用(运营方/组织管理员/团队负责人/成员改各自的);堵"运营方永久知道客户初始密码"的口子。
func (s *Service) ChangePassword(ctx context.Context, c session.Claims, oldPassword, newPassword string) error {
	m, err := s.store.GetMember(ctx, c.OrgID, c.MemberID)
	if errors.Is(err, repo.ErrNotFound) {
		return apperr.Unauthenticated("会话对应的账号不存在")
	}
	if err != nil {
		return apperr.Internal("").WithCause(err)
	}
	if m.PlatformPasswordHash == nil || !checkPassword(*m.PlatformPasswordHash, oldPassword) {
		return apperr.InvalidParam("当前密码不正确")
	}
	if n := len(newPassword); n < 8 || n > 64 {
		return apperr.InvalidParam("新密码长度须为 8–64 位")
	}
	if newPassword == oldPassword {
		return apperr.InvalidParam("新密码不能与当前密码相同")
	}
	hash, err := hashPassword(newPassword)
	if err != nil {
		return apperr.Internal("").WithCause(err)
	}
	if err := s.store.UpdateMemberPassword(ctx, c.OrgID, c.MemberID, hash); err != nil {
		return apperr.Internal("").WithCause(err)
	}
	s.audit(ctx, c, c.OrgID, "change_password", "member", &c.MemberID, nil)
	return nil
}

// UpdateMyDisplayName 个人设置·自助改显示名:**只改本人**(用 claims.MemberID,绝不信 body 传的 id,防越权改别人)。
// display_name 渲染到员工排行/顶栏,必须过 checkName(长度 + HTML/JS 危险字符黑名单)防存储型 XSS。
func (s *Service) UpdateMyDisplayName(ctx context.Context, c session.Claims, name string) (*model.Member, error) {
	if err := checkName("显示名", name, maxNameLen); err != nil {
		return nil, err
	}
	if err := s.store.UpdateMemberDisplayName(ctx, c.OrgID, c.MemberID, name); err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	s.audit(ctx, c, c.OrgID, "update_display_name", "member", &c.MemberID, nil)
	return s.Me(ctx, c)
}

// SeedOperator 在平台首次启动时确保存在一个运营方账号(引导账号)。
// 幂等:若该邮箱已存在则不改动。运营方账号挂在专属的运营方组织下,
// 是平台账号(无 new-api 代发 key,newapi_user_id 留空)。
func (s *Service) SeedOperator(ctx context.Context, email, password string) error {
	if email == "" || password == "" {
		return nil
	}
	// 已存在则跳过。
	if _, err := s.store.GetMemberByEmail(ctx, email); err == nil {
		s.log.Info("运营方引导账号已存在,跳过种子", "email", email)
		return nil
	} else if !errors.Is(err, repo.ErrNotFound) {
		return err
	}

	// 确保运营方组织存在。
	orgID, err := s.ensureOperatorOrg(ctx)
	if err != nil {
		return err
	}
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}
	m := &model.Member{
		OrgID:                orgID,
		LoginEmail:           email,
		Role:                 string(session.RoleOperator),
		PlatformPasswordHash: &hash,
	}
	id, err := s.store.CreateMemberProvisional(ctx, m)
	if err != nil {
		return err
	}
	// 运营方是平台账号、无代发 key:直接置 active、bootstrap_state=done(无上游用户)。
	if err := s.store.ActivatePlatformAccount(ctx, orgID, id); err != nil {
		return err
	}
	s.log.Info("已创建运营方引导账号", "email", email, "member_id", id, "org_id", orgID)
	return nil
}

// ensureOperatorOrg 取或建运营方自身组织,返回其 id。
func (s *Service) ensureOperatorOrg(ctx context.Context) (int64, error) {
	if org, err := s.store.GetOrganizationBySlug(ctx, OperatorOrgSlug); err == nil {
		return org.ID, nil
	} else if !errors.Is(err, repo.ErrNotFound) {
		return 0, err
	}
	return s.store.CreateOrganization(ctx, &model.Organization{
		Name: "平台运营方",
		Slug: OperatorOrgSlug,
	})
}
