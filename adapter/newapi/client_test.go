package newapi

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestCircuitBreaker_OpensCooldownHalfOpenRecover(t *testing.T) {
	now := time.Unix(0, 0)
	cb := newCircuitBreaker(3, 10*time.Second)
	cb.nowFn = func() time.Time { return now }

	// 连续 3 次失败 → 打开。
	for i := 0; i < 3; i++ {
		if !cb.allow() {
			t.Fatalf("should allow before threshold (i=%d)", i)
		}
		cb.onFailure()
	}
	if cb.allow() {
		t.Fatalf("circuit should be OPEN after 3 failures")
	}

	// 冷却未到 → 仍拒。
	now = now.Add(9 * time.Second)
	if cb.allow() {
		t.Fatalf("still within cooldown, should reject")
	}
	// 冷却到点 → 半开放一个探测,后续请求仍挡。
	now = now.Add(2 * time.Second)
	if !cb.allow() {
		t.Fatalf("after cooldown should allow one probe")
	}
	if cb.allow() {
		t.Fatalf("half-open should block additional requests until probe resolves")
	}
	// 探测失败 → 回到打开。
	cb.onFailure()
	if cb.allow() {
		t.Fatalf("failed probe should reopen circuit")
	}
	// 再次冷却 + 探测成功 → 关闭恢复。
	now = now.Add(11 * time.Second)
	if !cb.allow() {
		t.Fatalf("should allow probe after second cooldown")
	}
	cb.onSuccess()
	if !cb.allow() {
		t.Fatalf("circuit should be CLOSED after successful probe")
	}
}

func TestTokenBucket_RateLimits(t *testing.T) {
	now := time.Unix(0, 0)
	tb := newTokenBucket(10, 2) // 10 QPS,桶容量 2
	tb.nowFn = func() time.Time { return now }
	tb.last = now // 对齐注入时钟(构造时 last 取了真实 time.Now)

	// 先消耗满桶 2 个令牌:立即可得。
	if d := tb.reserve(); d != 0 {
		t.Fatalf("token 1 should be immediate, got %v", d)
	}
	if d := tb.reserve(); d != 0 {
		t.Fatalf("token 2 should be immediate, got %v", d)
	}
	// 桶空:需等约 0.1s(1/10)。
	d := tb.reserve()
	if d <= 0 || d > 200*time.Millisecond {
		t.Fatalf("token 3 should wait ~100ms, got %v", d)
	}
	// 推进时钟补充令牌后立即可得。
	now = now.Add(150 * time.Millisecond)
	if d := tb.reserve(); d != 0 {
		t.Fatalf("after refill should be immediate, got %v", d)
	}
}

func TestClassifyHTTP_Mapping(t *testing.T) {
	cases := []struct {
		step   string
		status int
		code   int
		class  errClass
	}{
		{stepCreateUser, http.StatusInternalServerError, CodeUpstreamDown, classRetryable},
		{stepCreateUser, http.StatusBadRequest, CodeUpstreamCreateU, classNonRetryable},
		{stepCreateToken, http.StatusBadRequest, CodeUpstreamCreateTok, classNonRetryable},
		{stepRevealKey, http.StatusBadRequest, CodeUpstreamRevealKey, classNonRetryable},
		{stepGetToken, http.StatusUnauthorized, CodeUpstreamAuth, classAuthExpired},
		{stepManageUser, http.StatusServiceUnavailable, CodeUpstreamDown, classRetryable},
	}
	for _, c := range cases {
		e := classifyHTTP(c.step, c.status, "")
		if e.PlatformCode != c.code {
			t.Errorf("step=%s status=%d: code=%d want %d", c.step, c.status, e.PlatformCode, c.code)
		}
		if e.class != c.class {
			t.Errorf("step=%s status=%d: class=%d want %d", c.step, c.status, e.class, c.class)
		}
	}
	// 超时分类。
	to := newTransportError(stepGetToken, true, context.DeadlineExceeded)
	if to.PlatformCode != CodeUpstreamTimeout || !to.Retryable() {
		t.Errorf("timeout should map to %d & retryable, got code=%d retryable=%v", CodeUpstreamTimeout, to.PlatformCode, to.Retryable())
	}
}

// 对同一个持续 5xx 的端点连续调用:退避重试耗尽 + 连续失败累计触发熔断,
// 之后直接 ErrCircuitOpen(不再打上游)。直接驱动单出口 do,避免被 bootstrap 链路里
// 成功的 search 调用重置连续失败计数(那是熔断器的正确语义)。
func TestClient_PersistentFailureOpensCircuit(t *testing.T) {
	f := newFakeNewapi()
	defer f.close()
	f.faultCreateUser5xx = 1000 // 永远 5xx
	cfg := f.config()
	cfg.MaxRetries = 1  // 每次调用 2 次尝试
	cfg.CBThreshold = 3 // 3 次连续失败熔断
	a := New(cfg, nil)

	body := map[string]any{"username": "x", "password": "y"}
	var sawCircuitOpen bool
	upstreamCallsAtOpen := 0
	for i := 0; i < 6; i++ {
		_, err := a.c.do(context.Background(), stepCreateUser, "POST", "/api/user/", adminAuth(a.c.cfg), body)
		if err == nil {
			t.Fatalf("expected failure under persistent 5xx")
		}
		if err == ErrCircuitOpen {
			sawCircuitOpen = true
			upstreamCallsAtOpen = f.callCount(stepCreateUser)
			break
		}
	}
	if !sawCircuitOpen {
		t.Fatalf("circuit breaker should have opened under persistent upstream failure")
	}
	// 熔断后再调:仍直接 ErrCircuitOpen,且不再增加上游调用(止住对 new-api 的打击)。
	_, err := a.c.do(context.Background(), stepCreateUser, "POST", "/api/user/", adminAuth(a.c.cfg), body)
	if err != ErrCircuitOpen {
		t.Fatalf("once open, calls should short-circuit with ErrCircuitOpen, got %v", err)
	}
	if f.callCount(stepCreateUser) != upstreamCallsAtOpen {
		t.Errorf("open circuit must not hit upstream again: before=%d after=%d", upstreamCallsAtOpen, f.callCount(stepCreateUser))
	}
}
