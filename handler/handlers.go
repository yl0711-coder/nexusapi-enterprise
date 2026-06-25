package handler

import (
	"net/http"

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
	res, err := h.svc.Login(r.Context(), in.Email, in.Password)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, map[string]any{
		"token":  res.Token,
		"member": toMemberView(res.Member),
	})
}

func (h *Handler) handleMe(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	m, err := h.svc.Me(r.Context(), c)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	// 改动⑥-2:/me 透出 mvp_mode,前端据此藏掉本期封锁的菜单/按钮(真正拦截以后端 mvpGate 为准)。
	writeOK(w, r, http.StatusOK, struct {
		memberView
		MvpMode bool `json:"mvp_mode"`
	}{toMemberView(m), h.mvpMode})
}

// ---- 组织 ----

func (h *Handler) handleListOrgs(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	page, size, offset := parsePaging(r, 20)
	includeArchived := r.URL.Query().Get("include_archived") == "true" // T12:默认隐藏已归档
	orgs, total, err := h.svc.ListOrgs(r.Context(), c, size, offset, includeArchived)
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
}

func (h *Handler) handleCreateOrg(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	var in createOrgReq
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	res, err := h.svc.CreateOrg(r.Context(), c, service.CreateOrgInput{
		Name: in.Name, Slug: in.Slug, AdminEmail: in.AdminEmail, AdminPassword: in.AdminPassword,
		NewapiUserGroup: in.NewapiUserGroup,
	})
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
	})
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
	teams, err := h.svc.ListTeams(r.Context(), c, orgID)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	views := make([]teamView, 0, len(teams))
	for _, t := range teams {
		views = append(views, toTeamView(t))
	}
	writeOK(w, r, http.StatusOK, views)
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
		views = append(views, toTierView(t))
	}
	writeOK(w, r, http.StatusOK, views)
}

type createTierReq struct {
	Name         string           `json:"name"`
	ModelSet     []string         `json:"model_set"`
	ModelCap     map[string]int64 `json:"model_cap"`
	DailyLimit   *int64           `json:"daily_limit"`
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
		Name: in.Name, ModelSet: in.ModelSet, ModelCap: in.ModelCap, DailyLimit: in.DailyLimit,
		WeeklyLimit: in.WeeklyLimit, MonthlyLimit: in.MonthlyLimit, NewapiGroup: in.NewapiGroup,
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
	DailyLimit   *int64            `json:"daily_limit"`
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
		Name: in.Name, DailyLimit: in.DailyLimit, WeeklyLimit: in.WeeklyLimit,
		MonthlyLimit: in.MonthlyLimit, NewapiGroup: in.NewapiGroup,
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
	// api_key 明文仅此一次回显(10 §1.8.1)。
	writeOK(w, r, http.StatusCreated, map[string]any{
		"member_id":        res.MemberID,
		"newapi_user_id":   res.NewapiUserID,
		"api_key":          res.APIKey,
		"key_masked":       res.KeyMasked,
		"login_email":      res.LoginEmail,
		"initial_password": res.InitialPassword, // 仅此一次,交付成员、首登改密
		"tier_id":          res.TierID,
		"models":           res.Models,
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
	writeOK(w, r, http.StatusOK, toMemberView(m))
}

func (h *Handler) handleRotateKey(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	memberID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	apiKey, masked, err := h.svc.RotateKey(r.Context(), c, c.OrgID, memberID)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, map[string]any{"api_key": apiKey, "key_masked": masked})
}

// handleCreateMemberToken 员工自助建/重建 API key,选模型分组(改动③)。
func (h *Handler) handleCreateMemberToken(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	memberID, err := pathInt64(r, "id")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	var in struct {
		Group string `json:"group"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	apiKey, masked, err := h.svc.CreateMemberToken(r.Context(), c, memberID, in.Group)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusCreated, map[string]any{"api_key": apiKey, "key_masked": masked})
}

// handleMemberUsableGroups 列本企业可用模型分组(改动③:自助建 key 的分组选择器)。
func (h *Handler) handleMemberUsableGroups(w http.ResponseWriter, r *http.Request) {
	c, _ := claimsFrom(r.Context())
	groups, err := h.svc.MemberUsableGroups(r.Context(), c)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, map[string]any{"groups": groups})
}
