package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/nexusapi-platform/enterprise/adapter/newapi"
	"github.com/nexusapi-platform/enterprise/model"
	"github.com/nexusapi-platform/enterprise/pkg/apperr"
	"github.com/nexusapi-platform/enterprise/repo"
)

// errConcurrentSettlement 表示推进水位时 version 已被并发写者改动(AdvanceCursor 未命中),
// 须整批回滚(GZ-01 修复3)。单节点单写者下不应发生;多节点由选主治,此处兜底正确性。
var errConcurrentSettlement = errors.New("结算水位推进未命中(并发写者),整批回滚")

// settlementLagSec 是结算滞后窗口:只结算 now-lag 之前的 log,避开 in-flight 写入(03 §4 软边界)。
const settlementLagSec int64 = 5

// settlementMaxPages 单次结算最多读多少页(每页 100),界住窗口、绝不全表(05 §1.1)。
const settlementMaxPages = 20

// M3 补漏扫描(20-§6):lag 只挡"提交延迟 < lag"的行;超 lag 的迟提交行(DB 顿一下/长事务)id 高水位当轮没看见、
// 时间窗又推过去了 → 不补扫即永久漏。每轮重扫 since 前 overlap 秒,已入 usage_detail 的查重跳过(幂等)。
const settlementRescanOverlapSec int64 = 600 // 补扫回看窗口:提交延迟 >10min 视为病态(告警级),不再追
const settlementRescanMaxPages = 5           // 补扫页上限(区间正常几乎空;超限=部分补扫+告警,下轮随 since 滑动续扫)

// bucketAgg 是一个 (org,user,key,model,小时桶) 的聚合累加。
// keyID(v2 M0-S2):日志 token_id 映射回的平台稳定 key_id(0=未归因)。
type bucketAgg struct {
	orgID, memberID, newapiUserID int64
	keyID                         int64
	teamID                        *int64
	model                         string
	bucket                        time.Time
	consumed                      int64
	maxTS                         int64
}

// tokenAttr 模型2 结算归因缓存项:按 new-api token_id 查到的平台成员 + 稳定 key_id(found=false 即非平台 token)。
type tokenAttr struct {
	found  bool
	member *model.Member
	keyID  int64
}

// settleSink 结算聚合槽:补漏(1a)与主窗口(1b)两阶段共用的聚合结果与查询缓存。
// 架构B(33 §12-9):company_balance 扣款已退役,槽内不再持 billing flag 缓存。
type settleSink struct {
	aggs          map[string]*bucketAgg
	details       []repo.DetailRow // 逐条明细(幂等键=newapi_log_id),与 ledger 同事务落
	attr          *attrCaches      // 归因缓存(user_id 主键 + token 一致性断言,attribution.go)
	orphanAlerted map[int64]bool   // token_id -> 已告警(每个孤儿令牌每轮只告警一次)
	backfillMode  bool             // 回填槽(R6 nit):跳过孤儿/归因告警刷屏(回填只写报表)
}

func newSettleSink(backfill bool) *settleSink {
	return &settleSink{
		aggs: map[string]*bucketAgg{}, attr: newAttrCaches(),
		orphanAlerted: map[int64]bool{}, backfillMode: backfill,
	}
}

// ingestEntry 归因并聚合一条消费日志(架构B,33 §3.3 AttributeLog):
//   - 归因按 **user_id** 为主键(成员=各自 new-api user),token→member 与 member→org 一致性断言,
//     不一致落**未归因桶**(member_id=0)+ 告警(防跨组织串台,31-ADR §8);
//   - user=金库 且 token=同组织成员 → A 版遗留数据按 token 归因(org 断言已过);
//   - user=金库 且 token 未登记 → 未归因桶(member_id=0,不丢行——报表总额必须与消费日志对得上,19-F5);
//   - user 非平台(共用生产实例,主站普通客户的日志同在 logs 表)→ 跳过。
func (s *Service) ingestEntry(ctx context.Context, sk *settleSink, e newapi.LogEntry) error {
	att, err := s.attributeUsage(ctx, sk.attr, int64(e.UserID), e.TokenID)
	if err != nil {
		return err
	}
	if att.skip {
		return nil // 非平台组织(主站客户),跳过
	}
	if att.mismatch && !sk.backfillMode {
		s.alertAttributionMismatch(ctx, sk.attr, att.orgID, int64(e.UserID), e.TokenID, e.ID, "settlement")
	}
	var memberID, keyID int64
	var teamID *int64
	if att.member != nil {
		memberID, keyID, teamID = att.member.ID, att.keyID, att.member.TeamID
		// GZ-03:开通失败的成员令牌仍在消费 = 孤儿消费。告警使漏归"可发现",仍正常聚合(报表不漏)。
		// 回填期跳过(R6 nit):回填全历史会把当前 failed 成员的历史消费逐窗刷屏告警。
		if !sk.backfillMode && att.member.BootstrapState == model.BootstrapFailed && !sk.orphanAlerted[e.TokenID] {
			sk.orphanAlerted[e.TokenID] = true
			s.log.Error("孤儿消费告警:开通失败的成员令牌仍在产生消费(需核 new-api)",
				"member_id", att.member.ID, "org_id", att.member.OrgID, "token_id", e.TokenID, "model", e.ModelName)
			mid := att.member.ID
			s.auditSystem(ctx, att.member.OrgID, "orphan_consumption", "member", &mid, map[string]any{
				"token_id": e.TokenID, "model": e.ModelName,
			}, "alert")
		}
	}
	bkt := hourBucket(e.CreatedAt)
	key := fmt.Sprintf("%d|%d|%d|%s|%d", att.orgID, e.UserID, keyID, e.ModelName, bkt.Unix())
	a := sk.aggs[key]
	if a == nil {
		a = &bucketAgg{orgID: att.orgID, memberID: memberID, newapiUserID: int64(e.UserID), keyID: keyID, teamID: teamID, model: e.ModelName, bucket: bkt}
		sk.aggs[key] = a
	}
	a.consumed += e.Quota
	if e.CreatedAt > a.maxTS {
		a.maxTS = e.CreatedAt
	}
	sk.details = append(sk.details, repo.DetailRow{
		OrgID: att.orgID, MemberID: memberID, NewapiUserID: int64(e.UserID), KeyID: keyID, TeamID: teamID,
		ModelName: e.ModelName, NewapiLogID: e.ID,
		PromptTokens: e.PromptTokens, CompletionTokens: e.CompletionTokens, ConsumedQuota: e.Quota,
		LogTS: time.Unix(e.CreatedAt, 0).UTC(),
	})
	return nil
}

// commitUsageOnly 是**只写报表**的落账段(24-§4.4):在调用方事务内把本轮聚合落进
// usage_ledger(小时桶原子自增)+ 逐条明细落进 usage_detail(INSERT IGNORE 幂等)。
// forward 结算与历史回填**共用此段**,保证两条路径报表口径逐字节一致。
//
// 【钱路径绊线】此函数只吃 aggs + details、只返 err;绝不引用 perOrg / maxLogID / cursor.Version / postBal——
// 扣余额(DeductBalanceTx)与推全局水位(AdvanceCursorTx)是 forward 独有、留在 RunSettlement 事务里,
// 回填只调本函数(不扣钱、不推水位,§10 红线在代码层落死)。若某天发现"需要"上述任一状态,立即停手回来对边界。
func (s *Service) commitUsageOnly(ctx context.Context, tx *sql.Tx, aggs map[string]*bucketAgg, details []repo.DetailRow) error {
	for _, a := range aggs {
		if err := s.store.AddToLedgerBucketTx(ctx, tx, &repo.LedgerBucket{
			OrgID: a.orgID, MemberID: a.memberID, NewapiUserID: a.newapiUserID, KeyID: a.keyID, TeamID: a.teamID,
			ModelName: a.model, TimeBucket: a.bucket, ConsumedQuota: a.consumed,
			LogMaxTS: time.Unix(a.maxTS, 0).UTC(),
		}); err != nil {
			return err
		}
	}
	// v2 M2-2:逐条明细与 ledger 落账同事务原子写(INSERT IGNORE 幂等);observe 下也写(报表数据,不涉钱)。
	if err := s.store.InsertUsageDetailTx(ctx, tx, details); err != nil {
		return err
	}
	return nil
}

// RunSettlement 是消费报表结算的单次扫描(leader 单写者,03 §3.1 / GZ-01 原子化版):
// 选一个不超限的时间子窗口(路径B,永不截断少收)→ 读 new-api 消费 logs 按 (org,user,key,model,小时桶) 聚合 →
// 在一个事务里原子提交:usage_ledger 落账 + 逐条明细 + 推进水位;
// 任一步失败或水位推进未命中即整批回滚、本轮不推水位、下轮干净重做(无双计)。
//
// 架构B(33 §5/§12-9,BE③ 拆除):**company_balance 扣款分支已删除**——平台不执行扣费,
// 消费真相在 new-api(成员 user.quota 原生双扣硬停,31-ADR §4.4),平台只做分配账本(ledger_transfer,BE②)
// 与消费报表(本函数)。observe 短路一并拆除:本函数本就只写报表,无"不碰钱"分支可言。
// 提交后副作用只剩单模型软限额告警(只读,不停服)。返回本次新落账的总消耗(quota,报表口径)。
func (s *Service) RunSettlement(ctx context.Context) (int64, error) {
	// B5:leader-only 准入(v1 环境判断,v2 换租约只改实现)。非 leader 直接 no-op,绝不写 ledger/扣费/推水位。
	if ok, _, err := s.leadership.CanRunTick(ctx); err != nil {
		return 0, err
	} else if !ok {
		return 0, nil
	}
	// 串行化所有 forward(settlement-worker + escrow-drain 两入口)与历史回填:三者共用 settlementMu,
	// 绝不并发写 ledger——回填/forward-补漏在 600s 重叠带靠 detail 幂等去重,并发即 TOCTOU 双算(24-§3.3)。
	s.settlementMu.Lock()
	defer s.settlementMu.Unlock()
	cur, err := s.store.GetOrCreateCursor(ctx, 0) // org_id=0 全局 leader 水位
	if err != nil {
		return 0, err
	}
	// M3 首跑基线(20-§6/19-F5):v1 从接入时点起观测、不回填历史——共用生产实例,历史日志绝大多数是主站
	// 普通客户的,全量回扫又慢又无用(还会经 API 翻几个月的页)。首跑(ts=0)只把水位推到 now−lag,下轮起增量。
	if cur.LastSettledTS == 0 {
		base := s.now().Unix() - settlementLagSec
		return 0, s.store.WithTx(ctx, func(tx *sql.Tx) error {
			ok, aerr := s.store.AdvanceCursorTx(ctx, tx, 0, base, 0, cur.Version)
			if aerr != nil {
				return aerr
			}
			if !ok {
				return errConcurrentSettlement
			}
			s.log.Info("结算首跑基线:水位置为接入时点,不回填历史", "base_ts", base)
			return nil
		})
	}
	since := cur.LastSettledTS
	until := s.now().Unix() - settlementLagSec
	if until <= since {
		return 0, nil // 窗口为空
	}

	// 路径B(GZ-01 修复2):上游 /api/log/ 写死 id desc(升序不可行),故绝不"截断后按已读最大 ts 推进"
	// (那会把未读到的最旧日志永久挡在窗外=少收)。改为:选一个 [since, untilSub] 子窗口使其日志数
	// <= settlementMaxPages*100(能一次完整读尽),只完整结算该子窗口、水位推到 untilSub;大 backlog 逐块排空。
	untilSub := until
	for {
		_, total, perr := s.upstream.ReadConsumptionLogs(ctx, since, untilSub, 1, 100)
		if perr != nil {
			return 0, mapUpstream(perr)
		}
		if total <= settlementMaxPages*100 {
			break // 该子窗口可一次读尽
		}
		if untilSub <= since+1 {
			// 单秒 > 2000 条的极端(当前流量不会到):告警,只能尽力处理这一秒(避免死循环卡住水位)。
			s.log.Error("结算单秒日志数超上限,可能截断(极端,请关注)", "since", since, "total", total)
			break
		}
		untilSub = since + (untilSub-since)/2 // 二分缩小子窗口
	}

	// 1) 聚合槽(1a 补漏 + 1b 主窗口两阶段共用;归因/聚合逻辑收口在 ingestEntry)。
	sk := newSettleSink(false)
	maxLogID := cur.LastSettledLogID

	// 1a) M3 补漏扫描(20-§6):lag 只挡"提交延迟<lag"的行;超 lag 迟提交的行(DB 顿一下/长事务)当轮 id 水位
	// 没看见、时间窗又推过去 → 不补扫即永久漏。重扫 [since−overlap, since),已入 usage_detail 的查重跳过。
	// **绝不在此推进 maxLogID**:该区间靠查重幂等;高水位只能在"完全可见"的主窗口内推进,否则时钟偏斜的
	// 大 id 会把主窗口外未读的行永久挡在水位下。
	// BUG-1(三总监验收):补漏区间上界必须 since−1——logs API 时间过滤双闭区间,补漏到 since 会与主窗口
	// [since, untilSub] 在 created_at==since 上重叠;一条恰在 since、迟提交(id>水位)的行会被两阶段各算一次
	// (detail 的 INSERT IGNORE 只防落库重复,同轮内存里 ledger 桶已累加两次)→ 报表多算、ledger 与 detail 背离。
	rescanSince := since - settlementRescanOverlapSec
	if rescanSince < 0 {
		rescanSince = 0
	}
	rescanUntil := since - 1
	var rescan []newapi.LogEntry
	for page := 1; page <= settlementRescanMaxPages; page++ {
		entries, total, rerr := s.upstream.ReadConsumptionLogs(ctx, rescanSince, rescanUntil, page, 100)
		if rerr != nil {
			return 0, mapUpstream(rerr)
		}
		for _, e := range entries {
			if e.Quota > 0 {
				rescan = append(rescan, e)
			}
		}
		if page*100 >= total {
			break
		}
		if page == settlementRescanMaxPages {
			// 区间行数超页上限(正常流量不会):部分补扫,区间随 since 推进滑动、下轮续扫;告警观察。
			s.log.Warn("补漏扫描区间行数超页上限,本轮部分补扫", "total", total, "pages", settlementRescanMaxPages)
		}
	}
	if len(rescan) > 0 {
		ids := make([]int64, len(rescan))
		for i, e := range rescan {
			ids[i] = e.ID
		}
		existing, ferr := s.store.FilterExistingDetailLogIDs(ctx, ids)
		if ferr != nil {
			return 0, apperr.Internal("").WithCause(ferr)
		}
		for _, e := range rescan {
			if existing[e.ID] {
				continue // 已落过明细(绝大多数)——ledger 桶是累加非按行幂等,靠此查重防重复计入
			}
			if err := s.ingestEntry(ctx, sk, e); err != nil {
				return 0, err
			}
			s.log.Warn("补漏:发现迟提交漏行,已补入本轮落账", "log_id", e.ID, "log_ts", e.CreatedAt)
		}
	}

	// 1b) 主窗口:完整读 [since, untilSub] 并聚合。log-id 级去重:只处理 id > 水位的日志,每条只计一次;
	// untilSub 已按提交可见性滞后(settlementLagSec)收边 → 该区间在读取时已完全可见,推进 id 高水位安全。
	for page := 1; page <= settlementMaxPages; page++ {
		entries, total, rerr := s.upstream.ReadConsumptionLogs(ctx, since, untilSub, page, 100)
		if rerr != nil {
			return 0, mapUpstream(rerr)
		}
		for _, e := range entries {
			if e.ID <= cur.LastSettledLogID {
				continue // 已结算过(水位去重),绝不重扣
			}
			if e.ID > maxLogID {
				maxLogID = e.ID
			}
			if e.Quota <= 0 {
				continue
			}
			if err := s.ingestEntry(ctx, sk, e); err != nil {
				return 0, err
			}
		}
		if page*100 >= total {
			break
		}
	}
	aggs, details := sk.aggs, sk.details

	// 留存桶(软限额检测用)+ 报表口径总消耗。本轮都是 id>水位 的新日志,每条只计一次。
	var settled []*bucketAgg
	var totalSettled int64
	for _, a := range aggs {
		settled = append(settled, a)
		totalSettled += a.consumed
	}

	// 2) 一个事务原子提交:落账(ledger 桶累加 + 逐条明细,与历史回填共用的**只写报表**段,24-§4.4)
	//    + 推水位到 untilSub(GZ-01 修复1/3)。任一步失败或 AdvanceCursor 未命中 → 回滚,下轮干净重做。
	//    架构B(33 §5/§12-9):原"逐组织扣 company_balance + 守恒断言 + 状态硬停"扣款分支已整体拆除
	//    (第二账砍掉,钱的执行在 new-api 原生双扣);落账无条件全组织写,报表是核心交付。
	txErr := s.store.WithTx(ctx, func(tx *sql.Tx) error {
		if err := s.commitUsageOnly(ctx, tx, aggs, details); err != nil {
			return err
		}
		// 水位推到 untilSub(子窗口已完整读尽);ok=false 即有并发写者改了 version,整批回滚(GZ-01 修复3)。
		ok, err := s.store.AdvanceCursorTx(ctx, tx, 0, untilSub, maxLogID, cur.Version)
		if err != nil {
			return err
		}
		if !ok {
			return errConcurrentSettlement
		}
		return nil
	})
	if txErr != nil {
		// 回滚:本轮不推水位,下一轮干净重做(无半截 ledger、无双计)。
		s.log.Error("结算事务回滚(下轮重做)", "err", txErr, "since", since, "until_sub", untilSub)
		return 0, txErr
	}

	// 3) 提交后副作用(绝不在持事务时调 new-api/HTTP):单模型日上限软限额检测
	//    (E4:跨阈值则告警;默认仅告警,只读不停服)。
	for _, a := range settled {
		s.checkModelSoftLimit(ctx, a)
	}
	return totalSettled, nil
}

// convergeOrgQuotas 对组织全部就绪成员经唯一下发出口 applyMemberOverride 重算下发(GZ-04 方案②即时收敛触发):
// 用于 recomputeOrgStatus 翻 stopped 进/出旗标后让硬停(→0)/恢复(→正常额)当拍生效。
// 硬停值由 applyMemberOverride 内的 gateByOrgStatus 按组织状态统一决策,故仍是单写者、不与 quota-worker 对撞
// (取代旧 hardStopOrg/restoreOrgQuotas 的"直接写0/直接下发"——那是绕过单一出口的双写者隐患,已删)。
func (s *Service) convergeOrgQuotas(ctx context.Context, orgID int64) {
	members, err := s.store.ListActiveOverridableMembers(ctx, orgID)
	if err != nil {
		s.log.Error("即时收敛:列成员失败", "org_id", orgID, "err", err)
		return
	}
	for _, m := range members {
		if _, err := s.applyMemberOverride(ctx, m); err != nil {
			s.log.Error("即时收敛:下发成员 quota 失败", "org_id", orgID, "member_id", m.ID, "err", err)
		}
	}
	s.auditSystem(ctx, orgID, "converge_quota", "organization", &orgID, map[string]any{"members": len(members)}, "ok")
}

// checkModelSoftLimit 检测某成员某模型今日累计是否刚跨过层级 model_cap;跨过则告警(E4,默认仅告警)。
// "收该模型权限"作可配升级项(改令牌 model_limits 去掉该模型),本期仅告警避免 logs 滞后误伤。
func (s *Service) checkModelSoftLimit(ctx context.Context, a *bucketAgg) {
	member, err := s.store.GetMember(ctx, a.orgID, a.memberID)
	if err != nil || member.TierID == nil {
		return
	}
	tier, err := s.store.GetTier(ctx, a.orgID, *member.TierID)
	if err != nil || len(tier.ModelCap) == 0 {
		return
	}
	cap, ok := tier.ModelCap[a.model]
	if !ok || cap <= 0 {
		return
	}
	now := s.now()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	sumToday, err := s.store.SumMemberModelToday(ctx, a.orgID, a.memberID, a.model, dayStart)
	if err != nil {
		return
	}
	// 仅在"本次落账刚把今日累计推过上限"时告警一次(天然去重)。
	if sumToday-a.consumed <= cap && sumToday > cap {
		title := fmt.Sprintf("单模型用量超限:%s", a.model)
		body := fmt.Sprintf("模型 %s 今日已用 %d 超上限 %d(软限额,默认仅告警)", a.model, sumToday, cap)
		s.notify(ctx, a.orgID, a.memberID, "soft_limit", title, body)
		if admins, e := s.store.ListOrgAdminIDs(ctx, a.orgID); e == nil {
			for _, aid := range admins {
				s.notify(ctx, a.orgID, aid, "soft_limit", title, body)
			}
		}
		s.auditSystem(ctx, a.orgID, "model_soft_limit_exceeded", "member", &a.memberID,
			map[string]any{"model": a.model, "today": sumToday, "cap": cap}, "ok")
	}
}

// billingReconcileTolerance 计费对账容差(quota):小额误差(in-flight 跨桶等)不告警。
const billingReconcileTolerance int64 = 0

// ReconcileBilling 报表对账(修少收 bug 的 part-b):
// 对"上一个完整小时",逐组织比对 new-api.logs 真实总额 vs usage_ledger 该小时桶总额,
// 不一致(尤其平台 < 真账 = 漏记)即告警(日志 + 审计 + 通知运营)。只读、不补扣,人工核对。
// 整点对齐使比对精确;只读一小时窗口(绝不全表)。
// 架构B(BE③ 归因改造):归因改按 user_id 主键(attributeUsage,与结算同一收口);
// billing_enabled 过滤随 company_balance 扣款退役一并去掉——本对账只管报表账,全平台组织都对。
func (s *Service) ReconcileBilling(ctx context.Context) error {
	if !s.fundingEnabled {
		return nil // v1 escrow 休眠(20-§9):worker 静默短路(v2 开 flag 恢复)
	}
	now := s.now()
	hourEnd := hourBucket(now.Unix())    // 当前小时开始
	hourStart := hourEnd.Add(-time.Hour) // 上一个完整小时开始

	// 1) new-api logs 上个小时各组织真实消耗(平台组织的日志;主站客户跳过)。
	logByOrg := map[int64]int64{}
	caches := newAttrCaches()
	since, until := hourStart.Unix(), hourEnd.Unix()-1
	// 单条日志归因累加(user_id 主键;未归因桶也计入本组织总额——与结算落账口径一致)。
	ingest := func(e newapi.LogEntry) error {
		if e.Quota <= 0 {
			return nil
		}
		att, aerr := s.attributeUsage(ctx, caches, int64(e.UserID), e.TokenID)
		if aerr != nil {
			return aerr
		}
		if att.skip {
			return nil
		}
		logByOrg[att.orgID] += e.Quota
		return nil
	}
	// B6b:整小时可能 >2000 行,原来固定 20 页硬截断 → 漏读 → 假"少收"告警刷屏。改子窗口二分(复刻结算手法),
	// 逐子窗口完整读尽、绝不截断。加 billing_enabled 组织过滤本就少量,基本一子窗口即完。
	for sub := since; sub <= until; {
		subEnd := until
		for {
			_, total, err := s.upstream.ReadConsumptionLogs(ctx, sub, subEnd, 1, 100)
			if err != nil {
				return mapUpstream(err)
			}
			if total <= settlementMaxPages*100 {
				break
			}
			if subEnd <= sub {
				s.log.Error("计费对账单秒日志数超上限(极端,请关注)", "sub", sub, "total", total)
				break
			}
			subEnd = sub + (subEnd-sub)/2 // 二分缩小子窗口
		}
		for page := 1; page <= settlementMaxPages; page++ {
			entries, total, err := s.upstream.ReadConsumptionLogs(ctx, sub, subEnd, page, 100)
			if err != nil {
				return mapUpstream(err)
			}
			for _, e := range entries {
				if ierr := ingest(e); ierr != nil {
					return ierr
				}
			}
			if page*100 >= total {
				break
			}
		}
		sub = subEnd + 1
	}

	// 2) usage_ledger 同小时桶各组织已结算消耗。
	ledgerByOrg, err := s.store.SumLedgerByOrgForBucket(ctx, hourStart)
	if err != nil {
		return err
	}

	// 3) 逐组织比对;平台 < 真账(少收)或偏差超容差 → 告警(只报不补)。
	for orgID, logged := range logByOrg {
		settled := ledgerByOrg[orgID]
		diff := logged - settled // >0 = 少收
		if diff > billingReconcileTolerance || diff < -billingReconcileTolerance {
			s.log.Error("计费对账不一致(疑少收/多收,人工核对)",
				"org_id", orgID, "hour", hourStart.Format(time.RFC3339), "newapi_logs", logged, "ledger", settled, "diff", diff)
			s.auditSystem(ctx, orgID, "billing_reconcile_mismatch", "balance", &orgID, map[string]any{
				"hour": hourStart.Format(time.RFC3339), "newapi_logs": logged, "ledger": settled, "diff": diff,
			}, "mismatch")
			for _, adminID := range s.orgAdminIDs(ctx, orgID) {
				s.notify(ctx, orgID, adminID, "billing_alert", "计费对账异常",
					"检测到本组织计费与上游用量不一致,运营方将核对处理")
			}
		}
	}
	return nil
}

// ReconcileBalanceLedger 已退役(架构B,33 §5/§12-9,BE③ 拆除):它对账的是 company_balance 第二账
// (total_consumed ↔ SUM(usage_ledger)),第二账已随扣款分支砍掉,余额=读求和派生(金库+Σ成员,实时读 new-api),
// 无本地余额账可对。划账守恒对账走 BE② 的 ReconcileTransfers(账本读-核-补环,33 §3.1)。
// 恒 no-op 保留签名:worker 调用点由 BE② 同批接手摘除,阶段2 组长合入对齐后连同本函数删除。
func (s *Service) ReconcileBalanceLedger(ctx context.Context) error {
	return nil
}

// PurgeOldUsageDetail 清理超过保留期的逐条明细;reconcile worker 周期调用。只删本库,不碰 new-api。
// 保留期可配(24-§6,NEXUS_USAGE_DETAIL_RETENTION_DAYS):**默认 0 = 永久保留(直接跳过,不删)** ——
// 历史全量回填后 detail 承载"逐条随时可查",故默认关清理;设正整数 N 才清 N 天前(量涨到千万行级再启用)。
func (s *Service) PurgeOldUsageDetail(ctx context.Context) error {
	if s.usageDetailRetentionDays <= 0 {
		return nil // 0 或未配 = 永久保留,不清理
	}
	cutoff := s.now().Add(-time.Duration(s.usageDetailRetentionDays) * 24 * time.Hour)
	n, err := s.store.PurgeUsageDetailBefore(ctx, cutoff)
	if err != nil {
		return err
	}
	if n > 0 {
		s.log.Info("用量明细保留清理", "purged", n, "cutoff", cutoff.Format(time.RFC3339), "retention_days", s.usageDetailRetentionDays)
	}
	return nil
}

// hourBucket 把 unix 秒取整到小时桶(UTC)。
func hourBucket(unixSec int64) time.Time {
	t := time.Unix(unixSec, 0).UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, time.UTC)
}
