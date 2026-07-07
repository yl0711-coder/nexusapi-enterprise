// 托管多桶/入账/续充真账测试(v2 R3,涉钱必真账):对真 newapi 验
//   ① 入账用 add 非 override(窗口=旧值+额,不是覆盖);② 守恒:已释放(窗口增量)+托管 = 总充值,绝不超拨;
//   ③ 窗口封顶 escrowWindowCap、溢出入托管;④ 续充把托管并入窗口(add)。
package integration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/nexusapi-platform/enterprise/adapter/newapi"
	"github.com/nexusapi-platform/enterprise/model"
	"github.com/nexusapi-platform/enterprise/pkg/apperr"
	"github.com/nexusapi-platform/enterprise/pkg/crypto"
	"github.com/nexusapi-platform/enterprise/pkg/session"
	"github.com/nexusapi-platform/enterprise/repo"
	"github.com/nexusapi-platform/enterprise/service"
)

func isForbidden(err error) bool {
	var e *apperr.Error
	return errors.As(err, &e) && e.Code == apperr.CodeForbidden
}

// B档#1(结算扣款侧,P0·堵HIGH-1盲区):非 observe RunSettlement 真扣钱 → 守恒 + 水位去重防双扣。
func TestIntegration_SettlementDeductDedup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	svc, store, _, _ := escrowSvc(t, ctx, "nexus_sd") // 非 observe
	newapiSQLDSN := os.Getenv("NEXUS_IT_NEWAPI_SQL_DSN")
	const orgID, memberID = int64(801), int64(1)
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO organization (id, name, slug, billing_enabled) VALUES (?, 'sd-org', 'sd-slug', 1)`, orgID); err != nil {
		t.Fatalf("建组织失败: %v", err)
	}
	cred, perr := svc.EnsureOrgProvisioned(ctx, orgID, "sd-org")
	if perr != nil {
		t.Fatalf("开通失败: %v", perr)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO member (id, org_id, login_email, status, bootstrap_state) VALUES (?, ?, 'sd@t.local', 'active', 'done')`, memberID, orgID); err != nil {
		t.Fatalf("建成员失败: %v", err)
	}
	tokenID := selfServeMemberToken(t, ctx, svc, store, orgID, memberID)
	opc := session.Claims{Role: session.RoleOperator, OrgID: orgID}
	const A = int64(50_000_000)
	if _, err := svc.Recharge(ctx, opc, orgID, service.RechargeInput{AmountQuota: A, TransferNo: "sd-1"}); err != nil {
		t.Fatalf("入账失败: %v", err)
	}
	ndb, _ := sql.Open("mysql", newapiSQLDSN)
	defer ndb.Close()
	var maxLogID int64
	_ = ndb.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM logs`).Scan(&maxLogID)
	logTS := time.Now().Unix() - 300 // 安全早于结算滞后窗口
	if _, err := store.DB().ExecContext(ctx, `UPDATE settlement_cursor SET last_settled_ts=?, last_settled_log_id=? WHERE org_id=0`, logTS-1, maxLogID); err != nil {
		t.Fatalf("置结算水位失败: %v", err)
	}
	const C = int64(20_000_000)
	seedConsumptionLogTok(t, newapiSQLDSN, int64(cred.NewapiUserID), tokenID, "gpt-sd", C, logTS)

	readBal := func() (consumed, balance int64) {
		_ = store.DB().QueryRowContext(ctx, `SELECT total_consumed, balance FROM company_balance WHERE org_id=?`, orgID).Scan(&consumed, &balance)
		return
	}
	if _, err := svc.RunSettlement(ctx); err != nil {
		t.Fatalf("首次结算失败: %v", err)
	}
	c1, b1 := readBal()
	if c1 != C {
		t.Fatalf("🔴首次结算应扣消费 C=%d,实 total_consumed=%d", C, c1)
	}
	if b1 != A-C {
		t.Fatalf("🔴守恒破:balance 应=A−C=%d,实=%d", A-C, b1)
	}
	if _, err := svc.RunSettlement(ctx); err != nil { // 二次结算:水位去重,绝不双扣
		t.Fatalf("二次结算失败: %v", err)
	}
	c2, b2 := readBal()
	if c2 != C || b2 != A-C {
		t.Fatalf("🔴重跑结算双扣了(去重失效):total_consumed %d→%d、balance %d→%d", c1, c2, b1, b2)
	}
	t.Logf("B档#1 结算真扣钱去重守恒 ok: 扣 C=%d、balance=A−C=%d、守恒成立;重跑水位去重不双扣", C, A-C)
}

// B档#1(结算扣款侧·堵HIGH-1盲区):非 observe 余额耗尽→硬停 converge 把成员 token override→0;充值回正→恢复档额。
func TestIntegration_HardStopConvergeRecover(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	svc, store, _, _ := escrowSvc(t, ctx, "nexus_hstop") // 非 observe
	newapiSQLDSN := os.Getenv("NEXUS_IT_NEWAPI_SQL_DSN")
	const orgID, memberID = int64(802), int64(1)
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO organization (id, name, slug, billing_enabled, hard_stop_enabled) VALUES (?, 'hstop-org', 'hstop-slug', 1, 1)`, orgID); err != nil {
		t.Fatalf("建组织失败: %v", err)
	}
	cred, perr := svc.EnsureOrgProvisioned(ctx, orgID, "hstop-org")
	if perr != nil {
		t.Fatalf("开通失败: %v", perr)
	}
	ml := int64(25_000_000)
	tierID, terr := store.CreateTier(ctx, &model.Tier{OrgID: orgID, Name: "hstop-tier", MonthlyLimit: &ml})
	if terr != nil {
		t.Fatalf("建档失败: %v", terr)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO member (id, org_id, tier_id, login_email, status, bootstrap_state) VALUES (?, ?, ?, 'hstop@t.local', 'active', 'done')`, memberID, orgID, tierID); err != nil {
		t.Fatalf("建成员失败: %v", err)
	}
	tokenID := selfServeMemberToken(t, ctx, svc, store, orgID, memberID) // 初始 unlimited
	opc := session.Claims{Role: session.RoleOperator, OrgID: orgID}
	const A = int64(10_000_000)
	if _, err := svc.Recharge(ctx, opc, orgID, service.RechargeInput{AmountQuota: A, TransferNo: "hs-1"}); err != nil {
		t.Fatalf("入账失败: %v", err)
	}
	ndb, _ := sql.Open("mysql", newapiSQLDSN)
	defer ndb.Close()
	var maxLogID int64
	_ = ndb.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM logs`).Scan(&maxLogID)
	logTS := time.Now().Unix() - 300
	if _, err := store.DB().ExecContext(ctx, `UPDATE settlement_cursor SET last_settled_ts=?, last_settled_log_id=? WHERE org_id=0`, logTS-1, maxLogID); err != nil {
		t.Fatalf("置结算水位失败: %v", err)
	}
	const consume = int64(15_000_000) // > A → 结算后 balance ≤0
	seedConsumptionLogTok(t, newapiSQLDSN, int64(cred.NewapiUserID), tokenID, "gpt-hs", consume, logTS)

	tokUnlimRemain := func() (unlim int, remain int64) {
		_ = ndb.QueryRowContext(ctx, `SELECT unlimited_quota, remain_quota FROM tokens WHERE id=?`, tokenID).Scan(&unlim, &remain)
		return
	}
	orgStatus := func() (s string) {
		_ = store.DB().QueryRowContext(ctx, `SELECT status FROM organization WHERE id=?`, orgID).Scan(&s)
		return
	}
	if _, err := svc.RunSettlement(ctx); err != nil {
		t.Fatalf("结算失败: %v", err)
	}
	if s := orgStatus(); s != model.OrgStatusStopped {
		t.Fatalf("🔴余额耗尽应 stopped,实=%s", s)
	}
	unlim, remain := tokUnlimRemain()
	if unlim != 0 || remain != 0 {
		t.Fatalf("🔴硬停应把成员 token override→0(unlimited=0/remain=0),实 unlimited=%d/remain=%d(HIGH-1类:硬停没停到人=漏钱)", unlim, remain)
	}
	// 恢复:充值回正 → active → converge → 成员 token 恢复到档月额(非 0)。
	if _, err := svc.Recharge(ctx, opc, orgID, service.RechargeInput{AmountQuota: 30_000_000, TransferNo: "hs-2"}); err != nil {
		t.Fatalf("恢复充值失败: %v", err)
	}
	if s := orgStatus(); s != model.OrgStatusActive {
		t.Fatalf("🔴充值回正应 active,实=%s", s)
	}
	_, remain2 := tokUnlimRemain()
	if remain2 != ml {
		t.Fatalf("🔴恢复应下发档月额 %d,实 remain=%d", ml, remain2)
	}
	t.Logf("B档#1 硬停converge→0与恢复 ok: 余额耗尽→stopped+成员token override 0(硬停停到人);充值回正→active+恢复档额 %d", ml)
}

// HIGH-1(真站审查):非 observe 周期重置/硬停经 ListActiveOverridableMembers 真下发成员 token。原查询查 member 表不存在的
// newapi_user_id 列 → ERROR 1054 → 该下发的成员一个都下发不了=漏钱;修后按 newapi_token_id 查。去修复本用例变红。
func TestIntegration_ResetDownlinkNonObserve(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	svc, store, _, _ := escrowSvc(t, ctx, "nexus_hs") // 非 observe
	newapiSQLDSN := os.Getenv("NEXUS_IT_NEWAPI_SQL_DSN")
	const orgID, memberID = int64(701), int64(1)
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO organization (id, name, slug) VALUES (?, 'hs-org', 'hs-slug')`, orgID); err != nil {
		t.Fatalf("建组织失败: %v", err)
	}
	if _, err := svc.EnsureOrgProvisioned(ctx, orgID, "hs-org"); err != nil {
		t.Fatalf("开通失败: %v", err)
	}
	ml := int64(25_000_000)
	tierID, terr := store.CreateTier(ctx, &model.Tier{OrgID: orgID, Name: "hs-tier", MonthlyLimit: &ml})
	if terr != nil {
		t.Fatalf("建档失败: %v", terr)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO member (id, org_id, tier_id, login_email, status, bootstrap_state) VALUES (?, ?, ?, 'hs@t.local', 'active', 'done')`, memberID, orgID, tierID); err != nil {
		t.Fatalf("建成员失败: %v", err)
	}
	tokenID := selfServeMemberToken(t, ctx, svc, store, orgID, memberID) // token 初始 unlimited_quota=true
	// 一条 due 的日策略(last_reset_at NULL → 跨当日边界 → 非 observe 触发重置下发)。
	if err := store.UpsertQuotaPolicy(ctx, &repo.QuotaPolicy{OrgID: orgID, Scope: "org", ScopeID: orgID, Period: "daily", LimitQuota: ml}); err != nil {
		t.Fatalf("建策略失败: %v", err)
	}

	if _, err := svc.ResetDuePolicies(ctx); err != nil {
		t.Fatalf("周期重置失败: %v", err)
	}
	// 验:成员 token 从 unlimited 被下发成有限额(HIGH-1 未修则 ListActiveOverridableMembers 报 1054→不下发→仍 unlimited)。
	ndb, _ := sql.Open("mysql", newapiSQLDSN)
	defer ndb.Close()
	var unlimited int
	var remain int64
	if err := ndb.QueryRowContext(ctx, `SELECT unlimited_quota, remain_quota FROM tokens WHERE id = ?`, tokenID).Scan(&unlimited, &remain); err != nil {
		t.Fatalf("查 token 失败: %v", err)
	}
	if unlimited != 0 {
		t.Fatalf("🔴HIGH-1:非 observe 周期重置应把成员 token 下发有限额(unlimited=0),实 unlimited=%d——ListActiveOverridableMembers 未生效=硬停/重置漏钱雷", unlimited)
	}
	if remain != ml {
		t.Fatalf("🔴下发额度应=档月额 %d,实=%d", ml, remain)
	}
	// 直接查也应无 1054、返回该就绪成员。
	all, aerr := store.ListActiveOverridableMembers(ctx, orgID)
	if aerr != nil {
		t.Fatalf("🔴HIGH-1:ListActiveOverridableMembers 仍报错(1054?): %v", aerr)
	}
	if len(all) != 1 {
		t.Fatalf("应列出 1 个就绪成员(有 token),实=%d", len(all))
	}
	t.Logf("HIGH-1 真账 ok: 非 observe 周期重置经 ListActiveOverridableMembers(改查 newapi_token_id)真下发成员 token 有限额(unlimited→0,remain=%d),无 1054", remain)
}

// M1(真站黑盒复现):停用成员用未过期会话 token 自助建/轮换 key 绕过禁用。修后三自助端点前置 status==active。
func TestIntegration_DisabledMemberSelfServeBlocked(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	svc, store, _, _ := escrowSvc(t, ctx, "nexus_m1blk")
	const orgID, memberID = int64(702), int64(1)
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO organization (id, name, slug) VALUES (?, 'm1-org', 'm1-slug')`, orgID); err != nil {
		t.Fatalf("建组织失败: %v", err)
	}
	if _, err := svc.EnsureOrgProvisioned(ctx, orgID, "m1-org"); err != nil {
		t.Fatalf("开通失败: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO member (id, org_id, login_email, status, bootstrap_state) VALUES (?, ?, 'm1@t.local', 'active', 'done')`, memberID, orgID); err != nil {
		t.Fatalf("建成员失败: %v", err)
	}
	_ = selfServeMemberToken(t, ctx, svc, store, orgID, memberID)
	admin := session.Claims{Role: session.RoleOrgAdmin, OrgID: orgID, MemberID: 999}
	member := session.Claims{Role: session.RoleMember, OrgID: orgID, MemberID: memberID} // 停用前签发的会话

	if err := svc.SetMemberStatus(ctx, admin, orgID, memberID, false); err != nil { // 停用(禁用不删)
		t.Fatalf("停用失败: %v", err)
	}
	// A1(28-§阻断):员工 key 纯只读——三自助端点对 member 一律 403(基于角色,不再依赖 status)。
	// 停用后成员用自己会话调 → 403。
	if _, _, e := svc.CreateMemberToken(ctx, member, memberID, "default"); !isForbidden(e) {
		t.Fatalf("🔴A1:member CreateMemberToken 应 403,实=%v", e)
	}
	if _, _, e := svc.RotateKey(ctx, member, orgID, memberID); !isForbidden(e) {
		t.Fatalf("🔴A1:member RotateKey 应 403,实=%v", e)
	}
	if e := svc.SetKeyIPWhitelist(ctx, member, orgID, memberID, "203.0.113.5"); !isForbidden(e) {
		t.Fatalf("🔴A1:member SetKeyIPWhitelist 应 403,实=%v", e)
	}
	if _, e := svc.MemberUsableGroups(ctx, member); !isForbidden(e) {
		t.Fatalf("🔴A1:member MemberUsableGroups 应 403,实=%v", e)
	}
	// 恢复启用后,member 侧仍一律 403(读只读,自助已整体下线,不再"恢复后可自助")。
	if err := svc.SetMemberStatus(ctx, admin, orgID, memberID, true); err != nil {
		t.Fatalf("恢复启用失败: %v", err)
	}
	if _, _, e := svc.RotateKey(ctx, member, orgID, memberID); !isForbidden(e) {
		t.Fatalf("🔴A1:启用后 member RotateKey 仍应 403(纯只读),实=%v", e)
	}
	t.Logf("A1 真账 ok: member 自助 CreateMemberToken/RotateKey/SetKeyIPWhitelist/MemberUsableGroups 一律 403(纯只读,不依赖 status)")
}

// F-A/F-C(真站联调发现):OpenMember 的 FinalizeBootstrap 失败必须补偿——删孤儿 token + 标 failed + 释放邮箱,不卡 provisioning。
// 注入:令下一个 new-api token id=nn + 预置平台 stale member_key_token 占用 uk_key_token_newapi=nn(复现真站 reset 脏库),
// OpenMember 建 token 得 nn → finalize 插 member_key_token(nn) 撞 uk 真报错(insertKeyTokenTx 是普通 INSERT 非 IGNORE)。
// 去掉 F-A 补偿则本用例变红(member 卡 provisioning + 孤儿 token active)。
func TestIntegration_OpenMemberFinalizeCompensation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	svc, store, _, _ := escrowSvc(t, ctx, "nexus_fin")
	newapiSQLDSN := os.Getenv("NEXUS_IT_NEWAPI_SQL_DSN")
	const orgID = int64(601)
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO organization (id, name, slug) VALUES (?, 'fin-org', 'fin-slug')`, orgID); err != nil {
		t.Fatalf("建组织失败: %v", err)
	}
	if _, err := svc.EnsureOrgProvisioned(ctx, orgID, "fin-org"); err != nil {
		t.Fatalf("开通失败: %v", err)
	}
	tierID, terr := store.CreateTier(ctx, &model.Tier{OrgID: orgID, Name: "fin-tier"})
	if terr != nil {
		t.Fatalf("建档失败: %v", terr)
	}

	ndb, err := sql.Open("mysql", newapiSQLDSN)
	if err != nil {
		t.Fatalf("连 newapi 库失败: %v", err)
	}
	defer ndb.Close()
	var maxTok int64
	_ = ndb.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM tokens`).Scan(&maxTok)
	nn := maxTok + 100
	if _, err := ndb.ExecContext(ctx, fmt.Sprintf("ALTER TABLE tokens AUTO_INCREMENT = %d", nn)); err != nil {
		t.Fatalf("置 tokens auto_increment 失败: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`INSERT INTO member_key_token (key_id, org_id, member_id, newapi_token_id, token_name, is_current, rotation, status)
		 VALUES (999, ?, 999, ?, 'stale', 1, 0, 'active')`, orgID, nn); err != nil {
		t.Fatalf("预置 stale key_token 失败: %v", err)
	}

	admin := session.Claims{Role: session.RoleOrgAdmin, OrgID: orgID, MemberID: 999}
	if _, oerr := svc.OpenMember(ctx, admin, orgID, service.OpenMemberInput{Name: "钱一", Email: "fin@t.local", TierID: &tierID}); oerr == nil {
		t.Fatalf("🔴注入 finalize 失败(uk 撞 %d)应报错,实成功——注入未生效", nn)
	}
	// ① 成员不卡 provisioning:bootstrap_state=failed + 邮箱释放(failed- 前缀)。
	var bs, email string
	if err := store.DB().QueryRowContext(ctx, `SELECT bootstrap_state, login_email FROM member WHERE org_id=? ORDER BY id DESC LIMIT 1`, orgID).Scan(&bs, &email); err != nil {
		t.Fatalf("查成员失败: %v", err)
	}
	if bs != model.BootstrapFailed {
		t.Fatalf("🔴finalize 失败后 bootstrap_state 应=failed(不卡 provisioning),实=%s", bs)
	}
	if !strings.HasPrefix(email, "failed-") {
		t.Fatalf("🔴finalize 失败应释放邮箱(failed- 前缀),实=%s", email)
	}
	// ② 无孤儿:补偿删除 new-api token nn(不再 active)。
	var live int
	_ = ndb.QueryRowContext(ctx, `SELECT COUNT(*) FROM tokens WHERE id=? AND deleted_at IS NULL`, nn).Scan(&live)
	if live != 0 {
		t.Fatalf("🔴finalize 失败应补偿删除孤儿 token,实 new-api 仍有 active token id=%d", nn)
	}
	// ③ 同邮箱可重开成功(邮箱已释放;新 token 得 nn+1 不撞)。
	if _, rerr := svc.OpenMember(ctx, admin, orgID, service.OpenMemberInput{Name: "钱一", Email: "fin@t.local", TierID: &tierID}); rerr != nil {
		t.Fatalf("🔴释放邮箱后同邮箱应可重开,实错: %v", rerr)
	}
	t.Logf("F-A/F-C 真账 ok: finalize 失败(uk 撞 nn=%d)→补偿删孤儿 token(不再active)+标 bootstrap_state=failed+释放邮箱→同邮箱重开成功", nn)
}

// 步骤5:401 自愈——破坏 org access_token → token 操作 401 → EnsureFreshCred 重登刷新 → 重试成功。
func TestIntegration_CredSelfHeal401(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	svc, store, _, km := escrowSvc(t, ctx, "nexus_heal")
	newapiSQLDSN := os.Getenv("NEXUS_IT_NEWAPI_SQL_DSN")
	const orgID, memberID = int64(501), int64(1)
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO organization (id, name, slug) VALUES (?, 'heal-org', 'heal-slug')`, orgID); err != nil {
		t.Fatalf("建组织失败: %v", err)
	}
	if _, err := svc.EnsureOrgProvisioned(ctx, orgID, "heal-org"); err != nil {
		t.Fatalf("开通失败: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO member (id, org_id, login_email, status, bootstrap_state) VALUES (?, ?, 'heal@t.local', 'active', 'done')`, memberID, orgID); err != nil {
		t.Fatalf("建成员失败: %v", err)
	}
	tokenID := selfServeMemberToken(t, ctx, svc, store, orgID, memberID) // 有效凭证下建 token

	// 破坏 org access_token:用**同一把 keyring**加密一个垃圾令牌落库(=失效 access_token)。
	garbage, err := km.EncryptString("invalid-access-token-xyz")
	if err != nil {
		t.Fatalf("加密垃圾令牌失败: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE organization SET newapi_access_token_enc = ? WHERE id = ?`, []byte(garbage), orgID); err != nil {
		t.Fatalf("破坏 access_token 失败: %v", err)
	}

	// token 操作(禁用)→ 垃圾令牌 401 → withOrgCred → EnsureFreshCred 用存的密码重登刷新 → 重试成功。
	admin := session.Claims{Role: session.RoleOrgAdmin, OrgID: orgID, MemberID: 999}
	if err := svc.SetMemberStatus(ctx, admin, orgID, memberID, false); err != nil {
		t.Fatalf("🔴401 自愈失败:token 操作应经重登自愈成功,实错: %v", err)
	}
	// 落库 access_token 已刷新(不再是垃圾)。
	var afterEnc []byte
	if err := store.DB().QueryRowContext(ctx, `SELECT newapi_access_token_enc FROM organization WHERE id = ?`, orgID).Scan(&afterEnc); err != nil {
		t.Fatalf("读刷新后凭证失败: %v", err)
	}
	if string(afterEnc) == garbage {
		t.Fatalf("🔴401 自愈应刷新落库 access_token,实仍是垃圾值")
	}
	// 验禁用确实生效(自愈后重试成功,token status=2)。
	ndb, _ := sql.Open("mysql", newapiSQLDSN)
	defer ndb.Close()
	var st int
	_ = ndb.QueryRowContext(ctx, `SELECT status FROM tokens WHERE id = ?`, tokenID).Scan(&st)
	if st != 2 {
		t.Fatalf("🔴自愈后禁用应生效 status=2,实=%d", st)
	}
	t.Logf("步骤5 401自愈真账 ok: 破坏 org access_token→token 操作401→EnsureFreshCred 重登刷新落库→重试成功(禁用生效 status=2)")
}

// 步骤4:成员三态——禁用(token置禁用不删,key保留)/恢复(启用同key)/离职(删token+软删转离职列表)。
func TestIntegration_MemberLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	svc, store, _, _ := escrowSvc(t, ctx, "nexus_life")
	newapiSQLDSN := os.Getenv("NEXUS_IT_NEWAPI_SQL_DSN")
	const orgID, memberID = int64(401), int64(1)
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO organization (id, name, slug) VALUES (?, 'life-org', 'life-slug')`, orgID); err != nil {
		t.Fatalf("建组织失败: %v", err)
	}
	if _, err := svc.EnsureOrgProvisioned(ctx, orgID, "life-org"); err != nil {
		t.Fatalf("开通失败: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO member (id, org_id, login_email, status, bootstrap_state) VALUES (?, ?, 'life@t.local', 'active', 'done')`, memberID, orgID); err != nil {
		t.Fatalf("建成员失败: %v", err)
	}
	tokenID := selfServeMemberToken(t, ctx, svc, store, orgID, memberID) // 真 token

	ndb, err := sql.Open("mysql", newapiSQLDSN)
	if err != nil {
		t.Fatalf("连 newapi 库失败: %v", err)
	}
	defer ndb.Close()
	tokenStatus := func() int {
		var s sql.NullInt64
		_ = ndb.QueryRowContext(ctx, `SELECT status FROM tokens WHERE id = ?`, tokenID).Scan(&s)
		return int(s.Int64)
	}
	if st := tokenStatus(); st != 1 {
		t.Fatalf("初始 token 应 enabled=1,实=%d", st)
	}
	admin := session.Claims{Role: session.RoleOrgAdmin, OrgID: orgID, MemberID: 999}

	// 禁用不删:token 置禁用(status=2),指针保留、成员 disabled。
	if err := svc.SetMemberStatus(ctx, admin, orgID, memberID, false); err != nil {
		t.Fatalf("禁用失败: %v", err)
	}
	if st := tokenStatus(); st != 2 {
		t.Fatalf("🔴禁用应把 token 置禁用 status=2(不删),实=%d", st)
	}
	m, _ := store.GetMember(ctx, orgID, memberID)
	if m.NewapiTokenID == nil || *m.NewapiTokenID != tokenID {
		t.Fatalf("🔴禁用不应删 token 指针(禁用不删,key 保留)")
	}
	if m.Status != model.MemberStatusDisabled {
		t.Fatalf("成员状态应 disabled,实=%s", m.Status)
	}

	// 恢复:启用同一 token(status=1,同 key)。
	if err := svc.SetMemberStatus(ctx, admin, orgID, memberID, true); err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	if st := tokenStatus(); st != 1 {
		t.Fatalf("🔴恢复应把 token 置启用 status=1,实=%d", st)
	}
	m2, _ := store.GetMember(ctx, orgID, memberID)
	if m2.NewapiTokenID == nil || *m2.NewapiTokenID != tokenID {
		t.Fatalf("🔴恢复应是同一 key(token 不变)")
	}

	// 离职:删 token + 软删转离职列表(活跃列表消失)。
	if err := svc.OffboardMember(ctx, admin, orgID, memberID); err != nil {
		t.Fatalf("离职失败: %v", err)
	}
	if _, gerr := store.GetMember(ctx, orgID, memberID); gerr == nil {
		t.Fatalf("🔴离职后成员应从活跃列表消失(软删)")
	}
	off, total, _ := store.ListOffboardedMembers(ctx, orgID, 10, 0)
	if total != 1 || len(off) != 1 {
		t.Fatalf("🔴离职成员应在离职列表,实 total=%d", total)
	}
	t.Logf("步骤4 成员三态真账 ok: 禁用→token status=2(不删/指针保留/近实时)、恢复→status=1(同 key)、离职→删 token+软删转离职列表(活跃列表消失,可恢复)")
}

// escrowSvc 起一套对真 MySQL(独立库 dbName)+ 真 newapi 的非 observe 服务(escrow 涉钱测试公共脚手架)。返回同一把 keyring。
func escrowSvc(t *testing.T, ctx context.Context, dbName string) (*service.Service, *repo.Store, newapi.NewapiAdapter, *crypto.Keyring) {
	t.Helper()
	dsn := os.Getenv("NEXUS_IT_DSN")
	newapiURL := os.Getenv("NEXUS_IT_NEWAPI_URL")
	newapiSQLDSN := os.Getenv("NEXUS_IT_NEWAPI_SQL_DSN")
	if dsn == "" || newapiURL == "" || newapiSQLDSN == "" {
		t.Skip("跳过:需 NEXUS_IT_DSN / NEXUS_IT_NEWAPI_URL / NEXUS_IT_NEWAPI_SQL_DSN")
	}
	ensureDatabase(t, newapiSQLDSN, dbName)
	if !strings.Contains(dsn, "/nexus?") {
		t.Fatalf("NEXUS_IT_DSN 不含 /nexus?: %q", dsn)
	}
	store, err := repo.Open(ctx, strings.Replace(dsn, "/nexus?", "/"+dbName+"?", 1))
	if err != nil {
		t.Fatalf("连 %s 失败: %v", dbName, err)
	}
	t.Cleanup(func() { _, _ = store.DB().Exec("DROP DATABASE IF EXISTS " + dbName); store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	// 预置结算水位到 now:reconcile 的 drain(RunSettlement)成 no-op(escrow 测试不测结算,避免拉共享 newapi 日志 backlog)。
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO settlement_cursor (org_id, last_settled_ts, last_settled_log_id) VALUES (0, UNIX_TIMESTAMP(), 0)`); err != nil {
		t.Fatalf("置结算水位失败: %v", err)
	}
	adminToken, adminUID := setupRC4(t, newapiURL)
	keyring := mustKeyring(t)
	signer, _ := session.NewSigner([]byte("integration-test-session-key-32b!!"), time.Hour)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	upstream := newapi.New(newapi.Config{BaseURL: newapiURL, AdminToken: adminToken, AdminUserID: adminUID, Timeout: 15 * time.Second}, nil)
	svc := service.New(service.Deps{Store: store, Upstream: upstream, Keyring: keyring, Signer: signer, Logger: log, FundingEnabled: true}) // escrow 涉钱测试:开 v2 funding 闸
	return svc, store, upstream, keyring
}

// F1/F2:并发入账无丢失更新 + 守恒 + 无 seq 撞 uk/越 cap(R5 修复:per-org 锁 + 单事务 FOR UPDATE)。
func TestIntegration_EscrowConcurrency(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	svc, store, upstream, _ := escrowSvc(t, ctx, "nexus_escc")
	const orgID = int64(101) // 各 escrow 测试用不同 orgID:共享同一 newapi,同 orgID→同 org username→adopt 撞密码
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO organization (id, name, slug) VALUES (?, 'escc-org', 'escc-slug')`, orgID); err != nil {
		t.Fatalf("建组织失败: %v", err)
	}
	opc := session.Claims{Role: session.RoleOperator, OrgID: orgID}
	cred, err := svc.EnsureOrgProvisioned(ctx, orgID, "escc-org")
	if err != nil {
		t.Fatalf("开通失败: %v", err)
	}
	base, _ := upstream.GetUserQuota(ctx, cred.NewapiUserID) // 清零后应=0

	const N = 8
	const A = int64(100_000_000) // 8*A=8e8 < cap 2e9,全进窗口
	var wg sync.WaitGroup
	errs := make([]error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = svc.Recharge(ctx, opc, orgID, service.RechargeInput{AmountQuota: A, TransferNo: fmt.Sprintf("escc-%d", i)})
		}(i)
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Fatalf("并发入账#%d 失败(seq 撞 uk/锁?): %v", i, e)
		}
	}
	window, _ := upstream.GetUserQuota(ctx, cred.NewapiUserID)
	bal, _ := svc.GetDerivedBalance(ctx, opc, orgID)
	if window-base != int64(N)*A { // 无丢失更新:N 笔 add 全到账
		t.Fatalf("🔴并发入账丢失更新:窗口增量=%d 应=%d(N*A)——锁/事务失守", window-base, int64(N)*A)
	}
	released := window - base
	if released+bal.HoldingQuota != int64(N)*A {
		t.Fatalf("🔴守恒破:已释放%d+托管%d 应=%d", released, bal.HoldingQuota, int64(N)*A)
	}
	t.Logf("F1/F2 并发入账真账 ok: %d 笔并发无丢失更新(窗口=N*A=%d)+守恒,无 seq 撞 uk/越 cap", N, int64(N)*A)
}

// F3/F4:退款真减 newapi 窗口(非只减死账=双付)+ 对账自愈 newapi 写残窗(R5 修复)。
func TestIntegration_EscrowRefundReconcile(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	svc, store, upstream, _ := escrowSvc(t, ctx, "nexus_escrr")
	const orgID = int64(102) // 不同 orgID 隔离 newapi org user(见 EscrowConcurrency 注释)
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO organization (id, name, slug) VALUES (?, 'escrr-org', 'escrr-slug')`, orgID); err != nil {
		t.Fatalf("建组织失败: %v", err)
	}
	opc := session.Claims{Role: session.RoleOperator, OrgID: orgID}
	cred, err := svc.EnsureOrgProvisioned(ctx, orgID, "escrr-org")
	if err != nil {
		t.Fatalf("开通失败: %v", err)
	}
	base, _ := upstream.GetUserQuota(ctx, cred.NewapiUserID)

	const A = int64(200_000_000)
	if _, err := svc.Recharge(ctx, opc, orgID, service.RechargeInput{AmountQuota: A, TransferNo: "rr-1"}); err != nil {
		t.Fatalf("入账失败: %v", err)
	}
	w1, _ := upstream.GetUserQuota(ctx, cred.NewapiUserID)

	// F3:退款 Z → newapi 窗口必须真减 Z(不是只减 company_balance 死账)。
	const Z = int64(50_000_000)
	if _, err := svc.DebitBalance(ctx, opc, orgID, service.DebitInput{AmountQuota: Z, Reason: "线下退款冲正"}); err != nil {
		t.Fatalf("退款失败: %v", err)
	}
	w2, _ := upstream.GetUserQuota(ctx, cred.NewapiUserID)
	if w1-w2 != Z {
		t.Fatalf("🔴F3 退款未真减 newapi 窗口(=双付):窗口 %d→%d 应减 %d", w1, w2, Z)
	}
	bal, _ := svc.GetDerivedBalance(ctx, opc, orgID)
	released := w2 - base
	if released+bal.HoldingQuota != A-Z {
		t.Fatalf("🔴退款后守恒破:已释放%d+托管%d 应=充值−退款%d", released, bal.HoldingQuota, A-Z)
	}

	// F4 对账口径(R5 后裁定:只减不加)。此测无成员消费 → 目标窗口=已释放(桶1)=w2。
	target := w2

	// F4a 超拨→自动 SUBTRACT 到目标:手工 ADD E 使窗口高于应有 → 对账减回(安全方向)。
	const E = int64(40_000_000)
	if err := upstream.ManageUserQuota(ctx, cred.NewapiUserID, newapi.QuotaAdd, E); err != nil {
		t.Fatalf("模拟超拨失败: %v", err)
	}
	if err := svc.ReconcileEscrow(ctx); err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	wSub, _ := upstream.GetUserQuota(ctx, cred.NewapiUserID)
	if wSub != target {
		t.Fatalf("🔴F4 超拨未自动减回目标:应=%d(减掉超拨 E),实=%d", target, wSub)
	}

	// F4b 欠拨→**绝不自动 ADD**:手工 SUBTRACT D 使窗口低于应有 → 对账只告警不补,窗口保持不变(§15 终极安全:对账永不自动加)。
	const D = int64(30_000_000)
	if err := upstream.ManageUserQuota(ctx, cred.NewapiUserID, newapi.QuotaSubtract, D); err != nil {
		t.Fatalf("模拟欠拨失败: %v", err)
	}
	wLow, _ := upstream.GetUserQuota(ctx, cred.NewapiUserID)
	if err := svc.ReconcileEscrow(ctx); err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	wAfterUnder, _ := upstream.GetUserQuota(ctx, cred.NewapiUserID)
	if wAfterUnder != wLow {
		t.Fatalf("🔴F4 对账对欠拨自动加了(终极安全破,对账绝不自动 ADD):窗口应保持 %d,实=%d", wLow, wAfterUnder)
	}
	// v1 M5 余额可见性(20-§3):org_admin(客户)看**读求和合计**(GetBalance=实时window+Σholding,只回 available),
	// 但**看不到 window/holding 拆分**(GetDerivedBalance 运营方专属,内部概念不漏给客户)。
	adminC := session.Claims{Role: session.RoleOrgAdmin, OrgID: orgID, MemberID: 1}
	if _, derr := svc.GetDerivedBalance(ctx, adminC, orgID); derr == nil {
		t.Fatalf("🔴window/holding 拆分(escrow-balance)应仅运营方可见,org_admin 竟可读")
	}
	gb, berr := svc.GetBalance(ctx, adminC, orgID)
	if berr != nil {
		t.Fatalf("org_admin 应可看读求和余额(GetBalance): %v", berr)
	}
	opBal, oerr := svc.GetDerivedBalance(ctx, session.Claims{Role: session.RoleOperator, OrgID: orgID}, orgID)
	if oerr != nil {
		t.Fatalf("operator 读拆分失败: %v", oerr)
	}
	if gb.AvailableQuota != opBal.AvailableQuota { // 客户合计 == 运营方拆分之和(同一读求和口径,零分叉)
		t.Fatalf("🔴客户读求和合计应=window+holding=%d,实=%d", opBal.AvailableQuota, gb.AvailableQuota)
	}
	if gb.BillingKind != model.BillingKindWallet {
		t.Fatalf("钱包组织 billing_kind 应=wallet,实=%s", gb.BillingKind)
	}
	t.Logf("F3/F4 真账 ok: 退款真减 newapi 窗口(−%d)+守恒;对账超拨自动减回目标(+%d 减掉)、欠拨绝不自动加(−%d 保持不补);v1 M5 org_admin 看读求和合计%d(=window+holding)不见拆分", Z, E, D, gb.AvailableQuota)
}

// OBS-3 回归:并发首开同组织 → 单一 org user + 落库 access_token 有效(不落被旋转作废的失效 token)。
func TestIntegration_ProvisionConcurrency(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	svc, store, upstream, _ := escrowSvc(t, ctx, "nexus_prov")
	const orgID = int64(201)
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO organization (id, name, slug) VALUES (?, 'prov-org', 'prov-slug')`, orgID); err != nil {
		t.Fatalf("建组织失败: %v", err)
	}
	const N = 5
	var wg sync.WaitGroup
	creds := make([]newapi.MemberCred, N)
	errs := make([]error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); creds[i], errs[i] = svc.EnsureOrgProvisioned(ctx, orgID, "prov-org") }(i)
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Fatalf("并发开通#%d 失败: %v", i, e)
		}
	}
	uid0 := creds[0].NewapiUserID
	for i, c := range creds {
		if c.NewapiUserID != uid0 {
			t.Fatalf("🔴并发开通竞态分叉:#%d user_id=%d != %d(应同一 org user)", i, c.NewapiUserID, uid0)
		}
	}
	// 落库 access_token 有效(非被旋转作废):用开通返回凭证建一个 token 应成功。
	if _, err := upstream.CreateToken(ctx, creds[0], newapi.TokenSpec{Name: "nexus_provtest_v1", UnlimitedQuota: true, ExpiredTime: -1}); err != nil {
		t.Fatalf("🔴落库 access_token 失效(OBS-3 旋转竞态未修):用开通凭证建 token 失败: %v", err)
	}
	t.Logf("OBS-3 并发首开真账 ok: %d 并发 → 单一 org user(#%d)+ 落库 access_token 有效(建 token 成功)", N, uid0)
}

// 步骤3:自动续充 worker——窗口 < 阈值触发,从托管补窗口到上限(与手工同锁同原子路径)。
func TestIntegration_AutoRefill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	svc, store, upstream, _ := escrowSvc(t, ctx, "nexus_arf")
	const orgID = int64(301)
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO organization (id, name, slug) VALUES (?, 'arf-org', 'arf-slug')`, orgID); err != nil {
		t.Fatalf("建组织失败: %v", err)
	}
	opc := session.Claims{Role: session.RoleOperator, OrgID: orgID}
	cred, err := svc.EnsureOrgProvisioned(ctx, orgID, "arf-org")
	if err != nil {
		t.Fatalf("开通失败: %v", err)
	}
	// 入账 3e9:窗口填到上限 2e9,余 1e9 入托管。
	const R = int64(3_000_000_000)
	if _, err := svc.Recharge(ctx, opc, orgID, service.RechargeInput{AmountQuota: R, TransferNo: "ar-1"}); err != nil {
		t.Fatalf("入账失败: %v", err)
	}
	w0, _ := upstream.GetUserQuota(ctx, cred.NewapiUserID)
	balBefore, _ := svc.GetDerivedBalance(ctx, opc, orgID)
	if balBefore.HoldingQuota <= 0 {
		t.Fatalf("入账溢出应有托管,实 holding=%d(窗口 %d)", balBefore.HoldingQuota, w0)
	}
	// 模拟消费把窗口降到阈值(DEFAULT_NEW 5e7)以下 → 触发自动续充。
	consume := w0 - int64(10_000_000)
	if err := upstream.ManageUserQuota(ctx, cred.NewapiUserID, newapi.QuotaSubtract, consume); err != nil {
		t.Fatalf("模拟消费失败: %v", err)
	}
	wLow, _ := upstream.GetUserQuota(ctx, cred.NewapiUserID)

	if err := svc.AutoRefill(ctx); err != nil { // worker 一 tick(内部重算阈值+触发续充)
		t.Fatalf("自动续充失败: %v", err)
	}
	wAfter, _ := upstream.GetUserQuota(ctx, cred.NewapiUserID)
	balAfter, _ := svc.GetDerivedBalance(ctx, opc, orgID)
	merged := wAfter - wLow
	if merged <= 0 {
		t.Fatalf("🔴自动续充未补窗口:窗口 %d→%d(阈值 5e7,窗口低于阈值应触发)", wLow, wAfter)
	}
	if balBefore.HoldingQuota-balAfter.HoldingQuota != merged { // 补入额==托管减少额(守恒)
		t.Fatalf("🔴自动续充守恒破:托管减 %d 应=窗口补入 %d", balBefore.HoldingQuota-balAfter.HoldingQuota, merged)
	}
	if wAfter > escrowWindowCapTest {
		t.Fatalf("🔴自动续充后窗口超上限:%d", wAfter)
	}
	t.Logf("步骤3 自动续充真账 ok: 窗口降到 %d<阈值 → 从托管并入 %d 补窗口到 %d(托管 %d→%d),补入额==托管减少额", wLow, merged, wAfter, balBefore.HoldingQuota, balAfter.HoldingQuota)
}

const escrowWindowCapTest = int64(2_000_000_000)

func TestIntegration_EscrowRecharge(t *testing.T) {
	dsn := os.Getenv("NEXUS_IT_DSN")
	newapiURL := os.Getenv("NEXUS_IT_NEWAPI_URL")
	newapiSQLDSN := os.Getenv("NEXUS_IT_NEWAPI_SQL_DSN")
	if dsn == "" || newapiURL == "" || newapiSQLDSN == "" {
		t.Skip("跳过:需 NEXUS_IT_DSN / NEXUS_IT_NEWAPI_URL / NEXUS_IT_NEWAPI_SQL_DSN")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	ensureDatabase(t, newapiSQLDSN, "nexus_escrow")
	if !strings.Contains(dsn, "/nexus?") {
		t.Fatalf("NEXUS_IT_DSN 不含 /nexus?: %q", dsn)
	}
	store, err := repo.Open(ctx, strings.Replace(dsn, "/nexus?", "/nexus_escrow?", 1))
	if err != nil {
		t.Fatalf("连 nexus_escrow 失败: %v", err)
	}
	defer store.Close()
	defer func() { _, _ = store.DB().Exec("DROP DATABASE IF EXISTS nexus_escrow") }()
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	adminToken, adminUID := setupRC4(t, newapiURL)
	keyring := mustKeyring(t)
	signer, _ := session.NewSigner([]byte("integration-test-session-key-32b!!"), time.Hour)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	upstream := newapi.New(newapi.Config{BaseURL: newapiURL, AdminToken: adminToken, AdminUserID: adminUID, Timeout: 15 * time.Second}, nil)
	svc := service.New(service.Deps{Store: store, Upstream: upstream, Keyring: keyring, Signer: signer, Logger: log, FundingEnabled: true}) // escrow 涉钱测试:开 v2 funding 闸

	const orgID = int64(103) // 不同 orgID 隔离 newapi org user(见 EscrowConcurrency 注释)
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO organization (id, name, slug) VALUES (?, 'escrow-org', 'escrow-org-slug')`, orgID); err != nil {
		t.Fatalf("建组织失败: %v", err)
	}
	opc := session.Claims{Role: session.RoleOperator, OrgID: orgID}

	// 先开通 org user 拿基线窗口(newapi 建用户可能带默认额度,故用增量判定)。
	cred, err := svc.EnsureOrgProvisioned(ctx, orgID, "escrow-org")
	if err != nil {
		t.Fatalf("开通 org user 失败: %v", err)
	}
	base, err := upstream.GetUserQuota(ctx, cred.NewapiUserID)
	if err != nil {
		t.Fatalf("读基线窗口失败: %v", err)
	}

	const r1 = int64(1_000_000_000) // $2000 < 窗口上限 2e9
	if _, err := svc.Recharge(ctx, opc, orgID, service.RechargeInput{AmountQuota: r1, TransferNo: "esc-t1"}); err != nil {
		t.Fatalf("入账1失败: %v", err)
	}
	w1, _ := upstream.GetUserQuota(ctx, cred.NewapiUserID)
	if w1-base != r1 { // ① add 非 override:窗口 = 基线 + r1(增量)
		t.Fatalf("🔴 入账应 add 增量:窗口 base=%d → w1=%d,增量应=%d 实=%d(若=r1 不含base 则是 override)", base, w1, r1, w1-base)
	}
	bal1, _ := svc.GetDerivedBalance(ctx, opc, orgID)
	if bal1.HoldingQuota != 0 {
		t.Fatalf("小额入账不应有托管,实 holding=%d", bal1.HoldingQuota)
	}

	// 入账2:大额溢出窗口 → 桶1 填到上限、余下入托管。
	const r2 = int64(2_000_000_000)
	if _, err := svc.Recharge(ctx, opc, orgID, service.RechargeInput{AmountQuota: r2, TransferNo: "esc-t2"}); err != nil {
		t.Fatalf("入账2失败: %v", err)
	}
	w2, _ := upstream.GetUserQuota(ctx, cred.NewapiUserID)
	bal2, _ := svc.GetDerivedBalance(ctx, opc, orgID)
	if w2 > 2_000_000_000 { // ③ 窗口封顶
		t.Fatalf("🔴 窗口超上限 escrowWindowCap:w2=%d", w2)
	}
	// ② 守恒:已释放(窗口增量 w2-base)+ 托管 = 总充值 r1+r2,绝不超拨。
	released := w2 - base
	if released+bal2.HoldingQuota != r1+r2 {
		t.Fatalf("🔴 守恒破:已释放%d + 托管%d = %d 应=总充值%d", released, bal2.HoldingQuota, released+bal2.HoldingQuota, r1+r2)
	}
	if bal2.HoldingQuota <= 0 {
		t.Fatalf("大额入账溢出应入托管,实 holding=%d(窗口 w2=%d)", bal2.HoldingQuota, w2)
	}

	// ④ 续充:模拟窗口被消费(subtract)腾出空间,再把托管并入窗口(add)。
	const consume = int64(800_000_000)
	if err := upstream.ManageUserQuota(ctx, cred.NewapiUserID, newapi.QuotaSubtract, consume); err != nil {
		t.Fatalf("模拟消费失败: %v", err)
	}
	wBefore, _ := upstream.GetUserQuota(ctx, cred.NewapiUserID)
	holdBefore := bal2.HoldingQuota
	if _, err := svc.RefillWindow(ctx, opc, orgID); err != nil {
		t.Fatalf("续充失败: %v", err)
	}
	wAfter, _ := upstream.GetUserQuota(ctx, cred.NewapiUserID)
	balAfter, _ := svc.GetDerivedBalance(ctx, opc, orgID)
	merged := wAfter - wBefore
	if merged <= 0 {
		t.Fatalf("续充应把托管 add 进窗口:窗口 %d→%d", wBefore, wAfter)
	}
	if holdBefore-balAfter.HoldingQuota != merged { // 续充守恒:托管减少 = 窗口增加
		t.Fatalf("🔴 续充守恒破:托管减 %d 应=窗口增 %d", holdBefore-balAfter.HoldingQuota, merged)
	}
	if wAfter > 2_000_000_000 {
		t.Fatalf("🔴 续充后窗口超上限:%d", wAfter)
	}
	// 总账守恒不变:续充只是托管→窗口搬运,(窗口增量+托管)仍=总充值。
	if (wAfter-base+consume)+balAfter.HoldingQuota != r1+r2 {
		t.Fatalf("🔴 续充后总账守恒破:已释放%d+消费%d+托管%d 应=总充值%d", wAfter-base, consume, balAfter.HoldingQuota, r1+r2)
	}

	t.Logf("R3 托管/入账/续充真账 ok: add非override(w增量=r1) + 守恒(已释放+托管=总充值) + 窗口封顶 + 溢出入托管 + 续充并桶")
}
