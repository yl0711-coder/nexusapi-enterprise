// Package lock 提供按键串行化能力。
//
// 代发 key bootstrap 要求"同一成员的 bootstrap 全局串行化"(见 10 §2.6):
// GET /api/user/token 一调即旋转、旧的立即失效,两路并发会互相作废存下来的
// access_token。MVP 单实例用进程内 KeyedMutex 即可;水平扩展后换成基于
// Redis/MySQL 的分布式锁(与 03 §1 的 leader 选主同栈),实现同一 KeyedLocker
// 接口即可,业务侧不改。
package lock

import (
	"context"
	"sync"
)

// KeyedLocker 按字符串键加锁。Acquire 成功返回 release 函数;失败返回 error。
// 分布式实现可在此接口下替换(带租约/超时),业务层只依赖本接口。
type KeyedLocker interface {
	// Acquire 阻塞直到拿到 key 的锁或 ctx 取消。返回的 release 必须被调用一次。
	Acquire(ctx context.Context, key string) (release func(), err error)

	// TryAcquire 不阻塞:拿到返回 (release, true);已被他人持有返回 (nil, false)。
	// 对应 10 §2.6"pending(他人持锁中)→ 拒绝并发请求返 409"。
	TryAcquire(key string) (release func(), ok bool)
}

// InProcessLocker 是 KeyedLocker 的进程内实现(MVP 单实例用)。
// 每个 key 一把 mutex,引用计数到 0 时回收,避免 key 空间无界增长。
type InProcessLocker struct {
	mu    sync.Mutex
	locks map[string]*refCountedMutex
}

type refCountedMutex struct {
	mu   sync.Mutex
	refs int
}

// NewInProcessLocker 创建进程内按键锁。
func NewInProcessLocker() *InProcessLocker {
	return &InProcessLocker{locks: make(map[string]*refCountedMutex)}
}

func (l *InProcessLocker) get(key string) *refCountedMutex {
	l.mu.Lock()
	defer l.mu.Unlock()
	m := l.locks[key]
	if m == nil {
		m = &refCountedMutex{}
		l.locks[key] = m
	}
	m.refs++
	return m
}

func (l *InProcessLocker) put(key string, m *refCountedMutex) {
	l.mu.Lock()
	defer l.mu.Unlock()
	m.refs--
	if m.refs == 0 {
		delete(l.locks, key)
	}
}

// Acquire 阻塞获取 key 的锁,支持 ctx 取消。
func (l *InProcessLocker) Acquire(ctx context.Context, key string) (func(), error) {
	m := l.get(key)
	// 用带取消的获取:在独立 goroutine 里抢 mutex,ctx 取消则放弃并回收引用。
	got := make(chan struct{})
	go func() {
		m.mu.Lock()
		close(got)
	}()
	select {
	case <-got:
		var once sync.Once
		return func() {
			once.Do(func() {
				m.mu.Unlock()
				l.put(key, m)
			})
		}, nil
	case <-ctx.Done():
		// 仍需等待那个 goroutine 拿到锁后立即释放,避免泄漏。
		go func() {
			<-got
			m.mu.Unlock()
			l.put(key, m)
		}()
		return nil, ctx.Err()
	}
}

// TryAcquire 非阻塞获取。
func (l *InProcessLocker) TryAcquire(key string) (func(), bool) {
	m := l.get(key)
	if !m.mu.TryLock() {
		l.put(key, m)
		return nil, false
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Unlock()
			l.put(key, m)
		})
	}, true
}
