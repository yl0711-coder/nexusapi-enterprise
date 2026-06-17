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
	"encoding/base64"
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

	ts := httptest.NewServer(handler.New(svc, signer, log, "it").Routes())
	defer ts.Close()
	api := &apiClient{t: t, base: ts.URL}

	// 4) 运营方登录。
	opTok := login(api, opEmail, opPassword)

	// 5) 运营方建客户组织(连带建组织管理员)。slug 唯一(带随机后缀防重跑撞)。
	slug := "acme-" + randSuffix()
	var orgResp struct {
		Org                  struct{ ID int64 `json:"id"` } `json:"org"`
		AdminEmail           string                         `json:"admin_email"`
		AdminInitialPassword string                         `json:"admin_initial_password"`
	}
	st := api.do("POST", "/api/v1/organizations", opTok, map[string]any{
		"name": "Acme 公司", "slug": slug, "admin_email": "admin@" + slug + ".com",
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
	var teamResp struct{ ID int64 `json:"id"` }
	if st := api.do("POST", fmt.Sprintf("/api/v1/organizations/%d/teams", orgID), adminTok,
		map[string]any{"name": "研发一组"}, &teamResp); st != http.StatusCreated {
		t.Fatalf("建团队 HTTP=%d", st)
	}
	var tierResp struct{ ID int64 `json:"id"` }
	if st := api.do("POST", fmt.Sprintf("/api/v1/organizations/%d/tiers", orgID), adminTok,
		map[string]any{"name": "标准档", "model_set": []string{"gpt-5.4", "claude-sonnet-4-6"}}, &tierResp); st != http.StatusCreated {
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
		Pagination struct{ Total int `json:"total"` } `json:"pagination"`
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
		Org                  struct{ ID int64 `json:"id"` } `json:"org"`
		AdminEmail           string                         `json:"admin_email"`
		AdminInitialPassword string                         `json:"admin_initial_password"`
	}
	api.do("POST", "/api/v1/organizations", opTok, map[string]any{
		"name": "Beta 公司", "slug": slug2, "admin_email": "admin@" + slug2 + ".com",
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

	// ===== 里程碑 3c:计价/折扣联动(总折扣单向写入 new-api GroupRatio + 只读回显)=====
	discGroup := "org" + strconv.FormatInt(orgID, 10)
	if st := api.do("PUT", fmt.Sprintf("/api/v1/organizations/%d/pricing", orgID), opTok,
		map[string]any{"mode": "total", "newapi_group": discGroup, "group_ratio": 0.8}, nil); st != http.StatusOK {
		t.Fatalf("配置折扣 HTTP=%d", st)
	}
	var pv struct {
		Mode               string   `json:"mode"`
		GroupRatioUpstream *float64 `json:"group_ratio_upstream"`
	}
	api.do("GET", fmt.Sprintf("/api/v1/organizations/%d/pricing", orgID), adminTok, nil, &pv)
	if pv.Mode != "total" || pv.GroupRatioUpstream == nil || *pv.GroupRatioUpstream != 0.8 {
		t.Errorf("折扣回显应为 total + new-api 分组倍率 0.8: %+v", pv)
	} else {
		t.Log("3c 折扣联动 ok: 总折扣单向写入 new-api GroupRatio=0.8 且只读回显一致")
	}
	if st := api.do("PUT", fmt.Sprintf("/api/v1/organizations/%d/pricing", orgID), adminTok,
		map[string]any{"mode": "total", "group_ratio": 0.5}, nil); st != http.StatusForbidden {
		t.Errorf("组织管理员配折扣应 403(客户只读),得 %d", st)
	} else {
		t.Log("3c 折扣 RBAC ok: 组织管理员配置 → 403(客户只读),仅运营方可配")
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
	if st := api.do("POST", "/api/v1/approvals", memberTok,
		map[string]any{"amount_quota": 200000000, "duration": "today", "reason": "大项目"}, &big); st != http.StatusCreated {
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
	if qf, _ := getNewapiUser(t, newapiURL, adminToken, adminUID, uid); qf != qBeforeFinal+200000000 {
		t.Errorf("二审通过下发后 quota 应 +2e8: %d→%d", qBeforeFinal, qf)
	} else {
		t.Logf("US-06 二审 ok: pending→l1_approved→approved 且下发,quota %d→%d", qBeforeFinal, qf)
	}
	t.Log("里程碑 4 e2e 全通过:自动通过即时下发 + 二审两级流程 + 成员裁决403 + 站内通知")

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

	// 再结算一次 → 去重不重复扣。
	if d2, _ := svc.RunSettlement(ctx); d2 != 0 || readBal() != balAfter {
		t.Errorf("重复结算应去重不再扣: d2=%d bal=%d", d2, readBal())
	} else {
		t.Log("3b 去重 ok: 同一桶重复结算不再扣")
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

	t.Log("里程碑 3b e2e 全通过:读logs扣费 + 去重防重复扣 + 守恒 + 硬停(逐组织开关)+ 充值解硬停恢复")
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
			Data    struct{ ID int `json:"id"` } `json:"data"`
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

// getNewapiUser 以管理员身份查 new-api 用户的 quota 与 status(1=enabled,2=disabled)。
func getNewapiUser(t *testing.T, base, adminToken string, adminUID int, userID int64) (int64, int) {
	req, _ := http.NewRequest("GET", fmt.Sprintf("%s/api/user/%d", base, userID), nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("New-Api-User", strconv.Itoa(adminUID))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("查 new-api 用户失败: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
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

func randSuffix() string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return strings.ToLower(base64.RawURLEncoding.EncodeToString(b))
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
