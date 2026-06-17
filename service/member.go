package service

import (
	"context"
	"errors"
	"time"

	"github.com/nexusapi-platform/enterprise/adapter/newapi"
	"github.com/nexusapi-platform/enterprise/model"
	"github.com/nexusapi-platform/enterprise/pkg/apperr"
	"github.com/nexusapi-platform/enterprise/pkg/session"
	"github.com/nexusapi-platform/enterprise/repo"
)

// OpenMemberInput 开通成员入参(US-01:姓名 + 团队 + 层级)。
type OpenMemberInput struct {
	Name   string
	Email  string // 平台登录邮箱;留空则自动派生
	TeamID *int64
	TierID *int64 // 留空套组织默认层级
}

// OpenMemberResult 开通成员产物。APIKey 明文仅本次回显一次(10 §1.8.1 / §3.1)。
type OpenMemberResult struct {
	MemberID        int64
	NewapiUserID    int64
	APIKey          string // 明文,仅此一次
	KeyMasked       string
	InitialPassword string // 成员平台登录初始密码,仅此一次回显(交付成员、首登改密;C2)
	LoginEmail      string
	TierID          *int64
	Models          []string
}

// OpenMember 开通成员 = 建 new-api 用户 + 代发 key + 绑层级(US-01,E05)。
//
// RBAC(E05):组织管理员(本 org)/ 团队负责人(仅本 team);运营方 403*(经支持会话才行)。
// 链路(03 §3.2 / 10 §2):provisional 落库 → 派生确定性 username/password → adapter.BootstrapMember
// (建用户→代理登录取 access_token→建 token→取明文 key)→ 加密存 access_token/password →
// FinalizeBootstrap(绑 newapi_user_id/令牌/脱敏 key,置 active)→ 写 audit_log → 返回明文 key 一次。
func (s *Service) OpenMember(ctx context.Context, c session.Claims, orgID int64, in OpenMemberInput) (*OpenMemberResult, error) {
	if err := assertOrgScope(c, orgID); err != nil {
		return nil, err
	}
	if err := assertRole(c, session.RoleOrgAdmin, session.RoleTeamLeader); err != nil {
		return nil, err
	}
	if in.Name == "" {
		return nil, apperr.InvalidParam("成员姓名必填")
	}
	if err := firstErr(checkLen("姓名", in.Name, maxNameLen), checkLen("邮箱", in.Email, maxEmailLen)); err != nil {
		return nil, err
	}

	// 团队负责人:只能开到本团队;team_id 缺省套本团队,显式跨团队 → 403。
	if c.Role == session.RoleTeamLeader {
		if in.TeamID == nil {
			tid := c.TeamID
			in.TeamID = &tid
		}
		if err := assertTeamScope(c, in.TeamID); err != nil {
			return nil, err
		}
	}

	ctx, cancel := withTimeout(ctx, 30*time.Second)
	defer cancel()

	// 校验组织存在。
	org, err := s.store.GetOrganization(ctx, orgID)
	if errors.Is(err, repo.ErrNotFound) {
		return nil, apperr.NotFound("组织不存在")
	}
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}

	// 校验团队(若指定)属本 org。
	if in.TeamID != nil {
		if _, err := s.store.GetTeam(ctx, orgID, *in.TeamID); errors.Is(err, repo.ErrNotFound) {
			return nil, apperr.InvalidParam("指定团队不存在")
		} else if err != nil {
			return nil, apperr.Internal("").WithCause(err)
		}
	}

	// 解析层级:指定 → 校验属本 org;否则套组织默认层级。
	tier, err := s.resolveTier(ctx, orgID, in.TierID, org.DefaultTierID)
	if err != nil {
		return nil, err
	}

	// 自动派生登录邮箱(若未提供)。
	email := in.Email
	if email == "" {
		email = org.Slug + "-" + randEmailSuffix() + "@nexus.local"
	}

	// 生成 new-api 密码(加密留存,重 bootstrap 兜底)+ 平台登录初始密码(bcrypt)。
	newapiPw, err := genNewapiPassword()
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	platPw, err := genPlatformPassword()
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	platHash, err := hashPassword(platPw)
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	newapiPwEnc, err := s.keyring.EncryptString(newapiPw)
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}

	displayName := in.Name
	prov := &model.Member{
		OrgID:                orgID,
		TeamID:               in.TeamID,
		LoginEmail:           email,
		DisplayName:          &displayName,
		Role:                 string(session.RoleMember),
		PlatformPasswordHash: &platHash,
		MemberPasswordEnc:    []byte(newapiPwEnc),
	}
	if tier != nil {
		prov.TierID = &tier.ID
	}
	memberID, err := s.store.CreateMemberProvisional(ctx, prov)
	if errors.Is(err, repo.ErrConflict) {
		return nil, apperr.Conflict("该邮箱在本组织已存在")
	}
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}

	// 代发 key 全链路(adapter,真机打 new-api)。username 确定性派生(<=20)。
	res, berr := s.upstream.BootstrapMember(ctx, newapi.BootstrapInput{
		OrgID:       orgID,
		MemberID:    memberID,
		Username:    deriveUsername(orgID, memberID),
		Password:    newapiPw,
		DisplayName: in.Name,
	})
	if berr != nil {
		// 补偿:标 bootstrap 失败(adapter 已 disable 上游半成品用户)。不展示半截账号。
		if merr := s.store.MarkBootstrapFailed(ctx, orgID, memberID); merr != nil {
			s.log.Error("标记 bootstrap 失败也失败", "member_id", memberID, "err", merr)
		}
		s.audit(ctx, c, orgID, "open_member", "member", &memberID, map[string]any{"name": in.Name, "result": "bootstrap_failed"})
		return nil, mapUpstream(berr)
	}

	// 成功:加密 access_token,回填成员。
	accessTokenEnc, err := s.keyring.EncryptString(res.AccessToken)
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	keyMasked := maskKey(res.PlaintextKey)
	tokenID := int64(res.TokenID)
	final := &model.Member{
		ID:                memberID,
		OrgID:             orgID,
		NewapiUserID:      int64(res.NewapiUserID),
		AccessTokenEnc:    []byte(accessTokenEnc),
		MemberPasswordEnc: []byte(newapiPwEnc),
		NewapiTokenID:     &tokenID,
		KeyMasked:         &keyMasked,
		KeyRotation:       1, // bootstrap 建的是 v1(adapter defaultBootstrapTokenSpec)
	}
	if err := s.store.FinalizeBootstrap(ctx, final); err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}

	// 设 new-api 用户分组 = org_{id}(A2:折扣按用户分组归属,GroupGroupRatio 按此查)。best-effort。
	if err := s.upstream.SetUserGroup(ctx, res.NewapiUserID, orgUserGroup(orgID)); err != nil {
		s.log.Warn("设成员 new-api 用户分组失败(可后续补)", "member_id", memberID, "err", err)
	}
	// 令牌 model_limits = 层级模型集(B2:网关数据面限模型,真拦截、不依赖平台在线)。best-effort。
	if tier != nil && len(tier.ModelSet) > 0 {
		cred := newapi.MemberCred{NewapiUserID: res.NewapiUserID, AccessToken: res.AccessToken}
		spec := newapi.TokenSpec{Name: deriveTokenName(memberID, 1), UnlimitedQuota: true, ExpiredTime: -1, Group: "default", ModelLimits: tier.ModelSet}
		if err := s.upstream.UpdateToken(ctx, cred, res.TokenID, spec); err != nil {
			s.log.Warn("设令牌 model_limits 失败(可后续补)", "member_id", memberID, "err", err)
		}
	}

	// 下发初始 quota = 解析基线(tier 链 + 显式覆盖,B1)。失败不回滚开通(key 是主交付物),仅告警。
	final.TierID = prov.TierID
	final.TeamID = in.TeamID
	final.NewapiUserID = int64(res.NewapiUserID)
	initialQuota, oerr := s.applyMemberOverride(ctx, final)
	if oerr != nil {
		s.log.Warn("开通成员后下发初始 quota 失败(可后续重算补下发)", "member_id", memberID, "err", oerr)
	}

	s.audit(ctx, c, orgID, "open_member", "member", &memberID, map[string]any{
		"name": in.Name, "newapi_user_id": res.NewapiUserID, "adopted_existing": res.AdoptedExisting, "init_quota": initialQuota,
	})

	out := &OpenMemberResult{
		MemberID:        memberID,
		NewapiUserID:    int64(res.NewapiUserID),
		APIKey:          res.PlaintextKey,
		KeyMasked:       keyMasked,
		InitialPassword: platPw, // 仅此一次回显;成员首登改密
		LoginEmail:      email,
	}
	if tier != nil {
		out.TierID = &tier.ID
		out.Models = tier.ModelSet
	}
	return out, nil
}

// resolveTier 解析开通时绑定的层级:指定 tierID → 校验属本 org;否则套组织默认。
// 都没有 → 422(US-01 前置:需有层级)。
func (s *Service) resolveTier(ctx context.Context, orgID int64, tierID, orgDefault *int64) (*model.Tier, error) {
	target := tierID
	if target == nil {
		target = orgDefault
	}
	if target == nil {
		return nil, apperr.InvalidParam("未指定层级且组织未设默认层级")
	}
	t, err := s.store.GetTier(ctx, orgID, *target)
	if errors.Is(err, repo.ErrNotFound) {
		return nil, apperr.InvalidParam("指定层级不存在")
	}
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	return t, nil
}

// ListMembers 列成员(分页/搜索/团队/状态,10 §1.5)。团队负责人只见本团队。
func (s *Service) ListMembers(ctx context.Context, c session.Claims, orgID int64, f repo.MemberFilter) ([]*model.Member, int, error) {
	if err := assertOrgScope(c, orgID); err != nil {
		return nil, 0, err
	}
	if err := assertRole(c, session.RoleOperator, session.RoleOrgAdmin, session.RoleTeamLeader); err != nil {
		return nil, 0, err
	}
	// 团队负责人强制只看本团队(忽略/覆盖 team 过滤)。
	if c.Role == session.RoleTeamLeader {
		tid := c.TeamID
		f.TeamID = &tid
	}
	return s.store.ListMembers(ctx, orgID, f)
}

// GetMember 取成员详情(组织管理员/团队负责人本团队/成员本人)。
func (s *Service) GetMember(ctx context.Context, c session.Claims, orgID, memberID int64) (*model.Member, error) {
	if err := assertOrgScope(c, orgID); err != nil {
		return nil, err
	}
	m, err := s.store.GetMember(ctx, orgID, memberID)
	if errors.Is(err, repo.ErrNotFound) {
		return nil, apperr.NotFound("成员不存在")
	}
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	// 团队负责人越团队 → 403;成员只能看本人。
	if c.Role == session.RoleTeamLeader {
		if err := assertTeamScope(c, m.TeamID); err != nil {
			return nil, err
		}
	}
	if err := assertSelf(c, memberID); err != nil {
		return nil, err
	}
	return m, nil
}

// SetKeyIPWhitelist 设自己 key 的 IP 白名单(E22,token allow_ips,支持单 IP/CIDR;就地更新不旋转 key)。
// 拦截由 new-api 网关数据面执行,与平台可用性解耦(03 §3.4.2)。MVP 仅对本人。
func (s *Service) SetKeyIPWhitelist(ctx context.Context, c session.Claims, orgID, memberID int64, allowIPs string) error {
	if err := assertOrgScope(c, orgID); err != nil {
		return err
	}
	if c.MemberID != memberID {
		return apperr.Forbidden("仅可改本人 key 的 IP 白名单")
	}
	m, err := s.store.GetMember(ctx, orgID, memberID)
	if errors.Is(err, repo.ErrNotFound) {
		return apperr.NotFound("成员不存在")
	}
	if err != nil {
		return apperr.Internal("").WithCause(err)
	}
	if m.BootstrapState != model.BootstrapDone || len(m.AccessTokenEnc) == 0 || m.NewapiTokenID == nil {
		return apperr.New(apperr.CodeInvalidParam, 409, "该成员尚无可用 key")
	}
	accessToken, err := s.keyring.DecryptString(string(m.AccessTokenEnc))
	if err != nil {
		return apperr.Internal("").WithCause(err)
	}
	cred := newapi.MemberCred{NewapiUserID: int(m.NewapiUserID), AccessToken: accessToken}
	spec := newapi.TokenSpec{Name: deriveTokenName(memberID, m.KeyRotation), UnlimitedQuota: true, ExpiredTime: -1, AllowIPs: allowIPs}
	if err := s.upstream.UpdateToken(ctx, cred, int(*m.NewapiTokenID), spec); err != nil {
		return mapUpstream(err)
	}
	s.audit(ctx, c, orgID, "set_key_ip_whitelist", "member", &memberID, map[string]any{"allow_ips": allowIPs})
	return nil
}

// RotateKey 轮换成员的明文 key(US-07,E21)。MVP 仅支持对自己轮换;
// 代他人走支持/协助路径(本里程碑不实现)。返回新明文 key,仅此一次。
func (s *Service) RotateKey(ctx context.Context, c session.Claims, orgID, memberID int64) (apiKey, masked string, err error) {
	if err := assertOrgScope(c, orgID); err != nil {
		return "", "", err
	}
	// MVP:只能轮换自己的 key(US-07 失败分支:代他人不在本期)。
	if c.MemberID != memberID {
		return "", "", apperr.Forbidden("MVP 仅支持轮换本人的 key")
	}
	ctx, cancel := withTimeout(ctx, 20*time.Second)
	defer cancel()

	m, err := s.store.GetMember(ctx, orgID, memberID)
	if errors.Is(err, repo.ErrNotFound) {
		return "", "", apperr.NotFound("成员不存在")
	}
	if err != nil {
		return "", "", apperr.Internal("").WithCause(err)
	}
	if m.BootstrapState != model.BootstrapDone || len(m.AccessTokenEnc) == 0 || m.NewapiTokenID == nil {
		return "", "", apperr.New(apperr.CodeInvalidParam, 409, "该成员尚无可用 key,无法轮换")
	}

	accessToken, err := s.keyring.DecryptString(string(m.AccessTokenEnc))
	if err != nil {
		return "", "", apperr.Internal("").WithCause(err)
	}
	cred := newapi.MemberCred{NewapiUserID: int(m.NewapiUserID), AccessToken: accessToken}
	nextRotation := m.KeyRotation + 1
	spec := newapi.TokenSpec{Name: deriveTokenName(memberID, nextRotation), UnlimitedQuota: true, ExpiredTime: -1}

	newID, newKey, berr := s.upstream.RotateToken(ctx, cred, int(*m.NewapiTokenID), spec)
	if berr != nil {
		return "", "", mapUpstream(berr)
	}
	masked = maskKey(newKey)
	if err := s.store.UpdateMemberKey(ctx, orgID, memberID, int64(newID), masked, nextRotation); err != nil {
		return "", "", apperr.Internal("").WithCause(err)
	}
	s.audit(ctx, c, orgID, "rotate_key", "member", &memberID, map[string]any{"rotation": nextRotation})
	return newKey, masked, nil
}
