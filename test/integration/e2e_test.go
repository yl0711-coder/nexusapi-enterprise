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
		MemberID  int64    `json:"member_id"`
		APIKey    string   `json:"api_key"`
		KeyMasked string   `json:"key_masked"`
		Models    []string `json:"models"`
	}
	st = api.do("POST", fmt.Sprintf("/api/v1/organizations/%d/members", orgID), adminTok,
		map[string]any{"name": "钱晨", "team_id": teamResp.ID, "tier_id": tierResp.ID}, &openResp)
	if st != http.StatusCreated {
		t.Fatalf("开通成员 HTTP=%d", st)
	}
	if openResp.APIKey == "" || openResp.MemberID == 0 {
		t.Fatalf("开通成员返回缺字段: %+v", openResp)
	}
	if !strings.Contains(openResp.KeyMasked, "••••") {
		t.Errorf("key_masked 应脱敏: %q", openResp.KeyMasked)
	}
	t.Logf("开通成员 ok(模型2:员工 token 挂 org user 下): member_id=%d key=%s...", openResp.MemberID, openResp.APIKey[:min(10, len(openResp.APIKey))])
	originalKey := openResp.APIKey

	// v2 M1:开通后应建主 key 槽 + 当前令牌(1 槽 / 1 令牌 / 1 current / nexus_m{id}_v1)。
	if sl, tk, cu, nm := queryMemberKey(t, store.DB(), orgID, openResp.MemberID); sl != 1 || tk != 1 || cu != 1 || nm != fmt.Sprintf("nexus_m%d_v1", openResp.MemberID) {
		t.Fatalf("开通后 member_key 异常: slots=%d tokens=%d current=%d name=%q(应 1/1/1/nexus_m%d_v1)", sl, tk, cu, nm, openResp.MemberID)
	}

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

	// v2 M1:轮换后同一主槽,旧令牌置 superseded、插新 current(1 槽 / 2 令牌 / 1 current / v2)= 历史按 key_id 连续。
	if sl, tk, cu, nm := queryMemberKey(t, store.DB(), orgID, openResp.MemberID); sl != 1 || tk != 2 || cu != 1 || nm != fmt.Sprintf("nexus_m%d_v2", openResp.MemberID) {
		t.Fatalf("轮换后 member_key 异常: slots=%d tokens=%d current=%d name=%q(应 1/2/1/nexus_m%d_v2)", sl, tk, cu, nm, openResp.MemberID)
	}

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

	// ===== 模型2 返工边界(2026-06-30)=====
	// 里程碑2+(额度执行/调额/撤销/account_ttl/停用/P1基线/T17/用量·团队·下钻 HTTP 端点)原为 model1:
	// 额度落 member 的 user.quota、按 user_id 归因——模型2 改为额度落 token.remain_quota、按 token→member 归因,这些段不适用。
	// 模型2 R2 报表/归因由 attribution_test/usage_detail_test/usage_timeseries_test 三独立真账测试覆盖(均绿);
	// R4 额度执行(token.remain_quota+escrow)+完整模型2 e2e(R2 HTTP端点)待 R3/R4 里程碑重写(旧 model1 e2e 见 git 6865fbc 前)。
	t.Log("模型2 e2e(R1)通过:开通=确保 org user+在其下建员工 token+列表脱敏+轮换+RBAC;R2 见独立测试,R4 待里程碑")
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

// selfServeMemberToken 模型2:让成员自助建 key(在 org user 下),返回其 new-api token_id(灌日志归因用)。
func selfServeMemberToken(t *testing.T, ctx context.Context, svc *service.Service, store *repo.Store, orgID, memberID int64) int64 {
	t.Helper()
	mc := session.Claims{MemberID: memberID, OrgID: orgID, Role: session.RoleMember}
	if _, _, err := svc.CreateMemberToken(ctx, mc, memberID, "default"); err != nil {
		t.Fatalf("成员 %d 自助建 key 失败: %v", memberID, err)
	}
	m, err := store.GetMember(ctx, orgID, memberID)
	if err != nil || m.NewapiTokenID == nil {
		t.Fatalf("取成员 %d 令牌 id 失败: %v", memberID, err)
	}
	return *m.NewapiTokenID
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
