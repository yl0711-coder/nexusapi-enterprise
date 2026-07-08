package handler

import (
	"net/http/httptest"
	"testing"
	"time"
)

// TestAttemptLimiter 锁死 A2 退避:达阈值锁定、锁定期过后解锁、成功 reset 清零、指数退避加长。
func TestAttemptLimiter(t *testing.T) {
	now := time.Now()
	a := newAttemptLimiter()
	a.nowFn = func() time.Time { return now }
	const k = "acct:victim"

	for i := 0; i < a.threshold-1; i++ {
		a.fail(k)
	}
	if a.retryAfter(k) > 0 {
		t.Fatalf("阈值(%d)前不应锁定", a.threshold)
	}
	a.fail(k) // 第 threshold 次 → 锁定
	first := a.retryAfter(k)
	if first <= 0 {
		t.Fatalf("达阈值应锁定")
	}
	// 锁定期过后解锁。
	now = now.Add(first + time.Second)
	if a.retryAfter(k) > 0 {
		t.Fatalf("锁定期过后应解锁")
	}
	// 再失败一次 → 指数退避,锁定更久。
	a.fail(k)
	if second := a.retryAfter(k); second <= first {
		t.Fatalf("指数退避:第二次锁定(%v)应比第一次(%v)长", second, first)
	}
	// 成功 reset 清零。
	a.reset(k)
	if a.retryAfter(k) > 0 {
		t.Fatalf("reset 后应清零")
	}
}

// TestClientIP(39号 P2-3):代理头仅在可信链路证明(X-Origin-Verify 匹配)下才被采信;
// 未配置 secret / 头不匹配(伪造)一律回落 RemoteAddr,防伪造头削弱 per-IP 限流。
func TestClientIP(t *testing.T) {
	r := httptest.NewRequest("POST", "/x", nil)
	r.RemoteAddr = "10.0.0.1:5000"
	if got := clientIP(r); got != "10.0.0.1" {
		t.Fatalf("无代理头应取 RemoteAddr host,实 %q", got)
	}
	// 未配置 secret:带代理头也不信(伪造场景),仍 RemoteAddr。
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 10.0.0.1")
	r.Header.Set("CF-Connecting-IP", "9.9.9.9")
	if got := clientIP(r); got != "10.0.0.1" {
		t.Fatalf("未配置 secret 时代理头不可信,应回落 RemoteAddr,实 %q", got)
	}
	// 配置 secret + 头匹配:采信 CF-Connecting-IP 优先。
	originVerifySecret = "test-origin-secret"
	defer func() { originVerifySecret = "" }()
	r.Header.Set("X-Origin-Verify", "test-origin-secret")
	if got := clientIP(r); got != "9.9.9.9" {
		t.Fatalf("可信链路应优先 CF-Connecting-IP,实 %q", got)
	}
	r.Header.Del("CF-Connecting-IP")
	if got := clientIP(r); got != "1.2.3.4" {
		t.Fatalf("可信链路无 CF 头应取 XFF 首段,实 %q", got)
	}
	// secret 不匹配(伪造 X-Origin-Verify):回落 RemoteAddr。
	r.Header.Set("X-Origin-Verify", "wrong")
	if got := clientIP(r); got != "10.0.0.1" {
		t.Fatalf("X-Origin-Verify 不匹配应回落 RemoteAddr,实 %q", got)
	}
}
