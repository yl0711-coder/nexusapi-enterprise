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
type settleSink struct {
	aggs          map[string]*bucketAgg
	details       []repo.DetailRow // 逐条明细(幂等键=newapi_log_id),与 ledger 同事务落
	flagCache     map[int64]bool   // orgID -> billing_enabled(只闸扣余额一步,裁定B)
	attrCache     map[int64]tokenAttr
	orgByUser     map[int64]int64 // newapi user_id -> 平台 org id(0=非平台组织;共用实例,主站客户日志按此跳过)
	orphanAlerted map[int64]bool  // token_id -> 已告警(每个孤儿令牌每轮只告警一次)
}

// ingestEntry 归因并聚合一条消费日志(M4 时点归因,20-§6):
//   - 平台成员 token → 归原持有成员(member_key_token append-only + member 不过滤软删 → 轮换/离职后历史不串不丢);
//   - 非成员 token 但 user 属平台组织 → **未知桶**(member_id=0,不丢行——门B 企业在 new-api 侧自建的令牌也在
//     花组织的钱,报表总额必须与消费日志对得上,19-F5);
//   - user 非平台组织(共用生产实例,主站普通客户的日志同在 logs 表)→ 跳过。
func (s *Service) ingestEntry(ctx context.Context, sk *settleSink, e newapi.LogEntry) error {
	att, cached := sk.attrCache[e.TokenID]
	if !cached {
		mm, kid, found, aerr := s.store.GetMemberByNewapiTokenID(ctx, e.TokenID)
		if aerr != nil {
			return apperr.Internal("").WithCause(aerr) // DB 错,非上游故障(P1-3:勿误走 mapUpstream)
		}
		att = tokenAttr{found: found, member: mm, keyID: kid}
		sk.attrCache[e.TokenID] = att
	}
	var m *model.Member
	var keyID int64
	if att.found {
		m, keyID = att.member, att.keyID
		// GZ-03:开通失败的成员令牌仍在消费 = 孤儿消费。告警使漏扣"可发现",仍正常聚合(money 不漏)。
		if m.BootstrapState == model.BootstrapFailed && !sk.orphanAlerted[e.TokenID] {
			sk.orphanAlerted[e.TokenID] = true
			s.log.Error("孤儿消费告警:开通失败的成员令牌仍在产生消费(需核 new-api)",
				"member_id", m.ID, "org_id", m.OrgID, "token_id", e.TokenID, "model", e.ModelName)
			s.auditSystem(ctx, m.OrgID, "orphan_consumption", "member", &m.ID, map[string]any{
				"token_id": e.TokenID, "model": e.ModelName,
			}, "alert")
		}
	} else {
		orgID, ok := sk.orgByUser[int64(e.UserID)]
		if !ok {
			id, found, oerr := s.store.GetOrgIDByNewapiUserID(ctx, int64(e.UserID))
			if oerr != nil {
				return apperr.Internal("").WithCause(oerr)
			}
			if !found {
				id = 0
			}
			orgID = id
			sk.orgByUser[int64(e.UserID)] = id
		}
		if orgID == 0 {
			return nil // 非平台组织(主站客户),跳过
		}
		m, keyID = &model.Member{ID: 0, OrgID: orgID}, 0 // M4 未知桶:member_id=0(前端显示"未归因")
	}
	// v1 裁定B(20-§2.1):落账与 billing_enabled 解耦(全组织落账);flag 只闸事务内扣余额一步。
	if _, ok := sk.flagCache[m.OrgID]; !ok {
		f, ferr := s.store.GetOrgBillingFlags(ctx, m.OrgID)
		if ferr != nil {
			return ferr
		}
		sk.flagCache[m.OrgID] = f.BillingEnabled
	}
	bkt := hourBucket(e.CreatedAt)
	key := fmt.Sprintf("%d|%d|%d|%s|%d", m.OrgID, e.UserID, keyID, e.ModelName, bkt.Unix())
	a := sk.aggs[key]
	if a == nil {
		a = &bucketAgg{orgID: m.OrgID, memberID: m.ID, newapiUserID: int64(e.UserID), keyID: keyID, teamID: m.TeamID, model: e.ModelName, bucket: bkt}
		sk.aggs[key] = a
	}
	a.consumed += e.Quota
	if e.CreatedAt > a.maxTS {
		a.maxTS = e.CreatedAt
	}
	sk.details = append(sk.details, repo.DetailRow{
		OrgID: m.OrgID, MemberID: m.ID, NewapiUserID: int64(e.UserID), KeyID: keyID, TeamID: m.TeamID,
		ModelName: e.ModelName, NewapiLogID: e.ID,
		PromptTokens: e.PromptTokens, CompletionTokens: e.CompletionTokens, ConsumedQuota: e.Quota,
		LogTS: time.Unix(e.CreatedAt, 0).UTC(),
	})
	return nil
}

// RunSettlement 是计费结算的单次扫描(leader 单写者,03 §3.1 / GZ-01 原子化版):
// 选一个不超限的时间子窗口(路径B,永不截断少收)→ 读 new-api 消费 logs 按 (org,user,model,小时桶) 聚合 →
// 在一个事务里原子提交:usage_ledger 落账 + 扣对应组织 company_balance(乐观锁,FOR UPDATE)+ 推进水位;
// 任一步失败或水位推进未命中即整批回滚、本轮不推水位、下轮干净重做(无双计无双扣)。
// 提交后再做守恒断言 / 状态-硬停 / 软限额 / 审计(绝不在持事务时调 new-api)。
// v1 裁定B(20-§2.1):**落账全组织无条件(报表是 v1 核心交付);billing_enabled 只闸"扣余额"一步**(平台执行扣费=v2)。
// 返回本次新落账的总消耗(quota)。
func (s *Service) RunSettlement(ctx context.Context) (int64, error) {
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
	sk := &settleSink{
		aggs:      map[string]*bucketAgg{},
		flagCache: map[int64]bool{}, attrCache: map[int64]tokenAttr{},
		orgByUser: map[int64]int64{}, orphanAlerted: map[int64]bool{},
	}
	maxLogID := cur.LastSettledLogID

	// 1a) M3 补漏扫描(20-§6):lag 只挡"提交延迟<lag"的行;超 lag 迟提交的行(DB 顿一下/长事务)当轮 id 水位
	// 没看见、时间窗又推过去 → 不补扫即永久漏。重扫 [since−overlap, since),已入 usage_detail 的查重跳过。
	// **绝不在此推进 maxLogID**:该区间靠查重幂等;高水位只能在"完全可见"的主窗口内推进,否则时钟偏斜的
	// 大 id 会把主窗口外未读的行永久挡在水位下。
	rescanSince := since - settlementRescanOverlapSec
	if rescanSince < 0 {
		rescanSince = 0
	}
	var rescan []newapi.LogEntry
	for page := 1; page <= settlementRescanMaxPages; page++ {
		entries, total, rerr := s.upstream.ReadConsumptionLogs(ctx, rescanSince, since, page, 100)
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
	aggs, details, flagCache := sk.aggs, sk.details, sk.flagCache

	// 聚合每组织新增消耗 + 留存桶(软限额用)。本轮都是 id>水位 的新日志,每条只扣一次 → 全额计扣。
	perOrg := map[int64]int64{}
	var settled []*bucketAgg
	for _, a := range aggs {
		perOrg[a.orgID] += a.consumed
		settled = append(settled, a)
	}

	// 2) 一个事务原子提交:落账 + 逐组织扣余额(读回) + 推水位到 untilSub(GZ-01 修复1/3)。
	//    任一步失败或 AdvanceCursor 未命中 → 回滚,本轮不推水位、不计扣,下轮干净重做。
	postBal := map[int64]*model.Balance{}
	var totalDeducted int64
	txErr := s.store.WithTx(ctx, func(tx *sql.Tx) error {
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
		// 改动⑤:observe 下整体跳过扣余额(只落账)。postBal 留空 → 提交后守恒断言/状态硬停/扣费审计
		// 全不触发(那些都靠 postBal 驱动),既不动钱也不触发任何停服/翻转。落账+推水位照常。
		if !s.observeMode {
			for orgID, amount := range perOrg {
				if amount <= 0 {
					continue
				}
				if !flagCache[orgID] { // v1 裁定B:billing_enabled 只管"平台执行扣费"——未开计费组织已落账但不扣余额
					continue
				}
				bal, err := s.store.DeductBalanceTx(ctx, tx, orgID, amount)
				if err != nil {
					return err // 含 ErrOptimisticLock(FOR UPDATE 下不应发生)/ErrNotFound;整批回滚
				}
				postBal[orgID] = bal
				totalDeducted += amount
			}
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
		// 回滚:本轮不推水位、不计扣,下一轮干净重做(无半截 ledger、无双扣)。
		s.log.Error("结算事务回滚(下轮重做)", "err", txErr, "since", since, "until_sub", untilSub)
		return 0, txErr
	}

	// 改动⑤:observe 模式落账完成 → 记一条可见日志(落了账、未扣钱),postBal 为空使下面副作用整体空转。
	if s.observeMode {
		var observed int64
		for _, amount := range perOrg {
			observed += amount
		}
		if len(aggs) > 0 {
			s.log.Info("结算·观测模式:已落账未扣钱(MVP)", "orgs", len(perOrg), "buckets", len(aggs),
				"observed_quota", observed, "until_sub", untilSub)
		}
	}

	// 3) 提交后副作用(绝不在持事务时调 new-api/HTTP):守恒断言 + 状态/硬停 + 软限额 + 审计。
	for orgID, bal := range postBal {
		// 守恒断言:balance 必须 == total_recharged - total_refunded - total_consumed(R2-S1)。
		if bal.Balance != bal.TotalRecharged-bal.TotalRefunded-bal.TotalConsumed {
			s.log.Error("守恒断言失败!", "org_id", orgID, "balance", bal.Balance,
				"recharged", bal.TotalRecharged, "refunded", bal.TotalRefunded, "consumed", bal.TotalConsumed)
		}
		if err := s.recomputeOrgStatus(ctx, orgID, bal); err != nil {
			s.log.Error("结算后重算组织状态失败", "org_id", orgID, "err", err)
		}
		s.auditSystem(ctx, orgID, "settlement_deduct", "balance", &orgID, map[string]any{
			"deducted": perOrg[orgID], "balance_after": bal.Balance,
		}, "ok")
	}
	// 单模型日上限软限额检测(E4:跨阈值则告警;默认仅告警,收权限可配)。
	for _, a := range settled {
		s.checkModelSoftLimit(ctx, a)
	}
	return totalDeducted, nil
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

// ReconcileBilling 计费对账(守恒断言的真账版,修少收 bug 的 part-b):
// 对"上一个完整小时",逐组织比对 new-api.logs 真实总额 vs usage_ledger 该小时桶总额,
// 不一致(尤其平台 < 真账 = 少收)即告警(日志 + 审计 + 通知运营)。只读、不补扣,人工核对。
// 整点对齐使比对精确;只读一小时窗口(绝不全表)。
func (s *Service) ReconcileBilling(ctx context.Context) error {
	if !s.fundingEnabled {
		return nil // v1 escrow 休眠(20-§9):worker 静默短路(v2 开 flag 恢复)
	}
	now := s.now()
	hourEnd := hourBucket(now.Unix())    // 当前小时开始
	hourStart := hourEnd.Add(-time.Hour) // 上一个完整小时开始

	// 1) new-api logs 上个小时各组织真实消耗(只收 billing_enabled 组织的平台成员)。
	logByOrg := map[int64]int64{}
	memberCache := map[int64]*model.Member{}
	flagCache := map[int64]bool{}
	since, until := hourStart.Unix(), hourEnd.Unix()-1
	for page := 1; page <= settlementMaxPages; page++ {
		entries, total, err := s.upstream.ReadConsumptionLogs(ctx, since, until, page, 100)
		if err != nil {
			return mapUpstream(err)
		}
		for _, e := range entries {
			if e.Quota <= 0 {
				continue
			}
			// 模型2:按 token_id 归因(成员共享 org user)。缓存按 token_id。
			m := memberCache[e.TokenID]
			if m == nil {
				mm, _, found, merr := s.store.GetMemberByNewapiTokenID(ctx, e.TokenID)
				if merr != nil {
					return apperr.Internal("").WithCause(merr) // DB 错,非上游故障(P1-3:勿误走 mapUpstream)
				}
				if !found {
					memberCache[e.TokenID] = &model.Member{} // 非平台 token,标记跳过
					continue
				}
				memberCache[e.TokenID] = mm
				m = mm
			}
			if m.ID == 0 {
				continue
			}
			billing, ok := flagCache[m.OrgID]
			if !ok {
				f, ferr := s.store.GetOrgBillingFlags(ctx, m.OrgID)
				if ferr != nil {
					return ferr
				}
				billing = f.BillingEnabled
				flagCache[m.OrgID] = billing
			}
			if !billing {
				continue
			}
			logByOrg[m.OrgID] += e.Quota
		}
		if page*100 >= total {
			break
		}
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

// ReconcileBalanceLedger 余额-台账真账对账(GZ-01 修复4 / 治 D4):校验每组织
// company_balance.total_consumed == SUM(usage_ledger.consumed_quota)。GZ-01 把"落账+扣余额"收进同一
// 事务后两者天然一致;本对账是纵深防御,兜住任何未预期分歧(尤其"ledger 写了但余额没扣"的少收——
// 这类 ReconcileBilling 发现不了,因为它比的是 logs↔ledger)。只读、不补扣,不一致即告警人工核对。
// 注:比的是累计值,若灰度前历史数据已有分歧会一并报出(可后续设基线;本期作信息性告警)。
func (s *Service) ReconcileBalanceLedger(ctx context.Context) error {
	if !s.fundingEnabled {
		return nil // v1 escrow 休眠(20-§9):worker 静默短路(v2 开 flag 恢复)
	}
	// MVP 观测模式防御(改动⑥-3,2026-06-24 拍板):observe 下 ledger 照写但 total_consumed 不动(没扣)→
	// 二者必然背离 → 本对账每轮误报。故 observe 下整段跳过。即便有人为看真账把整个 reconcile worker 跑起来也不误报。
	if s.observeMode {
		return nil
	}
	ledger, err := s.store.SumLedgerConsumedByOrg(ctx)
	if err != nil {
		return err
	}
	consumed, err := s.store.ListOrgTotalConsumed(ctx)
	if err != nil {
		return err
	}
	seen := map[int64]bool{}
	for orgID := range consumed {
		seen[orgID] = true
	}
	for orgID := range ledger {
		seen[orgID] = true
	}
	for orgID := range seen {
		tc, lg := consumed[orgID], ledger[orgID]
		if tc == lg {
			continue
		}
		s.log.Error("余额-台账对账不一致(GZ-01 D4,人工核对)",
			"org_id", orgID, "total_consumed", tc, "ledger_sum", lg, "diff", tc-lg)
		s.auditSystem(ctx, orgID, "balance_ledger_mismatch", "balance", &orgID, map[string]any{
			"total_consumed": tc, "ledger_sum": lg, "diff": tc - lg,
		}, "mismatch")
	}
	return nil
}

// usageDetailRetentionDays 逐条明细保留天数(13 §4.4:下钻明细落库保 90 天)。
const usageDetailRetentionDays = 90

// PurgeOldUsageDetail 清理超过保留期(90 天)的逐条明细;reconcile worker 周期调用。只删本库,不碰 new-api。
func (s *Service) PurgeOldUsageDetail(ctx context.Context) error {
	cutoff := s.now().Add(-time.Duration(usageDetailRetentionDays) * 24 * time.Hour)
	n, err := s.store.PurgeUsageDetailBefore(ctx, cutoff)
	if err != nil {
		return err
	}
	if n > 0 {
		s.log.Info("用量明细保留清理", "purged", n, "cutoff", cutoff.Format(time.RFC3339))
	}
	return nil
}

// hourBucket 把 unix 秒取整到小时桶(UTC)。
func hourBucket(unixSec int64) time.Time {
	t := time.Unix(unixSec, 0).UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, time.UTC)
}
