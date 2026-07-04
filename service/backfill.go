package service

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/nexusapi-platform/enterprise/adapter/newapi"
	"github.com/nexusapi-platform/enterprise/repo"
)

// enqueueBackfill 门B 关联成功后触发历史回填(24-§7.3):快照全局 forward 边界 B、插 pending 任务。
// 仅由 org.Associate(门B)在关联提交后调用,非阻断。
//
// 【B_ts==0 守卫(24-§3.2 订正,涉钱正确性)】cursor 未初始化(forward 尚未首跑)时:
//   - 直接以 (0,0) 为界:belongsToBackfill 恒 false,回填吃不到历史 + forward 基线又跳过历史 → 静默丢史。
//   - 简单置 boundary_ts=now 也不行:回填会盖 [now-lag, now),而 forward 基线后的**主窗口**也处理这段
//     (主窗口只按 id 水位去重、不查 detail 幂等 —— 只有 rescan 查),两边各 += 一次 → ledger 双算。
//   - 正解:先强制 forward 基线一轮(cursor=(0,0) 时 RunSettlement 只推水位到 now-lag、不读日志,极快),
//     再以真实 cursor 为界 → 退化成正常情形:回填 <=B 与 forward >B 无缝无叠,唯一重叠(rescan 600s)由 detail 幂等兜。
func (s *Service) enqueueBackfill(ctx context.Context, orgID, newapiUserID int64, username string) error {
	if username == "" {
		return fmt.Errorf("门B 回填触发缺 new-api username(org=%d)", orgID)
	}
	cur, err := s.store.GetOrCreateCursor(ctx, 0)
	if err != nil {
		return err
	}
	if cur.LastSettledTS == 0 {
		if _, serr := s.RunSettlement(ctx); serr != nil { // cursor=(0,0) 时只做基线(不读日志),快
			return fmt.Errorf("回填触发前强制 forward 基线失败: %w", serr)
		}
		cur, err = s.store.GetOrCreateCursor(ctx, 0)
		if err != nil {
			return err
		}
		if cur.LastSettledTS == 0 {
			return fmt.Errorf("强制 forward 基线后 cursor 仍为 0(org=%d),放弃触发以免丢史或双算", orgID)
		}
	}
	return s.store.InsertBackfillJob(ctx, &repo.BackfillJob{
		OrgID: orgID, NewapiUserID: newapiUserID, NewapiUsername: username,
		BoundaryTS: cur.LastSettledTS, BoundaryLogID: cur.LastSettledLogID, CursorTS: cur.LastSettledTS,
	})
}

// belongsToBackfill 判定一条日志属回填侧(24-§3.1,(created_at, id) 词典序):
//
//	created_at < B_ts  ||  (created_at == B_ts && id <= B_id)
//
// 与 forward"只处理 > B"精确互补 → 两流在日志级别绝不相交(命根子)。
// 用 (ts,id) 而非纯 id:首跑 B_id 可能为 0,纯 id 会把全部历史误判为 forward 侧而漏掉(24-§3.2)。
func belongsToBackfill(createdAt, id, bTS, bID int64) bool {
	return createdAt < bTS || (createdAt == bTS && id <= bID)
}

// RunBackfillSlice 跑一"片"历史回填:取最早未完成任务,从 cursor_ts 往 0 方向处理至多
// backfillWindowsPerTick 个子窗口即让位(tick 分片,防大回填饿死 forward)。
//
// 涉钱红线(24-§10):回填**只调 commitUsageOnly**——绝不扣 company_balance、绝不推进全局 settlement_cursor。
// 并发红线(24-§3.3):每个子窗口在 settlementMu 下完成(与所有 forward 互斥),使回填与 forward-补漏在
// 600s 重叠带靠 detail 幂等去重时绝不并发 TOCTOU 双算。**仅由 settlement-worker 单 goroutine 调用**。
func (s *Service) RunBackfillSlice(ctx context.Context) error {
	job, err := s.store.NextBackfillJob(ctx)
	if err != nil {
		return err
	}
	if job == nil {
		return nil // 无待回填任务
	}
	if job.Status == "pending" {
		if err := s.store.SetBackfillStatus(ctx, job.OrgID, "running"); err != nil {
			return err
		}
	}
	hi := job.CursorTS
	for windows := 0; hi > 0 && windows < s.backfillWindowsPerTick; windows++ {
		rows, earliest, lo, werr := s.backfillOneWindow(ctx, job, hi)
		if werr != nil {
			// 非致命:记错但保持 running,下轮/崩溃后从 cursor_ts 续(detail 幂等,重跑安全,24-§9)。
			_ = s.store.SetBackfillError(ctx, job.OrgID, werr.Error())
			return werr
		}
		hi = lo - 1
		if err := s.store.UpdateBackfillProgress(ctx, job.OrgID, hi, rows, earliest); err != nil {
			return err
		}
		job.RowsIngested += rows
		s.backfillThrottle(ctx)
	}
	if hi <= 0 {
		if err := s.store.SetBackfillStatus(ctx, job.OrgID, "done"); err != nil {
			return err
		}
		s.log.Info("历史回填完成", "org_id", job.OrgID, "rows_ingested", job.RowsIngested)
	}
	return nil
}

// backfillOneWindow 在 settlementMu 保护下处理 [lo, hi] 一个子窗口(lo 由二分选出,使窗口日志数
// <= settlementMaxPages*100,可一次完整读尽,绝不截断):读页 → 过滤(belongsToBackfill + 防御性 UserID
// 归属)→ detail 幂等去重 → ingestEntry 归因聚合 → commitUsageOnly 单事务落账。
// 返回本窗口新落行数、见到的最早日志 ts(0=无)、窗口下界 lo。
func (s *Service) backfillOneWindow(ctx context.Context, job *repo.BackfillJob, hi int64) (rows, earliest, lo int64, err error) {
	// 与所有 forward 互斥:读页/去重/落账全程持锁,使"FilterExisting → ingest → commit"相对 forward 原子,
	// 杜绝 600s 重叠带的跨写者 TOCTOU(24-§3.3)。量小,持锁时间短;v1 escrow-drain 休眠、几无争用。
	s.settlementMu.Lock()
	defer s.settlementMu.Unlock()

	lo, err = s.backfillLowerBound(ctx, job.NewapiUsername, hi)
	if err != nil {
		return 0, 0, hi, err
	}

	sk := &settleSink{
		aggs: map[string]*bucketAgg{}, flagCache: map[int64]bool{},
		attrCache: map[int64]tokenAttr{}, orgByUser: map[int64]int64{}, orphanAlerted: map[int64]bool{},
	}
	var keep []newapi.LogEntry
	for page := 1; page <= settlementMaxPages; page++ {
		entries, total, rerr := s.upstream.ReadConsumptionLogsByUsername(ctx, job.NewapiUsername, lo, hi, page, 100)
		if rerr != nil {
			return 0, 0, hi, mapUpstream(rerr)
		}
		for _, e := range entries {
			if e.Quota <= 0 {
				continue
			}
			if int64(e.UserID) != job.NewapiUserID {
				continue // 防御性归属断言(24-§8):username 精确匹配之外再核 user_id,绝不把别组织日志灌进来
			}
			if !belongsToBackfill(e.CreatedAt, e.ID, job.BoundaryTS, job.BoundaryLogID) {
				continue // 属 forward 侧(> B),不碰
			}
			keep = append(keep, e)
		}
		if page*100 >= total {
			break
		}
	}
	if len(keep) == 0 {
		return 0, 0, lo, nil
	}

	// detail 幂等去重(24-§3.3 第二重保险):ledger 无按行去重,ingest 前查 usage_detail 已存在的跳过。
	ids := make([]int64, len(keep))
	for i, e := range keep {
		ids[i] = e.ID
	}
	existing, ferr := s.store.FilterExistingDetailLogIDs(ctx, ids)
	if ferr != nil {
		return 0, 0, hi, ferr
	}
	for _, e := range keep {
		if existing[e.ID] {
			continue
		}
		if ierr := s.ingestEntry(ctx, sk, e); ierr != nil {
			return 0, 0, hi, ierr
		}
		rows++
		if earliest == 0 || e.CreatedAt < earliest {
			earliest = e.CreatedAt
		}
	}
	if len(sk.aggs) > 0 || len(sk.details) > 0 {
		if terr := s.store.WithTx(ctx, func(tx *sql.Tx) error {
			return s.commitUsageOnly(ctx, tx, sk.aggs, sk.details) // 只写报表两表,不扣钱、不推全局水位
		}); terr != nil {
			return 0, 0, hi, terr
		}
	}
	return rows, earliest, lo, nil
}

// backfillLowerBound 二分选窗口下界 lo(复刻 settlement.go:171-186 手法,方向相反):固定已知上界 hi,
// 从 0 向 hi 收缩,直到 [lo, hi] 日志数 <= settlementMaxPages*100(能一次完整读尽)。加 username 过滤后
// 单用户单窗口量很小,基本一次命中 lo=0(整段历史一窗读尽)。
func (s *Service) backfillLowerBound(ctx context.Context, username string, hi int64) (int64, error) {
	lo := int64(0)
	for {
		_, total, err := s.upstream.ReadConsumptionLogsByUsername(ctx, username, lo, hi, 1, 100)
		if err != nil {
			return 0, mapUpstream(err)
		}
		if total <= settlementMaxPages*100 {
			return lo, nil
		}
		if lo >= hi-1 {
			// 单秒 > 上限的极端(单用户几乎不可能):告警,尽力处理这一窗,避免死循环卡住回填。
			s.log.Error("回填单窗口日志数超上限(极端),尽力处理这一窗", "lo", lo, "hi", hi, "total", total)
			return lo, nil
		}
		lo = hi - (hi-lo)/2 // 向已知端 hi 收缩(镜像 forward 的 untilSub 二分)
	}
}

// backfillThrottle 页/窗间限速(NEXUS_BACKFILL_QPS);ctx 取消即返回。
func (s *Service) backfillThrottle(ctx context.Context) {
	if s.backfillQPS <= 0 {
		return
	}
	select {
	case <-time.After(time.Second / time.Duration(s.backfillQPS)):
	case <-ctx.Done():
	}
}
