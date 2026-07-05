package service

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/nexusapi-platform/enterprise/adapter/newapi"
	"github.com/nexusapi-platform/enterprise/pkg/apperr"
	"github.com/nexusapi-platform/enterprise/pkg/session"
	"github.com/nexusapi-platform/enterprise/repo"
)

// BackfillStatusView 门B 组织历史回填状态(前端展示,24-§9 UX:回填中 / 已同步·起点 / 失败·重跑)。
type BackfillStatusView struct {
	Status         string `json:"status"`           // pending|running|done|failed|none(门A 或未触发,无历史)
	RowsIngested   int64  `json:"rows_ingested"`    // 已灌逐条数(回填中可见增长)
	EarliestSeenTS *int64 `json:"earliest_seen_ts"` // 历史起点(unix秒);done 时前端显示"已同步,起点 xx"
	LastError      string `json:"last_error"`       // failed 时的错误(供运营方判断是否重跑)
}

// GetBackfillStatus 读某组织历史回填状态(24-§9)。运营方 + org_admin 可见本组织。
func (s *Service) GetBackfillStatus(ctx context.Context, c session.Claims, orgID int64) (*BackfillStatusView, error) {
	if err := assertOrgScope(c, orgID); err != nil {
		return nil, err
	}
	if err := assertRole(c, session.RoleOperator, session.RoleOrgAdmin); err != nil {
		return nil, err
	}
	j, err := s.store.GetBackfillJob(ctx, orgID)
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	if j == nil {
		return &BackfillStatusView{Status: "none"}, nil // 门A 或未触发:无历史回填
	}
	v := &BackfillStatusView{Status: j.Status, RowsIngested: j.RowsIngested}
	if j.EarliestSeenTS.Valid {
		e := j.EarliestSeenTS.Int64
		v.EarliestSeenTS = &e
	}
	if j.LastError.Valid {
		v.LastError = j.LastError.String
	}
	return v, nil
}

// RequeueBackfill 运营方"重新回填"(24-§9,幂等):cursor 回到 boundary、status=pending,worker 下轮重跑
// (detail 幂等,不会双算)。仅运营方。
func (s *Service) RequeueBackfill(ctx context.Context, c session.Claims, orgID int64) error {
	if err := assertRole(c, session.RoleOperator); err != nil {
		return err
	}
	j, err := s.store.GetBackfillJob(ctx, orgID)
	if err != nil {
		return apperr.Internal("").WithCause(err)
	}
	if j == nil {
		return apperr.InvalidParam("该组织无历史回填任务(门A 新建组织无历史,或未触发)")
	}
	if err := s.store.RequeueBackfillJob(ctx, orgID); err != nil {
		return apperr.Internal("").WithCause(err)
	}
	s.audit(ctx, c, orgID, "backfill_requeue", "backfill", &orgID, nil)
	return nil
}

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
// 并发红线(24-§3.3):每个子窗口的"去重→落账"写相在 settlementMu 下完成(与所有 forward 互斥;HTTP 读页在
// 锁外,不阻塞 forward——R6 中危修),使回填与 forward-补漏在 600s 重叠带靠 detail 幂等去重时绝不并发 TOCTOU
// 双算。**仅由 settlement-worker 单 goroutine 调用**。
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

// backfillOneWindow 处理 [lo, hi] 一个子窗口(lo 由二分选出,使窗口日志数 <= settlementMaxPages*100,
// 可一次完整读尽、绝不截断)。分两相以缩短持锁(R6 中危修:HTTP 读页移出锁,不阻塞 forward):
//
//	读相(**锁外**):二分选窗 + 逐页拉 + 过滤(belongsToBackfill + 防御性 UserID 归属)→ keep。HTTP 慢,不持锁。
//	写相(**settlementMu 内**,与所有 forward 互斥,持锁极短):detail 幂等去重 → ingestEntry 归因聚合 →
//	  commitUsageOnly 单事务落账。
//
// 正确性(为何读页可在锁外):跨写者 TOCTOU 只发生在 600s 重叠带的"去重→落账"上,而这一整段在锁内原子完成、
// FilterExisting 反映 forward 已提交态;锁外读到的行若被 forward 抢先提交,锁内 FilterExisting 必查重跳过 →
// 绝不双算(24-§3.3)。boundary 在触发时已快照固定,belongsToBackfill 过滤与 forward 进度无关。
// 返回本窗口新落行数、见到的最早日志 ts(0=无)、窗口下界 lo。仅由 settlement-worker 单 goroutine 调用。
func (s *Service) backfillOneWindow(ctx context.Context, job *repo.BackfillJob, hi int64) (rows, earliest, lo int64, err error) {
	// ---- 读相(锁外)----
	lo, err = s.backfillLowerBound(ctx, job.NewapiUsername, hi)
	if err != nil {
		return 0, 0, hi, err
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

	// ---- 写相(settlementMu 内,与所有 forward 互斥;持锁仅一次 FilterExisting + 内存聚合 + 一个本地事务)----
	s.settlementMu.Lock()
	defer s.settlementMu.Unlock()
	// detail 幂等去重(24-§3.3 第二重保险):ledger 无按行去重,ingest 前查 usage_detail 已存在的跳过。
	ids := make([]int64, len(keep))
	for i, e := range keep {
		ids[i] = e.ID
	}
	existing, ferr := s.store.FilterExistingDetailLogIDs(ctx, ids)
	if ferr != nil {
		return 0, 0, hi, ferr
	}
	sk := &settleSink{
		aggs: map[string]*bucketAgg{}, flagCache: map[int64]bool{},
		attrCache: map[int64]tokenAttr{}, orgByUser: map[int64]int64{}, orphanAlerted: map[int64]bool{},
		backfillMode: true, // 回填:跳过孤儿告警刷屏 + 无用的 billing flag 查询(回填从不扣钱,flagCache 不被读)
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

// ReconcileBackfillLedger 回填-台账对账安全网(24-§9,§4.4 长期安全网):对已回填完成(done)的组织,
// 周期比对 SUM(usage_ledger) 与 new-api /api/log/stat 权威总消耗(截至 forward 水位,避开未结算的近期尾巴),
// 漂移即告警(日志 + 审计)。只读、绝不改账——把"回填/forward 静默多算或漏算"的发现窗口从月级压到一个对账周期。
// v1 报表期即挂:观测到漂移可提前修;v2 开计费后余额从 ledger 派生,这道网直接护住钱。
func (s *Service) ReconcileBackfillLedger(ctx context.Context) error {
	cur, err := s.store.GetOrCreateCursor(ctx, 0)
	if err != nil {
		return err
	}
	if cur.LastSettledTS <= 0 {
		return nil // forward 尚未首跑,无水位可对
	}
	jobs, err := s.store.ListDoneBackfillJobs(ctx)
	if err != nil {
		return err
	}
	for _, j := range jobs {
		authoritative, aerr := s.upstream.SumConsumedQuotaByUsername(ctx, j.NewapiUsername, cur.LastSettledTS)
		if aerr != nil {
			s.log.Warn("回填-台账对账取权威值失败(下轮重试)", "org_id", j.OrgID, "err", aerr)
			continue
		}
		ledgerSum, lerr := s.store.SumLedgerByOrg(ctx, j.OrgID)
		if lerr != nil {
			return lerr
		}
		if ledgerSum != authoritative {
			s.log.Error("回填-台账对账漂移(人工核对:多=双算/少=漏)",
				"org_id", j.OrgID, "ledger_sum", ledgerSum, "authoritative", authoritative, "diff", ledgerSum-authoritative)
			orgID := j.OrgID
			s.auditSystem(ctx, j.OrgID, "backfill_ledger_mismatch", "backfill", &orgID, map[string]any{
				"ledger_sum": ledgerSum, "authoritative": authoritative, "diff": ledgerSum - authoritative,
			}, "mismatch")
		}
	}
	return nil
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
