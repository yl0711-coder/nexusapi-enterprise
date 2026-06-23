package worker

import (
	"log/slog"
	"runtime/debug"
)

// safeTick 在隔离的 recover 下执行一轮 tick:panic 不向上冒泡、只记 error 日志 +(后续)指标,
// 返回后由调用方继续下一轮 ticker。绝不让单次 tick panic 掀翻整个进程(GZ-02 修复1)。
//
// 关键:recover 作用域必须就在每一轮 tick 内(包住 fn 调用),这样 panic 后 Run 的 for-select
// 继续存活、下一个 ticker 照常进来。绝不能把 recover 放在 Run 最外层——那样 panic 后整条 Run
// goroutine 结束、worker 静默死掉不再 tick(比崩进程更隐蔽)。
func safeTick(log *slog.Logger, name string, fn func()) {
	defer func() {
		if v := recover(); v != nil {
			log.Error("worker tick panic(已恢复,继续下一轮)",
				"worker", name, "panic", v, "stack", string(debug.Stack()))
			// TODO(可观测工单): 在此 +1 worker_tick_panic_total{worker=name} 指标。
		}
	}()
	fn()
}
