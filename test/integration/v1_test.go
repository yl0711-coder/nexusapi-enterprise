// v1 观测管理版验收(20-§15):门B关联(role闸/池子分文不动=涉钱红线)/裁定A(observe下开通真建令牌)/
// 裁定B(报表与billing解耦)/escrow休眠/硬停disable用户/订阅口径/时点归因/补漏扫描。
// NativeOrgStop/NativeMemberStop 的真 403 需可调渠道(测试栈无 mock 渠道),属 dev 真站联调验收项(源码已证:
// billing_session.go:355 池子≤0 403;PreConsumeTokenQuota:398 令牌不足 403),不进自动栈。
package integration

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nexusapi-platform/enterprise/adapter/newapi"
	"github.com/nexusapi-platform/enterprise/model"
	"github.com/nexusapi-platform/enterprise/pkg/session"
	"github.com/nexusapi-platform/enterprise/repo"
	"github.com/nexusapi-platform/enterprise/service"
)

// v1Svc 起一套 v1 生产形态服务:funding 关(escrow 休眠)+ observe 可选。返回 upstream 供造数。
func v1Svc(t *testing.T, ctx context.Context, dbName string, observe bool) (*service.Service, *repo.Store, newapi.NewapiAdapter) {
	t.Helper()
	dsn := os.Getenv("NEXUS_IT_DSN")
	newapiURL := os.Getenv("NEXUS_IT_NEWAPI_URL")
	newapiSQLDSN := os.Getenv("NEXUS_IT_NEWAPI_SQL_DSN")
	if dsn == "" || newapiURL == "" || newapiSQLDSN == "" {
		t.Skip("跳过:需 NEXUS_IT_DSN / NEXUS_IT_NEWAPI_URL / NEXUS_IT_NEWAPI_SQL_DSN")
	}
	ensureDatabase(t, newapiSQLDSN, dbName)
	store, err := repo.Open(ctx, strings.Replace(dsn, "/nexus?", "/"+dbName+"?", 1))
	if err != nil {
		t.Fatalf("连 %s 失败: %v", dbName, err)
	}
	t.Cleanup(func() { _, _ = store.DB().Exec("DROP DATABASE IF EXISTS " + dbName); store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO settlement_cursor (org_id, last_settled_ts, last_settled_log_id) VALUES (0, UNIX_TIMESTAMP(), 0)`); err != nil {
		t.Fatalf("置结算水位失败: %v", err)
	}
	adminToken, adminUID := setupRC4(t, newapiURL)
	signer, _ := session.NewSigner([]byte("integration-test-session-key-32b!!"), time.Hour)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	upstream := newapi.New(newapi.Config{BaseURL: newapiURL, AdminToken: adminToken, AdminUserID: adminUID, Timeout: 15 * time.Second}, nil)
	svc := service.New(service.Deps{Store: store, Upstream: upstream, Keyring: mustKeyring(t), Signer: signer, Logger: log,
		ObserveMode: observe, FundingEnabled: false}) // v1 生产形态:escrow 休眠
	return svc, store, upstream
}

// mkEnterpriseUser 造一个"企业已有的 new-api 普通用户"(门B 关联对象):普通用户 + access token。
func mkEnterpriseUser(t *testing.T, ctx context.Context, upstream newapi.NewapiAdapter, name string) newapi.MemberCred {
	t.Helper()
	res, err := upstream.BootstrapMember(ctx, newapi.BootstrapInput{
		OrgID: 9000, MemberID: int64(time.Now().UnixNano() % 100000), Username: name, Password: "Ent3rprise!Pw", DisplayName: name, SkipToken: true,
	})
	if err != nil {
		t.Fatalf("造企业用户失败: %v", err)
	}
	return newapi.MemberCred{NewapiUserID: res.NewapiUserID, AccessToken: res.AccessToken}
}

// 架构B(33 §3.2,取代旧「裁定A」用例):开通成员 = 建平台账号 + 成员服务账号 + 首笔划账,**不铸 key**;
// 回显登录凭证一次;金库→成员划账守恒;金库不足整体失败(quarantined)。
func TestIntegration_OpenMemberBuildsToken(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	svc, store, upstream := v1Svc(t, ctx, "nexus_v1obt", false)
	const orgID = int64(901)
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO organization (id, name, slug) VALUES (?, 'obt-org', 'obt-slug')`, orgID); err != nil {
		t.Fatalf("建组织失败: %v", err)
	}
	// 门A:金库惰性开通(OpenMember 内 EnsureOrgProvisioned)——先手动确保 + 注资,验证幂等复用。
	treasury, perr := svc.EnsureOrgProvisioned(ctx, orgID, "obt-org")
	if perr != nil {
		t.Fatalf("开金库失败: %v", perr)
	}
	if err := upstream.IncreaseUserQuota(ctx, treasury.NewapiUserID, 5_000_000); err != nil {
		t.Fatalf("金库注资失败: %v", err)
	}
	amount := int64(2_000_000)
	tierID, terr := store.CreateTier(ctx, &model.Tier{OrgID: orgID, Name: "obt-tier", AmountRaw: &amount})
	if terr != nil {
		t.Fatalf("建档失败: %v", terr)
	}
	admin := session.Claims{Role: session.RoleOrgAdmin, OrgID: orgID, MemberID: 999}
	res, err := svc.OpenMember(ctx, admin, orgID, service.OpenMemberInput{Name: "架构B开通", TierID: &tierID})
	if err != nil {
		t.Fatalf("开通成员失败: %v", err)
	}
	if res.InitialPassword == "" || res.NewapiUserID == 0 || res.InitialQuotaRaw != amount {
		t.Fatalf("🔴开通返回异常(应回显登录凭证+服务账号+首笔额度): %+v", res)
	}
	// 守恒:金库 5M-2M=3M,成员=2M(读 DB 实时)。
	tq, _ := upstream.GetUserQuota(ctx, treasury.NewapiUserID)
	mq, _ := upstream.GetUserQuota(ctx, int(res.NewapiUserID))
	if tq != 3_000_000 || mq != 2_000_000 {
		t.Fatalf("🔴开通划账守恒破:金库=%d(期 3M) 成员=%d(期 2M)", tq, mq)
	}
	// 不铸 key:成员名下无令牌归属行。
	var nTok int
	_ = store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM member_key_token WHERE org_id=? AND member_id=?`, orgID, res.MemberID).Scan(&nTok)
	if nTok != 0 {
		t.Fatalf("🔴架构B 开通不应铸 key,实归属行=%d", nTok)
	}
	// tier_id 必填(契约)。
	if _, err := svc.OpenMember(ctx, admin, orgID, service.OpenMemberInput{Name: "缺档位"}); err == nil {
		t.Fatal("🔴缺 tier_id 应拒")
	}
	// 金库不足(剩 3M,档位要 3.5M)→ 整体失败(孤儿隔离 quarantined,不半成功)。
	big := int64(3_500_000)
	bigTier, _ := store.CreateTier(ctx, &model.Tier{OrgID: orgID, Name: "obt-big", AmountRaw: &big})
	if _, err := svc.OpenMember(ctx, admin, orgID, service.OpenMemberInput{Name: "金库不足", TierID: &bigTier}); err == nil {
		t.Fatal("🔴金库不足应整体失败")
	}
	t.Logf("架构B 开通真账 ok: 服务账号+登录凭证回显+首笔划账守恒+不铸 key+tier 必填+金库不足整体失败")
}

// 裁定B(20-§2.1)+M4:billing_enabled=关 的组织报表也有数据(同步解耦);未映射令牌归未知桶(member_id=0)不丢行。
func TestIntegration_ReportAllOrgs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	svc, store, upstream := v1Svc(t, ctx, "nexus_v1rpt", true)
	newapiSQLDSN := os.Getenv("NEXUS_IT_NEWAPI_SQL_DSN")
	const orgID, memberID = int64(902), int64(1)
	// billing_enabled 默认 0(v1 全组织关)。
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO organization (id, name, slug) VALUES (?, 'rpt-org', 'rpt-slug')`, orgID); err != nil {
		t.Fatalf("建组织失败: %v", err)
	}
	cred, perr := svc.EnsureOrgProvisioned(ctx, orgID, "rpt-org")
	if perr != nil {
		t.Fatalf("开通失败: %v", perr)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO member (id, org_id, login_email, status, bootstrap_state) VALUES (?, ?, 'rpt@t.local', 'active', 'done')`, memberID, orgID); err != nil {
		t.Fatalf("建成员失败: %v", err)
	}
	tokenID := selfServeMemberToken(t, ctx, svc, store, orgID, memberID)
	// 企业在 new-api 侧自建的令牌(平台无映射)也在花组织的钱 → 未知桶。
	extTokID, terr := upstream.CreateToken(ctx, cred, newapi.TokenSpec{Name: "ent_self_made", UnlimitedQuota: true, ExpiredTime: -1})
	if terr != nil {
		t.Fatalf("造企业自建令牌失败: %v", terr)
	}
	ndb, _ := sql.Open("mysql", newapiSQLDSN)
	defer ndb.Close()
	var maxLogID int64
	_ = ndb.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM logs`).Scan(&maxLogID)
	logTS := time.Now().Unix() - 300
	if _, err := store.DB().ExecContext(ctx, `UPDATE settlement_cursor SET last_settled_ts=?, last_settled_log_id=? WHERE org_id=0`, logTS-1, maxLogID); err != nil {
		t.Fatalf("置结算水位失败: %v", err)
	}
	// A1(五路验收):成员令牌造**3 条**同小时桶日志(合 1 个 ledger 桶)——验"调用次数=底层请求数 3"非"ledger 行数 1"。
	seedConsumptionLogTok(t, newapiSQLDSN, int64(cred.NewapiUserID), tokenID, "gpt-rpt", 2_000_000, logTS)
	seedConsumptionLogTok(t, newapiSQLDSN, int64(cred.NewapiUserID), tokenID, "gpt-rpt", 2_000_000, logTS+1)
	seedConsumptionLogTok(t, newapiSQLDSN, int64(cred.NewapiUserID), tokenID, "gpt-rpt", 1_000_000, logTS+2)
	seedConsumptionLogTok(t, newapiSQLDSN, int64(cred.NewapiUserID), int64(extTokID), "gpt-rpt", 3_000_000, logTS)

	if _, err := svc.RunSettlement(ctx); err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	// A1 断言:报表按成员的"调用次数"= usage_detail 底层请求数(3),不是 ledger 桶行数(1)。
	rep, uerr := svc.OrgUsage(ctx, session.Claims{Role: session.RoleOrgAdmin, OrgID: orgID, MemberID: 999}, orgID, 24)
	if uerr != nil {
		t.Fatalf("读用量报表失败: %v", uerr)
	}
	var memCount int
	for _, b := range rep.ByMember {
		if b.MemberID == memberID {
			memCount = b.Count
		}
	}
	if memCount != 3 {
		t.Fatalf("🔴A1:成员调用次数应=底层请求数 3(3 条同桶日志),实=%d(1=错用 ledger 行数)", memCount)
	}
	var memberQ, unknownQ int64
	_ = store.DB().QueryRowContext(ctx, `SELECT COALESCE(SUM(consumed_quota),0) FROM usage_ledger WHERE org_id=? AND member_id=?`, orgID, memberID).Scan(&memberQ)
	_ = store.DB().QueryRowContext(ctx, `SELECT COALESCE(SUM(consumed_quota),0) FROM usage_ledger WHERE org_id=? AND member_id=0`, orgID).Scan(&unknownQ)
	if memberQ != 5_000_000 { // 2M+2M+1M 合桶
		t.Fatalf("🔴裁定B:billing 关的组织报表应有数据(成员 5000000),实=%d", memberQ)
	}
	if unknownQ != 3_000_000 {
		t.Fatalf("🔴M4:企业自建令牌消费应归未知桶(member_id=0,3000000)不丢行,实=%d", unknownQ)
	}
	// 花费总额 == 消费日志求和(BillFromConsumeLog)。
	if memberQ+unknownQ != 8_000_000 {
		t.Fatalf("🔴报表总额应==消费日志求和 8000000,实=%d", memberQ+unknownQ)
	}
	t.Logf("裁定B+M4+A1 真账 ok: billing 关组织照落账(成员5000000/3次调用)+企业自建令牌归未知桶(3000000)不丢行;调用次数=底层请求数非桶行数")
}

// 门B(20-§8):role 闸拒 admin;user_id 不符拒;关联成功导入令牌;重复关联拒;
// 【#6 涉钱红线】关联全程企业池子余额分文不动(AssocPoolUntouched)。
func TestIntegration_AssociateOrg(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	svc, _, upstream := v1Svc(t, ctx, "nexus_v1asc", true)
	opc := session.Claims{Role: session.RoleOperator}
	// 造"企业现有用户":普通用户 + 预存池子 + 2 枚现有令牌。
	entCred := mkEnterpriseUser(t, ctx, upstream, "entuser1")
	const entQuota = int64(777_000_000)
	if err := upstream.ManageUserQuota(ctx, entCred.NewapiUserID, newapi.QuotaOverride, entQuota); err != nil {
		t.Fatalf("预存企业池子失败: %v", err)
	}
	for i := 1; i <= 2; i++ {
		if _, err := upstream.CreateToken(ctx, entCred, newapi.TokenSpec{Name: fmt.Sprintf("ent_tok_%d", i), UnlimitedQuota: true, ExpiredTime: -1}); err != nil {
			t.Fatalf("造企业令牌失败: %v", err)
		}
	}
	if err := upstream.AddOrgUsableGroup(ctx, "default", "vip"); err != nil { // 分组可用前置
		t.Fatalf("预配可用分组失败: %v", err)
	}

	// ① role 闸:用 admin(root)自己关联 → 拒。
	adminUID := 1
	if _, err := svc.CreateOrg(ctx, opc, service.CreateOrgInput{
		Name: "坏关联", Slug: "asc-bad1", AdminEmail: "b1@t.local", NewapiUserGroup: "default",
		Associate: &service.AssociateOrgInput{NewapiUserID: int64(adminUID), AccessToken: os.Getenv("NEXUS_IT_ADMIN_TOKEN_UNUSED") + "invalid"},
	}); err == nil {
		t.Fatalf("🔴无效/admin token 关联应拒")
	}
	// ② user_id 不符:token 是企业用户的,录入 id+1 → 拒。
	if _, err := svc.CreateOrg(ctx, opc, service.CreateOrgInput{
		Name: "坏关联2", Slug: "asc-bad2", AdminEmail: "b2@t.local", NewapiUserGroup: "default",
		Associate: &service.AssociateOrgInput{NewapiUserID: int64(entCred.NewapiUserID + 1), AccessToken: entCred.AccessToken},
	}); err == nil {
		t.Fatalf("🔴user_id 与 token 不符应拒(防串号)")
	}
	// ③ 正常关联:导入 2 成员;池子分文不动。
	res, err := svc.CreateOrg(ctx, opc, service.CreateOrgInput{
		Name: "企业甲", Slug: "asc-ok", AdminEmail: "ok@t.local", NewapiUserGroup: "default",
		Associate: &service.AssociateOrgInput{NewapiUserID: int64(entCred.NewapiUserID), AccessToken: entCred.AccessToken, NamePolicy: "inherit"},
	})
	if err != nil {
		t.Fatalf("关联失败: %v", err)
	}
	if res.ImportedMembers != 2 || res.ImportFailed != 0 {
		t.Fatalf("🔴应导入 2 成员失败 0,实 imported=%d failed=%d", res.ImportedMembers, res.ImportFailed)
	}
	if res.Org.CreatedByPlatform {
		t.Fatalf("🔴关联组织 created_by_platform 应=false")
	}
	after, _ := upstream.GetUserQuota(ctx, entCred.NewapiUserID)
	if after != entQuota {
		t.Fatalf("🔴【#6 涉钱红线】关联全程企业池子应分文不动:%d → %d", entQuota, after)
	}
	// 客户余额=读求和=企业池子(关联组织非 0,M5/v1-R1)。
	adminC := session.Claims{Role: session.RoleOrgAdmin, OrgID: res.Org.ID, MemberID: res.AdminMemberID}
	gb, berr := svc.GetBalance(ctx, adminC, res.Org.ID)
	if berr != nil || gb.AvailableQuota != entQuota {
		t.Fatalf("🔴关联组织读求和余额应=%d(非 0!),实=%v err=%v", entQuota, gb, berr)
	}
	// ④ 重复关联同一企业用户 → 拒。
	if _, err := svc.CreateOrg(ctx, opc, service.CreateOrgInput{
		Name: "企业乙", Slug: "asc-dup", AdminEmail: "dup@t.local", NewapiUserGroup: "default",
		Associate: &service.AssociateOrgInput{NewapiUserID: int64(entCred.NewapiUserID), AccessToken: entCred.AccessToken},
	}); err == nil {
		t.Fatalf("🔴重复关联同一 new-api 用户应拒")
	}
	// ⑤ 幂等重导:再跑 0 新增。
	imp, skipped, failed, rerr := svc.ReimportOrgTokens(ctx, opc, res.Org.ID)
	if rerr != nil || imp != 0 || skipped != 2 || failed != 0 {
		t.Fatalf("重导应幂等 0 新增且跳过已导入,实 imported=%d skipped=%d failed=%d err=%v", imp, skipped, failed, rerr)
	}
	t.Logf("门B 真账 ok: 校验闸(坏token/串号/重复=拒)+导入2成员(继承名)+池子 %d 分文不动(#6红线)+关联组织读求和余额非0+重导幂等", entQuota)
}

// 硬停(20-§4):disable org 用户(new-api status=2)+组织标 hard_stopped+管理写被屏蔽;解除恢复。
func TestIntegration_HardStopDisableUser(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	svc, store, _ := v1Svc(t, ctx, "nexus_v1hsd", true)
	newapiSQLDSN := os.Getenv("NEXUS_IT_NEWAPI_SQL_DSN")
	const orgID, memberID = int64(903), int64(1)
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO organization (id, name, slug) VALUES (?, 'hsd-org', 'hsd-slug')`, orgID); err != nil {
		t.Fatalf("建组织失败: %v", err)
	}
	cred, perr := svc.EnsureOrgProvisioned(ctx, orgID, "hsd-org")
	if perr != nil {
		t.Fatalf("开通失败: %v", perr)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO member (id, org_id, login_email, status, bootstrap_state) VALUES (?, ?, 'hsd@t.local', 'active', 'done')`, memberID, orgID); err != nil {
		t.Fatalf("建成员失败: %v", err)
	}
	_ = selfServeMemberToken(t, ctx, svc, store, orgID, memberID)
	hsdTier, terr := store.CreateTier(ctx, &model.Tier{OrgID: orgID, Name: "hsd-tier"})
	if terr != nil {
		t.Fatalf("建档失败: %v", terr)
	}
	opc := session.Claims{Role: session.RoleOperator, OrgID: orgID}
	admin := session.Claims{Role: session.RoleOrgAdmin, OrgID: orgID, MemberID: 999}

	if err := svc.HardStopOrg(ctx, opc, orgID, true); err != nil {
		t.Fatalf("硬停失败: %v", err)
	}
	ndb, _ := sql.Open("mysql", newapiSQLDSN)
	defer ndb.Close()
	var ustatus int
	_ = ndb.QueryRowContext(ctx, `SELECT status FROM users WHERE id=?`, cred.NewapiUserID).Scan(&ustatus)
	if ustatus != 2 {
		t.Fatalf("🔴硬停应 disable org 用户(status=2),实=%d", ustatus)
	}
	org, _ := store.GetOrganization(ctx, orgID)
	if org.Status != model.OrgStatusHardStopped {
		t.Fatalf("🔴组织状态应=hard_stopped,实=%s", org.Status)
	}
	// 硬停期管理写被屏蔽(withOrgCred/EnsureOrgProvisioned 前置闸,防 401 自愈告警噪音)。
	if err := svc.SetMemberStatus(ctx, admin, orgID, memberID, false); !isForbidden(err) {
		t.Fatalf("🔴硬停期管理写应 403,实=%v", err)
	}
	if _, err := svc.OpenMember(ctx, admin, orgID, service.OpenMemberInput{Name: "硬停期开通", TierID: &hsdTier}); !isForbidden(err) {
		t.Fatalf("🔴硬停期开通成员应 403,实=%v", err)
	}
	// 解除:enable + active + 管理恢复。
	if err := svc.HardStopOrg(ctx, opc, orgID, false); err != nil {
		t.Fatalf("解除失败: %v", err)
	}
	_ = ndb.QueryRowContext(ctx, `SELECT status FROM users WHERE id=?`, cred.NewapiUserID).Scan(&ustatus)
	if ustatus != 1 {
		t.Fatalf("🔴解除应 enable(status=1),实=%d", ustatus)
	}
	if err := svc.SetMemberStatus(ctx, admin, orgID, memberID, false); err != nil {
		t.Fatalf("🔴解除后管理写应恢复,实错: %v", err)
	}
	t.Logf("硬停真账 ok: disable org 用户(status=2)+组织 hard_stopped+管理写403屏蔽;解除→enable+恢复")
}

// 订阅口径(20-§7 H1):门A 开通即设 wallet_only(幂等);billing_kind=subscription 组织余额回 kind 不回数。
func TestIntegration_SubscriptionWalletOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	svc, store, upstream := v1Svc(t, ctx, "nexus_v1sub", true)
	const orgID = int64(904)
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO organization (id, name, slug) VALUES (?, 'sub-org', 'sub-slug')`, orgID); err != nil {
		t.Fatalf("建组织失败: %v", err)
	}
	cred, perr := svc.EnsureOrgProvisioned(ctx, orgID, "sub-org")
	if perr != nil {
		t.Fatalf("开通失败: %v", perr)
	}
	sub, serr := upstream.GetSelfSubscription(ctx, cred)
	if serr != nil {
		t.Fatalf("读订阅偏好失败: %v", serr)
	}
	if sub.BillingPreference != "wallet_only" {
		t.Fatalf("🔴H1:门A 开通即应设 wallet_only(堵订阅旁路),实=%q", sub.BillingPreference)
	}
	// 订阅计费组织(门B 检测到 active 订阅时置):余额回 kind 不回数字。
	if _, err := store.DB().ExecContext(ctx, `UPDATE organization SET billing_kind='subscription' WHERE id=?`, orgID); err != nil {
		t.Fatalf("置订阅组织失败: %v", err)
	}
	adminC := session.Claims{Role: session.RoleOrgAdmin, OrgID: orgID, MemberID: 1}
	gb, berr := svc.GetBalance(ctx, adminC, orgID)
	if berr != nil || gb.BillingKind != model.BillingKindSub || gb.AvailableQuota != 0 {
		t.Fatalf("🔴订阅组织余额应回 kind=subscription 不回数,实=%+v err=%v", gb, berr)
	}
	t.Logf("订阅口径真账 ok: 门A 开通即 wallet_only(new-api 侧读回确认);订阅组织余额显示'订阅计费'不回数")
}

// escrow 休眠(20-§9):v1 生产形态(funding 关)下充值/续充端点 404、worker 静默、escrow_bucket 无写入。
func TestIntegration_EscrowDormant(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	svc, store, _ := v1Svc(t, ctx, "nexus_v1dor", true)
	const orgID = int64(905)
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO organization (id, name, slug) VALUES (?, 'dor-org', 'dor-slug')`, orgID); err != nil {
		t.Fatalf("建组织失败: %v", err)
	}
	if _, err := svc.EnsureOrgProvisioned(ctx, orgID, "dor-org"); err != nil {
		t.Fatalf("开通失败: %v", err)
	}
	opc := session.Claims{Role: session.RoleOperator, OrgID: orgID}
	if _, err := svc.Recharge(ctx, opc, orgID, service.RechargeInput{AmountQuota: 1_000_000, TransferNo: "dor-1"}); err == nil {
		t.Fatalf("🔴v1 充值端点应 404(escrow 休眠)")
	}
	if _, err := svc.RefillWindow(ctx, opc, orgID); err == nil {
		t.Fatalf("🔴v1 续充端点应 404")
	}
	if err := svc.AutoRefill(ctx); err != nil {
		t.Fatalf("AutoRefill 应静默 no-op,实错: %v", err)
	}
	if err := svc.ReconcileEscrow(ctx); err != nil {
		t.Fatalf("ReconcileEscrow 应静默 no-op,实错: %v", err)
	}
	var buckets int
	_ = store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM escrow_bucket`).Scan(&buckets)
	if buckets != 0 {
		t.Fatalf("🔴v1 全流程 escrow_bucket 应无写入,实 %d 行", buckets)
	}
	t.Logf("escrow 休眠真账 ok: 充值/续充 404 + worker 静默 no-op + escrow_bucket 零写入")
}

// M3 补漏扫描:迟提交行(id≤水位、created_at 在已结算窗口、不在 detail)被下一轮补入,不永久漏。
func TestIntegration_LedgerRescueLateCommit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	svc, store, _ := v1Svc(t, ctx, "nexus_v1resc", true)
	newapiSQLDSN := os.Getenv("NEXUS_IT_NEWAPI_SQL_DSN")
	const orgID, memberID = int64(906), int64(1)
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO organization (id, name, slug) VALUES (?, 'resc-org', 'resc-slug')`, orgID); err != nil {
		t.Fatalf("建组织失败: %v", err)
	}
	cred, perr := svc.EnsureOrgProvisioned(ctx, orgID, "resc-org")
	if perr != nil {
		t.Fatalf("开通失败: %v", perr)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO member (id, org_id, login_email, status, bootstrap_state) VALUES (?, ?, 'resc@t.local', 'active', 'done')`, memberID, orgID); err != nil {
		t.Fatalf("建成员失败: %v", err)
	}
	tokenID := selfServeMemberToken(t, ctx, svc, store, orgID, memberID)
	// 模拟"迟提交漏行":该行 created_at 在水位**之前**(已结算过的窗口)、id 也 ≤ 水位——正常主窗口永远读不到它。
	logTS := time.Now().Unix() - 120
	seedConsumptionLogTok(t, newapiSQLDSN, int64(cred.NewapiUserID), tokenID, "gpt-late", 4_000_000, logTS)
	ndb, _ := sql.Open("mysql", newapiSQLDSN)
	defer ndb.Close()
	var maxLogID int64
	_ = ndb.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM logs`).Scan(&maxLogID)
	// 水位推到该行之后(ts 和 id 都盖过它)=当轮"没看见它就推过去了"。
	if _, err := store.DB().ExecContext(ctx, `UPDATE settlement_cursor SET last_settled_ts=?, last_settled_log_id=? WHERE org_id=0`, logTS+30, maxLogID); err != nil {
		t.Fatalf("置结算水位失败: %v", err)
	}
	if _, err := svc.RunSettlement(ctx); err != nil { // 补漏扫描应把它捞回来
		t.Fatalf("结算失败: %v", err)
	}
	var got int64
	_ = store.DB().QueryRowContext(ctx, `SELECT COALESCE(SUM(consumed_quota),0) FROM usage_ledger WHERE org_id=?`, orgID).Scan(&got)
	if got != 4_000_000 {
		t.Fatalf("🔴M3 补漏:迟提交行应被补扫捞回(4000000),实=%d(永久漏=报表少计)", got)
	}
	// 再跑一轮:不重复计入(detail 查重幂等)。
	if _, err := svc.RunSettlement(ctx); err != nil {
		t.Fatalf("二次结算失败: %v", err)
	}
	_ = store.DB().QueryRowContext(ctx, `SELECT COALESCE(SUM(consumed_quota),0) FROM usage_ledger WHERE org_id=?`, orgID).Scan(&got)
	if got != 4_000_000 {
		t.Fatalf("🔴补漏重跑不应重复计入,实=%d", got)
	}
	t.Logf("M3 补漏真账 ok: 迟提交漏行(id≤水位+窗口已过)被补扫捞回 4000000;重跑查重不双计")

	// BUG-1 边界回归(三总监验收):一条 created_at **恰等于 since**、id>水位的迟提交行,只能被主窗口收一次——
	// 补漏区间上界是 since−1,不与主窗口 [since, until] 重叠;修复前两阶段各算一次=报表多算。去修复(rescanUntil
	// 改回 since)本断言必红。
	var curTS int64
	_ = store.DB().QueryRowContext(ctx, `SELECT last_settled_ts FROM settlement_cursor WHERE org_id=0`).Scan(&curTS)
	seedConsumptionLogTok(t, newapiSQLDSN, int64(cred.NewapiUserID), tokenID, "gpt-edge", 2_000_000, curTS) // created_at==since
	time.Sleep(7 * time.Second)                                                                             // 让 until(now−lag5s) 越过 since,主窗口张开(否则窗口为空提前返回,边界行读不到)
	if _, err := svc.RunSettlement(ctx); err != nil {
		t.Fatalf("边界结算失败: %v", err)
	}
	_ = store.DB().QueryRowContext(ctx, `SELECT COALESCE(SUM(consumed_quota),0) FROM usage_ledger WHERE org_id=?`, orgID).Scan(&got)
	if got != 6_000_000 { // 4000000 + 边界行恰一次 2000000;双计=8000000
		t.Fatalf("🔴BUG-1:created_at==since 的迟提交行应只计一次(总额 6000000),实=%d(8000000=补漏与主窗口双计)", got)
	}
	t.Logf("BUG-1 边界真账 ok: created_at==since 的行只被主窗口收一次(总额 6000000),补漏区间 since-1 不重叠")
}

// M4 时点归因:离职(软删)成员的迟同步历史消费仍归原成员,不丢不串。
func TestIntegration_LedgerAttributionOffboarded(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	svc, store, _ := v1Svc(t, ctx, "nexus_v1attr", true)
	newapiSQLDSN := os.Getenv("NEXUS_IT_NEWAPI_SQL_DSN")
	const orgID, memberID = int64(907), int64(1)
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO organization (id, name, slug) VALUES (?, 'attr-org', 'attr-slug')`, orgID); err != nil {
		t.Fatalf("建组织失败: %v", err)
	}
	cred, perr := svc.EnsureOrgProvisioned(ctx, orgID, "attr-org")
	if perr != nil {
		t.Fatalf("开通失败: %v", perr)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO member (id, org_id, login_email, status, bootstrap_state) VALUES (?, ?, 'attr@t.local', 'active', 'done')`, memberID, orgID); err != nil {
		t.Fatalf("建成员失败: %v", err)
	}
	tokenID := selfServeMemberToken(t, ctx, svc, store, orgID, memberID)
	admin := session.Claims{Role: session.RoleOrgAdmin, OrgID: orgID, MemberID: 999}
	// 离职(软删,删 new-api token 但映射行保留)。
	if err := svc.OffboardMember(ctx, admin, orgID, memberID); err != nil {
		t.Fatalf("离职失败: %v", err)
	}
	// 离职**后**才同步到的历史消费(消费发生在离职前,日志迟到)。
	ndb, _ := sql.Open("mysql", newapiSQLDSN)
	defer ndb.Close()
	var maxLogID int64
	_ = ndb.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM logs`).Scan(&maxLogID)
	logTS := time.Now().Unix() - 300
	if _, err := store.DB().ExecContext(ctx, `UPDATE settlement_cursor SET last_settled_ts=?, last_settled_log_id=? WHERE org_id=0`, logTS-1, maxLogID); err != nil {
		t.Fatalf("置结算水位失败: %v", err)
	}
	seedConsumptionLogTok(t, newapiSQLDSN, int64(cred.NewapiUserID), tokenID, "gpt-attr", 6_000_000, logTS)
	if _, err := svc.RunSettlement(ctx); err != nil {
		t.Fatalf("结算失败: %v", err)
	}
	var got int64
	_ = store.DB().QueryRowContext(ctx, `SELECT COALESCE(SUM(consumed_quota),0) FROM usage_ledger WHERE org_id=? AND member_id=?`, orgID, memberID).Scan(&got)
	if got != 6_000_000 {
		t.Fatalf("🔴M4 时点归因:离职成员的迟同步历史消费应仍归原成员(6000000),实=%d(丢行或串未知桶)", got)
	}
	t.Logf("M4 时点归因真账 ok: 离职(软删)成员迟同步消费 6000000 仍归原成员,不丢不串")
}
