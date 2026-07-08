package handler

import (
	"net/http"
	"sync"
	"time"

	"github.com/nexusapi-platform/enterprise/pkg/apperr"
)

// criticalLimiter 敏感操作频率限流(B3,镜像 new-api middleware.CriticalRateLimit 口径:
// 默认 20 次 / 20 分钟,按调用者身份计数)。与登录退避 attemptLimiter(失败计数、成功清零)语义不同——
// 敏感动作(如揭示明文 key)**成功也计数**,限的是动作本身的频率。内存滑动窗口、单实例(与 A2 同口径,
// 多节点前随限流统一升级);窗口外记录惰性回收。
type criticalLimiter struct {
	mu     sync.Mutex
	hits   map[string][]time.Time
	max    int
	window time.Duration
	nowFn  func() time.Time
}

func newCriticalLimiter() *criticalLimiter {
	return &criticalLimiter{hits: map[string][]time.Time{}, max: 20, window: 20 * time.Minute, nowFn: time.Now}
}

// allow 记一次调用并判断是否放行(窗口内达 max 即拒,本次不计入)。
func (c *criticalLimiter) allow(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.nowFn()
	cut := now.Add(-c.window)
	kept := c.hits[key][:0]
	for _, t := range c.hits[key] {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= c.max {
		c.hits[key] = kept
		return false
	}
	c.hits[key] = append(kept, now)
	return true
}

// gate 限频闸:超限写 429(带 Retry-After 提示窗口)并返回 false。
func (c *criticalLimiter) gate(w http.ResponseWriter, r *http.Request, key string) bool {
	if c.allow(key) {
		return true
	}
	w.Header().Set("Retry-After", "60")
	writeErr(w, r, apperr.New(20429, http.StatusTooManyRequests, "敏感操作过于频繁,请稍后再试"))
	return false
}
