// Package integration 是里程碑 1 的端到端集成测试:对**真实 MySQL + 真实 rc.4**
// in-process 接好 repo/adapter/service/handler,经平台 REST API 走通 US-01(开通成员代发 key)
// 全链路 + RBAC 越权判定。未设环境变量时自动 skip(无 infra 也能 go test ./... 全绿)。
//
// 跑法见 test/docker-compose.integration.yml:
//
//	docker compose -f test/docker-compose.integration.yml up --build --abort-on-container-exit --exit-code-from tests
package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/nexusapi-platform/enterprise/adapter/newapi"
	"github.com/nexusapi-platform/enterprise/handler"
	"github.com/nexusapi-platform/enterprise/pkg/crypto"
	"github.com/nexusapi-platform/enterprise/pkg/session"
	"github.com/nexusapi-platform/enterprise/repo"
	"github.com/nexusapi-platform/enterprise/service"
)

const (
	opEmail    = "ops@nexus.local"
	opPassword = "OpsPass123"
)

func TestIntegration_OpenMember_E2E(t *testing.T) {
	dsn := os.Getenv("NEXUS_IT_DSN")
	newapiURL := os.Getenv("NEXUS_IT_NEWAPI_URL")
	if dsn == "" || newapiURL == "" {
		t.Skip("跳过集成测试:未设 NEXUS_IT_DSN / NEXUS_IT_NEWAPI_URL(见 docker-compose.integration.yml)")
	}

	// 可选:new-api 的库连接,用于建 nexus 库 + 造消费日志验扣费(3b)。仅本地集成 compose 设。
	newapiSQLDSN := os.Getenv("NEXUS_IT_NEWAPI_SQL_DSN")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// 1) 连 MySQL(带重试,等容器就绪)+ 迁移。
	if newapiSQLDSN != "" {
		ensureDatabase(t, newapiSQLDSN, "nexus") // 本地 compose 的 mysql 只建了 newapi 库,nexus 自建
	}
	store := openWithRetry(t, ctx, dsn)
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	// 2) 初始化 rc.4,取管理员 access_token + uid。
	adminToken, adminUID := setupRC4(t, newapiURL)

	// 3) in-process 接好 adapter + service + handler。
	keyring := mustKeyring(t)
	signer, err := session.NewSigner([]byte("integration-test-session-key-32b!!"), time.Hour)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	upstream := newapi.New(newapi.Config{BaseURL: newapiURL, AdminToken: adminToken, AdminUserID: adminUID, Timeout: 15 * time.Second}, nil)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := service.New(service.Deps{Store: store, Upstream: upstream, Keyring: keyring, Signer: signer, Logger: log})

	if err := svc.SeedOperator(ctx, opEmail, opPassword); err != nil {
		t.Fatalf("种子运营方失败: %v", err)
	}

	ts := httptest.NewServer(handler.New(svc, signer, log, "it", false).Routes())
	defer ts.Close()
	api := &apiClient{t: t, base: ts.URL}

	// 4) 运营方登录。
	opTok := login(api, opEmail, opPassword)

	// 5) 运营方建客户组织(连带建组织管理员)。slug 唯一(带随机后缀防重跑撞)。
	slug := "acme-" + randSuffix()
	var orgResp struct {
		Org struct {
			ID int64 `json:"id"`
		} `json:"org"`
		AdminEmail           string `json:"admin_email"`
		AdminInitialPassword string `json:"admin_initial_password"`
	}
	// 改动①:建组织前先在 new-api 给该用户分组挂上可用模型分组(否则 CreateOrg 校验返 422)。
	orgGrp := "grp-" + slug
	if err := upstream.AddOrgUsableGroup(ctx, orgGrp, "vip"); err != nil {
		t.Fatalf("预配组织用户分组可用分组失败: %v", err)
	}
	st := api.do("POST", "/api/v1/organizations", opTok, map[string]any{
		"name": "Acme 公司", "slug": slug, "admin_email": "admin@" + slug + ".com",
		"newapi_user_group": orgGrp,
	}, &orgResp)
	if st != http.StatusCreated {
		t.Fatalf("建组织 HTTP=%d", st)
	}
	orgID := orgResp.Org.ID
	if orgID == 0 || orgResp.AdminInitialPassword == "" {
		t.Fatalf("建组织返回异常: %+v", orgResp)
	}
	t.Logf("建组织 ok: org_id=%d admin=%s", orgID, orgResp.AdminEmail)

	// 6) 组织管理员登录。
	adminTok := login(api, orgResp.AdminEmail, orgResp.AdminInitialPassword)

	// 7) 管理员建团队 + 层级,设默认层级。
	var teamResp struct {
		ID int64 `json:"id"`
	}
	if st := api.do("POST", fmt.Sprintf("/api/v1/organizations/%d/teams", orgID), adminTok,
		map[string]any{"name": "研发一组"}, &teamResp); st != http.StatusCreated {
		t.Fatalf("建团队 HTTP=%d", st)
	}
	var tierResp struct {
		ID int64 `json:"id"`
	}
	if st := api.do("POST", fmt.Sprintf("/api/v1/organizations/%d/tiers", orgID), adminTok,
		map[string]any{"name": "标准档", "model_set": []string{"gpt-5.4", "claude-sonnet-4-6", "gpt-5-mini"}, "monthly_limit": 25000000, "model_cap": map[string]any{"gpt-5-mini": 1000000}}, &tierResp); st != http.StatusCreated {
		t.Fatalf("建层级 HTTP=%d", st)
	}
	if st := api.do("POST", fmt.Sprintf("/api/v1/tiers/%d/default", tierResp.ID), adminTok, nil, nil); st != http.StatusOK {
		t.Fatalf("设默认层级 HTTP=%d", st)
	}

	// 8) US-01:管理员开通成员(真机代发 key)。
	var openResp struct {
		MemberID     int64    `json:"member_id"`
		NewapiUserID int64    `json:"newapi_user_id"`
		APIKey       string   `json:"api_key"`
		KeyMasked    string   `json:"key_masked"`
		Models       []string `json:"models"`
	}
	st = api.do("POST", fmt.Sprintf("/api/v1/organizations/%d/members", orgID), adminTok,
		map[string]any{"name": "钱晨", "team_id": teamResp.ID, "tier_id": tierResp.ID}, &openResp)
	if st != http.StatusCreated {
		t.Fatalf("开通成员 HTTP=%d", st)
	}
	if openResp.APIKey == "" || openResp.NewapiUserID == 0 || openResp.MemberID == 0 {
		t.Fatalf("开通成员返回缺字段: %+v", openResp)
	}
	if !strings.Contains(openResp.KeyMasked, "••••") {
		t.Errorf("key_masked 应脱敏: %q", openResp.KeyMasked)
	}
	t.Logf("开通成员 ok: member_id=%d newapi_user_id=%d key=%s...", openResp.MemberID, openResp.NewapiUserID, openResp.APIKey[:min(10, len(openResp.APIKey))])
	originalKey := openResp.APIKey

	// 9) 列表脱敏:明文 key 绝不出现在列表里,只见 key_masked。
	var listResp struct {
		List []struct {
			ID        int64  `json:"id"`
			KeyMasked string `json:"key_masked"`
			Status    string `json:"status"`
		} `json:"list"`
		Pagination struct {
			Total int `json:"total"`
		} `json:"pagination"`
	}
	rawList := api.doRaw("GET", fmt.Sprintf("/api/v1/organizations/%d/members?page=1&page_size=20", orgID), adminTok, nil)
	if strings.Contains(rawList, originalKey) {
		t.Fatal("成员列表泄露了明文 key —— 违反红线(只能脱敏回显)")
	}
	_ = json.Unmarshal([]byte(extractData(t, rawList)), &listResp)
	foundActive := false
	for _, m := range listResp.List {
		if m.ID == openResp.MemberID {
			foundActive = m.Status == "active"
			if !strings.Contains(m.KeyMasked, "••••") {
				t.Errorf("列表 key 未脱敏: %q", m.KeyMasked)
			}
		}
	}
	if !foundActive {
		t.Errorf("新成员未出现在列表或非 active")
	}

	// 10) 轮换 key(成员本人):签发成员会话,调 :rotate,得新 key,旧 key 应不同。
	memberTok, _ := signer.Issue(session.Claims{MemberID: openResp.MemberID, OrgID: orgID, Role: session.RoleMember, TeamID: teamResp.ID})
	var rotResp struct {
		APIKey    string `json:"api_key"`
		KeyMasked string `json:"key_masked"`
	}
	if st := api.do("POST", fmt.Sprintf("/api/v1/members/%d/key:rotate", openResp.MemberID), memberTok, nil, &rotResp); st != http.StatusOK {
		t.Fatalf("轮换 key HTTP=%d", st)
	}
	if rotResp.APIKey == "" || rotResp.APIKey == originalKey {
		t.Errorf("轮换应得不同的新 key:old=%s new=%s", mask(originalKey), mask(rotResp.APIKey))
	}
	t.Logf("轮换 key ok: 新 masked=%s", rotResp.KeyMasked)

	// ===== RBAC 越权判定(08 §2)=====

	// (a) 运营方直接开通成员 → 403(E05:运营方拒,需经支持会话)。
	if st := api.do("POST", fmt.Sprintf("/api/v1/organizations/%d/members", orgID), opTok,
		map[string]any{"name": "x", "tier_id": tierResp.ID}, nil); st != http.StatusForbidden {
		t.Errorf("运营方开通成员应 403,得 %d", st)
	}

	// (b) 成员开通成员 → 403。
	if st := api.do("POST", fmt.Sprintf("/api/v1/organizations/%d/members", orgID), memberTok,
		map[string]any{"name": "x", "tier_id": tierResp.ID}, nil); st != http.StatusForbidden {
		t.Errorf("成员开通成员应 403,得 %d", st)
	}

	// (c) 跨 org:另建一个组织,用其管理员访问本 org 成员列表 → 404(不暴露存在性)。
	slug2 := "beta-" + randSuffix()
	var org2 struct {
		Org struct {
			ID int64 `json:"id"`
		} `json:"org"`
		AdminEmail           string `json:"admin_email"`
		AdminInitialPassword string `json:"admin_initial_password"`
	}
	orgGrp2 := "grp-" + slug2
	if err := upstream.AddOrgUsableGroup(ctx, orgGrp2, "vip"); err != nil {
		t.Fatalf("预配 org2 用户分组可用分组失败: %v", err)
	}
	api.do("POST", "/api/v1/organizations", opTok, map[string]any{
		"name": "Beta 公司", "slug": slug2, "admin_email": "admin@" + slug2 + ".com",
		"newapi_user_group": orgGrp2,
	}, &org2)
	admin2Tok := login(api, org2.AdminEmail, org2.AdminInitialPassword)
	if st := api.do("GET", fmt.Sprintf("/api/v1/organizations/%d/members", orgID), admin2Tok, nil, nil); st != http.StatusNotFound {
		t.Errorf("跨 org 访问应 404,得 %d", st)
	}

	// (d) 跨 team:团队负责人(T1)开通到别的团队 → 403。
	tlTok, _ := signer.Issue(session.Claims{MemberID: openResp.MemberID, OrgID: orgID, Role: session.RoleTeamLeader, TeamID: teamResp.ID})
	otherTeam := teamResp.ID + 999
	if st := api.do("POST", fmt.Sprintf("/api/v1/organizations/%d/members", orgID), tlTok,
		map[string]any{"name": "x", "team_id": otherTeam, "tier_id": tierResp.ID}, nil); st != http.StatusForbidden {
		t.Errorf("团队负责人跨团队开通应 403,得 %d", st)
	}

	// (e) 未带 token → 401。
	if st := api.do("GET", "/api/v1/me", "", nil, nil); st != http.StatusUnauthorized {
		t.Errorf("无 token 应 401,得 %d", st)
	}

	t.Log("里程碑 1 e2e 全通过:US-01 开通成员(真机代发 key)+ 列表脱敏 + 轮换 + RBAC(403/404/401)")

	// ===== 里程碑 2:额度执行(调额 / 撤销回退 / account_ttl 到期 worker 反向 / 停用恢复)=====
	uid := openResp.NewapiUserID
	q0, _ := getNewapiUser(t, newapiURL, adminToken, adminUID, uid)
	t.Logf("开通后 new-api 用户 quota=%d", q0)

	// US-03 调额 +5,000,000(今日)→ new-api 用户 quota 实增。
	var adj struct {
		NewCapQuota int64 `json:"new_cap_quota"`
		GrantID     int64 `json:"grant_id"`
	}
	if st := api.do("POST", fmt.Sprintf("/api/v1/members/%d/quota:adjust", openResp.MemberID), adminTok,
		map[string]any{"delta_quota": 5000000, "duration": "today", "reason": "赶项目"}, &adj); st != http.StatusOK {
		t.Fatalf("调额 HTTP=%d", st)
	}
	q1, _ := getNewapiUser(t, newapiURL, adminToken, adminUID, uid)
	if q1 != q0+5000000 {
		t.Errorf("调额后 quota 应 +5e6: q0=%d q1=%d", q0, q1)
	} else {
		t.Logf("US-03 调额 ok: new-api quota %d→%d, grant=%d", q0, q1, adj.GrantID)
	}

	// 撤销 grant → override 回退到基线。
	if st := api.do("DELETE", fmt.Sprintf("/api/v1/grants/%d", adj.GrantID), adminTok, nil, nil); st != http.StatusOK {
		t.Fatalf("撤销 grant HTTP=%d", st)
	}
	q2, _ := getNewapiUser(t, newapiURL, adminToken, adminUID, uid)
	if q2 != q0 {
		t.Errorf("撤销后 quota 应回退到 %d, 得 %d", q0, q2)
	} else {
		t.Logf("撤销回退 ok: new-api quota→%d", q2)
	}

	// US-04a account_ttl:近未来到期 → worker 反向 → new-api 用户 disable + member expired。
	exp := time.Now().Add(2 * time.Second).UTC().Format(time.RFC3339)
	if st := api.do("POST", fmt.Sprintf("/api/v1/members/%d/grants", openResp.MemberID), adminTok,
		map[string]any{"type": "account_ttl", "expire_at": exp, "reason": "实习生到期"}, nil); st != http.StatusCreated {
		t.Fatalf("account_ttl grant HTTP=%d", st)
	}
	time.Sleep(2500 * time.Millisecond)
	if n, err := svc.ReverseExpiredGrants(ctx, 100); err != nil || n < 1 {
		t.Fatalf("worker 反向到期 grant: n=%d err=%v", n, err)
	}
	if _, st := getNewapiUser(t, newapiURL, adminToken, adminUID, uid); st == 1 {
		t.Errorf("account_ttl 到期后 new-api 用户应被 disable(status!=1),得 status=%d", st)
	} else {
		t.Log("US-04a account_ttl 到期 worker 反向 ok: new-api 用户已 disable")
	}

	// US-05 停用/恢复:恢复(上一步被 disable)→ enabled;再停用 → disabled。
	if st := api.do("POST", fmt.Sprintf("/api/v1/members/%d/status", openResp.MemberID), adminTok,
		map[string]any{"enabled": true}, nil); st != http.StatusOK {
		t.Fatalf("恢复 HTTP=%d", st)
	}
	if _, st := getNewapiUser(t, newapiURL, adminToken, adminUID, uid); st != 1 {
		t.Errorf("恢复后 new-api 用户应 enabled(status=1),得 %d", st)
	}
	if st := api.do("POST", fmt.Sprintf("/api/v1/members/%d/status", openResp.MemberID), adminTok,
		map[string]any{"enabled": false}, nil); st != http.StatusOK {
		t.Fatalf("停用 HTTP=%d", st)
	}
	if _, st := getNewapiUser(t, newapiURL, adminToken, adminUID, uid); st == 1 {
		t.Errorf("停用后 new-api 用户应 disabled,得 enabled")
	} else {
		t.Log("US-05 停用/恢复 ok")
	}

	t.Log("里程碑 2 e2e 全通过:调额(new-api quota 实变)+ 撤销回退 + account_ttl 到期 worker 反向 + 停用/恢复")

	// ===== 里程碑 3a:充值入账 / 余额 / 申请 / 低位告警(钱进 + 只读 + 告警,不动客户服务)=====
	// US-08 运营方入账。
	var rc struct {
		BalanceAfter int64 `json:"balance_quota_after"`
	}
	if st := api.do("POST", fmt.Sprintf("/api/v1/organizations/%d/recharges", orgID), opTok,
		map[string]any{"amount_quota": 10000000, "transfer_no": "TR-" + randSuffix(), "note": "Q2 预付"}, &rc); st != http.StatusCreated {
		t.Fatalf("入账 HTTP=%d", st)
	}
	if rc.BalanceAfter != 10000000 {
		t.Errorf("入账后余额应=1e7, 得 %d", rc.BalanceAfter)
	} else {
		t.Logf("US-08 入账 ok: balance_after=%d", rc.BalanceAfter)
	}

	// 余额查询。
	var bal struct {
		Balance int64 `json:"balance_quota"`
	}
	api.do("GET", fmt.Sprintf("/api/v1/organizations/%d/balance", orgID), adminTok, nil, &bal)
	if bal.Balance != 10000000 {
		t.Errorf("余额查询应=1e7, 得 %d", bal.Balance)
	}

	// 入账幂等:同 transfer_no 不重复加。
	fixedTR := "TR-FIX-" + randSuffix()
	if st := api.do("POST", fmt.Sprintf("/api/v1/organizations/%d/recharges", orgID), opTok,
		map[string]any{"amount_quota": 3000000, "transfer_no": fixedTR}, nil); st != http.StatusCreated {
		t.Fatalf("首次入账 HTTP=%d", st)
	}
	if st := api.do("POST", fmt.Sprintf("/api/v1/organizations/%d/recharges", orgID), opTok,
		map[string]any{"amount_quota": 3000000, "transfer_no": fixedTR}, nil); st != http.StatusConflict {
		t.Errorf("重复 transfer_no 应 409(幂等),得 %d", st)
	} else {
		t.Log("入账幂等 ok: 重复 transfer_no → 409")
	}

	// 动钱红线:组织管理员入账 → 403。
	if st := api.do("POST", fmt.Sprintf("/api/v1/organizations/%d/recharges", orgID), adminTok,
		map[string]any{"amount_quota": 1, "transfer_no": "X"}, nil); st != http.StatusForbidden {
		t.Errorf("组织管理员入账应 403(动钱红线),得 %d", st)
	} else {
		t.Log("动钱红线 ok: 组织管理员入账 → 403")
	}

	// T3 回归:transfer_no 超长 → 422(不落库、不 500,故不扰动余额账)。正常长度 201 已被上面多次入账覆盖。
	if st := api.do("POST", fmt.Sprintf("/api/v1/organizations/%d/recharges", orgID), opTok,
		map[string]any{"amount_quota": 1000000, "transfer_no": strings.Repeat("x", 200)}, nil); st != http.StatusUnprocessableEntity {
		t.Errorf("T3 transfer_no 超长应 422,得 %d", st)
	} else {
		t.Log("回归 T3 ok: transfer_no 超长 → 422(不落库不 500)")
	}

	// US-09 组织管理员申请充值(不改余额)。
	balBefore := bal.Balance + 3000000 // 上面又入账了 3e6
	if st := api.do("POST", fmt.Sprintf("/api/v1/organizations/%d/recharge-requests", orgID), adminTok,
		map[string]any{"type": "topup", "amount_quota": 5000000, "note": "需补预付"}, nil); st != http.StatusCreated {
		t.Fatalf("申请充值 HTTP=%d", st)
	}
	var bal2 struct {
		Balance int64 `json:"balance_quota"`
	}
	api.do("GET", fmt.Sprintf("/api/v1/organizations/%d/balance", orgID), adminTok, nil, &bal2)
	if bal2.Balance != balBefore {
		t.Errorf("申请充值不应改余额: before=%d after=%d", balBefore, bal2.Balance)
	} else {
		t.Logf("US-09 申请充值 ok: 余额未变=%d", bal2.Balance)
	}

	// 成员申请充值 → 403(计费子集仅组织管理员)。
	if st := api.do("POST", fmt.Sprintf("/api/v1/organizations/%d/recharge-requests", orgID), memberTok,
		map[string]any{"type": "topup", "amount_quota": 1}, nil); st != http.StatusForbidden {
		t.Errorf("成员申请充值应 403,得 %d", st)
	}

	// 低位告警:把阈值设到当前余额之上,再入账触发 recompute → 组织状态 low。
	if err := store.SetLowWatermark(ctx, orgID, bal2.Balance+50000000); err != nil {
		t.Fatalf("设低位阈值: %v", err)
	}
	api.do("POST", fmt.Sprintf("/api/v1/organizations/%d/recharges", orgID), opTok,
		map[string]any{"amount_quota": 1000000, "transfer_no": "TR-LOW-" + randSuffix()}, nil)
	var orgv struct {
		Status string `json:"status"`
	}
	api.do("GET", fmt.Sprintf("/api/v1/organizations/%d", orgID), opTok, nil, &orgv)
	if orgv.Status != "low" {
		t.Errorf("余额低于阈值后组织状态应 low,得 %q", orgv.Status)
	} else {
		t.Log("US-11 低位告警 ok: 组织状态翻 low(硬停默认关,未切断服务)")
	}

	t.Log("里程碑 3a e2e 全通过:入账(余额增/幂等)+ 查询 + 申请充值(不改余额)+ 动钱红线 403 + 低位告警状态翻转")

	// ===== 里程碑 3b:计费开关 RBAC(总是跑)=====
	if st := api.do("PATCH", fmt.Sprintf("/api/v1/organizations/%d/billing-settings", orgID), adminTok,
		map[string]any{"billing_enabled": true}, nil); st != http.StatusForbidden {
		t.Errorf("组织管理员改计费开关应 403(仅运营方),得 %d", st)
	} else {
		t.Log("计费开关 RBAC ok: 组织管理员改 → 403")
	}

	// ===== 里程碑 3c:计价/折扣联动(写 new-api 分组特殊倍率 GroupGroupRatio + 只读回显)=====
	// 整体 8 折:base(default,未配=1)× 0.8 = 0.8,写 GroupGroupRatio[org_{id}][default]。
	if st := api.do("PUT", fmt.Sprintf("/api/v1/organizations/%d/pricing", orgID), opTok,
		map[string]any{"mode": "total", "discount_pct": 0.8}, nil); st != http.StatusOK {
		t.Fatalf("配置折扣 HTTP=%d", st)
	}
	var pv struct {
		Mode     string             `json:"mode"`
		Upstream map[string]float64 `json:"upstream_special_ratio"`
	}
	api.do("GET", fmt.Sprintf("/api/v1/organizations/%d/pricing", orgID), adminTok, nil, &pv)
	if pv.Mode != "total" || pv.Upstream["default"] != 0.8 {
		t.Errorf("折扣回显应 total + GroupGroupRatio[org][default]=0.8: %+v", pv)
	} else {
		t.Log("3c 折扣联动 ok: 写 new-api 分组特殊倍率 GroupGroupRatio=0.8(base 1×0.8)且只读回显一致")
	}
	if st := api.do("PUT", fmt.Sprintf("/api/v1/organizations/%d/pricing", orgID), adminTok,
		map[string]any{"mode": "total", "discount_pct": 0.5}, nil); st != http.StatusForbidden {
		t.Errorf("组织管理员配折扣应 403(客户只读),得 %d", st)
	} else {
		t.Log("3c 折扣 RBAC ok: 组织管理员配置 → 403(客户只读),仅运营方可配")
	}

	// ===== R2 回归:merge-preserve + 折扣对账(G)+ mode=none 删键 =====
	ctxBg := context.Background()
	ug := orgGrp
	pricingPath := fmt.Sprintf("/api/v1/organizations/%d/pricing", orgID)
	// 预置一个外部手工配的无关键 vip/default(模拟人工/其它工具配),平台后续读-改-写绝不能抹掉它。
	if err := upstream.SetGroupGroupRatio(ctxBg, "vip", "default", 0.66); err != nil {
		t.Fatalf("预置外部 vip 键失败: %v", err)
	}
	if st := api.do("PUT", pricingPath, opTok, map[string]any{"mode": "total", "discount_pct": 0.7}, nil); st != http.StatusOK {
		t.Fatalf("重配折扣 0.7 HTTP=%d", st)
	}
	if r, ok, _ := upstream.GetGroupGroupRatio(ctxBg, "vip", "default"); !ok || r != 0.66 {
		t.Errorf("merge-preserve 破:重配折扣后外部 vip 键被抹掉 ok=%v r=%v", ok, r)
	} else {
		t.Log("回归 merge-preserve ok: 平台重配折扣后外部 vip/default=0.66 仍存活(只改自己那条)")
	}
	// S2 并发写回归:20 并发写 conc 用户分组下不同令牌分组键,单写者锁若失效会丢更新(读-改-写覆盖)。
	// 断言全部存活(new-api 限流已由 compose 抬高,不再受预算干扰)。
	{
		const n = 20
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(k int) {
				defer wg.Done()
				_ = upstream.SetGroupGroupRatio(ctxBg, "conc", fmt.Sprintf("g%02d", k), 0.5)
			}(i)
		}
		wg.Wait()
		survived := 0
		for i := 0; i < n; i++ {
			if _, ok, _ := upstream.GetGroupGroupRatio(ctxBg, "conc", fmt.Sprintf("g%02d", i)); ok {
				survived++
			}
		}
		if survived != n {
			t.Errorf("S2 并发写丢更新:%d 条并发写仅存活 %d 条(单写者锁失效)", n, survived)
		} else {
			t.Logf("回归 S2 并发锁 ok: %d 并发写 GroupGroupRatio 全部存活,无丢更新", n)
		}
	}
	// 折扣对账(G):外部篡改 org 自己的特殊倍率 → reconcile 必须检出 tampered。
	if err := upstream.SetGroupGroupRatio(ctxBg, ug, "default", 0.123); err != nil {
		t.Fatalf("模拟外部篡改失败: %v", err)
	}
	var rec struct {
		DriftCount int `json:"drift_count"`
		Drifts     []struct {
			Kind       string `json:"kind"`
			TokenGroup string `json:"token_group"`
		} `json:"drifts"`
	}
	if st := api.do("POST", "/api/v1/pricing/reconcile", opTok, nil, &rec); st != http.StatusOK {
		t.Fatalf("对账触发 HTTP=%d", st)
	}
	foundTamper := false
	for _, d := range rec.Drifts {
		if d.TokenGroup == "default" && d.Kind == "tampered" {
			foundTamper = true
		}
	}
	if !foundTamper {
		t.Errorf("对账应检出 org default 被篡改(tampered),实得: %+v", rec)
	} else {
		t.Logf("回归 对账 G ok: 检出 %d 处漂移,含 org default tampered(只告警不改价)", rec.DriftCount)
	}
	if st := api.do("POST", "/api/v1/pricing/reconcile", adminTok, nil, nil); st != http.StatusForbidden {
		t.Errorf("组织管理员触发对账应 403,得 %d", st)
	}
	// mode=none:取消折扣应删掉 org 自己的键(不写 base、不堆死键),且不误删外部 vip 键。
	if st := api.do("PUT", pricingPath, opTok, map[string]any{"mode": "none"}, nil); st != http.StatusOK {
		t.Fatalf("取消折扣 HTTP=%d", st)
	}
	if _, ok, _ := upstream.GetGroupGroupRatio(ctxBg, ug, "default"); ok {
		t.Errorf("mode=none 应删 org default 键,但键仍在")
	} else {
		t.Log("回归 mode=none ok: 取消折扣删除 org default 特殊倍率键(回落基础倍率,不堆死键)")
	}
	if _, ok, _ := upstream.GetGroupGroupRatio(ctxBg, "vip", "default"); !ok {
		t.Error("mode=none 误删了外部 vip 键(merge-preserve 破)")
	}

	// ===== R3 回归:T1 读后写不丢键(连写/并发) + T6 折扣镜像累积一致 =====
	absEq := func(a, b float64) bool { d := a - b; return d < 1e-9 && d > -1e-9 }
	// T1-a 同一用户分组"快速连写"10 次(对应工单 T1"间隔 0ms 连写 10 次"):每次新增一个令牌分组,
	// authoritative 写 + 写后读校验应 10/10 不丢(new-api 限流已由 compose 抬高,不再受预算干扰)。
	{
		const n = 10
		desired := map[string]float64{}
		for i := 0; i < n; i++ {
			desired[fmt.Sprintf("g%02d", i)] = 0.3 + float64(i)*0.01
			if err := upstream.SetOrgGroupRatios(ctxBg, "rapid", desired); err != nil {
				t.Fatalf("T1 连写第 %d 次失败: %v", i, err)
			}
		}
		got := 0
		for i := 0; i < n; i++ {
			if r, ok, _ := upstream.GetGroupGroupRatio(ctxBg, "rapid", fmt.Sprintf("g%02d", i)); ok && absEq(r, 0.3+float64(i)*0.01) {
				got++
			}
		}
		if got != n {
			t.Errorf("T1 连写丢键:同分组连写 %d 次,仅 %d 条落库且值正确(read-after-write 回退)", n, got)
		} else {
			t.Logf("回归 T1 连写 ok: 同用户分组连写 %d 次 → %d/%d 令牌分组落库值正确(己方键以镜像为准,不被上游缓存回退)", n, got, n)
		}
	}
	// T1-b 批量改价:5 个不同 org 用户分组 0ms 顺序连写(运营方"一把配多客户"的真实路径)。
	// 每次权威写自己那条 + merge-preserve 其它,锁内读到上一次已提交态 → 应 5/5 累积、vip 不动。
	// (真·并发跨用户分组写受上游单 JSON+读缓存固有限制,生产按顺序连写处理,真并发残差交 reconcile 兜底。)
	{
		const n = 5
		for i := 0; i < n; i++ {
			if err := upstream.SetOrgGroupRatios(ctxBg, fmt.Sprintf("co%02d", i), map[string]float64{"default": 0.5}); err != nil {
				t.Fatalf("T1 批量连写第 %d 个失败: %v", i, err)
			}
		}
		got := 0
		for i := 0; i < n; i++ {
			if _, ok, _ := upstream.GetGroupGroupRatio(ctxBg, fmt.Sprintf("co%02d", i), "default"); ok {
				got++
			}
		}
		if got != n {
			t.Errorf("T1 批量连写丢键:期望 %d 实 %d", n, got)
		} else {
			t.Logf("回归 T1 批量 ok: %d 个不同 org 顺序连写 %d/%d 全累积(merge-preserve 跨 org 不丢)", n, got, n)
		}
		if _, ok, _ := upstream.GetGroupGroupRatio(ctxBg, "vip", "default"); !ok {
			t.Error("T1 批量连写误删外部 vip 键")
		}
	}
	// T6 per_group 多次配不同分组 → 平台镜像累积(含全部已配分组),与上游一致,none 能删全。
	{
		if st := api.do("PUT", pricingPath, opTok, map[string]any{"mode": "per_group", "discount_pct": 0.9, "token_groups": []string{"default"}}, nil); st != http.StatusOK {
			t.Fatalf("T6 per_group A HTTP=%d", st)
		}
		if st := api.do("PUT", pricingPath, opTok, map[string]any{"mode": "per_group", "discount_pct": 0.8, "token_groups": []string{"vision"}}, nil); st != http.StatusOK {
			t.Fatalf("T6 per_group B HTTP=%d", st)
		}
		var pv2 struct {
			Entries map[string]struct {
				Pct float64 `json:"pct"`
			} `json:"entries"`
		}
		api.do("GET", pricingPath, opTok, nil, &pv2)
		if len(pv2.Entries) != 2 || pv2.Entries["default"].Pct == 0 || pv2.Entries["vision"].Pct == 0 {
			t.Errorf("T6 镜像未累积:期望含 default+vision,实得 %+v", pv2.Entries)
		} else {
			t.Log("回归 T6 ok: per_group 多次配不同分组,镜像累积含 default+vision(不再覆盖发散)")
		}
		var rec2 struct {
			DriftCount int `json:"drift_count"`
		}
		api.do("POST", "/api/v1/pricing/reconcile", opTok, nil, &rec2)
		if rec2.DriftCount != 0 {
			t.Errorf("T6 镜像/上游应一致,reconcile drift_count=%d(非 0)", rec2.DriftCount)
		}
		api.do("PUT", pricingPath, opTok, map[string]any{"mode": "none"}, nil)
		_, okD, _ := upstream.GetGroupGroupRatio(ctxBg, ug, "default")
		_, okV, _ := upstream.GetGroupGroupRatio(ctxBg, ug, "vision")
		if okD || okV {
			t.Errorf("T6 none 未删全(default 在=%v / vision 在=%v)", okD, okV)
		} else {
			t.Log("回归 T6 none ok: 取消折扣把累积的 default+vision 两条键都删干净")
		}
	}

	// ===== 里程碑 4:申请-审批(US-06)+ 通知(US-13)=====
	qB4, _ := getNewapiUser(t, newapiURL, adminToken, adminUID, uid)
	// ① 自动通过(小额今日)→ 即时下发,new-api quota 增。
	var auto struct {
		ID    int64  `json:"id"`
		State string `json:"state"`
	}
	if st := api.do("POST", "/api/v1/approvals", memberTok,
		map[string]any{"amount_quota": 10000000, "duration": "today", "reason": "赶工"}, &auto); st != http.StatusCreated || auto.State != "auto_approved" {
		t.Fatalf("自动通过申请 HTTP=%d state=%s", st, auto.State)
	}
	if qa, _ := getNewapiUser(t, newapiURL, adminToken, adminUID, uid); qa != qB4+10000000 {
		t.Errorf("自动通过后 quota 应 +1e7: %d→%d", qB4, qa)
	} else {
		t.Logf("US-06 自动通过 ok: 即时下发,quota %d→%d", qB4, qa)
	}
	// 通知:成员收到审批结果站内通知。
	var nresp struct {
		Unread int `json:"unread"`
	}
	api.do("GET", "/api/v1/notifications", memberTok, nil, &nresp)
	if nresp.Unread < 1 {
		t.Errorf("自动通过应生成站内通知,unread=%d", nresp.Unread)
	} else {
		t.Logf("US-13 通知 ok: 成员收到 %d 条未读", nresp.Unread)
	}

	// ② 二审档(大额 > 一审上限)→ pending → 一审 → 二审 → approved 下发。
	var big struct {
		ID       int64  `json:"id"`
		State    string `json:"state"`
		IsLevel2 bool   `json:"is_level2"`
	}
	// 二审需 ≥30 万元 = 1.5e11 quota;用 40 万元 = 2e11 quota 触发(阈值已按元修正,A4)。
	if st := api.do("POST", "/api/v1/approvals", memberTok,
		map[string]any{"amount_quota": 200000000000, "duration": "today", "reason": "大项目"}, &big); st != http.StatusCreated {
		t.Fatalf("大额申请 HTTP=%d", st)
	}
	if big.State != "pending" || !big.IsLevel2 {
		t.Errorf("大额应 pending+二审: state=%s level2=%v", big.State, big.IsLevel2)
	}
	// 成员裁决自己的申请 → 403。
	if st := api.do("POST", fmt.Sprintf("/api/v1/approvals/%d/decide", big.ID), memberTok,
		map[string]any{"approved": true}, nil); st != http.StatusForbidden {
		t.Errorf("成员裁决应 403,得 %d", st)
	}
	// 一审(组织管理员)→ l1_approved。
	var d1 struct {
		State string `json:"state"`
	}
	api.do("POST", fmt.Sprintf("/api/v1/approvals/%d/decide", big.ID), adminTok, map[string]any{"approved": true}, &d1)
	if d1.State != "l1_approved" {
		t.Errorf("一审通过应 l1_approved,得 %s", d1.State)
	}
	// 二审(组织管理员)→ approved + 下发。
	qBeforeFinal, _ := getNewapiUser(t, newapiURL, adminToken, adminUID, uid)
	var d2 struct {
		State string `json:"state"`
	}
	api.do("POST", fmt.Sprintf("/api/v1/approvals/%d/decide", big.ID), adminTok, map[string]any{"approved": true}, &d2)
	if d2.State != "approved" {
		t.Errorf("二审通过应 approved,得 %s", d2.State)
	}
	if qf, _ := getNewapiUser(t, newapiURL, adminToken, adminUID, uid); qf != qBeforeFinal+200000000000 {
		t.Errorf("二审通过下发后 quota 应 +2e11: %d→%d", qBeforeFinal, qf)
	} else {
		t.Logf("US-06 二审 ok: pending→l1_approved→approved 且下发,quota %d→%d", qBeforeFinal, qf)
	}
	t.Log("里程碑 4 e2e 全通过:自动通过即时下发 + 二审两级流程 + 成员裁决403 + 站内通知")

	// ===== 里程碑 5:用量看板 + 运营方三层支持 =====
	// 用量看板(无真实用量时返回空结构 200)。
	if st := api.do("GET", fmt.Sprintf("/api/v1/organizations/%d/usage?since_hours=24", orgID), adminTok, nil, nil); st != http.StatusOK {
		t.Errorf("组织用量看板应 200,得 %d", st)
	} else {
		t.Log("用量看板 ok: GET /usage 200")
	}

	// 运营方支持态:只读态。
	var supRO struct {
		SessionID int64  `json:"session_id"`
		Token     string `json:"token"`
	}
	if st := api.do("POST", fmt.Sprintf("/api/v1/organizations/%d/support-sessions", orgID), opTok,
		map[string]any{"scope": "readonly", "ttl_seconds": 600, "reason": "排障"}, &supRO); st != http.StatusCreated {
		t.Fatalf("开只读支持会话 HTTP=%d", st)
	}
	// 只读态:能读(运营方经支持会话看到客户成员)。
	if st := api.do("GET", fmt.Sprintf("/api/v1/organizations/%d/members", orgID), supRO.Token, nil, nil); st != http.StatusOK {
		t.Errorf("只读支持态应能读成员列表,得 %d", st)
	}
	// 只读态:任何写 → 403(10402)。
	if st := api.do("POST", fmt.Sprintf("/api/v1/organizations/%d/members", orgID), supRO.Token,
		map[string]any{"name": "x", "tier_id": tierResp.ID}, nil); st != http.StatusForbidden {
		t.Errorf("只读支持态写应 403,得 %d", st)
	} else {
		t.Log("US-2.2 只读支持态 ok: 能读、任何写 → 403")
	}
	api.do("POST", fmt.Sprintf("/api/v1/support-sessions/%d/close", supRO.SessionID), opTok, nil, nil)

	// 协助态:普通写放行、动钱红线挡。
	var supAS struct {
		SessionID int64  `json:"session_id"`
		Token     string `json:"token"`
	}
	if st := api.do("POST", fmt.Sprintf("/api/v1/organizations/%d/support-sessions", orgID), opTok,
		map[string]any{"scope": "assist", "grant_type": "authorized", "ttl_seconds": 600, "reason": "协助配置"}, &supAS); st != http.StatusCreated {
		t.Fatalf("开协助支持会话 HTTP=%d", st)
	}
	// 协助态普通写(调额)放行。
	if st := api.do("POST", fmt.Sprintf("/api/v1/members/%d/quota:adjust", openResp.MemberID), supAS.Token,
		map[string]any{"delta_quota": 1000000, "duration": "today"}, nil); st != http.StatusOK {
		t.Errorf("协助态普通写(调额)应放行 200,得 %d", st)
	}
	// 协助态动钱 → 403 红线(10403)。
	if st := api.do("POST", fmt.Sprintf("/api/v1/organizations/%d/recharges", orgID), supAS.Token,
		map[string]any{"amount_quota": 1, "transfer_no": "X"}, nil); st != http.StatusForbidden {
		t.Errorf("协助态动钱应 403 红线,得 %d", st)
	} else {
		t.Log("US-2.2 协助态 ok: 普通写放行、动钱红线 → 403")
	}
	api.do("POST", fmt.Sprintf("/api/v1/support-sessions/%d/close", supAS.SessionID), opTok, nil, nil)
	t.Log("里程碑 5 e2e 全通过:用量看板 + 支持态只读(写403)+ 协助态(普通写放行/动钱红线403)")

	// ===== 补全功能(文档要求项)=====
	// 组织设置 PATCH(E19)。
	if st := api.do("PATCH", fmt.Sprintf("/api/v1/organizations/%d", orgID), adminTok, map[string]any{"timezone": "Asia/Shanghai"}, nil); st != http.StatusOK {
		t.Errorf("组织设置 PATCH 应 200,得 %d", st)
	}
	// 审批阈值 E13:配 + 读。
	api.do("PUT", fmt.Sprintf("/api/v1/organizations/%d/approval-rules", orgID), adminTok, map[string]any{"auto_max_quota": 60000000}, nil)
	var rules struct {
		AutoMax int64 `json:"auto_max_quota"`
	}
	api.do("GET", fmt.Sprintf("/api/v1/organizations/%d/approval-rules", orgID), adminTok, nil, &rules)
	if rules.AutoMax != 60000000 {
		t.Errorf("审批阈值配置回读应=6e7,得 %d", rules.AutoMax)
	} else {
		t.Log("E13 审批阈值可配 ok")
	}
	// 配额策略 E PUT/GET。
	api.do("PUT", fmt.Sprintf("/api/v1/organizations/%d/quota-policies", orgID), adminTok, map[string]any{"scope": "member", "scope_id": openResp.MemberID, "period": "daily", "limit_quota": 25000000}, nil)
	if st := api.do("GET", fmt.Sprintf("/api/v1/organizations/%d/quota-policies", orgID), adminTok, nil, nil); st != http.StatusOK {
		t.Errorf("配额策略列表应 200,得 %d", st)
	} else {
		t.Log("配额策略 PUT/GET ok")
	}
	// 角色任命 E17。
	if st := api.do("POST", fmt.Sprintf("/api/v1/members/%d/role", openResp.MemberID), adminTok, map[string]any{"role": "team_leader"}, nil); st != http.StatusOK {
		t.Errorf("角色任命应 200,得 %d", st)
	} else {
		t.Log("E17 角色任命 ok")
	}
	api.do("POST", fmt.Sprintf("/api/v1/members/%d/role", openResp.MemberID), adminTok, map[string]any{"role": "member"}, nil) // 改回
	// 批量导入 US-02。
	var bulk struct {
		Success int `json:"success"`
		Failed  int `json:"failed"`
	}
	if st := api.do("POST", fmt.Sprintf("/api/v1/organizations/%d/members:bulk", orgID), adminTok, map[string]any{"names": []string{"批量甲", "批量乙", "批量甲"}, "tier_id": tierResp.ID}, &bulk); st != http.StatusOK || bulk.Success != 2 || bulk.Failed != 1 {
		t.Errorf("批量导入应 成功2失败1(重名跳过): %+v st=%d", bulk, st)
	} else {
		t.Log("US-02 批量导入 ok: 成功2 失败1(同批重名跳过)")
	}
	// 服务状态(全角色)。
	if st := api.do("GET", "/api/v1/service-status", memberTok, nil, nil); st != http.StatusOK {
		t.Errorf("服务状态应 200,得 %d", st)
	}
	// IP 白名单 E22(成员对自己)。
	if st := api.do("POST", fmt.Sprintf("/api/v1/members/%d/key:ip-whitelist", openResp.MemberID), memberTok, map[string]any{"allow_ips": "203.0.113.0/24"}, nil); st != http.StatusOK {
		t.Errorf("IP白名单应 200,得 %d", st)
	} else {
		t.Log("E22 IP白名单 ok")
	}
	// 用量导出 CSV(不走信封,200 即可)。
	if st := api.do("GET", fmt.Sprintf("/api/v1/organizations/%d/usage/export", orgID), adminTok, nil, nil); st != http.StatusOK {
		t.Errorf("用量导出应 200,得 %d", st)
	}
	// 周期重置 worker:有 daily 策略 + 首次未重置 → 应重置该成员(返回>=1 或无错)。
	if rn, rerr := svc.ResetDuePolicies(ctx); rerr != nil {
		t.Errorf("周期重置失败: %v", rerr)
	} else {
		t.Logf("周期重置 ok: 本次重置成员数=%d", rn)
	}
	// D1 退款冲正(运营方减余额)+ S1 守恒:total_recharged 不被污染。
	var balBefore2 struct {
		Balance   int64 `json:"balance_quota"`
		Recharged int64 `json:"total_recharged_quota"`
	}
	api.do("GET", fmt.Sprintf("/api/v1/organizations/%d/balance", orgID), opTok, nil, &balBefore2)
	if balBefore2.Balance > 0 {
		var deb struct {
			After int64 `json:"balance_quota_after"`
		}
		if st := api.do("POST", fmt.Sprintf("/api/v1/organizations/%d/debits", orgID), opTok, map[string]any{"amount_quota": 1000000, "reason": "退款冲正"}, &deb); st != http.StatusOK || deb.After != balBefore2.Balance-1000000 {
			t.Errorf("减余额冲正应 -1e6: before=%d after=%d st=%d", balBefore2.Balance, deb.After, st)
		} else {
			t.Log("D1 退款冲正 ok: 运营方减余额 + 留痕")
		}
		// S1:冲正后累计充值不变(退款走 total_refunded,不污染 total_recharged)。
		var balAfter2 struct {
			Recharged int64 `json:"total_recharged_quota"`
		}
		api.do("GET", fmt.Sprintf("/api/v1/organizations/%d/balance", orgID), opTok, nil, &balAfter2)
		if balAfter2.Recharged != balBefore2.Recharged {
			t.Errorf("S1:冲正污染了累计充值 %d→%d", balBefore2.Recharged, balAfter2.Recharged)
		} else {
			t.Log("S1 守恒 ok: 冲正不动累计充值(退款独立流水)")
		}
		// 组织管理员减余额 → 403(动钱红线)。
		if st := api.do("POST", fmt.Sprintf("/api/v1/organizations/%d/debits", orgID), adminTok, map[string]any{"amount_quota": 1, "reason": "x"}, nil); st != http.StatusForbidden {
			t.Errorf("组织管理员减余额应 403,得 %d", st)
		}
	}
	// T9 回归:审批列表带申请人姓名(而非仅 #id)。前面里程碑 4 已由成员"钱晨"提交过申请。
	{
		var appList struct {
			List []struct {
				ApplicantID   int64  `json:"applicant_id"`
				ApplicantName string `json:"applicant_name"`
			} `json:"list"`
		}
		api.do("GET", fmt.Sprintf("/api/v1/organizations/%d/approvals?page=1&page_size=5", orgID), adminTok, nil, &appList)
		if len(appList.List) == 0 || appList.List[0].ApplicantName == "" {
			t.Errorf("T9 审批列表应带申请人姓名,实得 %+v", appList.List)
		} else {
			t.Logf("回归 T9 ok: 审批列表显示申请人姓名 %q(#%d)", appList.List[0].ApplicantName, appList.List[0].ApplicantID)
		}
	}
	// T10 回归:层级编辑/删除 + 默认档/被引用档删除防护。
	{
		var tmp struct {
			ID int64 `json:"id"`
		}
		if st := api.do("POST", fmt.Sprintf("/api/v1/organizations/%d/tiers", orgID), adminTok,
			map[string]any{"name": "临时档", "monthly_limit": 5000000}, &tmp); st != http.StatusCreated {
			t.Fatalf("T10 建临时层级 HTTP=%d", st)
		}
		if st := api.do("PUT", fmt.Sprintf("/api/v1/tiers/%d", tmp.ID), adminTok,
			map[string]any{"name": "临时档改", "monthly_limit": 9000000}, nil); st != http.StatusOK {
			t.Errorf("T10 改层级应 200,得 %d", st)
		}
		var tiers []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
			ML   *int64 `json:"monthly_limit_quota"`
		}
		api.do("GET", fmt.Sprintf("/api/v1/organizations/%d/tiers", orgID), adminTok, nil, &tiers)
		okEdit := false
		for _, tt := range tiers {
			if tt.ID == tmp.ID && tt.Name == "临时档改" && tt.ML != nil && *tt.ML == 9000000 {
				okEdit = true
			}
		}
		if !okEdit {
			t.Errorf("T10 改层级未生效: %+v", tiers)
		} else {
			t.Log("回归 T10 ok: 层级编辑生效(名称/月额度)")
		}
		// 删默认档(tierResp 仍是默认且被成员引用)→ 409 防护。
		if st := api.do("DELETE", fmt.Sprintf("/api/v1/tiers/%d", tierResp.ID), adminTok, nil, nil); st != http.StatusConflict {
			t.Errorf("T10 删默认/被引用档应 409,得 %d", st)
		}
		// 删无引用临时档 → 200。
		if st := api.do("DELETE", fmt.Sprintf("/api/v1/tiers/%d", tmp.ID), adminTok, nil, nil); st != http.StatusOK {
			t.Errorf("T10 删无引用层级应 200,得 %d", st)
		} else {
			t.Log("回归 T10 ok: 删无引用层级成功;删默认/被引用档被 409 挡")
		}
	}

	// T11 回归:自定义登录名(真实邮箱)→ 可登录;非法登录名 → 422。
	{
		custEmail := "real." + randSuffix() + "@client.com"
		var om struct {
			LoginEmail      string `json:"login_email"`
			InitialPassword string `json:"initial_password"`
		}
		if st := api.do("POST", fmt.Sprintf("/api/v1/organizations/%d/members", orgID), adminTok,
			map[string]any{"name": "自定义登录", "email": custEmail, "tier_id": tierResp.ID}, &om); st != http.StatusCreated {
			t.Fatalf("T11 自定义邮箱开通应 201,得 %d", st)
		}
		if om.LoginEmail != custEmail {
			t.Errorf("T11 登录名应=%s,得 %s", custEmail, om.LoginEmail)
		}
		if tok := login(api, custEmail, om.InitialPassword); tok == "" {
			t.Error("T11 自定义真实邮箱应能登录")
		} else {
			t.Log("回归 T11 ok: 自定义真实邮箱作登录名且可登录")
		}
		if st := api.do("POST", fmt.Sprintf("/api/v1/organizations/%d/members", orgID), adminTok,
			map[string]any{"name": "坏登录名", "email": "not valid!!", "tier_id": tierResp.ID}, nil); st != http.StatusUnprocessableEntity {
			t.Errorf("T11 非法登录名应 422,得 %d", st)
		} else {
			t.Log("回归 T11 ok: 非法登录名 → 422")
		}
	}
	// T12 回归:组织归档 → 默认列表隐藏、include_archived 可见;取消归档 → 恢复;非运营方 403。
	{
		listHas := func(inclArchived bool) bool {
			var lr struct {
				List []struct {
					ID int64 `json:"id"`
				} `json:"list"`
			}
			url := "/api/v1/organizations?page=1&page_size=100"
			if inclArchived {
				url += "&include_archived=true"
			}
			api.do("GET", url, opTok, nil, &lr)
			for _, o := range lr.List {
				if o.ID == orgID {
					return true
				}
			}
			return false
		}
		if st := api.do("POST", fmt.Sprintf("/api/v1/organizations/%d/archive", orgID), opTok, nil, nil); st != http.StatusOK {
			t.Fatalf("T12 归档应 200,得 %d", st)
		}
		if listHas(false) {
			t.Error("T12 归档后默认列表不应含该组织")
		}
		if !listHas(true) {
			t.Error("T12 include_archived 应能找到已归档组织")
		}
		if st := api.do("POST", fmt.Sprintf("/api/v1/organizations/%d/archive", orgID), adminTok, nil, nil); st != http.StatusForbidden {
			t.Errorf("T12 非运营方归档应 403,得 %d", st)
		}
		if st := api.do("POST", fmt.Sprintf("/api/v1/organizations/%d/unarchive", orgID), opTok, nil, nil); st != http.StatusOK {
			t.Fatalf("T12 取消归档应 200,得 %d", st)
		}
		if !listHas(false) {
			t.Error("T12 取消归档后默认列表应恢复该组织")
		} else {
			t.Log("回归 T12 ok: 归档隐藏+可检索+取消归档恢复+非运营方 403")
		}
	}

	// ===== T17 计费分组竖切回归 =====
	setupBillingGroupVip(t, newapiURL, adminToken, adminUID)
	{
		tiersPath := fmt.Sprintf("/api/v1/organizations/%d/tiers", orgID)
		// T17-5:配层级分组校验。不存在的分组 → 422。
		if st := api.do("POST", tiersPath, adminTok, map[string]any{"name": "T17坏分组", "newapi_group": "ghost-grp"}, nil); st != http.StatusUnprocessableEntity {
			t.Errorf("T17-5 不存在分组应 422,得 %d", st)
		}
		// vip 分组 + 该分组无可用渠道的模型 → 422。
		if st := api.do("POST", tiersPath, adminTok, map[string]any{"name": "T17坏模型", "newapi_group": "vip", "model_set": []string{"ghost-model-xyz"}}, nil); st != http.StatusUnprocessableEntity {
			t.Errorf("T17-5 分组无该模型渠道应 422,得 %d", st)
		}
		// vip + 该分组可路由的 gpt-4o → 201。
		var vtier struct {
			ID int64 `json:"id"`
		}
		if st := api.do("POST", tiersPath, adminTok, map[string]any{"name": "VIP档", "newapi_group": "vip", "model_set": []string{"gpt-4o"}, "monthly_limit": 25000000}, &vtier); st != http.StatusCreated {
			t.Fatalf("T17-5 vip+gpt-4o 应 201,得 %d", st)
		}
		t.Log("回归 T17-5 ok: 配层级分组×模型集预检(坏分组/坏模型 422、可路由 201)")
		// T17-1:开通 vip 档成员 → 令牌分组快照=vip。
		var vom struct {
			MemberID     int64  `json:"member_id"`
			NewapiUserID int64  `json:"newapi_user_id"`
			APIKey       string `json:"api_key"`
		}
		if st := api.do("POST", fmt.Sprintf("/api/v1/organizations/%d/members", orgID), adminTok, map[string]any{"name": "VIP成员", "tier_id": vtier.ID}, &vom); st != http.StatusCreated {
			t.Fatalf("T17-1 开通 vip 成员应 201,得 %d", st)
		}
		var mv struct {
			NewapiGroup *string `json:"newapi_group"`
		}
		api.do("GET", fmt.Sprintf("/api/v1/members/%d", vom.MemberID), adminTok, nil, &mv)
		if mv.NewapiGroup == nil || *mv.NewapiGroup != "vip" {
			t.Errorf("T17-1 成员令牌分组快照应=vip,得 %v", mv.NewapiGroup)
		} else {
			t.Log("回归 T17-1 ok: 开通成员令牌分组快照=vip(非 default)")
		}
		// T17-2:可用分组写到 new-api 真读的 key(group_ratio_setting.group_special_usable_group),含 org_x→vip。
		usable := readNewapiOption(t, newapiURL, adminToken, adminUID, "group_ratio_setting.group_special_usable_group")
		if !strings.Contains(usable, orgGrp) || !strings.Contains(usable, "vip") {
			t.Errorf("T17-2 可用分组(正确 key)应含 %s→vip,实得 %q", orgGrp, usable)
		} else {
			t.Log("回归 T17-2 ok: 业务分组 vip 写进 new-api 真读的可用分组 key")
		}
		// T17-2 必测(验收 P0 教训):用代发 key 真发一次请求,断言不被「无权访问该分组」403。
		// 集成栈渠道是 mock(上游打不通),但鉴权层(auth.go:386 分组可用性 / 391 分组倍率)会先过——
		// 只要可用分组写对,就不会是分组 403;mock 上游导致的其它错误(5xx/无可用渠道)均可接受。
		if vom.APIKey != "" {
			st, body := chatCall(t, newapiURL, vom.APIKey, "gpt-4o")
			if st == http.StatusForbidden && (strings.Contains(body, "分组") || strings.Contains(body, "group")) {
				t.Errorf("T17-2 P0:代发 key 真调用被分组 403(可用分组没写对):st=%d body=%s", st, body)
			} else {
				t.Logf("回归 T17-2 真调用 ok: 代发 key 过鉴权分组闸(非分组 403),st=%d", st)
			}
		}
		// T17-1 必测:轮换 vip 成员的 key 后,令牌分组不丢回 default(SQL 直查 newapi.tokens.group)。
		if newapiSQLDSN != "" {
			vMemberTok, _ := signer.Issue(session.Claims{MemberID: vom.MemberID, OrgID: orgID, Role: session.RoleMember})
			if st := api.do("POST", fmt.Sprintf("/api/v1/members/%d/key:rotate", vom.MemberID), vMemberTok, nil, nil); st != http.StatusOK {
				t.Errorf("T17-1 轮换 vip 成员 key 应 200,得 %d", st)
			}
			if g := tokenGroupBySQL(t, newapiSQLDSN, vom.NewapiUserID); g != "vip" {
				t.Errorf("T17-1 必测:轮换后令牌分组应仍=vip,实得 %q(丢回 default 即回归)", g)
			} else {
				t.Log("回归 T17-1 ok: 轮换 key 后令牌分组仍=vip(不丢回 default)")
			}
		}
		// T17-3:对 vip 配 per_group 折扣 → GroupGroupRatio[org_X][vip] 写入(命中折扣)。
		if st := api.do("PUT", fmt.Sprintf("/api/v1/organizations/%d/pricing", orgID), opTok, map[string]any{"mode": "per_group", "discount_pct": 0.8, "token_groups": []string{"vip"}}, nil); st != http.StatusOK {
			t.Fatalf("T17-3 配 vip 折扣应 200,得 %d", st)
		}
		ggr := readNewapiOption(t, newapiURL, adminToken, adminUID, "GroupGroupRatio")
		if !strings.Contains(ggr, orgGrp) || !strings.Contains(ggr, "vip") {
			t.Errorf("T17-3 GroupGroupRatio 应含 %s/vip,实得 %s", orgGrp, ggr)
		} else {
			t.Log("回归 T17-3 ok: 非 default 分组 vip 的 per_group 折扣写入 GroupGroupRatio(命中)")
		}
	}

	// E1 破玻璃本期关。
	if st := api.do("POST", fmt.Sprintf("/api/v1/organizations/%d/support-sessions", orgID), opTok, map[string]any{"scope": "assist", "grant_type": "break_glass", "ttl_seconds": 600}, nil); st != http.StatusForbidden {
		t.Errorf("破玻璃本期应 403(二期),得 %d", st)
	} else {
		t.Log("E1 破玻璃本期关 ok: → 403")
	}
	t.Log("补全功能全通过:组织设置/审批阈值/配额策略/角色任命/批量导入/服务状态/IP白名单/用量导出/周期重置/退款冲正/破玻璃关")

	// ===== 里程碑 3b:读 logs 扣费 + 去重 + 硬停(需 new-api 库连接造日志,本地集成 compose)=====
	if newapiSQLDSN == "" {
		t.Log("跳过 3b 扣费实测(未设 NEXUS_IT_NEWAPI_SQL_DSN);开关 RBAC 已验")
		t.Log("里程碑 3b 开关部分通过")
		return
	}

	// 开计费(关硬停、低位阈值清 0,状态判定干净)。
	if st := api.do("PATCH", fmt.Sprintf("/api/v1/organizations/%d/billing-settings", orgID), opTok,
		map[string]any{"billing_enabled": true, "hard_stop_enabled": false, "low_watermark_quota": 0}, nil); st != http.StatusOK {
		t.Fatalf("开计费 HTTP=%d", st)
	}

	readBal := func() int64 {
		var b struct {
			Balance  int64 `json:"balance_quota"`
			Consumed int64 `json:"total_consumed_quota"`
		}
		api.do("GET", fmt.Sprintf("/api/v1/organizations/%d/balance", orgID), opTok, nil, &b)
		return b.Balance
	}
	seedNow := time.Now().Unix()
	balB3b := readBal()

	// 造一条 4,000,000 消费日志 → 结算扣费。
	seedConsumptionLog(t, newapiSQLDSN, uid, "gpt-5-mini", 4000000, seedNow-10)
	deducted, derr := svc.RunSettlement(ctx)
	if derr != nil {
		t.Fatalf("结算失败: %v", derr)
	}
	balAfter := readBal()
	if deducted != 4000000 || balAfter != balB3b-4000000 {
		t.Errorf("结算应扣 4e6: deducted=%d before=%d after=%d", deducted, balB3b, balAfter)
	} else {
		t.Logf("3b 读logs扣费 ok: 扣 %d,余额 %d→%d", deducted, balB3b, balAfter)
	}

	// E4 单模型软限额:gpt-5-mini 今日 4e6 > model_cap 1e6 → 成员应收到 soft_limit 告警。
	rawN := api.doRaw("GET", "/api/v1/notifications", memberTok, nil)
	if !strings.Contains(rawN, "soft_limit") {
		t.Errorf("单模型超 cap 应生成 soft_limit 告警,通知里未见:%.200s", rawN)
	} else {
		t.Log("E4 单模型软限额 ok: gpt-5-mini 超 model_cap → 成员收到 soft_limit 告警")
	}

	// 再结算一次 → 去重不重复扣。
	if d2, _ := svc.RunSettlement(ctx); d2 != 0 || readBal() != balAfter {
		t.Errorf("重复结算应去重不再扣: d2=%d bal=%d", d2, readBal())
	} else {
		t.Log("3b 去重 ok: 同一桶重复结算不再扣")
	}

	// 🔴 阻断 bug 回归(灰度前 NO-GO 项):同一小时桶第 2 笔消费,分两次结算必须也全额入账。
	// 修复前 UpsertLedgerBucket 的 INSERT IGNORE 命中已存在小时桶 → inserted=false → 该笔永不扣(系统性少收)。
	// 真实流量里第 2 笔是后到的,故用结算滞后窗口(5s)模拟"后到 → 下一次结算窗口"。
	{
		balB2 := readBal()
		nowSec := time.Now().Unix()
		seedConsumptionLog(t, newapiSQLDSN, uid, "gpt-5-mini", 1500000, nowSec-3) // 同小时、新日志(更高 id)
		time.Sleep(4 * time.Second)                                               // 等过滞后窗口,使该日志进入下一次结算窗口
		d, derr := svc.RunSettlement(ctx)
		if derr != nil {
			t.Fatalf("同小时第二笔结算失败: %v", derr)
		}
		if d != 1500000 || readBal() != balB2-1500000 {
			t.Errorf("🔴 少收回归:同小时第2笔应入账 1.5e6,实 deducted=%d 余额 %d→%d(0/不变即 bug 复现)", d, balB2, readBal())
		} else {
			t.Log("回归(阻断bug) ok: 同一小时桶第2笔分两次结算也全额入账(log-id 水位去重,不再丢)")
		}
		// 守恒真账版:平台已结算总消耗 == new-api.logs 真实总额(本组织/成员)。
		// 注意:usage_ledger 在 nexus 库(store.DB()),logs 在 newapi 库(newapiSQLDSN)——别查错库。
		var consumed int64
		store.DB().QueryRowContext(ctx, "SELECT total_consumed FROM company_balance WHERE org_id=?", orgID).Scan(&consumed)
		ndb, derr2 := sql.Open("mysql", newapiSQLDSN)
		if derr2 != nil {
			t.Fatalf("连 newapi 库失败: %v", derr2)
		}
		var logSum int64
		ndb.QueryRowContext(ctx, "SELECT COALESCE(SUM(quota),0) FROM logs WHERE user_id=? AND type=2", uid).Scan(&logSum)
		ndb.Close()
		if consumed != logSum {
			t.Errorf("🔴 计费对账:平台 total_consumed=%d != new-api.logs 真实总额=%d(少收/多收)", consumed, logSum)
		} else {
			t.Logf("回归 计费对账(守恒真账版) ok: 平台 total_consumed == new-api.logs = %d", consumed)
		}
	}

	// 硬停:开 hard_stop,造一笔超过余额的消耗,重置游标(去重保证 4e6 不再扣),结算 → 余额≤0 → 成员被 disable+quota0。
	api.do("PATCH", fmt.Sprintf("/api/v1/organizations/%d/billing-settings", orgID), opTok,
		map[string]any{"hard_stop_enabled": true}, nil)
	bigConsume := balAfter + 1000000
	seedConsumptionLog(t, newapiSQLDSN, uid, "gpt-4o-mini", bigConsume, seedNow-10)
	if _, err := store.DB().ExecContext(ctx, "UPDATE settlement_cursor SET last_settled_ts=0 WHERE org_id=0"); err != nil {
		t.Fatalf("重置游标: %v", err)
	}
	if _, err := svc.RunSettlement(ctx); err != nil {
		t.Fatalf("硬停结算失败: %v", err)
	}
	if q, _ := getNewapiUser(t, newapiURL, adminToken, adminUID, uid); q != 0 {
		t.Errorf("硬停后成员 new-api quota 应=0,得 %d", q)
	} else {
		t.Log("3b 硬停 ok: 余额≤0 + 硬停开 → 成员 quota override 为 0(网关实时拒)")
	}
	var orgSt struct {
		Status string `json:"status"`
	}
	api.do("GET", fmt.Sprintf("/api/v1/organizations/%d", orgID), opTok, nil, &orgSt)
	if orgSt.Status != "stopped" {
		t.Errorf("硬停后组织状态应 stopped,得 %q", orgSt.Status)
	}

	// 充值恢复 → 解硬停,成员 quota 重算恢复 >0。
	api.do("POST", fmt.Sprintf("/api/v1/organizations/%d/recharges", orgID), opTok,
		map[string]any{"amount_quota": 100000000, "transfer_no": "TR-RESTORE-" + randSuffix()}, nil)
	if q, _ := getNewapiUser(t, newapiURL, adminToken, adminUID, uid); q <= 0 {
		t.Errorf("充值恢复后成员 quota 应>0,得 %d", q)
	} else {
		t.Logf("3b 解硬停 ok: 充值后成员 quota 重算恢复=%d", q)
	}

	// ===== GZ-01 D2:路径B 时间窗分块——单窗口 > 2000 条不截断、逐拍排空、零少收(现有用例未覆盖新分块代码)=====
	{
		api.do("POST", fmt.Sprintf("/api/v1/organizations/%d/recharges", orgID), opTok,
			map[string]any{"amount_quota": 100000000, "transfer_no": "TR-D2-" + randSuffix()}, nil)
		balBefore := readBal()
		const n = 2050
		const qEach = 1000
		base := time.Now().Unix() - 200 // backlog 起点(在 5s 滞后窗口之前)
		// ts 水位设到 backlog 之前(保留 log-id 水位,旧日志靠它去重),让这批新日志全进同一大窗口触发分块。
		if _, err := store.DB().ExecContext(ctx, "UPDATE settlement_cursor SET last_settled_ts=? WHERE org_id=0", base-10); err != nil {
			t.Fatalf("D2 设游标 ts: %v", err)
		}
		seedManyLogs(t, newapiSQLDSN, uid, "gpt-d2", qEach, n, base, 150) // 2050 条散布 150s → 单窗 >2000 须分块
		var total int64
		drained := false
		for i := 0; i < 40; i++ {
			d, derr := svc.RunSettlement(ctx)
			if derr != nil {
				t.Fatalf("D2 第 %d 拍结算失败: %v", i, derr)
			}
			total += d
			if d == 0 {
				drained = true
				break
			}
		}
		want := int64(n) * qEach
		switch {
		case !drained:
			t.Errorf("🔴 GZ-01 D2:40 拍内未排空,分块可能未推进(已结 %d/%d)", total, want)
		case total != want:
			t.Errorf("🔴 GZ-01 D2 少收:backlog %d 条应全额结算 %d,实结 %d(差 %d)", n, want, total, want-total)
		case readBal() != balBefore-want:
			t.Errorf("🔴 GZ-01 D2 余额对不上:%d→期望 %d,实 %d", balBefore, balBefore-want, readBal())
		default:
			t.Logf("GZ-01 D2 路径B ok: %d 条 backlog(散布150s、单窗>2000)逐拍分块全额结算 %d,零少收、余额一致", n, total)
		}
	}

	// ===== GZ-01 D1:结算事务原子性——扣余额中途失败→整批回滚(不半提交 ledger、不推水位)、恢复后可重做 =====
	{
		var rech, cons, bal, low, ver, refunded int64
		if err := store.DB().QueryRowContext(ctx,
			"SELECT total_recharged,total_consumed,balance,low_watermark,version,total_refunded FROM company_balance WHERE org_id=?", orgID).
			Scan(&rech, &cons, &bal, &low, &ver, &refunded); err != nil {
			t.Fatalf("D1 读余额行: %v", err)
		}
		nowSec := time.Now().Unix()
		if _, err := store.DB().ExecContext(ctx, "UPDATE settlement_cursor SET last_settled_ts=? WHERE org_id=0", nowSec-20); err != nil {
			t.Fatalf("D1 设游标 ts: %v", err)
		}
		seedConsumptionLog(t, newapiSQLDSN, uid, "gpt-d1", 777, nowSec-10)
		var logIDBefore int64
		store.DB().QueryRowContext(ctx, "SELECT last_settled_log_id FROM settlement_cursor WHERE org_id=0").Scan(&logIDBefore)
		// 故障注入:删该组织余额行 → 事务内 DeductBalanceTx 返 ErrNotFound → WithTx 整批回滚。
		if _, err := store.DB().ExecContext(ctx, "DELETE FROM company_balance WHERE org_id=?", orgID); err != nil {
			t.Fatalf("D1 删余额行: %v", err)
		}
		if _, err := svc.RunSettlement(ctx); err == nil {
			t.Errorf("🔴 GZ-01 D1:删余额行后结算应报错回滚,却成功了")
		}
		var logIDAfter int64
		store.DB().QueryRowContext(ctx, "SELECT last_settled_log_id FROM settlement_cursor WHERE org_id=0").Scan(&logIDAfter)
		if logIDAfter != logIDBefore {
			t.Errorf("🔴 GZ-01 D1:回滚后 cursor log_id 不应推进(%d→%d)", logIDBefore, logIDAfter)
		}
		// 恢复余额行后重做:那条 777 应全额入账(证明回滚无永久丢失、可重做、无双扣)。
		if _, err := store.DB().ExecContext(ctx,
			"INSERT INTO company_balance (org_id,total_recharged,total_consumed,balance,low_watermark,version,total_refunded) VALUES (?,?,?,?,?,?,?)",
			orgID, rech, cons, bal, low, ver, refunded); err != nil {
			t.Fatalf("D1 恢复余额行: %v", err)
		}
		d, err := svc.RunSettlement(ctx)
		if err != nil {
			t.Fatalf("D1 恢复后重跑失败: %v", err)
		}
		if d != 777 {
			t.Errorf("🔴 GZ-01 D1:恢复后重做应结算 777,实 %d", d)
		} else {
			t.Log("GZ-01 D1 原子回滚 ok: 扣余额失败→整批回滚(cursor 未推进、ledger 未半提交)→恢复后重做全额入账")
		}
	}

	// ===== GZ-03 返工复测:④建token成功后⑤失败 → 收口禁用孤儿 + 回写user_id + 孤儿消费不漏扣且告警 =====
	{
		os.Setenv("NEXUS_IT_FAULT_REVEAL_FAIL", "1") // 仅测试:令第⑤步失败,走收口路径
		var failResp map[string]any
		// 用组织管理员开通(运营方无开通权会被 403 挡在 bootstrap 之前)。
		stFail := api.do("POST", fmt.Sprintf("/api/v1/organizations/%d/members", orgID), adminTok,
			map[string]any{"name": "孤儿测试", "team_id": teamResp.ID, "tier_id": tierResp.ID}, &failResp)
		os.Unsetenv("NEXUS_IT_FAULT_REVEAL_FAIL") // 立即复原,绝不影响后续
		if stFail == http.StatusCreated {
			t.Fatalf("GZ-03:注入⑤失败后开通应失败,却返回 201")
		}
		// 收口后:墓碑行 bootstrap_state=failed 且已回写 newapi_user_id(洞3 修复)。
		var orphanMemberID, orphanUID int64
		if err := store.DB().QueryRowContext(ctx,
			"SELECT id, newapi_user_id FROM member WHERE org_id=? AND bootstrap_state='failed' AND newapi_user_id IS NOT NULL ORDER BY id DESC LIMIT 1", orgID).
			Scan(&orphanMemberID, &orphanUID); err != nil {
			t.Fatalf("🔴 GZ-03 洞3:未找到回写 newapi_user_id 的墓碑行(回写未生效?): %v", err)
		}
		t.Logf("GZ-03:⑤失败收口 → 墓碑行 member=%d 回写 newapi_user_id=%d", orphanMemberID, orphanUID)
		// 洞1:不留活跃孤儿——该 new-api 用户应已被禁用(status != 1)。
		if _, st := getNewapiUser(t, newapiURL, adminToken, adminUID, orphanUID); st == 1 {
			t.Errorf("🔴 GZ-03 洞1:收口后孤儿 new-api 用户仍 active(status=1),应被禁用")
		} else {
			t.Logf("GZ-03 洞1 ok: 孤儿 new-api 用户已禁用(status=%d)", st)
		}
		// 洞3:造孤儿消费 → 结算应认领计费(回写后反查得到、不漏扣)+ 落孤儿告警。
		nowSec := time.Now().Unix()
		if _, err := store.DB().ExecContext(ctx, "UPDATE settlement_cursor SET last_settled_ts=? WHERE org_id=0", nowSec-20); err != nil {
			t.Fatalf("GZ-03 设游标: %v", err)
		}
		seedConsumptionLog(t, newapiSQLDSN, orphanUID, "gpt-orphan", 333, nowSec-10)
		balB := readBal()
		d, derr := svc.RunSettlement(ctx)
		if derr != nil {
			t.Fatalf("GZ-03 孤儿消费结算失败: %v", derr)
		}
		if d != 333 || readBal() != balB-333 {
			t.Errorf("🔴 GZ-03 洞3 漏扣:孤儿消费 333 应被认领计费(不漏扣),实 deducted=%d 余额 %d→%d", d, balB, readBal())
		} else {
			t.Log("GZ-03 洞3 ok: 孤儿消费被结算认领计费(回写 user_id 后反查得到,不再静默漏扣)")
		}
		var alertCnt int
		store.DB().QueryRowContext(ctx,
			"SELECT COUNT(*) FROM audit_log WHERE action='orphan_consumption' AND target_id=?", orphanMemberID).Scan(&alertCnt)
		if alertCnt == 0 {
			t.Errorf("🔴 GZ-03 洞3:孤儿消费应触发 orphan_consumption 告警(审计),未见")
		} else {
			t.Logf("GZ-03 洞3 ok: 孤儿消费告警已落审计(orphan_consumption × %d)", alertCnt)
		}
	}

	// ===== 改动⑤(MVP 观测落账不扣钱)回归 =====
	// 同一组织 billing_enabled 仍为 true,但用 ObserveMode=true 的 service 跑结算:
	// 必须「落账(usage_ledger 增长)但不扣余额(balance/total_consumed 不动)」。这是 MVP 看板的地基:
	// 上线 observe → 钱一分不动,用量照常可见;将来翻掉 observe 即恢复真扣,结构一步到位。
	{
		obsSvc := service.New(service.Deps{Store: store, Upstream: upstream, Keyring: keyring, Signer: signer, Logger: log, ObserveMode: true})
		balBefore := readBal()
		var consumedBefore, ledgerBefore int64
		store.DB().QueryRowContext(ctx, "SELECT total_consumed FROM company_balance WHERE org_id=?", orgID).Scan(&consumedBefore)
		store.DB().QueryRowContext(ctx, "SELECT COALESCE(SUM(consumed_quota),0) FROM usage_ledger WHERE org_id=?", orgID).Scan(&ledgerBefore)

		nowSec := time.Now().Unix()
		seedConsumptionLog(t, newapiSQLDSN, uid, "gpt-5-mini", 2222222, nowSec-3) // 新日志(更高 id),过滞后窗口后入窗
		time.Sleep(4 * time.Second)
		deducted, derr := obsSvc.RunSettlement(ctx)
		if derr != nil {
			t.Fatalf("观测模式结算失败: %v", derr)
		}
		var consumedAfter, ledgerAfter int64
		store.DB().QueryRowContext(ctx, "SELECT total_consumed FROM company_balance WHERE org_id=?", orgID).Scan(&consumedAfter)
		store.DB().QueryRowContext(ctx, "SELECT COALESCE(SUM(consumed_quota),0) FROM usage_ledger WHERE org_id=?", orgID).Scan(&ledgerAfter)

		if deducted != 0 || readBal() != balBefore || consumedAfter != consumedBefore {
			t.Errorf("🔴 改动⑤:观测模式必须不扣钱 — deducted=%d(应0) 余额 %d→%d total_consumed %d→%d(均应不变)",
				deducted, balBefore, readBal(), consumedBefore, consumedAfter)
		} else if ledgerAfter != ledgerBefore+2222222 {
			t.Errorf("🔴 改动⑤:观测模式必须照常落账 — usage_ledger %d→%d(应 +2222222)", ledgerBefore, ledgerAfter)
		} else {
			t.Logf("改动⑤ ok: 观测模式落账不扣钱 — ledger +2222222 而 balance/total_consumed 纹丝不动(deducted=0)")
		}
	}

	// ===== 改动②③ MVP 开通/自助建 key 回归(总监复验"有条件 GO"补强:3 条) =====
	{
		// ② MVP 开通只建用户不建令牌:用 ObserveMode=true 的 service 开通 → 有 new-api 用户、无令牌/无明文 key。
		obsSvc2 := service.New(service.Deps{Store: store, Upstream: upstream, Keyring: keyring, Signer: signer, Logger: log, ObserveMode: true})
		var adminMID int64
		if err := store.DB().QueryRowContext(ctx, "SELECT id FROM member WHERE org_id=? AND role='org_admin' LIMIT 1", orgID).Scan(&adminMID); err != nil {
			t.Fatalf("取组织管理员 member_id 失败: %v", err)
		}
		adminClaims := session.Claims{MemberID: adminMID, OrgID: orgID, Role: session.RoleOrgAdmin}
		mvpRes, err := obsSvc2.OpenMember(ctx, adminClaims, orgID, service.OpenMemberInput{Name: "观测开通无key"})
		if err != nil {
			t.Fatalf("② MVP 开通失败: %v", err)
		}
		if mvpRes.NewapiUserID == 0 || mvpRes.APIKey != "" || mvpRes.KeyMasked != "" {
			t.Errorf("🔴 ② MVP 开通应只建用户不建令牌:user_id=%d(应非0) api_key=%q key_masked=%q(应均空)", mvpRes.NewapiUserID, mvpRes.APIKey, mvpRes.KeyMasked)
		} else {
			t.Logf("② ok: MVP(观测)开通只建 new-api 用户 #%d、不建令牌(无明文 key)", mvpRes.NewapiUserID)
		}

		// ===== 必做1(放行前·财务P0)回归:MVP(观测)藏价 =====
		// 同一组织、同一带价读端点(GetPricing/GetBalance),三种身份差分:
		//   - 客户 org_admin(SupportSessionID=0)→ 必须被拒(不得直连门户登录拿倍率/折扣/余额)
		//   - 运营方(operator)与运营方支持态(SupportSessionID!=0)→ 必须放行(看价正常)
		// 运营/支持读同一 org 成功、而客户读同一 org 失败 ⇒ 差异只能来自藏价闸,隔离住根因。
		operatorClaims := session.Claims{Role: session.RoleOperator}
		supportClaims := session.Claims{MemberID: adminMID, OrgID: orgID, Role: session.RoleOrgAdmin, SupportSessionID: 1}
		if _, err := obsSvc2.GetPricing(ctx, operatorClaims, orgID); err != nil {
			t.Errorf("🔴 必做1:运营方读 pricing 应放行,却被拒: %v", err)
		}
		if _, err := obsSvc2.GetPricing(ctx, supportClaims, orgID); err != nil {
			t.Errorf("🔴 必做1:运营方支持态读 pricing 应放行,却被拒: %v", err)
		}
		if _, err := obsSvc2.GetPricing(ctx, adminClaims, orgID); err == nil {
			t.Errorf("🔴 必做1:MVP 下客户 org_admin 直连读 pricing 必须被拒(泄倍率/折扣),却放行")
		}
		if _, err := obsSvc2.GetBalance(ctx, adminClaims, orgID); err == nil {
			t.Errorf("🔴 必做1:MVP 下客户 org_admin 直连读 balance 必须被拒(泄余额),却放行")
		}
		if _, err := obsSvc2.GetBalance(ctx, operatorClaims, orgID); err != nil {
			t.Errorf("🔴 必做1:运营方读 balance 应放行,却被拒: %v", err)
		}
		t.Logf("必做1 ok: MVP 藏价 — 运营/支持态读 pricing/balance 放行,客户 org_admin 直连读被拒")

		// 用刚开的这名全新 MVP 成员(无令牌、状态干净)自助建首把 key,走真实 CreateToken 路径(非复用被前面停用/硬停污染的老成员)。
		mvpMemberTok, _ := signer.Issue(session.Claims{MemberID: mvpRes.MemberID, OrgID: orgID, Role: session.RoleMember})

		// ③-a 员工自助建首把 key(本人 + 本企业可用分组 vip)→ 201 + 真 key。
		var ck struct {
			APIKey    string `json:"api_key"`
			KeyMasked string `json:"key_masked"`
		}
		if st := api.do("POST", fmt.Sprintf("/api/v1/members/%d/tokens", mvpRes.MemberID), mvpMemberTok, map[string]any{"group": "vip"}, &ck); st != http.StatusCreated || ck.APIKey == "" {
			t.Errorf("🔴 ③ 本人自助建首把 key(vip)应 201+出 key,得 st=%d key=%q", st, ck.APIKey)
		} else {
			t.Logf("③ ok: MVP 成员自助建首把 key(分组 vip)成功,出明文 key %s...(CreateToken 路径)", ck.APIKey[:min(8, len(ck.APIKey))])
		}

		// ③-b 越权:替别人建 key → 403(仅本人)。
		if st := api.do("POST", fmt.Sprintf("/api/v1/members/%d/tokens", mvpRes.MemberID+99999), mvpMemberTok, map[string]any{"group": "vip"}, nil); st != http.StatusForbidden {
			t.Errorf("🔴 ③ 替别人建 key 应 403,得 %d", st)
		} else {
			t.Log("③ ok: 替别人建 key → 403(仅本人)")
		}

		// ③-c 越界:选非本企业可用的分组 → 422(隔离边界)。
		if st := api.do("POST", fmt.Sprintf("/api/v1/members/%d/tokens", mvpRes.MemberID), mvpMemberTok, map[string]any{"group": "ghost-grp-xyz"}, nil); st != http.StatusUnprocessableEntity {
			t.Errorf("🔴 ③ 选非本企业分组应 422,得 %d", st)
		} else {
			t.Log("③ ok: 选非本企业可用分组 → 422(隔离边界)")
		}
	}

	t.Log("里程碑 3b e2e 全通过:读logs扣费 + 去重防重复扣 + 守恒 + 硬停(逐组织开关)+ 充值解硬停恢复 + GZ-01 D2路径B + D1原子回滚 + GZ-03 ⑤失败收口/不漏扣/告警 + 改动⑤观测落账不扣钱 + 改动②③开通无token/自助建key/越权隔离")
}

// ---- helpers ----

type apiClient struct {
	t    *testing.T
	base string
}

// do 发请求并(可选)反序列化 data 字段,返回 HTTP 状态码。
func (a *apiClient) do(method, path, token string, body any, out any) int {
	raw, status := a.doStatus(method, path, token, body)
	if out != nil && status >= 200 && status < 300 {
		if err := json.Unmarshal([]byte(extractData(a.t, raw)), out); err != nil {
			a.t.Fatalf("%s %s 解析 data 失败: %v\n原始: %s", method, path, err, raw)
		}
	}
	return status
}

func (a *apiClient) doRaw(method, path, token string, body any) string {
	raw, _ := a.doStatus(method, path, token, body)
	return raw
}

func (a *apiClient) doStatus(method, path, token string, body any) (string, int) {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, a.base+path, rdr)
	if err != nil {
		a.t.Fatalf("构造请求失败: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatalf("%s %s 请求失败: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return string(raw), resp.StatusCode
}

func login(api *apiClient, email, password string) string {
	var resp struct {
		Token string `json:"token"`
	}
	st := api.do("POST", "/api/v1/auth/login", "", map[string]any{"email": email, "password": password}, &resp)
	if st != http.StatusOK || resp.Token == "" {
		api.t.Fatalf("登录失败 email=%s HTTP=%d", email, st)
	}
	return resp.Token
}

// extractData 从信封里取出 data 字段的原始 JSON。
func extractData(t *testing.T, raw string) string {
	var env struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatalf("信封解析失败: %v\n原始: %s", err, raw)
	}
	return string(env.Data)
}

func openWithRetry(t *testing.T, ctx context.Context, dsn string) *repo.Store {
	deadline := time.Now().Add(90 * time.Second)
	for {
		store, err := repo.Open(ctx, dsn)
		if err == nil {
			return store
		}
		if time.Now().After(deadline) {
			t.Fatalf("连 MySQL 超时: %v", err)
		}
		time.Sleep(2 * time.Second)
	}
}

func mustKeyring(t *testing.T) *crypto.Keyring {
	key := make([]byte, crypto.KeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	kr, err := crypto.NewKeyring("v1", map[string][]byte{"v1": key})
	if err != nil {
		t.Fatal(err)
	}
	return kr
}

// setupRC4 初始化全新 rc.4,返回管理员 access_token + uid(双头鉴权,里程碑 0 实证契约)。
func setupRC4(t *testing.T, base string) (string, int) {
	jar, _ := cookiejar.New(nil)
	hc := &http.Client{Timeout: 15 * time.Second, Jar: jar}
	const rootPass = "RootPass123"

	loginRoot := func() (int, bool) {
		body, _ := json.Marshal(map[string]string{"username": "root", "password": rootPass})
		resp, err := hc.Post(base+"/api/user/login", "application/json", bytes.NewReader(body))
		if err != nil {
			return 0, false
		}
		defer resp.Body.Close()
		var env struct {
			Success bool `json:"success"`
			Data    struct {
				ID int `json:"id"`
			} `json:"data"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&env)
		return env.Data.ID, env.Success
	}

	// 等 rc.4 起来 + setup(带重试)。
	deadline := time.Now().Add(90 * time.Second)
	var uid int
	for {
		id, ok := loginRoot()
		if ok {
			uid = id
			break
		}
		body, _ := json.Marshal(map[string]string{"username": "root", "password": rootPass, "confirmPassword": rootPass})
		resp, err := hc.Post(base+"/api/setup", "application/json", bytes.NewReader(body))
		if err == nil {
			resp.Body.Close()
		}
		if id, ok := loginRoot(); ok {
			uid = id
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("rc.4 setup/login 超时")
		}
		time.Sleep(2 * time.Second)
	}
	if uid == 0 {
		uid = 1
	}

	req, _ := http.NewRequest("GET", base+"/api/user/token", nil)
	req.Header.Set("New-Api-User", strconv.Itoa(uid))
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("取管理员 access_token 失败: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	_ = json.Unmarshal(raw, &env)
	var token string
	if err := json.Unmarshal(env.Data, &token); err != nil || token == "" {
		var obj struct {
			AccessToken string `json:"access_token"`
			Key         string `json:"key"`
		}
		_ = json.Unmarshal(env.Data, &obj)
		token = obj.AccessToken
		if token == "" {
			token = obj.Key
		}
	}
	if token == "" {
		t.Fatalf("未取到管理员 access_token,原始: %s", raw)
	}
	return token, uid
}

// newapiAdminReq 以管理员身份直打 new-api(测试搭非 default 分组场景用)。返回响应体。
func newapiAdminReq(t *testing.T, method, base, path, adminToken string, adminUID int, body any) []byte {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, base+path, rd)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("New-Api-User", strconv.Itoa(adminUID))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("直打 new-api %s %s 失败: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return raw
}

// readNewapiOption 读 new-api 某 option 的值(字符串)。
func readNewapiOption(t *testing.T, base, adminToken string, adminUID int, key string) string {
	raw := newapiAdminReq(t, "GET", base, "/api/option/", adminToken, adminUID, nil)
	var env struct {
		Data []struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &env)
	for _, o := range env.Data {
		if o.Key == key {
			return o.Value
		}
	}
	return ""
}

// setupBillingGroupVip 在测试 new-api 建非 default 计费分组 vip(GroupRatio 0.5 + 一个渠道服务 gpt-4o
// 于 default,vip → /api/pricing 把 gpt-4o 列进 vip,供 T17 竖切验证)。
func setupBillingGroupVip(t *testing.T, base, adminToken string, adminUID int) {
	newapiAdminReq(t, "PUT", base, "/api/option/", adminToken, adminUID,
		map[string]string{"key": "GroupRatio", "value": `{"default":1,"vip":0.5,"enterprise":0.85}`})
	ch := map[string]any{
		"name": "T17-vip-ch", "type": 1, "key": "sk-mock-t17-DONOTUSE", "base_url": "https://mock-upstream.local",
		"models": "gpt-4o", "groups": []string{"default", "vip"}, "group": "default,vip",
		"model_mapping": "", "setting": "", "status_code_mapping": "", "auto_ban": 1, "weight": 0, "priority": 0, "tag": "",
	}
	newapiAdminReq(t, "POST", base, "/api/channel/", adminToken, adminUID, map[string]any{"mode": "single", "channel": ch})
	// 等 /api/pricing 反映新渠道(vip 含 gpt-4o);abilities/pricing 可能有缓存延迟。
	for i := 0; i < 15; i++ {
		raw := newapiAdminReq(t, "GET", base, "/api/pricing", adminToken, adminUID, nil)
		var env struct {
			Data []struct {
				ModelName    string   `json:"model_name"`
				EnableGroups []string `json:"enable_groups"`
			} `json:"data"`
		}
		_ = json.Unmarshal(raw, &env)
		for _, m := range env.Data {
			if m.ModelName == "gpt-4o" {
				for _, g := range m.EnableGroups {
					if g == "vip" {
						return
					}
				}
			}
		}
		time.Sleep(time.Second)
	}
	t.Log("警告:/api/pricing 15s 内未反映 vip→gpt-4o,T17-5 用例可能受影响")
}

// getNewapiUser 以管理员身份查 new-api 用户的 quota 与 status(1=enabled,2=disabled)。
func getNewapiUser(t *testing.T, base, adminToken string, adminUID int, userID int64) (int64, int) {
	// new-api 对高频 API 有限流(429,空/非 JSON body);测试压得紧时退避重试几次再判失败。
	var lastRaw []byte
	for attempt := 0; attempt < 8; attempt++ {
		req, _ := http.NewRequest("GET", fmt.Sprintf("%s/api/user/%d", base, userID), nil)
		req.Header.Set("Authorization", "Bearer "+adminToken)
		req.Header.Set("New-Api-User", strconv.Itoa(adminUID))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("查 new-api 用户失败: %v", err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		lastRaw = raw
		if resp.StatusCode == http.StatusTooManyRequests || len(raw) == 0 {
			time.Sleep(time.Duration(200*(attempt+1)) * time.Millisecond) // 退避让限流窗口恢复
			continue
		}
		var env struct {
			Data struct {
				Quota  int64 `json:"quota"`
				Status int   `json:"status"`
			} `json:"data"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("解析 new-api 用户失败: %v\n%s", err, raw)
		}
		return env.Data.Quota, env.Data.Status
	}
	t.Fatalf("查 new-api 用户被限流(重试用尽):%s", lastRaw)
	return 0, 0
}

// ensureDatabase 用一个已存在库的连接建另一个库(本地 compose 自建 nexus)。
func ensureDatabase(t *testing.T, dsn, dbname string) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("连 newapi 库失败: %v", err)
	}
	defer db.Close()
	deadline := time.Now().Add(90 * time.Second)
	for {
		if err = db.Ping(); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("等 MySQL 超时: %v", err)
		}
		time.Sleep(2 * time.Second)
	}
	if _, err := db.Exec("CREATE DATABASE IF NOT EXISTS " + dbname + " CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"); err != nil {
		t.Fatalf("建库 %s 失败: %v", dbname, err)
	}
}

// seedConsumptionLog 向 newapi.logs 造一条 type=2 消费日志(测试模拟用量,平台经 /api/log/ 读)。
func seedConsumptionLog(t *testing.T, dsn string, userID int64, model string, quota int64, createdAt int64) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("连 newapi 库失败: %v", err)
	}
	defer db.Close()
	_, err = db.Exec(
		"INSERT INTO logs (user_id, created_at, type, content, username, token_name, model_name, quota, "+
			"prompt_tokens, completion_tokens, use_time, is_stream, channel_id, channel_name, token_id, `group`, ip, request_id, other) "+
			"VALUES (?, ?, 2, '', '', '', ?, ?, 100, 200, 1, 0, 0, '', 0, 'default', '', '', '')",
		userID, createdAt, model, quota)
	if err != nil {
		t.Fatalf("造消费日志失败: %v", err)
	}
}

// seedManyLogs 批量造 count 条 type=2 消费日志(created_at 在 [baseTS, baseTS+spreadSec] 均匀散布),
// 用于 GZ-01 D2 路径B 测试(单窗口 >2000 条须分块逐拍排空)。分批多行 INSERT,远快于逐条。
func seedManyLogs(t *testing.T, dsn string, userID int64, model string, quotaEach int64, count int, baseTS, spreadSec int64) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("连 newapi 库失败: %v", err)
	}
	defer db.Close()
	const batch = 500
	const cols = "(user_id, created_at, type, content, username, token_name, model_name, quota, " +
		"prompt_tokens, completion_tokens, use_time, is_stream, channel_id, channel_name, token_id, `group`, ip, request_id, other)"
	for start := 0; start < count; start += batch {
		end := start + batch
		if end > count {
			end = count
		}
		var sb strings.Builder
		sb.WriteString("INSERT INTO logs " + cols + " VALUES ")
		args := make([]any, 0, (end-start)*4)
		for i := start; i < end; i++ {
			if i > start {
				sb.WriteString(",")
			}
			sb.WriteString("(?, ?, 2, '', '', '', ?, ?, 100, 200, 1, 0, 0, '', 0, 'default', '', '', '')")
			ts := baseTS + int64(i)*spreadSec/int64(count)
			args = append(args, userID, ts, model, quotaEach)
		}
		if _, err := db.Exec(sb.String(), args...); err != nil {
			t.Fatalf("批量造日志失败: %v", err)
		}
	}
}

// chatCall 用代发 key 真打一次 new-api /v1/chat/completions(控量),返回状态码 + body。
// T17-2 真调用验收:断言不被"无权访问分组"403。集成栈渠道是 mock(上游打不通),
// 故连接错/超时/5xx 均视为"过了鉴权分组闸"(返回 st=0/5xx),只有分组 403 才是回归。
func chatCall(t *testing.T, base, apiKey, model string) (int, string) {
	body, _ := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": "ping"}},
		"max_tokens": 4,
	})
	req, _ := http.NewRequest("POST", base+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	cli := &http.Client{Timeout: 30 * time.Second}
	resp, err := cli.Do(req)
	if err != nil {
		return 0, "transport_err:" + err.Error() // 连不通上游=已过鉴权分组闸,非 403
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// tokenGroupBySQL 直查 newapi.tokens 该用户最新令牌的分组(验证 T17-1 令牌分组真落库)。
func tokenGroupBySQL(t *testing.T, dsn string, userID int64) string {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("连 newapi 库失败: %v", err)
	}
	defer db.Close()
	var g string
	if err := db.QueryRow("SELECT `group` FROM tokens WHERE user_id = ? ORDER BY id DESC LIMIT 1", userID).Scan(&g); err != nil {
		t.Fatalf("查令牌分组失败: %v", err)
	}
	return g
}

func randSuffix() string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b) // hex:仅 0-9a-f,恒为合法 slug(slug 格式校验上线后不会误伤,T4)
}

func mask(s string) string {
	if len(s) <= 6 {
		return "***"
	}
	return s[:6] + "..."
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
