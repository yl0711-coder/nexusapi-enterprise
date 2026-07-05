package service

import "context"

// Leadership 是"本 tick 能否执行 leader-only 写工作(结算/回填/托管对账)"的单一决策支点(B5)。
// 今天由 envLeadership 给平凡实现(环境判断);将来接 LB 双活时换 leaseLeadership(MySQL 租约 + term fencing),
// **只 new 一个实现替换、调用点一行不改**。三个写入口(RunSettlement / RunBackfillSlice / ReconcileEscrow)
// 在动手前都调 CanRunTick;非 leader 直接 no-op 返回。
type Leadership interface {
	// CanRunTick 报告本 tick 是否应执行 leader-only 写工作。
	// fence 是任期栅栏 token:v1 恒 0(占位);v2 租约实现返回真 fence,透传进结算/回填写事务做 fencing
	// (settlement_cursor.version 乐观锁是第二层)。
	CanRunTick(ctx context.Context) (ok bool, fence int64, err error)
}

// envLeadership v1 单节点实现:NEXUS_WORKER_ENABLED != false 即视为 leader(由运维保证同时只一个节点开 worker)。
// 无租约、无 fencing——**多节点接 LB 前必须换 leaseLeadership**(B5);过渡期靠此 + 启动自检(main.go)防误配。
type envLeadership struct{ enabled bool }

// NewEnvLeadership 构造 v1 环境实现。enabled 来自 NEXUS_WORKER_ENABLED != false。
func NewEnvLeadership(enabled bool) Leadership { return &envLeadership{enabled: enabled} }

func (e *envLeadership) CanRunTick(_ context.Context) (bool, int64, error) {
	return e.enabled, 0, nil // v1:环境即 leader 信号;fence 恒 0 占位
}
