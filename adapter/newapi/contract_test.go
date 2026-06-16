package newapi

import (
	"context"
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
	if adminToken == "" || adminUserID == 0 {
		t.Fatal("需提供 NEWAPI_CONTRACT_ADMIN_TOKEN 与 NEWAPI_CONTRACT_ADMIN_USER_ID")
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
	uniq := time.Now().UnixNano()
	in := BootstrapInput{
		OrgID:       9001,
		MemberID:    uniq % 1_000_000,
		Username:    "ct_org9001_m" + strconv.FormatInt(uniq, 36),
		Password:    "Ct!" + strconv.FormatInt(uniq, 36) + "x9",
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

	// 2) 幂等重开 → 接管。
	if again, err := a.BootstrapMember(ctx, in); err != nil {
		t.Fatalf("幂等重开失败: %v", err)
	} else if !again.AdoptedExisting || again.NewapiUserID != res.NewapiUserID {
		t.Errorf("幂等重开应接管同一用户: %+v", again)
	}

	// 3) 调额 override 绝对值。
	if err := a.ManageUserQuota(ctx, res.NewapiUserID, QuotaOverride, 1_000_000); err != nil {
		t.Fatalf("ManageUserQuota override 失败(检查真机契约假设 #1): %v", err)
	}

	// 4) ProbeAccessToken 应有效。
	if valid, err := a.ProbeAccessToken(ctx, cred); err != nil || !valid {
		t.Fatalf("ProbeAccessToken: valid=%v err=%v", valid, err)
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
