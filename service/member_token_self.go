// 架构B 阶段1 · BE① 成员自助令牌(31-ADR §6 / 30-§7 / 29-§4.4 / 33 §3.5 member 组端点)。
//
// 成员登录组织后台,在「我的令牌」页像 new-api 用户那样自管多令牌;后台用**该成员服务账号凭证**
// (WithMemberCred,解密→调→401 自愈)代调 new-api——成员从不直连 new-api(31-ADR §2 主防线)。
//
// 【护栏落点】
//   - token 必建 finite(UnlimitedQuota:false),否则 token 层封顶被短路(31-ADR §12-7);
//   - 分组只列/只收该成员被授权档位的分组(tier_grant 反查,防自升计价档,31-ADR §5);
//   - 令牌数上限读 platform_setting.member_token_limit,repo 事务内行锁 count+insert 防并发绕过(30-§7);
//   - key 不可改(换 key=删了重建);揭示挡 SupportSessionID!=0,审计不记明文(30-§8);
//   - 归属账 member_key_token(append-only)记 token↔member,稳定 key_id 供历史日志归因续用。
package service

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/nexusapi-platform/enterprise/adapter/newapi"
	"github.com/nexusapi-platform/enterprise/model"
	"github.com/nexusapi-platform/enterprise/pkg/apperr"
	"github.com/nexusapi-platform/enterprise/pkg/session"
	"github.com/nexusapi-platform/enterprise/repo"
)

// MyTokenView 成员「我的令牌」列表项(29-§4.4;字段规范名=组长契约增补 33 §12-③:
// quota_raw=令牌配置额度 / remain_raw=剩余 / period_used_raw=本期用量,全 int64 raw,FE 用 quota_per_unit 换算美元)。
type MyTokenView struct {
	ID            int64  `json:"id"` // new-api token id(成员自己 user 下,PATCH/DELETE/reveal 以此定位)
	Name          string `json:"name"`
	KeyMasked     string `json:"key_masked"`
	Group         string `json:"group"`
	QuotaRaw      int64  `json:"quota_raw"`       // 配置额度(= 剩余+已用)
	RemainRaw     int64  `json:"remain_raw"`      // 剩余(raw)
	PeriodUsedRaw int64  `json:"period_used_raw"` // 本期用量(raw,new-api used_quota 原生计数)
	AllowIPs      string `json:"allow_ips"`
	Enabled       bool   `json:"enabled"`
	ExpiredTime   int64  `json:"expired_time"` // -1=永不过期
}

// validateAllowIPs 校验 IP 白名单(C11):逗号分隔,每段须为合法 IP 或 CIDR;空=不限。
func validateAllowIPs(s string) error {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	for _, part := range strings.Split(s, ",") {
		p := strings.TrimSpace(part)
		if p == "" {
			continue
		}
		if net.ParseIP(p) != nil {
			continue
		}
		if _, _, err := net.ParseCIDR(p); err == nil {
			continue
		}
		return apperr.InvalidParam("IP 白名单格式非法(须为单 IP 或 CIDR,逗号分隔):" + p)
	}
	return nil
}

// requireSelfServiceMember /me/tokens* 统一门禁:member-only(33 §3.5 RBAC 铁律,org_admin/operator 无写权)
// + 成员须 active(M1:防停用后拿未过期会话继续操作)+ 服务账号已开通。
// 支持态(SupportSessionID!=0)在 handler 红线白名单已整类拦下,这里聚焦角色/状态。
func (s *Service) requireSelfServiceMember(ctx context.Context, c session.Claims) (*model.Member, error) {
	if err := assertRole(c, session.RoleMember); err != nil {
		return nil, err
	}
	m, err := s.store.GetMember(ctx, c.OrgID, c.MemberID)
	if errors.Is(err, repo.ErrNotFound) {
		return nil, apperr.NotFound("成员不存在")
	}
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	if m.Status != model.MemberStatusActive {
		return nil, apperr.Forbidden("成员已停用,不可操作令牌")
	}
	if m.NewapiUserID == nil {
		return nil, apperr.New(apperr.CodeInvalidParam, 409, "该成员尚未开通服务账号,请联系管理员")
	}
	return m, nil
}

// memberAuthorizedGroups 成员被授权档位的分组集合(tier_grant 反查 ∪ visibility=all ∪ 自身档位;
// NULL 分组回落 组织默认令牌分组 ?? default)。排序去重,分组下拉与建/改校验共用同一真值来源。
func (s *Service) memberAuthorizedGroups(ctx context.Context, m *model.Member) ([]string, error) {
	raws, err := s.store.ListMemberAuthorizedGroups(ctx, m.OrgID, m.ID)
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	org, err := s.store.GetOrganization(ctx, m.OrgID)
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	fallback := "default"
	if org.DefaultTokenGroup != nil && *org.DefaultTokenGroup != "" {
		fallback = *org.DefaultTokenGroup
	}
	set := map[string]bool{}
	for _, g := range raws {
		if g == "" {
			g = fallback
		}
		set[g] = true
	}
	out := make([]string, 0, len(set))
	for g := range set {
		out = append(out, g)
	}
	sort.Strings(out)
	return out, nil
}

// assertGroupAuthorized 建/改令牌的分组校验:必须 ∈ 该成员被授权档位的分组(不是全组织可用分组)。
func (s *Service) assertGroupAuthorized(ctx context.Context, m *model.Member, group string) error {
	groups, err := s.memberAuthorizedGroups(ctx, m)
	if err != nil {
		return err
	}
	for _, g := range groups {
		if g == group {
			return nil
		}
	}
	return apperr.InvalidParam("该分组不在你被授权的档位范围内")
}

// MemberTokenLimit 每成员令牌数上限(platform_setting.member_token_limit;/me/tokens 响应固定回传)。
func (s *Service) MemberTokenLimit(ctx context.Context) int64 {
	n, _ := s.store.GetSettingInt64(ctx, "member_token_limit", 2)
	return n
}

// QuotaPerUnitSetting 平台 quota_per_unit(raw↔美元换算锚;GET /me 顶层回传,33 §12-①)。
func (s *Service) QuotaPerUnitSetting(ctx context.Context) int64 {
	n, _ := s.store.GetSettingInt64(ctx, "quota_per_unit", 500000)
	return n
}

// ListMyTokens GET /me/tokens:本人令牌列表(成员凭证代调 new-api,天然只见自己 user 下的 token)。
// 第二返回值 = token_limit(响应固定回传,33 §12-③)。
func (s *Service) ListMyTokens(ctx context.Context, c session.Claims) ([]MyTokenView, int64, error) {
	m, err := s.requireSelfServiceMember(ctx, c)
	if err != nil {
		return nil, 0, err
	}
	limit := s.MemberTokenLimit(ctx)
	ctx, cancel := withTimeout(ctx, 20*time.Second)
	defer cancel()
	var tokens []newapi.UserToken
	if werr := s.WithMemberCred(ctx, m.ID, func(cred newapi.MemberCred) error {
		ts, e := s.upstream.ListUserTokens(ctx, cred)
		if e != nil {
			return e
		}
		tokens = ts
		return nil
	}); werr != nil {
		return nil, 0, mapUpstream(werr)
	}
	out := make([]MyTokenView, 0, len(tokens))
	for _, t := range tokens {
		out = append(out, MyTokenView{
			ID: int64(t.ID), Name: t.Name, KeyMasked: t.KeyMasked, Group: t.Group,
			QuotaRaw: t.RemainQuota + t.UsedQuota, RemainRaw: t.RemainQuota, PeriodUsedRaw: t.UsedQuota,
			AllowIPs: t.AllowIPs, Enabled: t.Status == 1, ExpiredTime: t.ExpiredTime,
		})
	}
	return out, limit, nil
}

// CreateMyTokenInput POST /me/tokens 入参(33 §3.5:{name, group(限授权集), quota_raw?, allow_ips?})。
type CreateMyTokenInput struct {
	Name     string
	Group    string
	QuotaRaw *int64 // 可选令牌额度(raw);缺省 = 成员额度帽(仍 finite,护栏:绝不 unlimited)
	AllowIPs string
}

// CreateMyToken 建令牌:上游先建(成员凭证)→ 归属账事务内 count+insert 限额 → 超限补偿删上游 token。
// 不回明文 key(镜像 new-api:列表内「复制」走 key:reveal 揭示端点)。
func (s *Service) CreateMyToken(ctx context.Context, c session.Claims, in CreateMyTokenInput) (*MyTokenView, error) {
	m, err := s.requireSelfServiceMember(ctx, c)
	if err != nil {
		return nil, err
	}
	if in.Name == "" {
		return nil, apperr.InvalidParam("令牌名必填")
	}
	if err := checkName("令牌名", in.Name, 64); err != nil {
		return nil, err
	}
	if in.Group == "" {
		return nil, apperr.InvalidParam("请选择分组")
	}
	if err := s.assertGroupAuthorized(ctx, m, in.Group); err != nil {
		return nil, err
	}
	if err := validateAllowIPs(in.AllowIPs); err != nil {
		return nil, err
	}
	// 令牌额度:必 finite(31-ADR §12-7)。缺省 = 成员额度帽(足够大且有限;真上限=成员 user.quota 原生双扣)。
	cap64, _ := s.store.GetSettingInt64(ctx, "member_quota_cap_raw", 500000000)
	quota := cap64
	if in.QuotaRaw != nil {
		if *in.QuotaRaw <= 0 || *in.QuotaRaw > cap64 {
			return nil, apperr.InvalidParam(fmt.Sprintf("令牌额度必须为正且不超过成员上限(%d raw)", cap64))
		}
		quota = *in.QuotaRaw
	}
	limit64, _ := s.store.GetSettingInt64(ctx, "member_token_limit", 2)

	ctx, cancel := withTimeout(ctx, 20*time.Second)
	defer cancel()

	spec := newapi.TokenSpec{
		Name: in.Name, RemainQuota: quota, UnlimitedQuota: false, // 护栏:必 finite
		ExpiredTime: -1, Group: in.Group, AllowIPs: in.AllowIPs,
	}
	var tokenID int
	var masked string
	if werr := s.WithMemberCred(ctx, m.ID, func(cred newapi.MemberCred) error {
		// 重名拒:adapter CreateToken 按名 adopt-existing(幂等),同名会静默复用旧 token → 显式先查重。
		ts, e := s.upstream.ListUserTokens(ctx, cred)
		if e != nil {
			return e
		}
		for _, t := range ts {
			if t.Name == in.Name {
				return apperr.Conflict("已存在同名令牌,请换名")
			}
		}
		id, e := s.upstream.CreateToken(ctx, cred, spec)
		if e != nil {
			return e
		}
		tokenID = id
		// 取脱敏 key(列表回读,不取明文)。
		ts2, e := s.upstream.ListUserTokens(ctx, cred)
		if e == nil {
			for _, t := range ts2 {
				if t.ID == id {
					masked = t.KeyMasked
				}
			}
		}
		return nil
	}); werr != nil {
		var ae *apperr.Error
		if errors.As(werr, &ae) {
			return nil, ae
		}
		return nil, mapUpstream(werr)
	}

	// 归属账:事务内行锁 count+insert(超限=整事务回滚)→ 超限/失败补偿删上游 token(回到无孤儿干净态)。
	if _, ierr := s.store.InsertMemberTokenWithLimit(ctx, m.OrgID, m.ID, int(limit64), int64(tokenID), in.Name, masked); ierr != nil {
		if derr := s.WithMemberCred(ctx, m.ID, func(cred newapi.MemberCred) error {
			return s.upstream.DeleteToken(ctx, cred, tokenID)
		}); derr != nil {
			s.log.Error("建令牌补偿删除失败(孤儿 token 待清)", "member_id", m.ID, "token_id", tokenID, "err", derr)
		}
		if errors.Is(ierr, repo.ErrTokenLimit) {
			return nil, apperr.New(apperr.CodeInvalidParam, 409, fmt.Sprintf("令牌数已达上限(%d),请先删除不用的令牌", limit64))
		}
		return nil, apperr.Internal("").WithCause(ierr)
	}
	s.audit(ctx, c, m.OrgID, "create_my_token", "member", &m.ID, map[string]any{
		"token_id": tokenID, "name": in.Name, "group": in.Group, "quota_raw": quota,
	})
	return &MyTokenView{ID: int64(tokenID), Name: in.Name, KeyMasked: masked, Group: in.Group,
		QuotaRaw: quota, RemainRaw: quota, AllowIPs: in.AllowIPs, Enabled: true, ExpiredTime: -1}, nil
}

// UpdateMyTokenInput PATCH /me/tokens/:id 入参(nil=不改)。key/名不可改(换 key=删了重建,31-ADR §6)。
type UpdateMyTokenInput struct {
	Group    *string
	QuotaRaw *int64
	AllowIPs *string
}

// UpdateMyToken 改令牌(分组限授权集/令牌额度/IP;其余字段按现值重申,防被清空;强制 finite)。
func (s *Service) UpdateMyToken(ctx context.Context, c session.Claims, tokenID int64, in UpdateMyTokenInput) error {
	m, err := s.requireSelfServiceMember(ctx, c)
	if err != nil {
		return err
	}
	if in.Group == nil && in.QuotaRaw == nil && in.AllowIPs == nil {
		return apperr.InvalidParam("没有要修改的字段")
	}
	owns, oerr := s.store.MemberOwnsToken(ctx, m.OrgID, m.ID, tokenID)
	if oerr != nil {
		return apperr.Internal("").WithCause(oerr)
	}
	if !owns {
		return apperr.NotFound("令牌不存在") // 归属闸:不暴露他人 token 存在性(assertSelf 纵深)
	}
	cap64, _ := s.store.GetSettingInt64(ctx, "member_quota_cap_raw", 500000000)
	if in.QuotaRaw != nil && (*in.QuotaRaw <= 0 || *in.QuotaRaw > cap64) {
		return apperr.InvalidParam(fmt.Sprintf("令牌额度必须为正且不超过成员上限(%d raw)", cap64))
	}
	if in.Group != nil {
		if *in.Group == "" {
			return apperr.InvalidParam("分组不能为空")
		}
		if err := s.assertGroupAuthorized(ctx, m, *in.Group); err != nil {
			return err
		}
	}
	if in.AllowIPs != nil {
		if err := validateAllowIPs(*in.AllowIPs); err != nil {
			return err
		}
	}
	ctx, cancel := withTimeout(ctx, 20*time.Second)
	defer cancel()
	if werr := s.WithMemberCred(ctx, m.ID, func(cred newapi.MemberCred) error {
		ts, e := s.upstream.ListUserTokens(ctx, cred)
		if e != nil {
			return e
		}
		var cur *newapi.UserToken
		for i := range ts {
			if int64(ts[i].ID) == tokenID {
				cur = &ts[i]
				break
			}
		}
		if cur == nil {
			return apperr.NotFound("令牌不存在")
		}
		spec := newapi.TokenSpec{
			Name:           cur.Name, // 名不可改:按现名重申
			RemainQuota:    cur.RemainQuota,
			UnlimitedQuota: false, // 护栏:强制 finite(即使历史行是 unlimited 也借本次更新收敛)
			ExpiredTime:    cur.ExpiredTime,
			Group:          cur.Group,
			AllowIPs:       cur.AllowIPs,
			ModelLimits:    splitModelLimits(cur.ModelLimits),
		}
		if in.Group != nil {
			spec.Group = *in.Group
		}
		if in.QuotaRaw != nil {
			spec.RemainQuota = *in.QuotaRaw
		}
		if in.AllowIPs != nil {
			spec.AllowIPs = *in.AllowIPs
		}
		return s.upstream.UpdateToken(ctx, cred, int(tokenID), spec)
	}); werr != nil {
		var ae *apperr.Error
		if errors.As(werr, &ae) {
			return ae
		}
		return mapUpstream(werr)
	}
	s.audit(ctx, c, m.OrgID, "update_my_token", "member", &m.ID, map[string]any{"token_id": tokenID})
	return nil
}

// DeleteMyToken 删令牌(new-api 删除 + 归属账置 revoked;append-only,历史日志仍按旧 token_id 归因)。
func (s *Service) DeleteMyToken(ctx context.Context, c session.Claims, tokenID int64) error {
	m, err := s.requireSelfServiceMember(ctx, c)
	if err != nil {
		return err
	}
	owns, oerr := s.store.MemberOwnsToken(ctx, m.OrgID, m.ID, tokenID)
	if oerr != nil {
		return apperr.Internal("").WithCause(oerr)
	}
	if !owns {
		return apperr.NotFound("令牌不存在")
	}
	ctx, cancel := withTimeout(ctx, 20*time.Second)
	defer cancel()
	if werr := s.WithMemberCred(ctx, m.ID, func(cred newapi.MemberCred) error {
		return s.upstream.DeleteToken(ctx, cred, int(tokenID)) // 上游幂等:不存在视为成功
	}); werr != nil {
		return mapUpstream(werr)
	}
	if rerr := s.store.RevokeMemberToken(ctx, m.OrgID, m.ID, tokenID); rerr != nil {
		// 上游已删、归属账未收尾:如实报错,重调本端点收敛(上游删幂等)。
		return apperr.Internal("").WithCause(rerr)
	}
	s.audit(ctx, c, m.OrgID, "delete_my_token", "member", &m.ID, map[string]any{"token_id": tokenID})
	return nil
}

// RevealMyTokenKey POST /me/tokens/:id/key:reveal 揭示明文(仅本人;成员凭证代调;明文只即时回传,
// 绝不落库/不写日志)。显式挡 SupportSessionID!=0(30-§8 护栏,红线白名单之外的第二道闸)。
func (s *Service) RevealMyTokenKey(ctx context.Context, c session.Claims, tokenID int64) (string, error) {
	if c.SupportSessionID != 0 {
		return "", apperr.Forbidden("支持态不可读取明文 key(需客户本人)")
	}
	m, err := s.requireSelfServiceMember(ctx, c)
	if err != nil {
		return "", err
	}
	owns, oerr := s.store.MemberOwnsToken(ctx, m.OrgID, m.ID, tokenID)
	if oerr != nil {
		return "", apperr.Internal("").WithCause(oerr)
	}
	if !owns {
		return "", apperr.NotFound("令牌不存在")
	}
	ctx, cancel := withTimeout(ctx, 20*time.Second)
	defer cancel()
	var key string
	if werr := s.WithMemberCred(ctx, m.ID, func(cred newapi.MemberCred) error {
		k, e := s.upstream.RevealTokenKey(ctx, cred, int(tokenID))
		if e != nil {
			return e
		}
		key = k
		return nil
	}); werr != nil {
		return "", mapUpstream(werr)
	}
	// 揭示审计(兜底追责):操作者/目标 token/时间——绝不记 key 明文。
	s.audit(ctx, c, m.OrgID, "reveal_my_token_key", "member", &m.ID, map[string]any{"token_id": tokenID})
	return key, nil
}

// splitModelLimits new-api 令牌 model_limits 线格式为逗号串;改令牌重申现值时解析回 []string。
func splitModelLimits(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
