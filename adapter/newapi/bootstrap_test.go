package newapi

import (
	"context"
	"strings"
	"sync"
	"testing"
)

func testInput() BootstrapInput {
	return BootstrapInput{
		OrgID:       1,
		MemberID:    42,
		Username:    "org1_m42", // 确定性派生(§2.5)
		Password:    "pw-deterministic-42",
		DisplayName: "钱晨",
	}
}

// testInputAdopt v1.1 项B:模拟"重开/重试本方已拥有的用户名"(AllowAdopt=true)——撞"已存在"应 adopt 复用,
// 而非当外部用户报冲突。首次 provision(AllowAdopt=false)的撞名归属闸另由 orgusername 集成测试覆盖。
func testInputAdopt() BootstrapInput {
	in := testInput()
	in.AllowAdopt = true
	return in
}

func TestBootstrapMember_HappyPath(t *testing.T) {
	f := newFakeNewapi()
	defer f.close()
	a := New(f.config(), nil)

	res, err := a.BootstrapMember(context.Background(), testInput())
	if err != nil {
		t.Fatalf("bootstrap failed: %v", err)
	}
	if res.NewapiUserID == 0 || res.TokenID == 0 {
		t.Fatalf("expected user/token ids, got %+v", res)
	}
	if res.AdoptedExisting {
		t.Errorf("first bootstrap should create, not adopt")
	}
	// 明文 key 必须返回一次,且不是脱敏串(05 §5.2:列表脱敏,明文须经 GetTokenKey)。
	if res.PlaintextKey == "" || strings.Contains(res.PlaintextKey, "••••") {
		t.Errorf("expected plaintext key, got %q", res.PlaintextKey)
	}
	if res.AccessToken == "" {
		t.Errorf("expected access_token for storage")
	}
	// 全链路各调一次。
	for _, step := range []string{stepCreateUser, stepLogin, stepGetToken, stepCreateToken, stepRevealKey} {
		if f.callCount(step) != 1 {
			t.Errorf("step %s called %d times, want 1", step, f.callCount(step))
		}
	}
}

// rc.4 约束:username<=20、password 8-20。超限应在打上游前就被挡(清晰错误,非 50201)。
func TestBootstrapMember_RejectsInvalidCredLength(t *testing.T) {
	f := newFakeNewapi()
	defer f.close()
	a := New(f.config(), nil)

	long := testInput()
	long.Username = "org1_member_with_way_too_long_name" // >20
	if _, err := a.BootstrapMember(context.Background(), long); err == nil {
		t.Errorf("over-long username should be rejected")
	}
	if f.callCount(stepCreateUser) != 0 {
		t.Errorf("invalid creds must not hit upstream, got %d CreateUser calls", f.callCount(stepCreateUser))
	}

	shortPw := testInput()
	shortPw.Password = "short" // <8
	if _, err := a.BootstrapMember(context.Background(), shortPw); err == nil {
		t.Errorf("too-short password should be rejected")
	}
}

// §2.5:确定性 username 幂等 —— 重复开通同一成员,接管已存在用户/令牌,绝不重复建。
func TestBootstrapMember_IdempotentAdopt(t *testing.T) {
	f := newFakeNewapi()
	defer f.close()
	a := New(f.config(), nil)

	first, err := a.BootstrapMember(context.Background(), testInputAdopt())
	if err != nil {
		t.Fatalf("first bootstrap: %v", err)
	}
	second, err := a.BootstrapMember(context.Background(), testInputAdopt())
	if err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}
	if !second.AdoptedExisting {
		t.Errorf("second bootstrap should adopt existing user")
	}
	if second.NewapiUserID != first.NewapiUserID {
		t.Errorf("adopt must reuse same user_id: first=%d second=%d", first.NewapiUserID, second.NewapiUserID)
	}
	if second.TokenID != first.TokenID {
		t.Errorf("adopt must reuse same token (deterministic name): first=%d second=%d", first.TokenID, second.TokenID)
	}
	// 只建过一次用户、一次 token(其余都是接管)。
	if got := f.callCount(stepCreateToken); got != 1 {
		t.Errorf("CreateToken POST happened %d times, want 1 (rest adopted)", got)
	}
}

// §2.6:同一成员并发 bootstrap 必须在锁内串行化,否则两路 GET token 互相旋转作废,
// 会导致某次 CreateToken 用到失效 access_token 而失败。加锁后应全部成功且幂等收敛。
func TestBootstrapMember_ConcurrentSameMemberSerialized(t *testing.T) {
	f := newFakeNewapi()
	defer f.close()
	a := New(f.config(), nil)

	const N = 6
	var wg sync.WaitGroup
	results := make([]BootstrapResult, N)
	errs := make([]error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = a.BootstrapMember(context.Background(), testInputAdopt())
		}(i)
	}
	wg.Wait()

	for i := 0; i < N; i++ {
		if errs[i] != nil {
			t.Fatalf("concurrent bootstrap %d failed (lock should prevent mutual rotation): %v", i, errs[i])
		}
		if results[i].PlaintextKey == "" {
			t.Errorf("concurrent bootstrap %d returned empty key", i)
		}
		if results[i].NewapiUserID != results[0].NewapiUserID || results[i].TokenID != results[0].TokenID {
			t.Errorf("concurrent results diverged: %+v vs %+v", results[i], results[0])
		}
	}
	// 幂等:只真正建过一个用户、一个 token。
	if len(f.users) != 1 {
		t.Errorf("expected exactly 1 user created, got %d", len(f.users))
	}
	if got := f.callCount(stepCreateToken); got != 1 {
		t.Errorf("CreateToken POST happened %d times, want 1", got)
	}
}

// §2.2 ①:CreateUser 5xx → client 退避重试(可重试),最终成功,不留孤儿。
func TestBootstrapMember_CreateUser5xxThenRetrySucceeds(t *testing.T) {
	f := newFakeNewapi()
	defer f.close()
	f.faultCreateUser5xx = 2 // 前两次 500,第三次成功(MaxRetries=2 → 共 3 次)
	a := New(f.config(), nil)

	res, err := a.BootstrapMember(context.Background(), testInput())
	if err != nil {
		t.Fatalf("expected eventual success after retries, got %v", err)
	}
	if res.PlaintextKey == "" {
		t.Errorf("expected key after retry success")
	}
	if f.callCount(stepCreateUser) != 3 {
		t.Errorf("CreateUser attempted %d times, want 3 (2 fail + 1 ok)", f.callCount(stepCreateUser))
	}
}

// §2.2 ③ + §2.3:GET token 返回空 = 不可重试判失败,且因用户已建 → 补偿 disable。
func TestBootstrapMember_EmptyTokenFailsAndCompensates(t *testing.T) {
	f := newFakeNewapi()
	defer f.close()
	f.faultGetTokenEmpty = true
	a := New(f.config(), nil)

	_, err := a.BootstrapMember(context.Background(), testInput())
	if err == nil {
		t.Fatalf("expected failure on empty access_token")
	}
	ue := asUpstreamError("", err)
	if ue.PlatformCode != CodeUpstreamAuth {
		t.Errorf("want CodeUpstreamAuth(%d), got %d", CodeUpstreamAuth, ue.PlatformCode)
	}
	// §2.2 step③:绝不再调 GET token 碰运气(只调一次)。
	if got := f.callCount(stepGetToken); got != 1 {
		t.Errorf("GET token called %d times, want exactly 1 (no blind re-call)", got)
	}
	// §2.3:用户已建 → 补偿 disable(留可恢复禁用态,不删)。
	u := f.users[testInput().Username]
	if u == nil {
		t.Fatalf("user should still exist (not deleted)")
	}
	if u.enabled {
		t.Errorf("user should be disabled by compensation")
	}
}

// §2.2 ⑤:取明文 key 失败 → token 已建不回滚,返回带 token_id 的结果 + 错误供补取。
func TestBootstrapMember_RevealKeyFailKeepsToken(t *testing.T) {
	f := newFakeNewapi()
	defer f.close()
	f.faultRevealKey5xx = 5 // 超过重试次数,持续失败
	a := New(f.config(), nil)

	res, err := a.BootstrapMember(context.Background(), testInput())
	if err == nil {
		t.Fatalf("expected reveal-key failure")
	}
	if res.TokenID == 0 {
		t.Errorf("should return token_id for later key re-fetch, got %+v", res)
	}
	// 用户不该被 disable(§2.3:建 token/取 key 后不回滚用户)。
	if u := f.users[testInput().Username]; u != nil && !u.enabled {
		t.Errorf("user should NOT be disabled when only reveal-key failed")
	}
}

// §2.6/§2.7:GET token 旋转使旧 access_token 失效;ProbeAccessToken 能探测,
// RefreshAccessToken 用留存密码重取并恢复可用。
func TestRotationInvalidatesOldAndSelfHeal(t *testing.T) {
	f := newFakeNewapi()
	defer f.close()
	a := New(f.config(), nil)

	res, err := a.BootstrapMember(context.Background(), testInput())
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	oldCred := MemberCred{NewapiUserID: res.NewapiUserID, AccessToken: res.AccessToken}

	// 旧凭证此刻有效。
	if valid, err := a.ProbeAccessToken(context.Background(), oldCred); err != nil || !valid {
		t.Fatalf("old cred should be valid right after bootstrap: valid=%v err=%v", valid, err)
	}

	// 模拟"凭证丢失/误旋转":自愈重取 → 旧的立即失效。
	newAccess, err := a.RefreshAccessToken(context.Background(), testInput())
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if newAccess == res.AccessToken {
		t.Errorf("refreshed access_token should differ (rotation)")
	}
	if valid, err := a.ProbeAccessToken(context.Background(), oldCred); err != nil {
		t.Fatalf("probe old: %v", err)
	} else if valid {
		t.Errorf("old access_token should be invalid after rotation")
	}
	// 新凭证可用。
	newCred := MemberCred{NewapiUserID: res.NewapiUserID, AccessToken: newAccess}
	if valid, err := a.ProbeAccessToken(context.Background(), newCred); err != nil || !valid {
		t.Errorf("new cred should be valid: valid=%v err=%v", valid, err)
	}
}

// override 写绝对值(到 0 即硬停语义),天然幂等。
func TestManageUserQuota_Override(t *testing.T) {
	f := newFakeNewapi()
	defer f.close()
	a := New(f.config(), nil)
	res, err := a.BootstrapMember(context.Background(), testInput())
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if err := a.ManageUserQuota(context.Background(), res.NewapiUserID, QuotaOverride, 22500000); err != nil {
		t.Fatalf("override: %v", err)
	}
	if got := f.byID[res.NewapiUserID].quota; got != 22500000 {
		t.Errorf("quota override = %d, want 22500000", got)
	}
	// 幂等:再 override 同值不变。
	_ = a.ManageUserQuota(context.Background(), res.NewapiUserID, QuotaOverride, 22500000)
	if got := f.byID[res.NewapiUserID].quota; got != 22500000 {
		t.Errorf("override should be idempotent, got %d", got)
	}
	// 到 0 = 硬停。
	_ = a.ManageUserQuota(context.Background(), res.NewapiUserID, QuotaOverride, 0)
	if got := f.byID[res.NewapiUserID].quota; got != 0 {
		t.Errorf("override 0 = hard stop, got %d", got)
	}
}

// DeleteToken 对不存在的 token 幂等成功(目标态幂等,10 §4.5)。
func TestDeleteToken_Idempotent(t *testing.T) {
	f := newFakeNewapi()
	defer f.close()
	a := New(f.config(), nil)
	res, err := a.BootstrapMember(context.Background(), testInput())
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	cred := MemberCred{NewapiUserID: res.NewapiUserID, AccessToken: res.AccessToken}
	if err := a.DeleteToken(context.Background(), cred, res.TokenID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// 再删一次(已不存在)→ 仍成功。
	if err := a.DeleteToken(context.Background(), cred, res.TokenID); err != nil {
		t.Errorf("delete of missing token should be idempotent, got %v", err)
	}
}
