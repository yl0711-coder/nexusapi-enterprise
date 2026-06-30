// observe 漏闸回归测试(总监审计 2026-06-30,B/C):观测模式下 quota-worker 绝不写 new-api/不停服。
// 锁住两条闸:ResetDuePolicies(周期重置写绝对额度→可停服)与 ReconcileOrphans(禁用 new-api 用户)。
// 核心:observe 下有"待重置"策略也必须返回 0 且不 MarkPolicyReset(last_reset_at 仍 NULL);去掉闸→会被标记→本测试失败。
// 故意用 nil upstream:闸放行则永不触达上游;若闸回归,代码会走到上游写路径,以可见失败暴露(防静默回归)。
package integration

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/nexusapi-platform/enterprise/model"
	"github.com/nexusapi-platform/enterprise/pkg/session"
	"github.com/nexusapi-platform/enterprise/repo"
	"github.com/nexusapi-platform/enterprise/service"
)

func TestIntegration_ObserveQuotaWorkerGate(t *testing.T) {
	dsn := os.Getenv("NEXUS_IT_DSN")
	newapiSQLDSN := os.Getenv("NEXUS_IT_NEWAPI_SQL_DSN")
	if dsn == "" || newapiSQLDSN == "" {
		t.Skip("跳过:需 NEXUS_IT_DSN / NEXUS_IT_NEWAPI_SQL_DSN")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ensureDatabase(t, newapiSQLDSN, "nexus_gate")
	if !strings.Contains(dsn, "/nexus?") {
		t.Fatalf("NEXUS_IT_DSN 不含 /nexus?: %q", dsn)
	}
	store, err := repo.Open(ctx, strings.Replace(dsn, "/nexus?", "/nexus_gate?", 1))
	if err != nil {
		t.Fatalf("连 nexus_gate 失败: %v", err)
	}
	defer store.Close()
	defer func() { _, _ = store.DB().Exec("DROP DATABASE IF EXISTS nexus_gate") }()
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	// nil upstream:observe 闸放行则不触达;闸回归→走到 ManageUserQuota/SetUserStatus 会 panic 暴露。
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := service.New(service.Deps{Store: store, Logger: log, ObserveMode: true})

	const orgID = int64(1)
	db := store.DB()
	if _, err := db.ExecContext(ctx, `INSERT INTO organization (id, name, slug) VALUES (?, 'gate-org', 'gate-org-slug')`, orgID); err != nil {
		t.Fatalf("建组织失败: %v", err)
	}

	// 一条"待重置"的日策略(last_reset_at NULL → 跨当日 0 点边界 → 非 observe 下必触发重置+MarkPolicyReset)。
	if err := store.UpsertQuotaPolicy(ctx, &repo.QuotaPolicy{OrgID: orgID, Scope: "org", ScopeID: orgID, Period: "daily", LimitQuota: 1_000_000}); err != nil {
		t.Fatalf("建策略失败: %v", err)
	}

	// B:ResetDuePolicies observe 下必须返回 0 且不标记重置。
	n, err := svc.ResetDuePolicies(ctx)
	if err != nil || n != 0 {
		t.Fatalf("observe 下 ResetDuePolicies 应 (0,nil),实 (%d,%v)", n, err)
	}
	var lastReset sql.NullTime
	if err := db.QueryRowContext(ctx, `SELECT last_reset_at FROM quota_policy WHERE org_id = ?`, orgID).Scan(&lastReset); err != nil {
		t.Fatalf("查 last_reset_at 失败: %v", err)
	}
	if lastReset.Valid {
		t.Fatalf("observe 闸失效:策略被 MarkPolicyReset(last_reset_at=%v)——闸已回归,周期重置会写 new-api 绝对额度可停服", lastReset.Time)
	}

	// 注:模型2 已移除 ReconcileOrphans(member≠user 无孤儿用户),原 C 项删除。
	t.Logf("observe 漏闸回归 ok: ResetDuePolicies 在观测下短路返 0、不写 new-api(策略未被标记重置)")
}

// OBS-1 回归(R5 模型2 安全重审):observe 下改档/改成员触达 applyMemberOverride 时,绝不写员工 token 限额
//(写 RemainQuota+UnlimitedQuota=false=把员工从无限翻成限额=停人,违反观测铁律)。
// 闸下沉到 applyMemberOverride;本测试经 UpdateTier(observe 白名单放行的可达路径)触发,nil upstream:
// 闸生效则在 UpdateToken 前短路、永不触达上游;闸回归则走到 orgCred+UpdateToken,nil upstream panic 暴露。
func TestIntegration_ObserveOverrideGate(t *testing.T) {
	dsn := os.Getenv("NEXUS_IT_DSN")
	newapiSQLDSN := os.Getenv("NEXUS_IT_NEWAPI_SQL_DSN")
	if dsn == "" || newapiSQLDSN == "" {
		t.Skip("跳过:需 NEXUS_IT_DSN / NEXUS_IT_NEWAPI_SQL_DSN")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ensureDatabase(t, newapiSQLDSN, "nexus_ovgate")
	if !strings.Contains(dsn, "/nexus?") {
		t.Fatalf("NEXUS_IT_DSN 不含 /nexus?: %q", dsn)
	}
	store, err := repo.Open(ctx, strings.Replace(dsn, "/nexus?", "/nexus_ovgate?", 1))
	if err != nil {
		t.Fatalf("连 nexus_ovgate 失败: %v", err)
	}
	defer store.Close()
	defer func() { _, _ = store.DB().Exec("DROP DATABASE IF EXISTS nexus_ovgate") }()
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	keyring := mustKeyring(t)
	signer, serr := session.NewSigner([]byte("integration-test-session-key-32b!!"), time.Hour)
	if serr != nil {
		t.Fatalf("signer: %v", serr)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	// nil upstream:OBS-1 闸生效则不触达;闸回归→applyMemberOverride 走 orgCred+UpdateToken→nil upstream panic。
	svc := service.New(service.Deps{Store: store, Keyring: keyring, Signer: signer, Logger: log, ObserveMode: true})

	const orgID, memberID, tok1 = int64(1), int64(1), int64(70011)
	db := store.DB()
	if _, err := db.ExecContext(ctx, `INSERT INTO organization (id, name, slug) VALUES (?, 'ov-org', 'ov-org-slug')`, orgID); err != nil {
		t.Fatalf("建组织失败: %v", err)
	}
	ml := int64(25_000_000)
	tierID, err := store.CreateTier(ctx, &model.Tier{OrgID: orgID, Name: "base", MonthlyLimit: &ml})
	if err != nil {
		t.Fatalf("建档失败: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO member (id, org_id, tier_id, login_email) VALUES (?, ?, ?, 'ov@test.local')`, memberID, orgID, tierID); err != nil {
		t.Fatalf("建成员失败: %v", err)
	}
	masked := "sk-ov...001"
	t1 := tok1
	m1 := &model.Member{ID: memberID, OrgID: orgID, NewapiTokenID: &t1, KeyMasked: &masked, KeyRotation: 1}
	if err := store.FinalizeBootstrap(ctx, m1, "nexus_m1_v1"); err != nil { // 置 bootstrap_state=done + 落 token
		t.Fatalf("FinalizeBootstrap 失败: %v", err)
	}

	// observe 下改档名 → 对引用该档+done+有 token 的成员触发 applyMemberOverride。OBS-1 闸应在 UpdateToken 前短路。
	c := session.Claims{Role: session.RoleOrgAdmin, OrgID: orgID, MemberID: 999}
	newName := "base2"
	if _, err := svc.UpdateTier(ctx, c, orgID, tierID, service.UpdateTierInput{Name: &newName}); err != nil {
		t.Fatalf("observe 下 UpdateTier 失败: %v", err)
	}
	// 跑到这=无 panic=OBS-1 闸生效(nil upstream 从未触达,未给员工 token 下发限额=未停人)。
	t.Logf("OBS-1 观测闸回归 ok: observe 下改档触发 applyMemberOverride 在 UpdateToken 前短路、不写 new-api 限额")
}
