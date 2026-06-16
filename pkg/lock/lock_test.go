package lock

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestInProcessLocker_Serializes(t *testing.T) {
	l := NewInProcessLocker()
	var concurrent, maxConcurrent int32

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rel, err := l.Acquire(context.Background(), "k")
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			defer rel()
			c := atomic.AddInt32(&concurrent, 1)
			if c > atomic.LoadInt32(&maxConcurrent) {
				atomic.StoreInt32(&maxConcurrent, c)
			}
			time.Sleep(time.Millisecond)
			atomic.AddInt32(&concurrent, -1)
		}()
	}
	wg.Wait()
	if maxConcurrent != 1 {
		t.Errorf("same key must serialize, observed max concurrency = %d", maxConcurrent)
	}
}

func TestInProcessLocker_DifferentKeysParallel(t *testing.T) {
	l := NewInProcessLocker()
	relA, okA := l.TryAcquire("a")
	if !okA {
		t.Fatalf("first acquire of a should succeed")
	}
	defer relA()
	// 不同键互不阻塞。
	relB, okB := l.TryAcquire("b")
	if !okB {
		t.Fatalf("different key b should acquire while a held")
	}
	relB()
	// 同键已持有 → TryAcquire 返回 false(对应 §2.6 pending→409)。
	if _, ok := l.TryAcquire("a"); ok {
		t.Fatalf("held key a should not be re-acquirable")
	}
}

func TestInProcessLocker_ContextCancel(t *testing.T) {
	l := NewInProcessLocker()
	rel, _ := l.Acquire(context.Background(), "k")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := l.Acquire(ctx, "k"); err == nil {
		t.Errorf("acquire on cancelled ctx for held key should error")
	}
	rel()
}
