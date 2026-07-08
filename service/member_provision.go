// 架构B 阶段0(33 §3.2):成员服务账号 Provision saga + 成员凭证代调 + 上线闸。
// 成员 = 各自一个 new-api user(平台托管服务账号):密码平台生成、凭证加密存平台、成员永不直连 new-api。
package service

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"strings"

	"github.com/nexusapi-platform/enterprise/adapter/newapi"
	"github.com/nexusapi-platform/enterprise/pkg/apperr"
)

// genMemberUsername 生成成员 new-api 随机用户名(≤20 字符,31-ADR §11:无法编码可读工号,撞名由调用方重试)。
// 形如 m + 15 位 base32 小写 = 16 字符(高熵,撞外部用户概率可忽略;真撞了 BootstrapMember 预检会拒,重生成)。
func genMemberUsername() (string, error) {
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "m" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))[:15], nil
}

// ProvisionMemberServiceAccount 成员服务账号 saga(BE① 对外契约,33 §3.2)(33 §3.2;memberID 的平台行已存在,状态 provisioning):
//
//	① 随机名+密码 → BootstrapMember(SkipToken=true, AllowAdopt=false),撞名重生成重试(≤3)
//	② 凭证加密落库(SetMemberServiceAccount:user_id/username/access_token/password)
//	③ SetUserGroup(档位分组) ④ SetBillingPreference(wallet_only)(堵订阅旁路 ×N)
//	⑤ 首笔 Transfer(金库→成员,initialRaw;必设、正数、≤成员帽;金库不足=整体失败不半成功)
//
// 【孤儿清理,34 §3-③】②起任一步失败:new-api 无干净删 user 能力 → SetUserStatus(disable)
// + MarkMemberQuarantined(隔离标记,worker/统计跳过),不硬删;可重试(重入新名重建,隔离行留审计)。
func (s *Service) ProvisionMemberServiceAccount(ctx context.Context, orgID, memberID int64, group string, initialRaw int64, actor string) (int, error) {
	org, err := s.store.GetOrganization(ctx, orgID)
	if err != nil {
		return 0, apperr.Internal("").WithCause(err)
	}
	if org.NewapiUserID == nil {
		return 0, apperr.New(apperr.CodeInvalidParam, 409, "组织金库尚未开通")
	}
	// 成员额度帽(平台配置,默认 $1000=5亿 raw,int32 安全区)。
	cap, _ := s.store.GetSettingInt64(ctx, "member_quota_cap_raw", 500000000)
	if initialRaw <= 0 || initialRaw > cap {
		return 0, apperr.InvalidParam(fmt.Sprintf("初始额度必须为正且不超过成员上限(%d raw)", cap))
	}

	pw, gerr := genNewapiPassword()
	if gerr != nil {
		return 0, apperr.Internal("").WithCause(gerr)
	}
	// ① 建 user(撞名重生成,≤3 次;SkipToken=true 只建号不建令牌——令牌全由成员自助)。
	var res newapi.BootstrapResult
	var username string
	for attempt := 0; attempt < 3; attempt++ {
		u, uerr := genMemberUsername()
		if uerr != nil {
			return 0, apperr.Internal("").WithCause(uerr)
		}
		r, berr := s.upstream.BootstrapMember(ctx, newapi.BootstrapInput{
			OrgID: orgID, MemberID: memberID, Username: u, Password: pw,
			DisplayName: fmt.Sprintf("org%d-m%d", orgID, memberID), SkipToken: true, AllowAdopt: false,
		})
		if berr != nil {
			if newapi.IsUsernameConflict(berr) {
				continue // 撞外部用户:重生成随机名
			}
			return 0, mapUpstream(berr)
		}
		res, username = r, u
		break
	}
	if username == "" {
		return 0, apperr.Internal("成员用户名三次生成均冲突(异常,请排查)")
	}

	// —— 从这里起 new-api user 已存在,任一步失败走隔离(disable+quarantined,不硬删) ——
	quarantine := func(step string, cause error) (int, error) {
		s.log.Error("成员开通 saga 失败,隔离孤儿(disable+quarantined,可重试)", "member_id", memberID, "step", step, "err", cause)
		if derr := s.upstream.SetUserStatus(ctx, res.NewapiUserID, false); derr != nil {
			s.log.Error("隔离:disable 孤儿 user 失败(留活跃孤儿!需人工/重试收口)", "user_id", res.NewapiUserID, "err", derr)
		}
		if merr := s.store.MarkMemberQuarantined(ctx, orgID, memberID); merr != nil {
			s.log.Error("隔离:标记 quarantined 失败", "member_id", memberID, "err", merr)
		}
		return 0, mapUpstream(cause)
	}

	// ①.5 C1 硬校验(33 §11/总监裁定):验新建成员 user 的 quota=0。AllowAdopt=false 已防接管既有号;
	// 此处防 new-api 侧「新用户初始额度」(QuotaForNewUser)配置送钱——新号带非零额度会使首笔划账后
	// 成员额度 > 划账意图值(平台账本外的白送钱,守恒破坏)。非零即隔离,提示运维清零该配置后重试。
	if q0, qerr := s.upstream.GetUserQuota(ctx, res.NewapiUserID); qerr != nil {
		return quarantine("verify_zero_quota", qerr)
	} else if q0 != 0 {
		return quarantine("verify_zero_quota", fmt.Errorf("新建成员 user 初始 quota=%d≠0(new-api 侧疑配了新用户赠送额度,破坏划账守恒),请将 new-api「新用户初始额度」清零后重试", q0))
	}

	// ② 凭证加密落库(旋转 token 语义:取到必须立即存,丢了只能密码重登再取)。
	atEnc, e1 := s.keyring.EncryptString(res.AccessToken)
	pwEnc, e2 := s.keyring.EncryptString(pw)
	if e1 != nil || e2 != nil {
		return quarantine("encrypt_cred", fmt.Errorf("加密凭证失败: %v/%v", e1, e2))
	}
	if serr := s.store.SetMemberServiceAccount(ctx, orgID, memberID, int64(res.NewapiUserID), username, []byte(atEnc), []byte(pwEnc)); serr != nil {
		return quarantine("persist_cred", serr)
	}

	// ③ 设成员 user 分组(档位分组;GroupRatio/可用分组配置 ×N 由 BE① 阶段1 按 31-ADR §5 护栏补全)。
	if group != "" {
		if gerr := s.upstream.SetUserGroup(ctx, res.NewapiUserID, group); gerr != nil {
			return quarantine("set_group", gerr)
		}
	}
	// ④ wallet_only ×N(堵订阅旁路,31-ADR §7 纵深)。
	cred := newapi.MemberCred{NewapiUserID: res.NewapiUserID, AccessToken: res.AccessToken}
	if perr := s.upstream.SetBillingPreference(ctx, cred, "wallet_only"); perr != nil {
		return quarantine("wallet_only", perr)
	}

	// ⑤ 首笔划账(金库→成员;金库不足由 Transfer 内校验拒 → 整体失败不半成功,隔离孤儿)。
	idem := fmt.Sprintf("provision:%d:%d", orgID, memberID)
	if terr := s.Transfer(ctx, orgID, int(*org.NewapiUserID), res.NewapiUserID, memberID, initialRaw, ReasonInitialGrant, idem, actor); terr != nil {
		return quarantine("initial_grant", terr)
	}
	return res.NewapiUserID, nil
}

// WithMemberCred 用成员服务账号凭证代调 new-api(33 §3.2):解密 → 调 → 401 自愈(密码重登取新 token,
// **原子回存**——GET /api/user/token 一调即旋转,取到不立即落库=凭证丢失)→ 重试一次。
func (s *Service) WithMemberCred(ctx context.Context, memberID int64, fn func(cred newapi.MemberCred) error) error {
	c, ok, err := s.store.GetMemberServiceCred(ctx, memberID)
	if err != nil {
		return apperr.Internal("").WithCause(err)
	}
	if !ok {
		return apperr.New(apperr.CodeInvalidParam, 409, "该成员尚未开通服务账号")
	}
	at, derr := s.keyring.DecryptString(string(c.AccessTokenEnc))
	if derr != nil {
		return apperr.Internal("").WithCause(derr)
	}
	cred := newapi.MemberCred{NewapiUserID: int(c.NewapiUserID), AccessToken: at}
	ferr := fn(cred)
	if ferr == nil {
		return nil
	}
	// 401 自愈:探活确认失效才走重登(避免把业务 4xx 误当凭证失效)。
	alive, perr := s.upstream.ProbeAccessToken(ctx, cred)
	if perr != nil || alive {
		return ferr // 凭证没问题(或探活失败),原错误如实上抛
	}
	pw, pderr := s.keyring.DecryptString(string(c.PasswordEnc))
	if pderr != nil {
		return apperr.Internal("").WithCause(pderr)
	}
	newAT, rerr := s.upstream.RefreshAccessToken(ctx, newapi.BootstrapInput{Username: c.NewapiUsername, Password: pw})
	if rerr != nil {
		return mapUpstream(rerr)
	}
	newEnc, eerr := s.keyring.EncryptString(newAT)
	if eerr != nil {
		return apperr.Internal("").WithCause(eerr)
	}
	if uerr := s.store.UpdateMemberAccessToken(ctx, memberID, []byte(newEnc)); uerr != nil {
		// 新 token 已在手却存不进库:如实报错(下次调用将再次自愈;绝不吞——旋转语义下丢存等于丢凭证)。
		return apperr.Internal("").WithCause(uerr)
	}
	cred.AccessToken = newAT
	return fn(cred)
}

// MemberSubHit 上线闸命中项:仍挂 active 订阅的成员(应为空集)。
type MemberSubHit struct {
	MemberID     int64  `json:"member_id"`
	NewapiUserID int64  `json:"newapi_user_id"`
	Username     string `json:"username"`
}

// CheckActiveSubscriptions 上线闸(33 §3.4/34 §3-②,程序化不走人工 SQL):
// 遍历本组织已开通服务账号的成员,用各自凭证调 GetSelfSubscription(HasActive 现成),
// 返回 active 订阅命中清单——上线闸=命中数必须为 0(订阅旁路会击穿硬限额+双计费,31-ADR §7)。
func (s *Service) CheckActiveSubscriptions(ctx context.Context, orgID int64) ([]MemberSubHit, error) {
	creds, err := s.store.ListMembersWithServiceAccount(ctx, orgID)
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	var hits []MemberSubHit
	for _, c := range creds {
		mc := c
		werr := s.WithMemberCred(ctx, mc.MemberID, func(cred newapi.MemberCred) error {
			sub, serr := s.upstream.GetSelfSubscription(ctx, cred)
			if serr != nil {
				return serr
			}
			if sub.HasActive {
				hits = append(hits, MemberSubHit{MemberID: mc.MemberID, NewapiUserID: mc.NewapiUserID, Username: mc.NewapiUsername})
			}
			return nil
		})
		if werr != nil {
			// 单成员查失败不吞:上线闸要的是确定性,查不动=闸不过,如实上抛。
			return hits, apperr.Internal(fmt.Sprintf("成员 %d 订阅状态查询失败", mc.MemberID)).WithCause(werr)
		}
	}
	return hits, nil
}
