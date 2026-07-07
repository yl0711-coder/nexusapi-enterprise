package handler

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/nexusapi-platform/enterprise/pkg/apperr"
	"github.com/nexusapi-platform/enterprise/repo"
	"github.com/nexusapi-platform/enterprise/service"
)

// ---- 认证 ----

type loginReq struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	OTP      string `json:"otp"` // 2FA,本里程碑留钩子未校验
}

func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	var in loginReq
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	// A2:账号级 + IP 级失败退避(纵深防御,叠加 CF 边缘限流)。
	acct := "login:" + strings.ToLower(in.Email)
	ipk := "login-ip:" + clientIP(r)
	if !h.authLim.gate(w, r, acct, ipk) {
		return
	}
	res, err := h.svc.Login(r.Context(), in.Email, in.Password)
	if err != nil {
		if isCredFailure(err) { // 仅凭据错误计入(禁用账号/系统错不误锁)
			h.authLim.fail(acct)
			h.authLim.fail(ipk)
		}
		writeErr(w, r, err)
		return
	}
	h.authLim.reset(acct)
	h.authLim.reset(ipk)
	writeOK(w, r, http.StatusOK, map[string]any{
		"token":  res.Token,
		"member": toMemberView(res.Member),
	})
}

type changePasswordReq struct {
	OldPassword string `json:"old_password"`
	NewPassword string `json:"new_password"`
}

// POST /me/password — 个人设置·自助改平台登录密码(全角色)。
func (h *Handler) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	var in changePasswordReq
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	// A2:改密失败退避——防在线爆破旧密码(按成员 + IP 计数)。
	acct := "chpw:" + strconv.FormatInt(c.MemberID, 10)
	ipk := "chpw-ip:" + clientIP(r)
	if !h.authLim.gate(w, r, acct, ipk) {
		return
	}
	if err := h.svc.ChangePassword(r.Context(), c, in.OldPassword, in.NewPassword); err != nil {
		h.authLim.fail(acct) // 改密失败(旧密码错等)一律计入,节流旧密码猜测
		h.authLim.fail(ipk)
		writeErr(w, r, err)
		return
	}
	h.authLim.reset(acct)
	h.authLim.reset(ipk)
	writeOK(w, r, http.StatusOK, map[string]any{"ok": true})
}

// POST /members/{id}/password:reset — 管理员/团队负责人重置成员登录密码(C22),返回新初始密码一次。
func (h *Handler) handleResetMemberPassword(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	memberID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	pw, err := h.svc.ResetMemberPassword(r.Context(), c, c.OrgID, memberID)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, map[string]any{"initial_password": pw})
}

type updateMeReq struct {
	DisplayName string `json:"display_name"`
}

// PATCH /me — 个人设置·改显示名(全角色,只改本人)。
func (h *Handler) handleUpdateMe(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	var in updateMeReq
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	m, err := h.svc.UpdateMyDisplayName(r.Context(), c, in.DisplayName)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, toMemberView(m))
}

func (h *Handler) handleMe(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	m, err := h.svc.Me(r.Context(), c)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	// 改动⑥-2:/me 透出 mvp_mode,前端据此藏掉本期封锁的菜单/按钮(真正拦截以后端 mvpGate 为准)。
	// A3(28):透出本人组织状态,员工"我的用量·当前状态"显示真实状态(组织硬停等),不再硬编码"正常"。
	// F3(28):透出 API 接入地址(纯展示,未配置则前端不显示接入示例)。
	// 组长契约增补(33 §12-①):顶层回传 quota_per_unit——member/org_admin 靠它做 raw↔美元换算。
	writeOK(w, r, http.StatusOK, struct {
		memberView
		MvpMode        bool   `json:"mvp_mode"`
		OrgStatus      string `json:"org_status,omitempty"`
		GatewayBaseURL string `json:"gateway_base_url,omitempty"`
		QuotaPerUnit   int64  `json:"quota_per_unit"`
	}{toMemberView(m), h.mvpMode, h.svc.MyOrgStatus(r.Context(), c), h.gatewayBaseURL, h.svc.QuotaPerUnitSetting(r.Context())})
}

// ---- 组织 ----

func (h *Handler) handleListOrgs(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	page, size, offset := parsePaging(r, 20)
	includeArchived := r.URL.Query().Get("include_archived") == "true" // T12:默认隐藏已归档
	q := strings.TrimSpace(r.URL.Query().Get("q"))                     // B1(28):服务端搜索(名称/slug)
	orgs, total, err := h.svc.ListOrgs(r.Context(), c, q, size, offset, includeArchived)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	views := make([]orgView, 0, len(orgs))
	for _, o := range orgs {
		views = append(views, toOrgView(o))
	}
	writeOK(w, r, http.StatusOK, listResp{List: views, Pagination: makePageMeta(page, size, total)})
}

// POST /organizations/{id}/archive | /unarchive — 归档/取消归档(运营方,T12)。
func (h *Handler) handleArchiveOrg(w http.ResponseWriter, r *http.Request) {
	h.archiveOrg(w, r, true)
}
func (h *Handler) handleUnarchiveOrg(w http.ResponseWriter, r *http.Request) {
	h.archiveOrg(w, r, false)
}
func (h *Handler) archiveOrg(w http.ResponseWriter, r *http.Request, archived bool) {
	c, _ := claimsFrom(r.Context())
	orgID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	if err := h.svc.SetOrgArchived(r.Context(), c, orgID, archived); err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, map[string]any{"id": orgID, "archived": archived})
}

type createOrgReq struct {
	Name            string `json:"name"`
	Slug            string `json:"slug"`
	AdminEmail      string `json:"admin_email"`
	AdminPassword   string `json:"admin_password"`
	NewapiUserGroup string `json:"newapi_user_group"` // 改动①:运营手填 new-api 用户分组(必填)
	// 门B 关联现有 new-api 用户(v1,19-F1;缺省=门A 新建)。
	Associate *struct {
		NewapiUserID int64  `json:"newapi_user_id"`
		AccessToken  string `json:"access_token"`
		NamePolicy   string `json:"name_policy"` // inherit(默认)/random
	} `json:"associate"`
}

func (h *Handler) handleCreateOrg(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	var in createOrgReq
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	svcIn := service.CreateOrgInput{
		Name: in.Name, Slug: in.Slug, AdminEmail: in.AdminEmail, AdminPassword: in.AdminPassword,
		NewapiUserGroup: in.NewapiUserGroup,
	}
	if in.Associate != nil {
		svcIn.Associate = &service.AssociateOrgInput{
			NewapiUserID: in.Associate.NewapiUserID, AccessToken: in.Associate.AccessToken, NamePolicy: in.Associate.NamePolicy,
		}
	}
	res, err := h.svc.CreateOrg(r.Context(), c, svcIn)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusCreated, map[string]any{
		"org":             toOrgView(res.Org),
		"admin_member_id": res.AdminMemberID,
		"admin_email":     res.AdminEmail,
		// 初始密码仅本次回显一次,供运营方交付客户管理员。
		"admin_initial_password": res.AdminInitialPassword,
		"imported_members":       res.ImportedMembers,
		"import_failed":          res.ImportFailed,
	})
}

// handleHardStop 运维硬停/解除(20-§4,运营方风控):禁用/启用该组织的 new-api 用户,近实时 403 全部令牌。
func (h *Handler) handleHardStop(stop bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, _ := claimsFrom(r.Context())
		orgID, err := pathInt64(r, "id")
		if err != nil {
			writeErr(w, r, err)
			return
		}
		if err := h.svc.HardStopOrg(r.Context(), c, orgID, stop); err != nil {
			writeErr(w, r, err)
			return
		}
		writeOK(w, r, http.StatusOK, map[string]any{"hard_stopped": stop})
	}
}

// handleGetBackfill 读历史回填状态(24-§9:回填中 / 已同步·起点 / 失败)。运营方 + org_admin。
func (h *Handler) handleGetBackfill(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	orgID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	v, err := h.svc.GetBackfillStatus(r.Context(), c, orgID)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, v)
}

// handleRequeueBackfill 运营方"重新回填"(24-§9,幂等)。
func (h *Handler) handleRequeueBackfill(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	orgID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	if err := h.svc.RequeueBackfill(r.Context(), c, orgID); err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, map[string]any{"requeued": true})
}

func (h *Handler) handleGetOrg(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	orgID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	o, err := h.svc.GetOrg(r.Context(), c, orgID)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, toOrgView(o))
}

// ---- 团队 ----

func (h *Handler) handleListTeams(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	orgID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	teams, err := h.svc.ListTeamsWithCounts(r.Context(), c, orgID)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	views := make([]teamCountView, 0, len(teams))
	for _, t := range teams {
		views = append(views, teamCountView{teamView: toTeamView(t.Team), MemberCount: t.MemberCount})
	}
	writeOK(w, r, http.StatusOK, views)
}

// GET /organizations/{id}/teams/{tid} — 团队详情(含成员数,F1/F4)。
func (h *Handler) handleGetTeam(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	orgID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	tid, err := pathInt64(r, "tid")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	t, err := h.svc.GetTeamDetail(r.Context(), c, orgID, tid)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, teamCountView{teamView: toTeamView(t.Team), MemberCount: t.MemberCount})
}

type updateTeamReq struct {
	Name string `json:"name"`
}

// PATCH /organizations/{id}/teams/{tid} — 改团队名(F1·AC-F1-1)。
func (h *Handler) handleUpdateTeam(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	orgID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	tid, err := pathInt64(r, "tid")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	var in updateTeamReq
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	t, err := h.svc.UpdateTeam(r.Context(), c, orgID, tid, in.Name)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, toTeamView(t))
}

// POST /organizations/{id}/teams/{tid}/archive — 归档团队(F1·AC-F1-2/3)。
func (h *Handler) handleArchiveTeam(w http.ResponseWriter, r *http.Request) {
	h.teamStatusOp(w, r, true)
}

// POST /organizations/{id}/teams/{tid}/unarchive — 撤销归档(F1·AC-F1-2)。
func (h *Handler) handleUnarchiveTeam(w http.ResponseWriter, r *http.Request) {
	h.teamStatusOp(w, r, false)
}

func (h *Handler) teamStatusOp(w http.ResponseWriter, r *http.Request, archive bool) {
	c, _ := claimsFrom(r.Context())
	orgID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	tid, err := pathInt64(r, "tid")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	if archive {
		err = h.svc.ArchiveTeam(r.Context(), c, orgID, tid)
	} else {
		err = h.svc.UnarchiveTeam(r.Context(), c, orgID, tid)
	}
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, map[string]any{"ok": true})
}

// GET /organizations/{id}/teams/{tid}/usage — 团队下钻用量(F3·AC-F3-2)。
func (h *Handler) handleTeamUsage(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	orgID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	tid, err := pathInt64(r, "tid")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	u, err := h.svc.TeamUsage(r.Context(), c, orgID, tid, sinceHours(r))
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, usageView(u))
}

type createTeamReq struct {
	Name          string `json:"name"`
	DefaultTierID *int64 `json:"default_tier_id"`
}

func (h *Handler) handleCreateTeam(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	orgID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	var in createTeamReq
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	t, err := h.svc.CreateTeam(r.Context(), c, orgID, service.CreateTeamInput{Name: in.Name, DefaultTierID: in.DefaultTierID})
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusCreated, toTeamView(t))
}

// ---- 层级 ----

func (h *Handler) handleListTiers(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	orgID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	tiers, err := h.svc.ListTiers(r.Context(), c, orgID)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	views := make([]tierView, 0, len(tiers))
	for _, t := range tiers {
		v := toTierView(t)
		// 组长契约增补(33 §12-④):tier 读取随对象带出 grants 数组(best-effort,读挂给空数组)。
		if gs, gerr := h.svc.TierGrants(r.Context(), c, orgID, t.ID); gerr == nil {
			v.Grants = gs
		}
		views = append(views, v)
	}
	writeOK(w, r, http.StatusOK, views)
}

type createTierReq struct {
	Name         string           `json:"name"`
	ModelSet     []string         `json:"model_set"`
	ModelCap     map[string]int64 `json:"model_cap"`
	QuotaType    string           `json:"quota_type"`   // 架构B:fixed(默认)| subscription
	AmountRaw    *int64           `json:"amount_raw"`   // 架构B:额度值(raw)
	ResetPeriod  *string          `json:"reset_period"` // 架构B:daily|weekly|monthly(仅 subscription)
	Visibility   string           `json:"visibility"`   // 架构B:all | assigned(默认)
	DailyLimit   *int64           `json:"daily_limit"`  // Deprecated: 架构A 遗留
	WeeklyLimit  *int64           `json:"weekly_limit"`
	MonthlyLimit *int64           `json:"monthly_limit"`
	NewapiGroup  *string          `json:"newapi_group"`
}

func (h *Handler) handleCreateTier(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	orgID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	var in createTierReq
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	t, err := h.svc.CreateTier(r.Context(), c, orgID, service.CreateTierInput{
		Name: in.Name, ModelSet: in.ModelSet, ModelCap: in.ModelCap,
		QuotaType: in.QuotaType, AmountRaw: in.AmountRaw, ResetPeriod: in.ResetPeriod, Visibility: in.Visibility,
		DailyLimit: in.DailyLimit, WeeklyLimit: in.WeeklyLimit, MonthlyLimit: in.MonthlyLimit, NewapiGroup: in.NewapiGroup,
	})
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusCreated, toTierView(t))
}

// updateTierReq 改层级入参(T10;指针/带 set 标记区分"不改"与"清空")。
type updateTierReq struct {
	Name         *string           `json:"name"`
	ModelSet     *[]string         `json:"model_set"` // 传了(含空数组)=改;不传=保持
	ModelCap     *map[string]int64 `json:"model_cap"`
	QuotaType    *string           `json:"quota_type"`   // 架构B
	AmountRaw    *int64            `json:"amount_raw"`   // 架构B
	ResetPeriod  *string           `json:"reset_period"` // 架构B(传空串=清空)
	Visibility   *string           `json:"visibility"`   // 架构B
	DailyLimit   *int64            `json:"daily_limit"`  // Deprecated: 架构A 遗留
	WeeklyLimit  *int64            `json:"weekly_limit"`
	MonthlyLimit *int64            `json:"monthly_limit"`
	NewapiGroup  *string           `json:"newapi_group"`
}

// PUT /tiers/{id} — 改层级(组织管理员;org 取自会话)。
func (h *Handler) handleUpdateTier(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	tierID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	var in updateTierReq
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	su := service.UpdateTierInput{
		Name: in.Name, QuotaType: in.QuotaType, AmountRaw: in.AmountRaw, Visibility: in.Visibility,
		DailyLimit: in.DailyLimit, WeeklyLimit: in.WeeklyLimit,
		MonthlyLimit: in.MonthlyLimit, NewapiGroup: in.NewapiGroup,
	}
	if in.ResetPeriod != nil {
		if *in.ResetPeriod == "" {
			su.SetResetPeriod = true // 显式清空(subscription→fixed)
		} else {
			su.ResetPeriod = in.ResetPeriod
		}
	}
	if in.ModelSet != nil {
		su.SetModelSet = true
		su.ModelSet = *in.ModelSet
	}
	if in.ModelCap != nil {
		su.SetModelCap = true
		su.ModelCap = *in.ModelCap
	}
	t, err := h.svc.UpdateTier(r.Context(), c, c.OrgID, tierID, su)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, toTierView(t))
}

// DELETE /tiers/{id} — 删层级(组织管理员;org 取自会话)。被引用/默认档 → 409。
func (h *Handler) handleDeleteTier(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	tierID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	if err := h.svc.DeleteTier(r.Context(), c, c.OrgID, tierID); err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, map[string]any{"tier_id": tierID, "deleted": true})
}

func (h *Handler) handleSetDefaultTier(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	tierID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	// 默认层级是组织级操作,org 取自会话(仅组织管理员可调,其 OrgID 即其组织)。
	if err := h.svc.SetDefaultTier(r.Context(), c, c.OrgID, tierID); err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, map[string]any{"tier_id": tierID, "is_default": true})
}

// POST /tiers/{id}/grants — 档位授权到 all/成员/团队(org_admin;33 §12-④)。
func (h *Handler) handleCreateTierGrant(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	tierID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	var in struct {
		TargetType string `json:"target_type"` // all | member | team
		TargetID   int64  `json:"target_id"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	g, err := h.svc.CreateTierGrant(r.Context(), c, c.OrgID, tierID, in.TargetType, in.TargetID)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusCreated, g)
}

// DELETE /tiers/{id}/grants — 撤销档位授权(org_admin;body {grant_id},33 §12-④)。
func (h *Handler) handleDeleteTierGrant(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	tierID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	var in struct {
		GrantID int64 `json:"grant_id"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	if in.GrantID <= 0 {
		writeErr(w, r, apperr.InvalidParam("grant_id 必填"))
		return
	}
	if err := h.svc.DeleteTierGrant(r.Context(), c, c.OrgID, tierID, in.GrantID); err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, map[string]any{"grant_id": in.GrantID, "deleted": true})
}

// ---- 成员 ----

func (h *Handler) handleListMembers(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	orgID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	page, size, offset := parsePaging(r, 20)
	f := repo.MemberFilter{
		Q:      r.URL.Query().Get("q"),
		Status: r.URL.Query().Get("status"),
		Limit:  size,
		Offset: offset,
	}
	if tid := r.URL.Query().Get("team_id"); tid != "" {
		if n := atoiDefault(tid, 0); n > 0 {
			v := int64(n)
			f.TeamID = &v
		}
	}
	members, total, err := h.svc.ListMembers(r.Context(), c, orgID, f)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	views := make([]memberView, 0, len(members))
	for _, m := range members {
		views = append(views, toMemberView(m))
	}
	writeOK(w, r, http.StatusOK, listResp{List: views, Pagination: makePageMeta(page, size, total)})
}

func (h *Handler) handleListAllMembers(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	page, size, offset := parsePaging(r, 50)
	f := repo.MemberFilter{
		Q:      strings.TrimSpace(r.URL.Query().Get("q")),
		Status: strings.TrimSpace(r.URL.Query().Get("status")),
		Limit:  size,
		Offset: offset,
	}
	if oid := strings.TrimSpace(r.URL.Query().Get("org_id")); oid != "" {
		if n, err := strconv.ParseInt(oid, 10, 64); err == nil && n > 0 {
			f.OrgID = &n
		}
	}
	if tid := strings.TrimSpace(r.URL.Query().Get("team_id")); tid != "" {
		if n, err := strconv.ParseInt(tid, 10, 64); err == nil && n > 0 {
			f.TeamID = &n
		}
	}
	items, total, err := h.svc.ListAllMembers(r.Context(), c, f)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, map[string]any{"list": items, "pagination": makePageMeta(page, size, total)})
}

type openMemberReq struct {
	Name   string `json:"name"`
	Email  string `json:"email"`
	TeamID *int64 `json:"team_id"`
	TierID *int64 `json:"tier_id"`
}

func (h *Handler) handleOpenMember(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	orgID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	var in openMemberReq
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	res, err := h.svc.OpenMember(r.Context(), c, orgID, service.OpenMemberInput{
		Name: in.Name, Email: in.Email, TeamID: in.TeamID, TierID: in.TierID,
	})
	if err != nil {
		writeErr(w, r, err)
		return
	}
	// 架构B:开通不铸 key(令牌由成员在 /me/tokens 自助建);回显登录凭证仅此一次(33 §3.2)。
	writeOK(w, r, http.StatusCreated, map[string]any{
		"member_id":         res.MemberID,
		"login_email":       res.LoginEmail,
		"initial_password":  res.InitialPassword, // 仅此一次,交付成员、首登改密
		"newapi_user_id":    res.NewapiUserID,
		"initial_quota_raw": res.InitialQuotaRaw,
		"tier_id":           res.TierID,
		"models":            res.Models,
	})
}

func (h *Handler) handleGetMember(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	memberID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	m, err := h.svc.GetMember(r.Context(), c, c.OrgID, memberID)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	// 架构B(33 §3.5/§12-②):详情附额度快照(remaining_raw/used_raw/granted_raw/token_count,best-effort)。
	snap := h.svc.MemberQuotaSnapshotOf(r.Context(), m)
	// F3(28):本人视角附加档位名/可用模型清单(自助闭环:成员知道自己能调哪些模型),best-effort。
	if c.MemberID == memberID {
		ms, tn := h.svc.MemberTierInfo(r.Context(), c.OrgID, m)
		writeOK(w, r, http.StatusOK, struct {
			memberView
			TierName string                       `json:"tier_name,omitempty"`
			ModelSet []string                     `json:"model_set,omitempty"`
			Quota    *service.MemberQuotaSnapshot `json:"quota,omitempty"`
		}{toMemberView(m), tn, ms, snap})
		return
	}
	writeOK(w, r, http.StatusOK, struct {
		memberView
		Quota *service.MemberQuotaSnapshot `json:"quota,omitempty"`
	}{toMemberView(m), snap})
}
