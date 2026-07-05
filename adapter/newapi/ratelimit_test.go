package newapi

import (
	"testing"
	"time"
)

// TestCircuitBreaker_ProbeResolvesEveryPath 锁死 A1:半开探测名额在四种结果下都必须复位 probing,
// 否则半开态 allow() 恒返回 false → 平台对 new-api 全拒、需重启进程。
func TestCircuitBreaker_ProbeResolvesEveryPath(t *testing.T) {
	mk := func() (*circuitBreaker, *time.Time) {
		now := time.Now()
		cur := now
		cb := newCircuitBreaker(2, 10*time.Second)
		cb.nowFn = func() time.Time { return cur }
		return cb, &cur
	}
	// trip:连续失败到阈值 → open;推进时钟过冷却,使下次 allow 进半开。
	trip := func(cb *circuitBreaker, cur *time.Time) {
		cb.onFailure()
		cb.onFailure()
		if cb.allow() {
			t.Fatalf("刚打开(冷却内)应拒")
		}
		*cur = cur.Add(11 * time.Second)
	}

	// (a) 探测成功 → 闭合、恢复放行。
	cb, cur := mk()
	trip(cb, cur)
	if !cb.allow() {
		t.Fatalf("(a) 冷却后应放一个半开探测")
	}
	cb.onSuccess()
	if !cb.allow() {
		t.Fatalf("(a) 探测成功后应闭合放行")
	}

	// (b) 探测可重试失败 → 回 open;再冷却后仍能探测(不卡死)。
	cb, cur = mk()
	trip(cb, cur)
	cb.allow()
	cb.onFailure()
	if cb.allow() {
		t.Fatalf("(b) 探测失败应回 open、冷却内拒")
	}
	*cur = cur.Add(11 * time.Second)
	if !cb.allow() {
		t.Fatalf("(b) 再冷却后应能再放探测,不卡死")
	}

	// (c) 探测拿到不可重试(4xx/401)= 连通正常 → 按 onSuccess 结算 → 闭合。
	cb, cur = mk()
	trip(cb, cur)
	cb.allow()
	cb.onSuccess()
	if !cb.allow() {
		t.Fatalf("(c) 不可重试按成功结算后应闭合放行")
	}

	// (d) 探测被中止(ctx 取消 / 限速未过,请求根本没发出)→ probeAbort → 下次能再探测(A1 核心防卡死)。
	cb, cur = mk()
	trip(cb, cur)
	if !cb.allow() {
		t.Fatalf("(d) 应放半开探测")
	}
	cb.probeAbort()
	if !cb.allow() {
		t.Fatalf("(d) A1:探测中止后 probing 必须复位,否则半开永停卡死")
	}
}
