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
		Epoch:    m.SessionEpoch, // A3:签发时的会话代次,requireAuth 回查比对
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

// ValidateSession 会话有效性回查(A3):非支持态 token 每请求轻量校验成员 status==active 且 session_epoch 匹配
// (禁用/降级/改密/硬停任一自增 epoch 即令旧 token 失效,不必等 12h TTL)。
// 支持态 token(SupportSessionID!=0)跳过——其 MemberID 是运营方、OrgID 是目标 org,按目标 org 死查运营方成员必 404;
// 支持会话有效性由 CheckSupportGuard 校验(A4)。老 token 无 ep 字段 → Epoch=0 → 与默认 session_epoch=0 匹配,不被踢线。
func (s *Service) ValidateSession(ctx context.Context, c session.Claims) error {
	if c.SupportSessionID != 0 {
		return nil // 支持态:交 CheckSupportGuard,不按目标 org 查运营方成员(A4)
	}
	m, err := s.store.GetMember(ctx, c.OrgID, c.MemberID)
	if errors.Is(err, repo.ErrNotFound) {
		return apperr.Unauthenticated("会话已失效,请重新登录") // 不存在/离职软删
	}
	if err != nil {
		return apperr.Internal("").WithCause(err)
	}
	if m.Status != model.MemberStatusActive {
		return apperr.Unauthenticated("账号已停用,请重新登录")
	}
	if m.SessionEpoch != c.Epoch {
		return apperr.Unauthenticated("会话已失效(权限或密码已变更),请重新登录")
	}
	return nil
}

// Me 返回当前登录者身份(GET /me)。
func (s *Service) Me(ctx context.Context, c session.Claims) (*model.Member, error) {
	if c.SupportSessionID != 0 {
		// A4:支持态 token 的 MemberID 是运营方、OrgID 是目标 org,死查目标 org 成员必 404。
		// 返回支持态摘要(不查库):前端据此渲染目标 org 的 org_admin 视图 + 支持态标识,不再 401 把运营方踢回登录。
		dn := "运营方(支持态)"
		return &model.Member{ID: c.MemberID, OrgID: c.OrgID, Role: string(c.Role), DisplayName: &dn,
			LoginEmail: "support-session", Status: model.MemberStatusActive}, nil
	}
	m, err := s.store.GetMember(ctx, c.OrgID, c.MemberID)
	if errors.Is(err, repo.ErrNotFound) {
		return nil, apperr.Unauthenticated("会话对应的账号不存在")
	}
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	return m, nil
}

// MemberTierInfo 返回成员档位的名称与模型清单(F3/28:mykey 自助闭环,员工知道自己能调哪些模型)。
// best-effort:无 tier / 取不到一律返回空,绝不阻断成员详情。
func (s *Service) MemberTierInfo(ctx context.Context, orgID int64, m *model.Member) ([]string, string) {
	if m == nil || m.TierID == nil {
		return nil, ""
	}
	t, err := s.store.GetTier(ctx, orgID, *m.TierID)
	if err != nil {
		return nil, ""
	}
	return t.ModelSet, t.Name
}

// MyOrgStatus 返回调用者**本人所属组织**的服务状态(active/low/stopped/hard_stopped),供 /me 透出。
// A3(28-§阻断):员工"我的用量·当前状态"据此显示真实状态(尤其组织硬停),不再硬编码"正常"。
// 仅本人 org(c.OrgID,无跨 org),best-effort:取不到返回空串(前端按未知/正常处理),绝不阻断 /me。
func (s *Service) MyOrgStatus(ctx context.Context, c session.Claims) string {
	if c.OrgID == 0 {
		return ""
	}
	org, err := s.store.GetOrganization(ctx, c.OrgID)
	if err != nil {
		return ""
	}
	return org.Status
}

// ChangePassword 个人设置·自助改平台登录密码:校验旧密码 → 校验新密码 → bcrypt 重哈希 → 落库。
// 对所有平台账号通用(运营方/组织管理员/团队负责人/成员改各自的);堵"运营方永久知道客户初始密码"的口子。
func (s *Service) ChangePassword(ctx context.Context, c session.Claims, oldPassword, newPassword string) error {
	if c.SupportSessionID != 0 {
		return apperr.Forbidden("支持态下不可修改本人密码,请退出支持会话后操作") // A4:支持 token 的成员身份是目标 org,改密无意义且会误伤
	}
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
	// A3:改密自增 epoch,作废本人所有旧 token(含当前会话)——改密后须以新密码重新登录(安全口径:改密即注销旧会话)。
	if err := s.store.BumpMemberSessionEpoch(ctx, c.OrgID, c.MemberID); err != nil {
		return apperr.Internal("").WithCause(err)
	}
	s.audit(ctx, c, c.OrgID, "change_password", "member", &c.MemberID, nil)
	return nil
}

// UpdateMyDisplayName 个人设置·自助改显示名:**只改本人**(用 claims.MemberID,绝不信 body 传的 id,防越权改别人)。
// display_name 渲染到员工排行/顶栏,必须过 checkName(长度 + HTML/JS 危险字符黑名单)防存储型 XSS。
func (s *Service) UpdateMyDisplayName(ctx context.Context, c session.Claims, name string) (*model.Member, error) {
	if c.SupportSessionID != 0 {
		return nil, apperr.Forbidden("支持态下不可修改本人显示名,请退出支持会话后操作") // A4
	}
	if err := checkName("显示名", name, maxNameLen); err != nil {
		return nil, err
	}
	if err := s.store.UpdateMemberDisplayName(ctx, c.OrgID, c.MemberID, name); err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	s.audit(ctx, c, c.OrgID, "update_display_name", "member", &c.MemberID, nil)
	return s.Me(ctx, c)
}

// ResetMemberPassword 管理员/团队负责人重置某成员的平台登录密码(C22:忘密/找回路径)。生成新随机密码、
// bump epoch 作废该成员所有旧会话(A3),返回明文一次(供交付客户)。RBAC 同成员管理(loadManageableMember)。
func (s *Service) ResetMemberPassword(ctx context.Context, c session.Claims, orgID, memberID int64) (string, error) {
	if _, err := s.loadManageableMember(ctx, c, orgID, memberID); err != nil {
		return "", err
	}
	pw, err := genPlatformPassword()
	if err != nil {
		return "", apperr.Internal("").WithCause(err)
	}
	hash, err := hashPassword(pw)
	if err != nil {
		return "", apperr.Internal("").WithCause(err)
	}
	if err := s.store.UpdateMemberPassword(ctx, orgID, memberID, hash); err != nil {
		return "", apperr.Internal("").WithCause(err)
	}
	// A3:重置密码作废该成员所有旧 token(须以新密码重新登录)。
	if err := s.store.BumpMemberSessionEpoch(ctx, orgID, memberID); err != nil {
		return "", apperr.Internal("").WithCause(err)
	}
	s.audit(ctx, c, orgID, "reset_member_password", "member", &memberID, nil)
	return pw, nil
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
