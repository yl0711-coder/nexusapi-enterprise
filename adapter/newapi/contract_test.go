package newapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"strconv"
	"testing"
	"time"
)

// TestContract_AgainstRealRC4 是对**真实 rc.4 实例**的契约测试(05 §5.6 链路)。
// 这是里程碑 0 "消除最大不确定性"的关键一闸:验证 adapter 的线格式 / 鉴权 / 行为
// 与真实 new-api 完全一致。未设环境变量时自动 skip(无 Docker 也能全绿)。
//
// 跑法(见 README):
//
//	docker run -d -p 13000:3000 calciumion/new-api:v1.0.0-rc.4   # 首跑 POST /api/setup 建 root
//	NEWAPI_CONTRACT_BASE_URL=http://localhost:13000 \
//	NEWAPI_CONTRACT_ADMIN_TOKEN=<root_access_token> \
//	NEWAPI_CONTRACT_ADMIN_USER_ID=1 \
//	  go test ./adapter/newapi/ -run Contract -v
//
// 【真机首验重点(代码里已标注的契约假设,必须在真机确认)】
//  1. ManageUserQuota 线格式:POST /api/user/manage {action:"add_quota", mode, quota}
//     —— 若真机 rc.4 的额度调整实际走 UpdateUser(PUT /api/user/),据此改 adapter。
//  2. GET /api/user/token 返回结构(裸字符串 vs {access_token}/{key})。
//  3. /api/user/search 返回结构(数组 vs 分页包装)。
//  4. POST /api/token/:id/key 返回结构(裸字符串 vs {key})。
//  5. CreateUser 重名错误文案(isAlreadyExists 关键词命中)。
func TestContract_AgainstRealRC4(t *testing.T) {
	base := os.Getenv("NEWAPI_CONTRACT_BASE_URL")
	if base == "" {
		t.Skip("跳过真机契约测试:未设 NEWAPI_CONTRACT_BASE_URL(需先 docker run rc.4,见 README)")
	}
	adminToken := os.Getenv("NEWAPI_CONTRACT_ADMIN_TOKEN")
	adminUserID, _ := strconv.Atoi(os.Getenv("NEWAPI_CONTRACT_ADMIN_USER_ID"))
	// 容器化跑法:对全新 rc.4 自动 setup root 并取管理员 access_token,免手填 token。
	if adminToken == "" && os.Getenv("NEWAPI_CONTRACT_AUTO_SETUP") == "1" {
		tok, uid, err := autoSetupRC4(base)
		if err != nil {
			t.Fatalf("rc.4 自动 setup 失败: %v", err)
		}
		adminToken, adminUserID = tok, uid
		t.Logf("自动 setup 完成:admin user_id=%d", uid)
	}
	if adminToken == "" || adminUserID == 0 {
		t.Fatal("需提供 NEWAPI_CONTRACT_ADMIN_TOKEN + NEWAPI_CONTRACT_ADMIN_USER_ID,或设 NEWAPI_CONTRACT_AUTO_SETUP=1")
	}

	a := New(Config{
		BaseURL:     base,
		AdminToken:  adminToken,
		AdminUserID: adminUserID,
		Timeout:     15 * time.Second,
	}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// 唯一成员(避免与历史残留冲突);用纳秒戳派生确定性 username。
	// rc.4 约束:username<=20、password 8-20。用短唯一后缀派生,避免超长。
	uniq := time.Now().UnixNano() % 1_000_000
	in := BootstrapInput{
		OrgID:       9001,
		MemberID:    uniq,
		Username:    fmt.Sprintf("ct_m%06d", uniq),   // <=10 字符
		Password:    fmt.Sprintf("CtPw%06d!!", uniq), // 12 字符,含字母/数字/特殊
		DisplayName: "契约测试成员",
	}

	// 1) 全链路 bootstrap。
	res, err := a.BootstrapMember(ctx, in)
	if err != nil {
		t.Fatalf("BootstrapMember 真机失败: %v", err)
	}
	t.Logf("bootstrap ok: user_id=%d token_id=%d", res.NewapiUserID, res.TokenID)
	if res.PlaintextKey == "" || res.AccessToken == "" {
		t.Fatalf("缺少明文 key 或 access_token: %+v", res)
	}
	cred := MemberCred{NewapiUserID: res.NewapiUserID, AccessToken: res.AccessToken}

	// 清理:结束后禁用该用户(不删,保留 logs 关联;符合 §2.3)。
	defer func() {
		if err := a.SetUserStatus(context.Background(), res.NewapiUserID, false); err != nil {
			t.Logf("清理 disable 失败(需手动处理 user_id=%d): %v", res.NewapiUserID, err)
		}
	}()

	// 2) 刚 bootstrap 的凭证应有效(在任何重开/旋转之前探测)。
	if valid, err := a.ProbeAccessToken(ctx, cred); err != nil || !valid {
		t.Fatalf("ProbeAccessToken(新凭证): valid=%v err=%v", valid, err)
	}

	// 3) 调额 override 绝对值(管理员身份,不动 token)。
	if err := a.ManageUserQuota(ctx, res.NewapiUserID, QuotaOverride, 1_000_000); err != nil {
		t.Fatalf("ManageUserQuota override 失败: %v", err)
	}

	// 4) 幂等重开 → 接管同一用户。注意:重开会重新 login+GET token → **旋转 access_token**,
	//    旧 cred 随即失效(这是 rc.4 既定行为,§2.6)。生产中 service 凭 bootstrap_state=done
	//    短路、不会重跑;这里重开后必须改用 again 返回的新 token。
	again, err := a.BootstrapMember(ctx, in)
	if err != nil {
		t.Fatalf("幂等重开失败: %v", err)
	}
	if !again.AdoptedExisting || again.NewapiUserID != res.NewapiUserID {
		t.Errorf("幂等重开应接管同一用户: %+v", again)
	}
	// 旧 cred 应已因旋转失效;新 cred 有效。
	if valid, _ := a.ProbeAccessToken(ctx, cred); valid {
		t.Errorf("重开旋转后旧 access_token 应失效")
	}
	cred = MemberCred{NewapiUserID: again.NewapiUserID, AccessToken: again.AccessToken}
	if valid, err := a.ProbeAccessToken(ctx, cred); err != nil || !valid {
		t.Fatalf("重开后新凭证应有效: valid=%v err=%v", valid, err)
	}

	// 5) 轮换令牌(删旧建新)拿到新 key。
	newSpec := TokenSpec{Name: deriveTokenName(in.MemberID, 2), UnlimitedQuota: true, ExpiredTime: -1}
	newID, newKey, err := a.RotateToken(ctx, cred, res.TokenID, newSpec)
	if err != nil {
		t.Fatalf("RotateToken 失败: %v", err)
	}
	if newKey == "" || newID == res.TokenID {
		t.Errorf("轮换应得新 token + 新 key: id=%d key非空=%v", newID, newKey != "")
	}

	// 6) 删除新令牌(幂等)。
	if err := a.DeleteToken(ctx, cred, newID); err != nil {
		t.Errorf("DeleteToken 失败: %v", err)
	}
}

// autoSetupRC4 对全新 rc.4 实例完成初始化并取得管理员 access_token + user_id。
// new-api 首次启动通常自动创建 root/123456;若需显式 setup 则补一次 POST /api/setup。
// 返回的 token 用作 AdminAuth 的 Bearer,user_id 作 New-Api-User 头。
func autoSetupRC4(base string) (string, int, error) {
	jar, _ := cookiejar.New(nil)
	hc := &http.Client{Timeout: 15 * time.Second, Jar: jar}

	login := func(user, pass string) (int, bool) {
		body, _ := json.Marshal(map[string]string{"username": user, "password": pass})
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

	// rc.4 实证契约:setup 字段 confirmPassword(驼峰),密码 >=8 字符。
	const rootPass = "RootPass123"
	uid, ok := login("root", rootPass)
	if !ok {
		body, _ := json.Marshal(map[string]string{"username": "root", "password": rootPass, "confirmPassword": rootPass})
		resp, err := hc.Post(base+"/api/setup", "application/json", bytes.NewReader(body))
		if err != nil {
			return "", 0, fmt.Errorf("setup 请求失败: %w", err)
		}
		resp.Body.Close()
		uid, ok = login("root", rootPass)
		if !ok {
			return "", 0, fmt.Errorf("setup 后仍无法以 root 登录(检查 rc.4 初始化契约)")
		}
	}
	if uid == 0 {
		uid = 1 // root 通常为 id=1
	}

	// 取管理员 access_token:cookie + New-Api-User 头(双头,rc.4 强制)。
	req, _ := http.NewRequest("GET", base+"/api/user/token", nil)
	req.Header.Set("New-Api-User", strconv.Itoa(uid))
	resp, err := hc.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("取 access_token 失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var env struct {
		Success bool            `json:"success"`
		Data    json.RawMessage `json:"data"`
	}
	_ = json.Unmarshal(raw, &env)
	token := parseAccessToken(env.Data)
	if token == "" {
		return "", 0, fmt.Errorf("未取到管理员 access_token,原始返回: %s", string(raw))
	}
	if uid == 0 {
		uid = 1 // root 通常为 id=1
	}
	return token, uid, nil
}
