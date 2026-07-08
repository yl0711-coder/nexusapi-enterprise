package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nexusapi-platform/enterprise/adapter/newapi"
	"github.com/nexusapi-platform/enterprise/model"
	"github.com/nexusapi-platform/enterprise/pkg/apperr"
	"github.com/nexusapi-platform/enterprise/pkg/session"
	"github.com/nexusapi-platform/enterprise/repo"
)

// OpenMemberInput 开通成员入参(架构B,33 §3.5:{name, email?, team_id?, tier_id(必填)})。
type OpenMemberInput struct {
	Name   string
	Email  string // 平台登录邮箱;留空则自动派生
	TeamID *int64
	TierID *int64 // 必填(契约):档位 = 初始额度(amount_raw)+ 分组 的载体
}

// OpenMemberResult 开通成员产物(架构B):**不再铸 key**——令牌全由成员登录组织后台在 /me/tokens 自助建
// (31-ADR §6)。回显 = 平台登录凭证(login_email + 初始密码,仅此一次)+ 服务账号/额度概要。
type OpenMemberResult struct {
	MemberID        int64
	InitialPassword string // 成员平台登录初始密码,仅此一次回显(交付成员、首登改密;C2)
	LoginEmail      string
	NewapiUserID    int64 // 成员服务账号 user id(平台托管;成员不感知、不直连)
	InitialQuotaRaw int64 // 首笔划账额度(raw;金库→成员,守恒)
	TierID          *int64
	Models          []string
}

// OpenMember 开通成员(架构B,31-ADR §2/§11 门A;30-§5;33 §3.2/§3.5):
//
//	① 建平台登录账号(email+初始密码,provisioning 行)
//	② ProvisionMemberServiceAccount saga:建成员专属 new-api user(随机名≤20+撞名重试)→ 凭证加密落库
//	   → SetUserGroup(档位分组)→ wallet_only ×N → 首笔 Transfer(金库→成员,tier.amount_raw)
//	③ 置 active,回显登录凭证(仅此一次)
//
// 金库不足 = 整体失败不半成功(saga 内孤儿走 disable+quarantined,34 §3-③)。
// RBAC(E05):组织管理员(本 org)/ 团队负责人(仅本 team);运营方 403*(经支持会话才行,且开通成员入红线)。
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

	// 校验组织存在 + 硬停闸(硬停期间不开通)+ 金库已开通(成员的钱只能来自金库划账)。
	org, err := s.store.GetOrganization(ctx, orgID)
	if errors.Is(err, repo.ErrNotFound) {
		return nil, apperr.NotFound("组织不存在")
	}
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	if org.Status == model.OrgStatusHardStopped {
		return nil, apperr.New(apperr.CodeForbidden, 403, "组织已被运维硬停,管理操作暂不可用(解除后恢复)")
	}
	// 金库(31-ADR §2:组织=一个 new-api user 持钱池子):门A 惰性开通,幂等;
	// 刚开通的金库额度为 0 → 首笔划账会以「余额不足,请先充值」整体失败(不半成功,31-ADR §14)。
	if _, perr := s.EnsureOrgProvisioned(ctx, orgID, org.Name); perr != nil {
		return nil, perr
	}

	// 校验团队(若指定)属本 org。
	if in.TeamID != nil {
		if _, err := s.store.GetTeam(ctx, orgID, *in.TeamID); errors.Is(err, repo.ErrNotFound) {
			return nil, apperr.InvalidParam("指定团队不存在")
		} else if err != nil {
			return nil, apperr.Internal("").WithCause(err)
		}
	}

	// 档位必填(33 §3.5 契约):额度必设、正数、不可 0/无限(31-ADR §4.3);上限帽由 Provision saga 复核。
	if in.TierID == nil {
		return nil, apperr.InvalidParam("tier_id 必填(架构B:开通成员必须选定档位=初始额度+分组)")
	}
	tier, err := s.store.GetTier(ctx, orgID, *in.TierID)
	if errors.Is(err, repo.ErrNotFound) {
		return nil, apperr.InvalidParam("指定档位不存在")
	}
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	if tier.AmountRaw == nil || *tier.AmountRaw <= 0 {
		return nil, apperr.InvalidParam("该档位未配置有效额度(amount_raw 须为正数),请先按架构B完善档位")
	}
	grp := resolveTokenGroup(tier, org)

	// 自动派生登录邮箱(若未提供)。
	email := in.Email
	if email == "" {
		email = org.Slug + "-" + randEmailSuffix() + "@nexus.local"
	}

	// 平台登录初始密码(bcrypt)。new-api 侧密码由 Provision saga 生成并加密托管(成员永不知道,31-ADR §2)。
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
	tierID := tier.ID
	prov := &model.Member{
		OrgID:                orgID,
		TeamID:               in.TeamID,
		LoginEmail:           email,
		DisplayName:          &displayName,
		Role:                 string(session.RoleMember),
		NewapiGroup:          &grpSnap, // 档位分组快照(成员 user.Group,T17-1 口径沿用)
		TierID:               &tierID,
		PlatformPasswordHash: &platHash,
	}
	memberID, err := s.store.CreateMemberProvisional(ctx, prov)
	if errors.Is(err, repo.ErrConflict) {
		return nil, apperr.Conflict("该邮箱在本组织已存在")
	}
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}

	// 成员服务账号 saga(33 §3.2)。失败分两类:
	//   · CreateUser 前失败(无上游残留)→ 标 failed + 释放邮箱(允许同邮箱重开);
	//   · CreateUser 后失败(saga 内部已 disable+quarantined)→ 保留隔离行(留审计、可重试),不释放。
	uid, perr := s.ProvisionMemberServiceAccount(ctx, orgID, memberID, grp, *tier.AmountRaw, actorOf(c))
	if perr != nil {
		if m, gerr := s.store.GetMember(ctx, orgID, memberID); gerr == nil && m.BootstrapState == model.BootstrapQuarantined {
			s.audit(ctx, c, orgID, "open_member", "member", &memberID, map[string]any{"name": in.Name, "result": "quarantined"})
			return nil, perr
		}
		if merr := s.store.MarkBootstrapFailedAndRelease(ctx, orgID, memberID); merr != nil {
			s.log.Error("标记 bootstrap 失败/释放邮箱失败", "member_id", memberID, "err", merr)
		}
		s.audit(ctx, c, orgID, "open_member", "member", &memberID, map[string]any{"name": in.Name, "result": "provision_failed"})
		return nil, perr
	}

	// 置 active(无令牌:bootstrap done 即就绪,令牌由成员自助)。
	if aerr := s.store.ActivatePlatformAccount(ctx, orgID, memberID); aerr != nil {
		// 服务账号+首笔划账已成功,仅平台状态未置 active:如实报错,重试路径=运营/管理员重开会撞邮箱,
		// 故不释放邮箱;人工把 status 置 active 即可(数据无损,审计有痕)。
		s.log.Error("开通成员:置 active 失败(服务账号已就绪,需补状态)", "member_id", memberID, "err", aerr)
		return nil, apperr.Internal("").WithCause(aerr)
	}

	s.audit(ctx, c, orgID, "open_member", "member", &memberID, map[string]any{
		"name": in.Name, "member_newapi_user_id": uid, "tier_id": tier.ID, "initial_raw": *tier.AmountRaw,
	})

	out := &OpenMemberResult{
		MemberID:        memberID,
		InitialPassword: platPw, // 仅此一次回显;成员首登改密
		LoginEmail:      email,
		NewapiUserID:    int64(uid),
		InitialQuotaRaw: *tier.AmountRaw,
		TierID:          &tierID,
		Models:          tier.ModelSet,
	}
	return out, nil
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

// MemberRowExtra 成员列表行富化(39号复验:33 契约 GET /orgs/:id/members 明写"含额度/已用",
// 原实现只回基本行致列表额度三列全"-")。指针=未开通/读失败时为 nil(FE 显示"-",不冒充 $0)。
type MemberRowExtra struct {
	TierName      *string `json:"tier_name,omitempty"`
	RemainingRaw  *int64  `json:"remaining_raw,omitempty"`   // 成员 user.quota 实时真值
	ConsumedRaw   *int64  `json:"consumed_raw,omitempty"`    // usage_ledger 报表口径
	GrantedNetRaw *int64  `json:"granted_net_raw,omitempty"` // Σ到账 − Σ退回(applied 口径)
}

// EnrichMemberRows 批量富化成员列表行:tier 名一次查表;consumed/granted 本库聚合;
// remaining 有界并发读 new-api(与 sumMemberQuotas 同并发口径)。**fail-open**:任一成员富化
// 失败只置 nil 记 warn,不挂整个列表(列表可用性优先;精确值以详情/余额端点为准)。
func (s *Service) EnrichMemberRows(ctx context.Context, orgID int64, members []*model.Member) map[int64]*MemberRowExtra {
	out := make(map[int64]*MemberRowExtra, len(members))
	tierName := map[int64]string{}
	if tiers, err := s.store.ListTiers(ctx, orgID); err == nil {
		for _, t := range tiers {
			tierName[t.ID] = t.Name
		}
	} else {
		s.log.Warn("成员列表富化:读档位失败(tier_name 置空)", "org_id", orgID, "err", err)
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, balanceSumConcurrency)
	for _, m := range members {
		ex := &MemberRowExtra{}
		out[m.ID] = ex
		if m.TierID != nil {
			if n, ok := tierName[*m.TierID]; ok {
				ex.TierName = &n
			}
		}
		if m.NewapiUserID == nil || *m.NewapiUserID == 0 {
			continue // 未开通:三额度保持 nil
		}
		uid := *m.NewapiUserID
		if c, err := s.store.SumConsumedByNewapiUser(ctx, uid); err == nil {
			ex.ConsumedRaw = &c
		} else {
			s.log.Warn("成员列表富化:读已用失败", "member_id", m.ID, "err", err)
		}
		if g, err := s.store.SumAppliedNetByUser(ctx, uid); err == nil {
			ex.GrantedNetRaw = &g
		} else {
			s.log.Warn("成员列表富化:读累计划入失败", "member_id", m.ID, "err", err)
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(memberID int64, uid int64, ex *MemberRowExtra) {
			defer func() { // 请求路径 fan-out 必带 recover(39号阻断-2 同口径)
				if v := recover(); v != nil {
					s.log.Error("成员列表富化 goroutine panic(已恢复,该成员剩余置空)", "member_id", memberID, "panic", v)
				}
			}()
			defer wg.Done()
			defer func() { <-sem }()
			if q, err := s.upstream.GetUserQuota(ctx, int(uid)); err == nil {
				mu.Lock()
				ex.RemainingRaw = &q
				mu.Unlock()
			} else {
				s.log.Warn("成员列表富化:读剩余失败(置空)", "member_id", memberID, "err", err)
			}
		}(m.ID, uid, ex)
	}
	wg.Wait()
	return out
}

func (s *Service) ListAllMembers(ctx context.Context, c session.Claims, f repo.MemberFilter) ([]repo.MemberOverview, int, error) {
	if err := assertRole(c, session.RoleOperator); err != nil {
		return nil, 0, err
	}
	return s.store.ListAllMembers(ctx, f)
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
	// v1 硬停闸(20-§4)+【#6 涉钱红线 guard,20-§8】。BUG-4(三总监验收):涉钱/红线 guard 必须 **fail-closed**——
	// GetOrganization 出错(DB 瞬时故障)即拒,绝不"读不到就当没硬停/当门A"跳过 guard(否则关联组织+凭证缺失+DB错
	// 三者叠加时,企业池子会被悄悄换绑到平台新空用户上)。
	org, gerr := s.store.GetOrganization(ctx, orgID)
	if gerr != nil {
		return newapi.MemberCred{}, apperr.Internal("").WithCause(gerr) // fail-closed:读不到组织即拒
	}
	if org.Status == model.OrgStatusHardStopped {
		return newapi.MemberCred{}, apperr.New(apperr.CodeForbidden, 403, "组织已被运维硬停,管理操作暂不可用(解除后恢复)")
	}
	if cred, ok, err := s.loadOrgCred(ctx, orgID); err != nil {
		return newapi.MemberCred{}, err
	} else if ok {
		s.ensureWalletOnly(ctx, orgID, cred) // v1 H1(20-§7):幂等补设,不靠一次性动作
		return cred, nil                     // 快路径:已开通(无锁读)
	}
	// 【#6】门B 关联组织(created_by_platform=false)的 new-api user 是**企业资产**:凭证缺失时绝不允许平台
	// "新建 user+初始清零"兜底——新建虽然清的是新 user,但会把组织悄悄换绑到平台 user 上、企业原池子被甩开。
	// 显式拒 + 告警,运维重粘 access token(19-§7)。org 已 fail-closed 读取,此处判断可靠。
	if !org.CreatedByPlatform {
		s.log.Error("关联组织凭证缺失:拒绝平台新建 user 兜底,需运维重粘 access token", "org_id", orgID)
		return newapi.MemberCred{}, apperr.New(apperr.CodeInvalidParam, 409, "关联组织凭证待更新,请运维重新录入 access token")
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
	// v1.1 项B:门A 用户名 = 高熵随机名(取代可猜的 org<id>)+ 撞名有界重生成。AllowAdopt=false:随机名撞"已存在"
	// 一定是撞了外部用户 → adapter 返 UsernameConflict、绝不接管/不 disable → 这里重生成重试。生成后随凭证落库。
	const maxUsernameAttempts = 5
	var res newapi.BootstrapResult
	var username string
	for attempt := 1; ; attempt++ {
		username, err = genOrgUsername()
		if err != nil {
			return newapi.MemberCred{}, apperr.Internal("").WithCause(err)
		}
		r, berr := s.upstream.BootstrapMember(ctx, newapi.BootstrapInput{
			OrgID: orgID, MemberID: 0, Username: username, Password: pw, DisplayName: orgName, SkipToken: true, AllowAdopt: false,
		})
		if berr == nil {
			res = r
			break
		}
		if newapi.IsUsernameConflict(berr) {
			if attempt < maxUsernameAttempts {
				s.log.Warn("org 随机用户名撞名,重生成重试", "org_id", orgID, "attempt", attempt)
				continue
			}
			s.log.Error("org 随机用户名连续撞名达上限(生成器/环境异常,需人工核)", "org_id", orgID, "attempts", maxUsernameAttempts)
		}
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
	wrote, serr := s.store.SetOrgNewapiUser(ctx, orgID, int64(res.NewapiUserID), username, []byte(encAccessNew), []byte(encPw))
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
	// 注(#6):此清零只作用于**本次平台刚 Bootstrap 出来的新 user**;门B 关联组织在函数开头已被 guard 拒绝,
	// 物理到不了这——企业池子余额分文不动(测试 AssocPoolUntouched)。
	if err := s.upstream.ManageUserQuota(ctx, res.NewapiUserID, newapi.QuotaOverride, 0); err != nil {
		s.log.Warn("org user 初始额度清零失败(reconcile 将纠偏)", "org_id", orgID, "err", err)
	}
	cred := newapi.MemberCred{NewapiUserID: res.NewapiUserID, AccessToken: res.AccessToken}
	s.ensureWalletOnly(ctx, orgID, cred) // v1 H1(20-§7):门A 建号即设 wallet_only(堵订阅旁路);失败靠幂等补设
	return cred, nil
}

// ensureWalletOnly v1 订阅口径 H1(20-§7):钱包组织(billing_kind=wallet)幂等确保 new-api 侧
// billing_preference=wallet_only(billing_session.go:404 永不回退订阅)——否则该 user 一旦有 active 订阅,
// 消费走订阅**不扣 user.quota**(quota.go:411-425),读求和余额虚高、原生停服失效。
// 订阅组织(门B 关联时检测到 active 订阅)不碰企业计费方式(19-§8-①)。best-effort:失败告警,下次 provision 重试。
func (s *Service) ensureWalletOnly(ctx context.Context, orgID int64, cred newapi.MemberCred) {
	org, err := s.store.GetOrganization(ctx, orgID)
	if err != nil || org.BillingKind != model.BillingKindWallet {
		return
	}
	sub, gerr := s.upstream.GetSelfSubscription(ctx, cred)
	if gerr != nil {
		s.log.Warn("读订阅偏好失败(wallet_only 待下次补设)", "org_id", orgID, "err", gerr)
		return
	}
	if sub.BillingPreference == "wallet_only" {
		return // 已是目标态(幂等快路径)
	}
	if serr := s.upstream.SetBillingPreference(ctx, cred, "wallet_only"); serr != nil {
		s.log.Error("设 wallet_only 失败(订阅旁路风险,待下次 provision 补设)", "org_id", orgID, "err", serr)
		return
	}
	s.log.Info("org user 已设 wallet_only(堵订阅旁路 H1)", "org_id", orgID)
}

// ReassertWalletOnly B6a:周期再断言钱包组织的 wallet_only(堵订阅旁路 P0-2:组织后续自助买订阅、或当初
// SetBillingPreference 曾失败仅 warn → v2 消费走订阅=硬停失效+双计费)。逐钱包组织重设 wallet_only + 告警 active 订阅。
// 仅 fundingEnabled(v2)下有意义;leader-only。
func (s *Service) ReassertWalletOnly(ctx context.Context) error {
	if !s.fundingEnabled {
		return nil // v1 escrow 休眠:订阅旁路的钱风险仅 v2 触发
	}
	if ok, _, err := s.leadership.CanRunTick(ctx); err != nil {
		return err
	} else if !ok {
		return nil
	}
	orgIDs, err := s.store.ListWalletBillingOrgIDs(ctx)
	if err != nil {
		return err
	}
	for _, orgID := range orgIDs {
		cred, cerr := s.orgCred(ctx, orgID)
		if cerr != nil {
			continue // 未开通池子等,跳过
		}
		sub, gerr := s.upstream.GetSelfSubscription(ctx, cred)
		if gerr != nil {
			s.log.Warn("B6a 读订阅偏好失败(下轮重试)", "org_id", orgID, "err", gerr)
			continue
		}
		if sub.HasActive {
			oid := orgID
			s.log.Error("钱包计费组织出现 active 订阅(订阅旁路风险:消费可能走订阅不扣窗口,人工核)", "org_id", orgID)
			s.auditSystem(ctx, orgID, "subscription_bypass_alert", "organization", &oid, map[string]any{"has_active_sub": true}, "alert")
		}
		if sub.BillingPreference != "wallet_only" {
			if serr := s.upstream.SetBillingPreference(ctx, cred, "wallet_only"); serr != nil {
				s.log.Error("B6a 重设 wallet_only 失败(下轮重试)", "org_id", orgID, "err", serr)
			} else {
				s.log.Info("B6a 已重设 wallet_only(堵订阅旁路)", "org_id", orgID)
			}
		}
	}
	return nil
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

// orgNewapiUsername v1.1 项B:取组织存库的 new-api 用户名(随机名);NULL 遗留组织(灰度清库后不该有)兜底旧 org<id> 口径。
func (s *Service) orgNewapiUsername(ctx context.Context, orgID int64) string {
	if org, err := s.store.GetOrganization(ctx, orgID); err == nil && org.NewapiUsername != nil && *org.NewapiUsername != "" {
		return *org.NewapiUsername
	}
	s.log.Warn("组织无存库 new-api 用户名,兜底旧 org<id> 口径(遗留数据?)", "org_id", orgID)
	return deriveOrgUsername(orgID)
}

// EnsureFreshCred 401 自愈(R5后步骤5,§15):持 per-org 锁 → 锁内先 Probe 现存 access_token(有效即用,别无谓轮换)
// → 失效则用**存的加密密码重登**派生新 token、加密落库 → 返回新凭证。重登失败=放弃返错(调用方报警)。
// org user 凭证当内部密钥用(别给人登以降误旋转);probe-first 复用弱化 OBS-3 旋转竞态。
func (s *Service) EnsureFreshCred(ctx context.Context, orgID int64) (newapi.MemberCred, error) {
	release, lerr := s.quotaLocker.Acquire(ctx, provisionLockKey(orgID))
	if lerr != nil {
		return newapi.MemberCred{}, apperr.Internal("").WithCause(lerr)
	}
	defer release()
	cred, ok, err := s.loadOrgCred(ctx, orgID)
	if err != nil {
		return newapi.MemberCred{}, err
	}
	if !ok {
		return newapi.MemberCred{}, apperr.New(apperr.CodeInvalidParam, 409, "组织尚未开通 new-api 池子")
	}
	if valid, perr := s.upstream.ProbeAccessToken(ctx, cred); perr == nil && valid {
		return cred, nil // 现存令牌有效,复用(不轮换)
	}
	// 失效/探测失败:用存的密码重登派生新 access_token。
	pwEnc, gerr := s.store.GetOrgNewapiPassword(ctx, orgID)
	if gerr != nil {
		return newapi.MemberCred{}, apperr.Internal("").WithCause(gerr)
	}
	pw, derr := s.keyring.DecryptString(string(pwEnc))
	if derr != nil {
		return newapi.MemberCred{}, apperr.Internal("").WithCause(derr)
	}
	// v1.1 项B:重登用**库里存的随机用户名**(不再按 org<id> 猜)。遗留组织(NULL,灰度清库后不该有)兜底旧口径。
	username := s.orgNewapiUsername(ctx, orgID)
	newAccess, rerr := s.upstream.RefreshAccessToken(ctx, newapi.BootstrapInput{
		OrgID: orgID, MemberID: 0, Username: username, Password: pw,
	})
	if rerr != nil {
		return newapi.MemberCred{}, mapUpstream(rerr)
	}
	encNew, eerr := s.keyring.EncryptString(newAccess)
	if eerr != nil {
		return newapi.MemberCred{}, apperr.Internal("").WithCause(eerr)
	}
	if uerr := s.store.UpdateOrgAccessToken(ctx, orgID, []byte(encNew)); uerr != nil {
		return newapi.MemberCred{}, apperr.Internal("").WithCause(uerr)
	}
	return newapi.MemberCred{NewapiUserID: cred.NewapiUserID, AccessToken: newAccess}, nil
}

// withOrgCred 用 org 令牌凭证执行一次上游操作;若返 401(access_token 失效)→ EnsureFreshCred 刷新后**重试仅一次**;
// 仍失败/刷新失败=放弃返原错(调用方 mapUpstream + 已报警)。有界不循环、幂等(401=未执行,重试安全)。
func (s *Service) withOrgCred(ctx context.Context, orgID int64, fn func(cred newapi.MemberCred) error) error {
	// v1 硬停连带效应(20-§4):org 用户被 disable 后其 access token 也 403——硬停期屏蔽全部管理写操作
	// (预期,组织已停)+ 不进 401 自愈(重登失败只会刷告警噪音)。解除硬停后恢复。
	// BUG-4:guard fail-closed——DB 错即拒,绝不"读不到就当没硬停"放行。
	if org, gerr := s.store.GetOrganization(ctx, orgID); gerr != nil {
		return apperr.Internal("").WithCause(gerr)
	} else if org.Status == model.OrgStatusHardStopped {
		return apperr.New(apperr.CodeForbidden, 403, "组织已被运维硬停,管理操作暂不可用(解除后恢复)")
	}
	cred, err := s.orgCred(ctx, orgID)
	if err != nil {
		return err
	}
	opErr := fn(cred)
	if opErr == nil {
		return nil
	}
	var ue *newapi.UpstreamError
	if !errors.As(opErr, &ue) || !ue.AuthExpired() {
		return opErr // 非 401,原样返回
	}
	fresh, ferr := s.EnsureFreshCred(ctx, orgID)
	if ferr != nil {
		s.log.Error("🔴401 自愈:刷新 org 凭证失败,放弃(需人工/下轮重试)", "org_id", orgID, "err", ferr)
		return opErr
	}
	return fn(fresh) // 重试仅一次
}

// MemberQuotaSnapshot 成员额度快照(组长契约增补 33 §12-②规范名,全 int64 raw):
//
//	remaining_raw = 实时剩余(成员 user.quota,读 DB 不读缓存)
//	granted_raw   = Σ净划入(ledger_transfer applied 口径:入账-出账)
//	used_raw      = granted - remaining(派生,clamp≥0;消费真相在 new-api,平台只做分配账本)
//	token_count   = 当前 current+active 自助令牌数(33 §12-⑥:org_admin 只看脱敏+令牌数)
type MemberQuotaSnapshot struct {
	RemainingRaw int64 `json:"remaining_raw"`
	UsedRaw      int64 `json:"used_raw"`
	GrantedRaw   int64 `json:"granted_raw"`
	TokenCount   int   `json:"token_count"`
}

// MemberQuotaSnapshotOf best-effort 取成员额度快照(成员详情页);未开通服务账号/读失败 → nil(FE 显示"—")。
func (s *Service) MemberQuotaSnapshotOf(ctx context.Context, m *model.Member) *MemberQuotaSnapshot {
	if m == nil || m.NewapiUserID == nil {
		return nil
	}
	remaining, err := s.upstream.GetUserQuota(ctx, int(*m.NewapiUserID))
	if err != nil {
		s.log.Warn("成员额度快照:读实时余额失败", "member_id", m.ID, "err", err)
		return nil
	}
	granted, err := s.store.SumAppliedNetByUser(ctx, *m.NewapiUserID)
	if err != nil {
		s.log.Warn("成员额度快照:读账本净划入失败", "member_id", m.ID, "err", err)
		return nil
	}
	used := granted - remaining
	if used < 0 {
		used = 0 // 账本视角外的直充/漂移由对账环抓,展示层 clamp
	}
	n, err := s.store.CountMemberActiveTokens(ctx, m.OrgID, m.ID)
	if err != nil {
		n = 0
	}
	return &MemberQuotaSnapshot{RemainingRaw: remaining, UsedRaw: used, GrantedRaw: granted, TokenCount: n}
}
