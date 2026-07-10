// 托管多桶/入账/续充真账测试(v2 R3,涉钱必真账):对真 newapi 验
//
//	① 入账用 add 非 override(窗口=旧值+额,不是覆盖);② 守恒:已释放(窗口增量)+托管 = 总充值,绝不超拨;
//	③ 窗口封顶 escrowWindowCap、溢出入托管;④ 续充把托管并入窗口(add)。
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

// B档#1(架构B更新,BE③ 扣款分支拆除):RunSettlement 只写报表(usage_ledger)+ 水位去重防双计;
// company_balance **绝不再被扣**(第二账退役,33 §5/§12-9——扣款分支删除的硬验收,反向锁死)。
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
	readLedger := func() (sum int64) {
		_ = store.DB().QueryRowContext(ctx, `SELECT COALESCE(SUM(consumed_quota),0) FROM usage_ledger WHERE org_id=?`, orgID).Scan(&sum)
		return
	}
	if _, err := svc.RunSettlement(ctx); err != nil {
		t.Fatalf("首次结算失败: %v", err)
	}
	if l1 := readLedger(); l1 != C {
		t.Fatalf("🔴首次结算应落账 C=%d,实 usage_ledger=%d", C, l1)
	}
	c1, b1 := readBal()
	if c1 != 0 || b1 != A {
		t.Fatalf("🔴架构B下结算绝不扣 company_balance(第二账退役):应 consumed=0 balance=A=%d,实 consumed=%d balance=%d", A, c1, b1)
	}
	if _, err := svc.RunSettlement(ctx); err != nil { // 二次结算:水位去重,绝不双计
		t.Fatalf("二次结算失败: %v", err)
	}
	if l2 := readLedger(); l2 != C {
		t.Fatalf("🔴重跑结算双计了(去重失效):usage_ledger %d→%d", C, l2)
	}
	if c2, b2 := readBal(); c2 != 0 || b2 != A {
		t.Fatalf("🔴重跑后 company_balance 被动了(扣款分支未拆净):consumed=%d balance=%d", c2, b2)
	}
	t.Logf("B档#1(架构B) 结算只写报表 ok: 落账 C=%d 一次;重跑水位去重不双计;company_balance 恒不动(balance=A=%d)", C, A)
}

// B档#1(结算扣款侧·堵HIGH-1盲区):非 observe 余额耗尽→硬停 converge 把成员 token override→0;充值回正→恢复档额。
// 架构B退役下线(BE③,33 §5/§12-9):本用例依赖"结算扣 company_balance→余额耗尽→recomputeOrgStatus→override 硬停"
// 整条链,其中扣款分支已删(链条起点消失),且 override 硬停在 B 下被 disable 硬停取代(31-ADR §4.5,归 BE①)。
// 保留骨架供阶段2 组长对齐时决定删除或改写为 disable 链路验收。
func TestIntegration_HardStopConvergeRecover(t *testing.T) {
	// B2 重写(总监裁定:重写为 disable 路径,不删):架构B 硬停=disable 金库 + fan-out disable
	// **全部成员 user**(硬停停到人,原用例灵魂保留);解除=enable 金库 + **只 enable 应 active 成员**
	// (个别停用的不解——其 disable 语义独立于硬停)。A 版"余额耗尽→stopped→override→0"链随
	// 扣款分支/override 机器退役(33 §12-9),硬停改显式运维动作(HardStopOrg),不由计费状态联动。
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	svc, store, upstream, _ := escrowSvc(t, ctx, "nexus_hstop")
	newapiSQLDSN := os.Getenv("NEXUS_IT_NEWAPI_SQL_DSN")
	const orgID, m1, m2 = int64(802), int64(1), int64(2)
	// 金库 + 注资 + 档位 + 两个成员 saga 开通(m1 保持 active;m2 稍后个别停用,验解除时"不解")。
	treasuryCred := mkEnterpriseUser(t, ctx, upstream, "hstop-treasury")
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO organization (id, name, slug, newapi_user_id) VALUES (?, 'hstop-org', 'hstop-slug', ?)`, orgID, treasuryCred.NewapiUserID); err != nil {
		t.Fatalf("建组织失败: %v", err)
	}
	if err := upstream.IncreaseUserQuota(ctx, treasuryCred.NewapiUserID, 10_000_000); err != nil {
		t.Fatalf("金库注资失败: %v", err)
	}
	amount := int64(2_000_000)
	tierID, terr := store.CreateTier(ctx, &model.Tier{OrgID: orgID, Name: "hstop-tier", AmountRaw: &amount})
	if terr != nil {
		t.Fatalf("建档失败: %v", terr)
	}
	uids := map[int64]int{}
	for i, mid := range []int64{m1, m2} {
		if _, err := store.DB().ExecContext(ctx, `INSERT INTO member (id, org_id, tier_id, login_email, status, bootstrap_state) VALUES (?, ?, ?, ?, 'provisioning', 'pending')`, mid, orgID, tierID, fmt.Sprintf("hstop%d@t.local", i+1)); err != nil {
			t.Fatalf("建成员 %d 失败: %v", mid, err)
		}
		uid, perr := svc.ProvisionMemberServiceAccount(ctx, orgID, mid, "", amount, "test")
		if perr != nil {
			t.Fatalf("开通成员 %d 服务账号失败: %v", mid, perr)
		}
		if err := store.ActivatePlatformAccount(ctx, orgID, mid); err != nil {
			t.Fatalf("置 active 失败: %v", err)
		}
		uids[mid] = uid
	}
	admin := session.Claims{Role: session.RoleOrgAdmin, OrgID: orgID, MemberID: 999}
	// m2 个别停用(独立 disable 语义;解除硬停时它必须保持 disabled)。
	if err := svc.SetMemberStatus(ctx, admin, orgID, m2, false); err != nil {
		t.Fatalf("个别停用 m2 失败: %v", err)
	}

	ndb, err := sql.Open("mysql", newapiSQLDSN)
	if err != nil {
		t.Fatalf("连 newapi 库失败: %v", err)
	}
	defer ndb.Close()
	userStatus := func(uid int) int {
		var st sql.NullInt64
		_ = ndb.QueryRowContext(ctx, `SELECT status FROM users WHERE id = ?`, uid).Scan(&st)
		return int(st.Int64)
	}
	orgStatus := func() (s string) {
		_ = store.DB().QueryRowContext(ctx, `SELECT status FROM organization WHERE id=?`, orgID).Scan(&s)
		return
	}

	// 硬停(运营方):金库 + 全部成员 user(含已个别停用的 m2,幂等)都 disable。
	op := session.Claims{Role: session.RoleOperator}
	if err := svc.HardStopOrg(ctx, op, orgID, true); err != nil {
		t.Fatalf("硬停失败: %v", err)
	}
	if st := userStatus(treasuryCred.NewapiUserID); st != 2 {
		t.Fatalf("🔴硬停应 disable 金库 user(status=2),实=%d", st)
	}
	for mid, uid := range uids {
		if st := userStatus(uid); st != 2 {
			t.Fatalf("🔴硬停 fan-out 应 disable 成员 %d 的 user(status=2,硬停没停到人=漏钱),实=%d", mid, st)
		}
	}
	if s := orgStatus(); s != model.OrgStatusHardStopped {
		t.Fatalf("🔴硬停后组织应 hard_stopped,实=%s", s)
	}
	// 硬停期间管理写被屏蔽(fail-closed)。
	if err := svc.SetMemberStatus(ctx, admin, orgID, m1, false); err == nil {
		t.Fatalf("🔴硬停期间管理写应被屏蔽(403),实成功")
	}
	// 幂等:重复硬停不报错。
	if err := svc.HardStopOrg(ctx, op, orgID, true); err != nil {
		t.Fatalf("重复硬停应幂等,实错: %v", err)
	}

	// 解除:金库 + m1(应 active)enable;m2(个别停用)保持 disabled——不解。
	if err := svc.HardStopOrg(ctx, op, orgID, false); err != nil {
		t.Fatalf("解除硬停失败: %v", err)
	}
	if st := userStatus(treasuryCred.NewapiUserID); st != 1 {
		t.Fatalf("🔴解除应 enable 金库 user(status=1),实=%d", st)
	}
	if st := userStatus(uids[m1]); st != 1 {
		t.Fatalf("🔴解除应 enable 应 active 成员 m1(status=1),实=%d", st)
	}
	if st := userStatus(uids[m2]); st != 2 {
		t.Fatalf("🔴解除不得解个别停用成员 m2(其 disable 独立于硬停,应保持 status=2),实=%d", st)
	}
	if s := orgStatus(); s != model.OrgStatusActive {
		t.Fatalf("🔴解除后组织应 active,实=%s", s)
	}
	t.Logf("B2 硬停disable路径真账 ok: 硬停=disable金库+fan-out停到人(m1/m2 user全status=2)+管理写屏蔽+幂等;解除=金库+m1恢复、个别停用m2不解、组织回active")
}

// TestIntegration_ResetDownlinkNonObserve 已随 reset 周期重置 + override 下发机器整体退役而删除
// (33 §12-4:quota_policy/临时额度不在 B 契约内;架构B 成员额度=其 new-api user.quota,无"下发 token override"一说)。

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
	// 架构B(33 §5):A1「member-403 层」随「org 凭证建 token」路径整体退役;成员自助令牌改走 /me/tokens,
	// 门禁 = member-only + 成员须 active + 服务账号已开通(requireSelfServiceMember)。
	// 停用后成员用自己会话调 → 403(M1 语义保留:未过期会话不能绕过禁用)。
	if _, _, e := svc.ListMyTokens(ctx, member); !isForbidden(e) {
		t.Fatalf("🔴停用成员 ListMyTokens 应 403,实=%v", e)
	}
	if _, e := svc.CreateMyToken(ctx, member, service.CreateMyTokenInput{Name: "blk", Group: "default"}); !isForbidden(e) {
		t.Fatalf("🔴停用成员 CreateMyToken 应 403,实=%v", e)
	}
	if _, e := svc.RevealMyTokenKey(ctx, member, 1); !isForbidden(e) {
		t.Fatalf("🔴停用成员 key:reveal 应 403,实=%v", e)
	}
	// 上级角色对 /me/tokens 无写权(33 §3.5 RBAC 铁律:member-only)。
	if _, e := svc.CreateMyToken(ctx, admin, service.CreateMyTokenInput{Name: "adm", Group: "default"}); !isForbidden(e) {
		t.Fatalf("🔴org_admin 写 /me/tokens 应 403,实=%v", e)
	}
	// 恢复启用:该成员是 A 版存量行(无服务账号)→ 409 不可自助(而非放行)。
	if err := svc.SetMemberStatus(ctx, admin, orgID, memberID, true); err != nil {
		t.Fatalf("恢复启用失败: %v", err)
	}
	if _, _, e := svc.ListMyTokens(ctx, member); e == nil {
		t.Fatal("🔴无服务账号成员 ListMyTokens 应 409,实成功")
	}
	t.Logf("架构B 门禁 ok: 停用成员 /me/tokens 全 403(不依赖前端)+ 上级角色无写权 + 无服务账号 409")
}

// F-A/F-C(真站联调发现):OpenMember 的 FinalizeBootstrap 失败必须补偿——删孤儿 token + 标 failed + 释放邮箱,不卡 provisioning。
// 注入:令下一个 new-api token id=nn + 预置平台 stale member_key_token 占用 uk_key_token_newapi=nn(复现真站 reset 脏库),
// OpenMember 建 token 得 nn → finalize 插 member_key_token(nn) 撞 uk 真报错(insertKeyTokenTx 是普通 INSERT 非 IGNORE)。
// 去掉 F-A 补偿则本用例变红(member 卡 provisioning + 孤儿 token active)。
func TestIntegration_OpenMemberFinalizeCompensation(t *testing.T) {
	// 架构B(33 §5 退役):OpenMember 不再走「org 凭证建 token + FinalizeBootstrap」——本用例注入的
	// member_key_token uk 撞车路径已不存在(开通=Provision saga,孤儿处置=disable+quarantined,
	// 由 TestIntegration_ArchB_ProvisionSaga 覆盖)。留壳记档,阶段2 组长确认后删除。
	t.Skip("架构B:A 版 FinalizeBootstrap 补偿路径已退役(开通孤儿处置改由 ArchB_ProvisionSaga 覆盖)")
}

// 步骤5:401 自愈——破坏 org access_token → token 操作 401 → EnsureFreshCred 重登刷新 → 重试成功。
func TestIntegration_CredSelfHeal401(t *testing.T) {
	// 架构B 原生 401 自愈(灰度门槛项①,33 §3.2):成员服务账号 access_token 失效 → 成员自助建 key
	// (CreateMyToken→WithMemberCred)→ 401 → 探活确认失效 → 用存的密码重登刷新 → **原子回存** → 重试成功。
	// 自愈对象=成员自己的服务账号凭证;旧版从 A 版入口(SetMemberStatus→org 凭证 token 操作)验自愈,
	// 架构B 的 SetMemberStatus 走 admin 侧 SetUserStatus 不碰 org 凭证,该前提已随模型退役,故重写。
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	svc, store, upstream, km := escrowSvc(t, ctx, "nexus_heal")
	newapiSQLDSN := os.Getenv("NEXUS_IT_NEWAPI_SQL_DSN")
	const orgID, memberID = int64(501), int64(1)
	// 金库 + 注资 + 档位 + 成员 saga 开通(架构B 数据形状,照 MemberLifecycle)。
	treasuryCred := mkEnterpriseUser(t, ctx, upstream, "heal-treasury")
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO organization (id, name, slug, newapi_user_id) VALUES (?, 'heal-org', 'heal-slug', ?)`, orgID, treasuryCred.NewapiUserID); err != nil {
		t.Fatalf("建组织失败: %v", err)
	}
	if err := upstream.IncreaseUserQuota(ctx, treasuryCred.NewapiUserID, 10_000_000); err != nil {
		t.Fatalf("金库注资失败: %v", err)
	}
	amount := int64(2_000_000)
	tierID, terr := store.CreateTier(ctx, &model.Tier{OrgID: orgID, Name: "heal-tier", AmountRaw: &amount})
	if terr != nil {
		t.Fatalf("建档失败: %v", terr)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO member (id, org_id, tier_id, login_email, status, bootstrap_state) VALUES (?, ?, ?, 'heal@t.local', 'provisioning', 'pending')`, memberID, orgID, tierID); err != nil {
		t.Fatalf("建成员失败: %v", err)
	}
	if _, perr := svc.ProvisionMemberServiceAccount(ctx, orgID, memberID, "", amount, "test"); perr != nil {
		t.Fatalf("开通服务账号失败: %v", perr)
	}
	if err := store.ActivatePlatformAccount(ctx, orgID, memberID); err != nil {
		t.Fatalf("置 active 失败: %v", err)
	}

	// 破坏成员服务账号 access_token:同一 keyring 加密垃圾值落库(=凭证失效;密码仍有效,自愈靠它重登)。
	garbage, gerr := km.EncryptString("invalid-access-token-xyz")
	if gerr != nil {
		t.Fatalf("加密垃圾令牌失败: %v", gerr)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE member SET newapi_access_token_enc = ? WHERE id = ?`, []byte(garbage), memberID); err != nil {
		t.Fatalf("破坏成员凭证失败: %v", err)
	}

	// 成员自助建 key → WithMemberCred:垃圾凭证 401 → ProbeAccessToken 证死 → 密码重登刷新回存 → 重试成功。
	mc := session.Claims{Role: session.RoleMember, OrgID: orgID, MemberID: memberID}
	tv, cerr := svc.CreateMyToken(ctx, mc, service.CreateMyTokenInput{Name: "heal-key", Group: "default"})
	if cerr != nil {
		t.Fatalf("🔴401 自愈失败:成员自助建 key 应经重登自愈成功,实错: %v", cerr)
	}
	// 落库凭证已刷新(不再是垃圾)。
	var afterEnc []byte
	if err := store.DB().QueryRowContext(ctx, `SELECT newapi_access_token_enc FROM member WHERE id = ?`, memberID).Scan(&afterEnc); err != nil {
		t.Fatalf("读刷新后凭证失败: %v", err)
	}
	if string(afterEnc) == garbage {
		t.Fatalf("🔴401 自愈应原子回存刷新后的 access_token,实仍是垃圾值")
	}
	// token 真建出来(new-api tokens 表 status=1,挂在成员自己的 user 下)。
	ndb, err := sql.Open("mysql", newapiSQLDSN)
	if err != nil {
		t.Fatalf("连 newapi 库失败: %v", err)
	}
	defer ndb.Close()
	var st int
	if err := ndb.QueryRowContext(ctx, `SELECT status FROM tokens WHERE id = ?`, tv.ID).Scan(&st); err != nil {
		t.Fatalf("查 token 失败: %v", err)
	}
	if st != 1 {
		t.Fatalf("🔴自愈后建出的 token 应 status=1,实=%d", st)
	}
	t.Logf("架构B 401 自愈真账 ok: 破坏成员服务账号凭证→自助建 key 401→探活证死→密码重登刷新原子回存→重试成功(token id=%d status=1)", tv.ID)
}

// 步骤4:成员三态——禁用(token置禁用不删,key保留)/恢复(启用同key)/离职(删token+软删转离职列表)。
func TestIntegration_MemberLifecycle(t *testing.T) {
	// 架构B 生命周期(31-ADR §4.5 / 33 §3.2):停用/恢复 = disable/enable **成员自己的 new-api user**;
	// 离职 = disable → 静默 → 未用额度反向划账退回金库(守恒);恢复入职 = enable + 如新建重新分配。
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	svc, store, upstream, _ := escrowSvc(t, ctx, "nexus_life")
	newapiSQLDSN := os.Getenv("NEXUS_IT_NEWAPI_SQL_DSN")
	const orgID, memberID = int64(401), int64(1)
	// 金库 org(真 new-api user)+ 注资 $20。
	treasuryCred := mkEnterpriseUser(t, ctx, upstream, "life-treasury")
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO organization (id, name, slug, newapi_user_id) VALUES (?, 'life-org', 'life-slug', ?)`, orgID, treasuryCred.NewapiUserID); err != nil {
		t.Fatalf("建组织失败: %v", err)
	}
	if err := upstream.IncreaseUserQuota(ctx, treasuryCred.NewapiUserID, 10_000_000); err != nil {
		t.Fatalf("金库注资失败: %v", err)
	}
	// 档位(amount_raw=$4=2M)+ 成员行 → Provision saga 开通(建号+首笔划账)。
	amount := int64(2_000_000)
	tierID, terr := store.CreateTier(ctx, &model.Tier{OrgID: orgID, Name: "life-tier", AmountRaw: &amount})
	if terr != nil {
		t.Fatalf("建档失败: %v", terr)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO member (id, org_id, tier_id, login_email, status, bootstrap_state) VALUES (?, ?, ?, 'life@t.local', 'provisioning', 'pending')`, memberID, orgID, tierID); err != nil {
		t.Fatalf("建成员失败: %v", err)
	}
	uid, perr := svc.ProvisionMemberServiceAccount(ctx, orgID, memberID, "", amount, "test")
	if perr != nil {
		t.Fatalf("开通服务账号失败: %v", perr)
	}
	if err := store.ActivatePlatformAccount(ctx, orgID, memberID); err != nil {
		t.Fatalf("置 active 失败: %v", err)
	}

	ndb, err := sql.Open("mysql", newapiSQLDSN)
	if err != nil {
		t.Fatalf("连 newapi 库失败: %v", err)
	}
	defer ndb.Close()
	userStatus := func() int {
		var st sql.NullInt64
		_ = ndb.QueryRowContext(ctx, `SELECT status FROM users WHERE id = ?`, uid).Scan(&st)
		return int(st.Int64)
	}
	if st := userStatus(); st != 1 {
		t.Fatalf("初始成员 user 应 enabled=1,实=%d", st)
	}
	admin := session.Claims{Role: session.RoleOrgAdmin, OrgID: orgID, MemberID: 999}

	// 停用:disable 成员 user(status=2,连带停其全部令牌;非 override)。
	if err := svc.SetMemberStatus(ctx, admin, orgID, memberID, false); err != nil {
		t.Fatalf("禁用失败: %v", err)
	}
	if st := userStatus(); st != 2 {
		t.Fatalf("🔴禁用应 disable 成员 user(status=2),实=%d", st)
	}
	m, _ := store.GetMember(ctx, orgID, memberID)
	if m.Status != model.MemberStatusDisabled {
		t.Fatalf("成员状态应 disabled,实=%s", m.Status)
	}
	// 恢复:enable(额度不动)。
	if err := svc.SetMemberStatus(ctx, admin, orgID, memberID, true); err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	if st := userStatus(); st != 1 {
		t.Fatalf("🔴恢复应 enable(status=1),实=%d", st)
	}

	// 离职:disable → 静默 → 退额守恒(金库回到 10M-2M+2M=10M,成员=0)。
	if err := svc.OffboardMember(ctx, admin, orgID, memberID); err != nil {
		t.Fatalf("离职失败: %v", err)
	}
	if st := userStatus(); st != 2 {
		t.Fatalf("🔴离职应 disable 成员 user,实=%d", st)
	}
	tq, _ := upstream.GetUserQuota(ctx, treasuryCred.NewapiUserID)
	mq, _ := upstream.GetUserQuota(ctx, uid)
	if tq != 10_000_000 || mq != 0 {
		t.Fatalf("🔴离职退额守恒破:金库=%d(期 10M) 成员=%d(期 0)", tq, mq)
	}
	if _, gerr := store.GetMember(ctx, orgID, memberID); gerr == nil {
		t.Fatalf("🔴离职后成员应从活跃列表消失(软删)")
	}
	off, total, _ := store.ListOffboardedMembers(ctx, orgID, 10, 0)
	if total != 1 || len(off) != 1 {
		t.Fatalf("🔴离职成员应在离职列表,实 total=%d", total)
	}
	// 幂等重调:余额 0 → 不双退。
	if err := svc.OffboardMember(ctx, admin, orgID, memberID); err != nil {
		t.Fatalf("离职重调应幂等成功: %v", err)
	}
	tq2, _ := upstream.GetUserQuota(ctx, treasuryCred.NewapiUserID)
	if tq2 != 10_000_000 {
		t.Fatalf("🔴离职重调双退:金库=%d", tq2)
	}

	// 恢复入职:enable + 如新建重新分配(金库→成员再划 2M)。
	if err := svc.RestoreMember(ctx, admin, orgID, memberID, tierID); err != nil {
		t.Fatalf("恢复入职失败: %v", err)
	}
	if st := userStatus(); st != 1 {
		t.Fatalf("🔴恢复入职应 enable 成员 user,实=%d", st)
	}
	tq3, _ := upstream.GetUserQuota(ctx, treasuryCred.NewapiUserID)
	mq3, _ := upstream.GetUserQuota(ctx, uid)
	if tq3 != 8_000_000 || mq3 != 2_000_000 {
		t.Fatalf("🔴恢复重新分配守恒破:金库=%d(期 8M) 成员=%d(期 2M)", tq3, mq3)
	}
	t.Logf("架构B 生命周期真账 ok: 停用/恢复=disable/enable user;离职=disable→退额守恒(幂等不双退);恢复=enable+重新分配守恒")
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

// TestIntegration_EscrowConcurrency(记账侧并发守恒,断言依赖已删的下发侧 GetDerivedBalance)
// 已随 45号 14/15 删除;v2 若重启 escrow 记账侧,重写断言直读 escrow_bucket。

// TestIntegration_EscrowRefundReconcile 已随 escrow 分桶下发/对账侧退役删除(45号 14/15;防复活守卫见 EscrowRetired)。


// TestIntegration_AutoRefill 已随 escrow 分桶下发/对账侧退役删除(45号 14/15;防复活守卫见 EscrowRetired)。


// TestIntegration_EscrowRecharge(入账+窗口封顶+读穿断言,依赖已删的 GetDerivedBalance)
// 已随 45号 14/15 删除;v2 重启记账侧时连同 EscrowConcurrency 一并重写。

