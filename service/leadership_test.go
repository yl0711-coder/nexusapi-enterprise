package service

import (
	"context"
	"testing"
)

// TestUnit_LeadershipGatesWrites 锁死 B5:非 leader 时 RunSettlement 在触达 store 前 no-op
//（nil store 不 panic 即证准入在最前);leader 时 CanRunTick 放行。
// (RunBackfillSlice 曾同验,已随历史回填机器整体退役删除。)
func TestUnit_LeadershipGatesWrites(t *testing.T) {
	s := New(Deps{Leadership: NewEnvLeadership(false)}) // 非 leader,nil store
	if n, err := s.RunSettlement(context.Background()); err != nil || n != 0 {
		t.Fatalf("非 leader RunSettlement 应 (0,nil) 且不触 store,实 (%d,%v)", n, err)
	}
	ok, fence, err := NewEnvLeadership(true).CanRunTick(context.Background())
	if !ok || fence != 0 || err != nil {
		t.Fatalf("leader CanRunTick 应 (true,0,nil),实 (%v,%d,%v)", ok, fence, err)
	}
}
