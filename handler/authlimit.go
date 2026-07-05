package handler

import (
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nexusapi-platform/enterprise/pkg/apperr"
)

// attemptLimiter 是登录/改密的失败退避(A2 应用层纵深防御,与 CF 边缘限流叠加)。
// 连续失败达阈值后按指数退避锁定该 key。**per-account(email/memberID)为主**——攻击者无法伪造受害者账号;
// per-IP 为辅(CF 后取真实 IP;换 IP 可绕,故非主防线)。内存态、单实例(登录本就低频);成功即清零。
type attemptLimiter struct {
	mu        sync.Mutex
	st        map[string]*attemptState
	threshold int           // 连续失败达此数开始锁定
	baseLock  time.Duration // 基础锁定(指数退避)
	maxLock   time.Duration // 锁定上限
	nowFn     func() time.Time
}

type attemptState struct {
	fails       int
	lockedUntil time.Time
}

func newAttemptLimiter() *attemptLimiter {
	return &attemptLimiter{st: map[string]*attemptState{}, threshold: 5, baseLock: 30 * time.Second, maxLock: 15 * time.Minute, nowFn: time.Now}
}

// retryAfter 返回该 key 当前需等待的时长(>0 = 锁定中,应拒)。
func (a *attemptLimiter) retryAfter(key string) time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.st[key]
	if s == nil {
		return 0
	}
	if d := s.lockedUntil.Sub(a.nowFn()); d > 0 {
		return d
	}
	return 0
}

// fail 记一次失败;连续失败达阈值后按指数退避锁定。
func (a *attemptLimiter) fail(key string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.st[key]
	if s == nil {
		s = &attemptState{}
		a.st[key] = s
	}
	s.fails++
	if s.fails >= a.threshold {
		d := a.baseLock << uint(s.fails-a.threshold) // 指数退避
		if d <= 0 || d > a.maxLock {
			d = a.maxLock
		}
		s.lockedUntil = a.nowFn().Add(d)
	}
}

// reset 成功后清零。
func (a *attemptLimiter) reset(key string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.st, key)
}

// gate 检查两个 key(账号 + IP)是否锁定;锁定则写 429 + Retry-After 并返回 false。
func (a *attemptLimiter) gate(w http.ResponseWriter, r *http.Request, acctKey, ipKey string) bool {
	d := a.retryAfter(acctKey)
	if di := a.retryAfter(ipKey); di > d {
		d = di
	}
	if d <= 0 {
		return true
	}
	w.Header().Set("Retry-After", strconv.Itoa(int(d.Seconds())+1))
	writeErr(w, r, apperr.New(20429, http.StatusTooManyRequests, "尝试过于频繁,请稍后再试"))
	return false
}

// isCredFailure 仅"凭据错误"(CodeUnauthenticated)计入退避——禁用账号(Forbidden)/系统错(Internal)不误锁。
func isCredFailure(err error) bool {
	var e *apperr.Error
	return errors.As(err, &e) && e.Code == apperr.CodeUnauthenticated
}

// clientIP 取真实客户端 IP:CF-Connecting-IP(CF 注入)优先,再 X-Forwarded-For 首段,末 RemoteAddr。
func clientIP(r *http.Request) string {
	if ip := r.Header.Get("CF-Connecting-IP"); ip != "" {
		return ip
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
