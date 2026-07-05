package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nexusapi-platform/enterprise/adapter/newapi"
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
	// Associate 非 nil = 门B 关联现有 new-api 用户(v1,20-§8);nil = 门A 新建。进来后同一种组织(19-§3)。
	Associate *AssociateOrgInput
}

// AssociateOrgInput 门B 关联录入(19-F1):企业该 new-api 用户 + 其 access token(企业在 new-api 生成一次粘入)。
type AssociateOrgInput struct {
	NewapiUserID int64  // 必须与 access token 解出的 user 一致(防串号/粘错)
	AccessToken  string // 加密落库(同场景1 存法);平台此后用它全托管令牌
	NamePolicy   string // 导入成员显示名:inherit(默认,继承令牌名)/ random(随机串);管理员可随时改名(19-F2)
}

// CreateOrgResult 建组织产物。AdminInitialPassword 仅本次回显一次(供运营方交付客户管理员)。
type CreateOrgResult struct {
	Org                  *model.Organization
	AdminMemberID        int64
	AdminEmail           string
	AdminInitialPassword string
	ImportedMembers      int // 门B:本次导入成员数
	ImportFailed         int // 门B:导入失败数(可经"重新导入"幂等重跑补齐)
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

	// 门B 关联校验闸(20-§8,**全绿才建组织**,任一不过明确报错不留半截)。
	var assocCred newapi.MemberCred
	var assocUsername string // 门B:企业自己的 new-api 用户名,供历史回填按 username 精确过滤(24-§7.3)
	billingKind := model.BillingKindWallet
	if a := in.Associate; a != nil {
		if a.NewapiUserID <= 0 || a.AccessToken == "" {
			return nil, apperr.InvalidParam("关联模式须提供 new-api 用户 id 与 access token")
		}
		assocCred = newapi.MemberCred{NewapiUserID: int(a.NewapiUserID), AccessToken: a.AccessToken}
		// ① token 有效 + 解出的 user 恰好==录入的(防串号/粘错)。
		info, ierr := s.upstream.GetSelfInfo(ctx, assocCred)
		if ierr != nil {
			return nil, apperr.InvalidParam("access token 无效或已过期,请企业在 new-api 重新生成后再录入")
		}
		assocUsername = info.Username // 门B:回填按此 username 精确拉该企业用户历史日志(24-§7.3)
		if int64(info.ID) != a.NewapiUserID {
			return nil, apperr.InvalidParam(fmt.Sprintf("access token 属于用户 %d,与录入的 %d 不符(防串号,拒绝)", info.ID, a.NewapiUserID))
		}
		// ② role 闸:必须普通用户(拒 admin/超管——最小权限,防越权凭证放大爆炸半径;新-7)。
		if info.Role != newapi.RoleCommonUser {
			return nil, apperr.InvalidParam("该 new-api 用户是管理员/超级管理员,拒绝关联;请企业为组织专门建一个普通用户再生成 token")
		}
		// ③ 录入分组须与该用户在 new-api 的真实分组一致(防呆:分组是折扣/可用模型的归属边界)。
		if info.Group != in.NewapiUserGroup {
			return nil, apperr.InvalidParam(fmt.Sprintf("该用户在 new-api 的分组是 %q,与录入的 %q 不符", info.Group, in.NewapiUserGroup))
		}
		// ④ 未被其它组织关联(一个 new-api 用户只属一个组织;残余并发竞态由 uk_org_newapi_user 兜)。
		if _, taken, derr := s.store.GetOrgIDByNewapiUserID(ctx, a.NewapiUserID); derr != nil {
			return nil, apperr.Internal("").WithCause(derr)
		} else if taken {
			return nil, apperr.Conflict("该 new-api 用户已被其它组织关联")
		}
		// ⑤ 订阅口径(19-§8-①):有 active 订阅 → billing_kind=subscription(不强改企业计费,余额页显示"订阅计费");
		//    无订阅 → wallet + 设 wallet_only(堵订阅旁路 H1;失败由 provision 幂等补设兜底)。
		sub, serr := s.upstream.GetSelfSubscription(ctx, assocCred)
		if serr != nil {
			return nil, mapUpstream(serr)
		}
		if sub.HasActive {
			billingKind = model.BillingKindSub
		} else if sub.BillingPreference != "wallet_only" {
			if perr := s.upstream.SetBillingPreference(ctx, assocCred, "wallet_only"); perr != nil {
				s.log.Warn("关联组织设 wallet_only 失败(provision 幂等补设兜底)", "err", perr)
			}
		}
	}

	orgID, err := s.store.CreateOrganization(ctx, &model.Organization{
		Name: in.Name, Slug: in.Slug, NewapiUserGroup: &in.NewapiUserGroup,
		CreatedByPlatform: in.Associate == nil, // 0022 正交属性:门A=true(可清资产/可自愈)/门B=false(企业资产永不删)
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

	// 门B:绑定企业 user 凭证(password 留空——无密码不能自愈,失效运维重粘,19-§7)+ 导入现有令牌为成员。
	imported, importFailed := 0, 0
	if a := in.Associate; a != nil {
		encTok, eerr := s.keyring.EncryptString(a.AccessToken)
		if eerr != nil {
			return nil, apperr.Internal("").WithCause(eerr)
		}
		// 门B 关联:用企业自己的 new-api 用户名(不落 newapi_username,传空);password 留空(无自愈,运维重粘)。项B 门A-only。
		wrote, werr := s.store.SetOrgNewapiUser(ctx, orgID, a.NewapiUserID, "", []byte(encTok), nil)
		if werr != nil || !wrote {
			// uk_org_newapi_user 并发撞车(两运营方同时关联同一企业 user):补偿归档半截组织,明确报错(F2 不留半截)。
			_ = s.store.SetOrgArchived(ctx, orgID, true)
			if errors.Is(werr, repo.ErrConflict) || werr == nil {
				return nil, apperr.Conflict("该 new-api 用户刚被并发关联(本组织已回收),请核实后重试")
			}
			return nil, apperr.Internal("").WithCause(werr)
		}
		imported, importFailed = s.importOrgTokens(ctx, orgID, assocCred, a.NamePolicy)

		// 历史日志全量回填(24-§7.3):关联成功 → 快照全局 forward 边界 B、插 pending 任务(worker 串行回填)。
		// 非阻断:回填是报表补全(v1 不涉钱),失败不掀翻已成功的关联,可经运营方"重新回填"补。
		if berr := s.enqueueBackfill(ctx, orgID, a.NewapiUserID, assocUsername); berr != nil {
			s.log.Error("历史回填任务创建失败(不阻断关联,可经重新回填补)", "org_id", orgID, "err", berr)
		}
	}

	org, err := s.store.GetOrganization(ctx, orgID)
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	s.audit(ctx, c, orgID, "create_org", "organization", &orgID, map[string]any{
		"name": in.Name, "slug": in.Slug, "admin_email": in.AdminEmail,
		"mode": map[bool]string{true: "associated", false: "created"}[in.Associate != nil],
		"imported": imported, "import_failed": importFailed,
	})

	return &CreateOrgResult{Org: org, AdminMemberID: adminID, AdminEmail: in.AdminEmail, AdminInitialPassword: pw,
		ImportedMembers: imported, ImportFailed: importFailed}, nil
}

// importOrgTokens 门B 导入:把关联用户名下**全部**现有令牌逐个导入为平台成员(19-F2/20-§5)。
// 幂等:SELECT-then-skip(uk_key_token_newapi 已有行=已导入,跳过)——**绝不裸 INSERT 撞唯一键**(insertKeyTokenTx
// 是普通 INSERT,F-C 教训);重跑安全,失败的下次"重新导入"补齐。只读导入:分组/模型限制/状态带过来**不改动**、
// 明文 key 拿不到只存脱敏(员工继续用旧 key);失败复用 F-A 补偿纪律(标失败+释放邮箱),**绝不删企业令牌**(企业资产)。
func (s *Service) importOrgTokens(ctx context.Context, orgID int64, cred newapi.MemberCred, namePolicy string) (imported, failed int) {
	org, oerr := s.store.GetOrganization(ctx, orgID)
	if oerr != nil {
		s.log.Error("导入:读组织失败", "org_id", orgID, "err", oerr)
		return 0, 0
	}
	tokens, lerr := s.upstream.ListUserTokens(ctx, cred)
	if lerr != nil {
		s.log.Error("导入:拉取令牌列表失败(可重新导入)", "org_id", orgID, "err", lerr)
		return 0, 0
	}
	for _, t := range tokens {
		if _, _, found, aerr := s.store.GetMemberByNewapiTokenID(ctx, int64(t.ID)); aerr != nil {
			s.log.Error("导入:查重失败,跳过该令牌", "token_id", t.ID, "err", aerr)
			failed++
			continue
		} else if found {
			continue // 已导入(幂等重跑)
		}
		// A4(五路验收 WB-1):导入的 new-api 令牌名是**外部数据**,须过与 checkName 等价的清洗(挡 <>"'`\+控制字符+截断),
		// 防存储型 XSS(其它命名路径都走 checkName,唯独导入直存);对外部数据用清洗而非硬拒(不因企业令牌名带特殊字符就导入失败)。
		name := sanitizeExternalName(t.Name)
		if namePolicy == "random" || name == "" {
			name = "成员-" + randEmailSuffix()
		}
		grp := t.Group
		memberID, cerr := s.store.CreateMemberProvisional(ctx, &model.Member{
			OrgID: orgID, LoginEmail: org.Slug + "-" + randEmailSuffix() + "@nexus.local",
			DisplayName: &name, Role: string(session.RoleMember), NewapiGroup: &grp,
		})
		if cerr != nil {
			s.log.Error("导入:建成员失败", "token_id", t.ID, "err", cerr)
			failed++
			continue
		}
		masked := t.KeyMasked
		if masked == "" {
			masked = "••••"
		}
		tid := int64(t.ID)
		final := &model.Member{ID: memberID, OrgID: orgID, NewapiTokenID: &tid, KeyMasked: &masked, KeyRotation: 1}
		if ferr := s.store.FinalizeBootstrap(ctx, final, t.Name); ferr != nil {
			if merr := s.store.MarkBootstrapFailedAndRelease(ctx, orgID, memberID); merr != nil {
				s.log.Error("导入:finalize 失败且标记失败也失败", "member_id", memberID, "err", merr)
			}
			s.log.Error("导入:绑定令牌失败(标失败,重新导入可补)", "token_id", t.ID, "member_id", memberID, "err", ferr)
			failed++
			continue
		}
		if t.Status == 2 { // 带过来禁用状态(只改平台侧展示,不动 new-api——只读导入)
			_ = s.store.UpdateMemberStatus(ctx, orgID, memberID, model.MemberStatusDisabled)
		}
		imported++
	}
	s.log.Info("门B 令牌导入完成", "org_id", orgID, "total", len(tokens), "imported", imported, "failed", failed)
	return imported, failed
}

// HardStopOrg v1 运维硬停/解除(20-§4/19-F4,运营方风控):硬停 = **禁用该组织的 new-api 用户**
// (ManageUser disable,双缓存失效、下个请求近实时 403 全部令牌,new-api controller/user.go:977-984);解除 = enable。
// 与余额驱动的停服正交(有钱也能停:欠费纠纷/风控)。硬停期间该 org 的 access token 同样 403 →
// 平台管理写操作被 withOrgCred/EnsureOrgProvisioned 闸屏蔽、不进 401 自愈(防重登失败刷告警)。
// v1 弃 convergeOrgQuotas(按 company_balance 判零逐成员下发 0——审计 F3/H2:会误杀直充组织)。
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
		return apperr.New(apperr.CodeInvalidParam, 409, "组织尚未开通 new-api 池子")
	}
	if uerr := s.upstream.SetUserStatus(ctx, int(uid), !stop); uerr != nil {
		return mapUpstream(uerr)
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
	s.audit(ctx, c, orgID, action, "organization", &orgID, map[string]any{"newapi_user_id": uid})
	return nil
}

// ReimportOrgTokens 门B"重新导入"(运营方,幂等):导入中途失败/后续补齐用。只补建缺的,已导入的跳过。
func (s *Service) ReimportOrgTokens(ctx context.Context, c session.Claims, orgID int64) (imported, failed int, err error) {
	if err := assertRole(c, session.RoleOperator); err != nil {
		return 0, 0, err
	}
	org, gerr := s.store.GetOrganization(ctx, orgID)
	if errors.Is(gerr, repo.ErrNotFound) {
		return 0, 0, apperr.NotFound("组织不存在")
	}
	if gerr != nil {
		return 0, 0, apperr.Internal("").WithCause(gerr)
	}
	if org.CreatedByPlatform {
		return 0, 0, apperr.InvalidParam("仅关联型组织支持重新导入")
	}
	cred, cerr := s.orgCred(ctx, orgID)
	if cerr != nil {
		return 0, 0, cerr
	}
	imported, failed = s.importOrgTokens(ctx, orgID, cred, "inherit")
	s.audit(ctx, c, orgID, "reimport_tokens", "organization", &orgID, map[string]any{"imported": imported, "failed": failed})
	return imported, failed, nil
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

// ListOrgs 列出客户组织(运营方)。includeArchived=false 默认隐藏已归档(T12)。
func (s *Service) ListOrgs(ctx context.Context, c session.Claims, limit, offset int, includeArchived bool) ([]*model.Organization, int, error) {
	if err := assertRole(c, session.RoleOperator); err != nil {
		return nil, 0, err
	}
	return s.store.ListOrganizations(ctx, limit, offset, includeArchived)
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
