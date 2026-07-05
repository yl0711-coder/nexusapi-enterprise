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

// TestClientIP 优先 CF-Connecting-IP,再 X-Forwarded-For 首段。
func TestClientIP(t *testing.T) {
	r := httptest.NewRequest("POST", "/x", nil)
	r.RemoteAddr = "10.0.0.1:5000"
	if got := clientIP(r); got != "10.0.0.1" {
		t.Fatalf("无代理头应取 RemoteAddr host,实 %q", got)
	}
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 10.0.0.1")
	if got := clientIP(r); got != "1.2.3.4" {
		t.Fatalf("应取 XFF 首段,实 %q", got)
	}
	r.Header.Set("CF-Connecting-IP", "9.9.9.9")
	if got := clientIP(r); got != "9.9.9.9" {
		t.Fatalf("应优先 CF-Connecting-IP,实 %q", got)
	}
}
