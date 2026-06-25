package newapi

import (
	"context"
	"sync"
	"time"
)

// tokenBucket 是一个简单的令牌桶限速器(对上游整体限速,绝不全量打 new-api)。
// 懒补充:按距上次取令牌的时间差补充,避免后台 goroutine。
type tokenBucket struct {
	mu     sync.Mutex
	rate   float64 // 每秒补充令牌数
	burst  float64 // 桶容量
	tokens float64
	last   time.Time
	nowFn  func() time.Time // 便于测试注入时钟
}

func newTokenBucket(qps float64, burst int) *tokenBucket {
	return &tokenBucket{
		rate:   qps,
		burst:  float64(burst),
		tokens: float64(burst),
		last:   time.Now(),
		nowFn:  time.Now,
	}
}

// wait 阻塞直到取得一个令牌或 ctx 取消。
func (tb *tokenBucket) wait(ctx context.Context) error {
	for {
		wait := tb.reserve()
		if wait <= 0 {
			return nil
		}
		t := time.NewTimer(wait)
		select {
		case <-t.C:
			// 再循环确认拿到令牌(可能有并发竞争)。
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		}
	}
}

// reserve 尝试取一个令牌;够则立即返回 0,不够返回需等待的时长。
func (tb *tokenBucket) reserve() time.Duration {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	now := tb.nowFn()
	elapsed := now.Sub(tb.last).Seconds()
	if elapsed > 0 {
		tb.tokens += elapsed * tb.rate
		if tb.tokens > tb.burst {
			tb.tokens = tb.burst
		}
		tb.last = now
	}
	if tb.tokens >= 1 {
		tb.tokens--
		return 0
	}
	need := 1 - tb.tokens
	return time.Duration(need / tb.rate * float64(time.Second))
}

// circuitBreaker:连续失败到阈值则打开,冷却后转半开放一个探测请求。
//
//	closed   → 正常放行,累计连续失败;达阈值转 open
//	open     → 拒绝(返 ErrCircuitOpen);冷却到点转 half-open
//	half-open→ 放行一个探测;成功转 closed,失败回 open
type cbState int

const (
	cbClosed cbState = iota
	cbOpen
	cbHalfOpen
)

type circuitBreaker struct {
	mu          sync.Mutex
	state       cbState
	threshold   int
	cooldown    time.Duration
	consecFails int
	openedAt    time.Time
	probing     bool // half-open 已放出探测、尚未回结果
	nowFn       func() time.Time
}

func newCircuitBreaker(threshold int, cooldown time.Duration) *circuitBreaker {
	return &circuitBreaker{state: cbClosed, threshold: threshold, cooldown: cooldown, nowFn: time.Now}
}

// allow 报告是否放行本次请求。
func (cb *circuitBreaker) allow() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	switch cb.state {
	case cbClosed:
		return true
	case cbOpen:
		if cb.nowFn().Sub(cb.openedAt) >= cb.cooldown {
			cb.state = cbHalfOpen
			cb.probing = true
			return true // 放一个探测
		}
		return false
	case cbHalfOpen:
		// 已有探测在途,其它请求继续挡,直到探测回结果。
		if cb.probing {
			return false
		}
		cb.probing = true
		return true
	}
	return true
}

func (cb *circuitBreaker) onSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.consecFails = 0
	cb.state = cbClosed
	cb.probing = false
}

func (cb *circuitBreaker) onFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.consecFails++
	cb.probing = false
	if cb.state == cbHalfOpen {
		// 半开探测失败:回到打开,重置冷却。
		cb.state = cbOpen
		cb.openedAt = cb.nowFn()
		return
	}
	if cb.consecFails >= cb.threshold {
		cb.state = cbOpen
		cb.openedAt = cb.nowFn()
	}
}
