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
	if err := checkName("姓名", in.Name, maxNameLen); err != nil {
		return nil, err
	}
	// T11:自定义登录名(真实邮箱或用户名)提供时校验格式;留空则下面派生合成邮箱 fallback。
	if in.Email != "" {
		if err := checkLoginName(in.Email); err != nil {
			return nil, err
		}
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
	// 解析令牌计价分组(D1 两级:tier ?? org 默认 ?? default,T17-1)。
	grp := resolveTokenGroup(tier, org)

	// 自动派生登录邮箱(若未提供)。
	email := in.Email
	if email == "" {
		email = org.Slug + "-" + randEmailSuffix() + "@nexus.local"
	}

	// 平台登录初始密码(bcrypt)。模型2:member 不持 new-api 用户,不再生成/存 new-api 密码(那归 organization)。
	platPw, err := genPlatformPassword()
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	platHash, err := hashPassword(platPw)
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}

	displayName := in.Name
	grpSnap := grp
	prov := &model.Member{
		OrgID:                orgID,
		TeamID:               in.TeamID,
		LoginEmail:           email,
		DisplayName:          &displayName,
		Role:                 string(session.RoleMember),
		NewapiGroup:          &grpSnap, // 令牌分组快照(T17-1)
		PlatformPasswordHash: &platHash,
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

	// 模型2:确保组织有 new-api user(池子锚),员工 token 挂其下。EnsureOrgProvisioned 幂等(确定性 username adopt)。
	cred, perr := s.EnsureOrgProvisioned(ctx, orgID, org.Name)
	if perr != nil {
		// 组织池子未就绪:org user 共享、由 EnsureOrgProvisioned 自身幂等管,无孤儿可留;仅标成员失败 + 释放邮箱。
		if merr := s.store.MarkBootstrapFailedAndRelease(ctx, orgID, memberID); merr != nil {
			s.log.Error("标记 bootstrap 失败/释放邮箱失败", "member_id", memberID, "err", merr)
		}
		s.audit(ctx, c, orgID, "open_member", "member", &memberID, map[string]any{"name": in.Name, "result": "org_provision_failed"})
		return nil, perr
	}

	// 把员工令牌分组加进 org 可用分组(§3 硬约束,不补则令牌用业务分组 403)+ total 折扣覆盖新分组。best-effort,须在建 token 前。
	if err := s.upstream.AddOrgUsableGroup(ctx, s.orgUserGroup(ctx, orgID), grp); err != nil {
		s.log.Warn("加可用分组失败(令牌用业务分组会 403,需补)", "member_id", memberID, "group", grp, "err", err)
	}
	s.ensureTotalDiscountCoversGroup(ctx, orgID, grp)

	// 建员工 token(挂 org user 下,用 org 凭证)。观测期 SkipToken:只开账号,令牌由员工自助建(改动③)。
	final := &model.Member{ID: memberID, OrgID: orgID, TierID: prov.TierID, TeamID: in.TeamID}
	keyMasked, apiKey, finalTokenName := "", "", ""
	if !s.observeMode {
		spec := newapi.TokenSpec{Name: deriveTokenName(memberID, 1), UnlimitedQuota: true, ExpiredTime: -1, Group: grp}
		if tier != nil {
			spec.ModelLimits = tier.ModelSet // B2:网关数据面限模型(真拦截)
		}
		tokenID, berr := s.upstream.CreateToken(ctx, cred, spec)
		if berr != nil {
			// 建 token 失败:残留 token(若有)靠确定性名在重开时 adopt 自愈,无需禁用;标失败 + 释放邮箱。
			if merr := s.store.MarkBootstrapFailedAndRelease(ctx, orgID, memberID); merr != nil {
				s.log.Error("标记 bootstrap 失败/释放邮箱失败", "member_id", memberID, "err", merr)
			}
			s.audit(ctx, c, orgID, "open_member", "member", &memberID, map[string]any{"name": in.Name, "result": "create_token_failed"})
			return nil, mapUpstream(berr)
		}
		key, kerr := s.upstream.RevealTokenKey(ctx, cred, tokenID)
		if kerr != nil {
			// token 已建、取 key 失败:不回滚(确定性名重开可补取);标失败留重试。
			if merr := s.store.MarkBootstrapFailedAndRelease(ctx, orgID, memberID); merr != nil {
				s.log.Error("标记 bootstrap 失败失败", "member_id", memberID, "err", merr)
			}
			return nil, mapUpstream(kerr)
		}
		tid := int64(tokenID)
		keyMasked, apiKey, finalTokenName = maskKey(key), key, deriveTokenName(memberID, 1)
		final.NewapiTokenID = &tid
		final.KeyMasked = &keyMasked
		final.KeyRotation = 1
	}
	if err := s.store.FinalizeBootstrap(ctx, final, finalTokenName); err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}

	s.audit(ctx, c, orgID, "open_member", "member", &memberID, map[string]any{
		"name": in.Name, "org_newapi_user_id": cred.NewapiUserID,
	})

	out := &OpenMemberResult{
		MemberID:        memberID,
		APIKey:          apiKey,
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

// resolveTokenGroup 解析成员生效令牌计价分组(D1 两级,T17-1):
// tier.NewapiGroup ?? org.DefaultTokenGroup ?? "default"。
func resolveTokenGroup(tier *model.Tier, org *model.Organization) string {
	if tier != nil && tier.NewapiGroup != nil && *tier.NewapiGroup != "" {
		return *tier.NewapiGroup
	}
	if org != nil && org.DefaultTokenGroup != nil && *org.DefaultTokenGroup != "" {
		return *org.DefaultTokenGroup
	}
	return "default"
}

// memberTokenGroup 取成员令牌分组快照(轮换/白名单复用,防丢回 default;空=default)。
func memberTokenGroup(m *model.Member) string {
	if m != nil && m.NewapiGroup != nil && *m.NewapiGroup != "" {
		return *m.NewapiGroup
	}
	return "default"
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
	if m.BootstrapState != model.BootstrapDone || m.NewapiTokenID == nil {
		return apperr.New(apperr.CodeInvalidParam, 409, "该成员尚无可用 key")
	}
	cred, err := s.orgCred(ctx, orgID)
	if err != nil {
		return err
	}
	// 重申令牌分组快照,防白名单更新把令牌分组丢回 default(T17-1/Q2)。
	spec := newapi.TokenSpec{Name: deriveTokenName(memberID, m.KeyRotation), UnlimitedQuota: true, ExpiredTime: -1, AllowIPs: allowIPs, Group: memberTokenGroup(m)}
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
	if m.BootstrapState != model.BootstrapDone || m.NewapiTokenID == nil {
		return "", "", apperr.New(apperr.CodeInvalidParam, 409, "该成员尚无可用 key,无法轮换")
	}
	cred, err := s.orgCred(ctx, orgID)
	if err != nil {
		return "", "", err
	}
	nextRotation := m.KeyRotation + 1
	// 轮换重申令牌分组快照,防新 token 丢回 default(T17-1/Q2 必测)。
	spec := newapi.TokenSpec{Name: deriveTokenName(memberID, nextRotation), UnlimitedQuota: true, ExpiredTime: -1, Group: memberTokenGroup(m)}

	newID, newKey, berr := s.upstream.RotateToken(ctx, cred, int(*m.NewapiTokenID), spec)
	if berr != nil {
		return "", "", mapUpstream(berr)
	}
	masked = maskKey(newKey)
	if err := s.store.UpdateMemberKey(ctx, orgID, memberID, int64(newID), masked, spec.Name, nextRotation); err != nil {
		return "", "", apperr.Internal("").WithCause(err)
	}
	s.audit(ctx, c, orgID, "rotate_key", "member", &memberID, map[string]any{"rotation": nextRotation})
	return newKey, masked, nil
}

// MemberUsableGroups 列本企业可用的模型分组(改动③:自助建 key 的分组选择器只列这些,不暴露全系统分组)。
// 取调用者所属组织的用户分组 → GetOrgUsableGroups。
func (s *Service) MemberUsableGroups(ctx context.Context, c session.Claims) ([]string, error) {
	groups, err := s.upstream.GetOrgUsableGroups(ctx, s.orgUserGroup(ctx, c.OrgID))
	if err != nil {
		return nil, mapUpstream(err)
	}
	return groups, nil
}

// provisionLockKey 是组织首开(建 org user + 落库凭证)的 per-org 串行锁键(R5 OBS-3)。
func provisionLockKey(orgID int64) string { return fmt.Sprintf("provision:org:%d", orgID) }

// loadOrgCred 读组织已落库的池子凭证(解密 access_token)。ok=false=尚未开通;组织不存在→NotFound。
func (s *Service) loadOrgCred(ctx context.Context, orgID int64) (newapi.MemberCred, bool, error) {
	uid, encAccess, ok, err := s.store.GetOrgNewapiCred(ctx, orgID)
	if errors.Is(err, repo.ErrNotFound) {
		return newapi.MemberCred{}, false, apperr.NotFound("组织不存在")
	}
	if err != nil {
		return newapi.MemberCred{}, false, apperr.Internal("").WithCause(err)
	}
	if !ok {
		return newapi.MemberCred{}, false, nil
	}
	at, derr := s.keyring.DecryptString(string(encAccess))
	if derr != nil {
		return newapi.MemberCred{}, false, apperr.Internal("").WithCause(derr)
	}
	return newapi.MemberCred{NewapiUserID: int(uid), AccessToken: at}, true, nil
}

// EnsureOrgProvisioned 模型2:确保组织有一个 new-api user(池子锚),返回其凭证(给 org 下建员工 token 用)。
// 已开通→直接返回;未开通→建 org user(确定性 username,adopt-existing 幂等)+取 access_token,加密存。
// R5 OBS-3:持 per-org 开通锁 + 锁内 double-check 串行化首开——消除并发首开时"第二次 adopt 旋转作废前者
// access_token、写序与锁解耦致落库失效 token"的竞态;SetOrgNewapiUser 再加 IS NULL 守卫(多节点兜底)。
func (s *Service) EnsureOrgProvisioned(ctx context.Context, orgID int64, orgName string) (newapi.MemberCred, error) {
	if cred, ok, err := s.loadOrgCred(ctx, orgID); err != nil {
		return newapi.MemberCred{}, err
	} else if ok {
		return cred, nil // 快路径:已开通(无锁读)
	}
	release, lerr := s.quotaLocker.Acquire(ctx, provisionLockKey(orgID))
	if lerr != nil {
		return newapi.MemberCred{}, apperr.Internal("").WithCause(lerr)
	}
	defer release()
	if cred, ok, err := s.loadOrgCred(ctx, orgID); err != nil {
		return newapi.MemberCred{}, err
	} else if ok {
		return cred, nil // 等锁期间别的请求已开通
	}
	// 未开通:建 org user(SkipToken=true:只建 user + 取 access_token,不建令牌)。
	pw, err := genNewapiPassword()
	if err != nil {
		return newapi.MemberCred{}, apperr.Internal("").WithCause(err)
	}
	res, berr := s.upstream.BootstrapMember(ctx, newapi.BootstrapInput{
		OrgID: orgID, MemberID: 0, Username: deriveOrgUsername(orgID), Password: pw, DisplayName: orgName, SkipToken: true,
	})
	if berr != nil {
		return newapi.MemberCred{}, mapUpstream(berr)
	}
	encAccessNew, err := s.keyring.EncryptString(res.AccessToken)
	if err != nil {
		return newapi.MemberCred{}, apperr.Internal("").WithCause(err)
	}
	encPw, err := s.keyring.EncryptString(pw)
	if err != nil {
		return newapi.MemberCred{}, apperr.Internal("").WithCause(err)
	}
	wrote, serr := s.store.SetOrgNewapiUser(ctx, orgID, int64(res.NewapiUserID), []byte(encAccessNew), []byte(encPw))
	if serr != nil {
		return newapi.MemberCred{}, apperr.Internal("").WithCause(serr)
	}
	if !wrote {
		// 多节点首开竞态(本进程锁管不到跨节点):别处已先落库凭证。本次新建的 newapi user 成无主孤儿(待清),改用已落库凭证。
		s.log.Warn("org 池子凭证已被并发写入(多节点首开竞态),本次新建 user 成孤儿待清", "org_id", orgID, "orphan_user", res.NewapiUserID)
		if cred, ok, lerr := s.loadOrgCred(ctx, orgID); lerr == nil && ok {
			return cred, nil
		}
		return newapi.MemberCred{}, apperr.Internal("org 凭证落库竞态且回读失败")
	}
	// 设 org user 分组 = org_{id}(折扣按用户分组归属,A2)。best-effort。
	if err := s.upstream.SetUserGroup(ctx, res.NewapiUserID, s.orgUserGroup(ctx, orgID)); err != nil {
		s.log.Warn("设 org user 分组失败(可后续补)", "org_id", orgID, "err", err)
	}
	// 模型2 escrow 对账前提:org user 初始额度/邀请赠送清零,使"已释放(桶1)==newapi(quota+used)"恒成立。
	// 一次性、刚建无令牌无消费,override 0 安全(override 禁令针对花钱热路径,不含此处)。失败也由 reconcile 自愈。
	if err := s.upstream.ManageUserQuota(ctx, res.NewapiUserID, newapi.QuotaOverride, 0); err != nil {
		s.log.Warn("org user 初始额度清零失败(reconcile 将纠偏)", "org_id", orgID, "err", err)
	}
	return newapi.MemberCred{NewapiUserID: res.NewapiUserID, AccessToken: res.AccessToken}, nil
}

// orgCred 取组织的 new-api 凭证(模型2:员工 token 一律在 org user 下建/管,用 org 的 user_id + access_token)。
// 组织尚未开通池子 user → 409;access_token 解密自 organization.newapi_access_token_enc。
func (s *Service) orgCred(ctx context.Context, orgID int64) (newapi.MemberCred, error) {
	uid, encAccess, ok, err := s.store.GetOrgNewapiCred(ctx, orgID)
	if err != nil {
		return newapi.MemberCred{}, apperr.Internal("").WithCause(err)
	}
	if !ok {
		return newapi.MemberCred{}, apperr.New(apperr.CodeInvalidParam, 409, "组织尚未开通 new-api 池子,无法操作令牌")
	}
	accessToken, err := s.keyring.DecryptString(string(encAccess))
	if err != nil {
		return newapi.MemberCred{}, apperr.Internal("").WithCause(err)
	}
	return newapi.MemberCred{NewapiUserID: int(uid), AccessToken: accessToken}, nil
}

// CreateMemberToken 员工自助建/重建 API key,选一个本企业可用的模型分组(改动③·方案A 单 key)。
// 方案A:平台只跟踪最近一枚——首次(无令牌)CreateToken;重建(已有令牌)RotateToken 替换上一枚(旧 key 失效)。
// RBAC:仅本人(MVP)。校验所选分组 ∈ 本企业可用模型分组(隔离边界:不能选别家分组)。返回明文 key(仅回显一次)+ 脱敏。
func (s *Service) CreateMemberToken(ctx context.Context, c session.Claims, memberID int64, group string) (apiKey, masked string, err error) {
	if c.MemberID != memberID {
		return "", "", apperr.Forbidden("仅支持为本人建 key")
	}
	if group == "" {
		return "", "", apperr.InvalidParam("请选择一个模型分组")
	}
	ctx, cancel := withTimeout(ctx, 20*time.Second)
	defer cancel()

	m, err := s.store.GetMember(ctx, c.OrgID, memberID)
	if errors.Is(err, repo.ErrNotFound) {
		return "", "", apperr.NotFound("成员不存在")
	}
	if err != nil {
		return "", "", apperr.Internal("").WithCause(err)
	}
	cred, derr := s.orgCred(ctx, c.OrgID)
	if derr != nil {
		return "", "", derr
	}

	// 隔离边界:所选分组必须在本企业可用模型分组内(default 天然可用)。
	if group != "default" {
		usable, gerr := s.upstream.GetOrgUsableGroups(ctx, s.orgUserGroup(ctx, c.OrgID))
		if gerr != nil {
			return "", "", mapUpstream(gerr)
		}
		ok := false
		for _, g := range usable {
			if g == group {
				ok = true
				break
			}
		}
		if !ok {
			return "", "", apperr.InvalidParam("该模型分组不在本企业可用范围内")
		}
	}

	nextRotation := m.KeyRotation + 1
	spec := newapi.TokenSpec{Name: deriveTokenName(memberID, nextRotation), UnlimitedQuota: true, ExpiredTime: -1, Group: group}

	var newID int
	var newKey string
	if m.NewapiTokenID == nil {
		tid, berr := s.upstream.CreateToken(ctx, cred, spec) // 首次自助建
		if berr != nil {
			return "", "", mapUpstream(berr)
		}
		k, berr := s.upstream.RevealTokenKey(ctx, cred, tid)
		if berr != nil {
			return "", "", mapUpstream(berr)
		}
		newID, newKey = tid, k
	} else {
		id, k, berr := s.upstream.RotateToken(ctx, cred, int(*m.NewapiTokenID), spec) // 重建:替换上一枚
		if berr != nil {
			return "", "", mapUpstream(berr)
		}
		newID, newKey = id, k
	}
	masked = maskKey(newKey)
	if err := s.store.UpdateMemberKey(ctx, c.OrgID, memberID, int64(newID), masked, spec.Name, nextRotation); err != nil {
		return "", "", apperr.Internal("").WithCause(err)
	}
	s.audit(ctx, c, c.OrgID, "create_member_token", "member", &memberID, map[string]any{"group": group, "rotation": nextRotation})
	return newKey, masked, nil
}
