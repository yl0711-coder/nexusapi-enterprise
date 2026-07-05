package service

import (
	"context"
	"testing"
)

// TestUnit_LeadershipGatesWrites 锁死 B5:非 leader 时 RunSettlement/RunBackfillSlice 在触达 store 前 no-op
//（nil store 不 panic 即证准入在最前);leader 时 CanRunTick 放行。
func TestUnit_LeadershipGatesWrites(t *testing.T) {
	s := New(Deps{Leadership: NewEnvLeadership(false)}) // 非 leader,nil store
	if n, err := s.RunSettlement(context.Background()); err != nil || n != 0 {
		t.Fatalf("非 leader RunSettlement 应 (0,nil) 且不触 store,实 (%d,%v)", n, err)
	}
	if err := s.RunBackfillSlice(context.Background()); err != nil {
		t.Fatalf("非 leader RunBackfillSlice 应 nil 且不触 store,实 %v", err)
	}
	ok, fence, err := NewEnvLeadership(true).CanRunTick(context.Background())
	if !ok || fence != 0 || err != nil {
		t.Fatalf("leader CanRunTick 应 (true,0,nil),实 (%v,%d,%v)", ok, fence, err)
	}
}
