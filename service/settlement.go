package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nexusapi-platform/enterprise/adapter/newapi"
	"github.com/nexusapi-platform/enterprise/model"
	"github.com/nexusapi-platform/enterprise/repo"
)

// settlementLagSec 是结算滞后窗口:只结算 now-lag 之前的 log,避开 in-flight 写入(03 §4 软边界)。
const settlementLagSec int64 = 5

// settlementMaxPages 单次结算最多读多少页(每页 100),界住窗口、绝不全表(05 §1.1)。
const settlementMaxPages = 20

// bucketAgg 是一个 (org,user,model,小时桶) 的聚合累加。
type bucketAgg struct {
	orgID, memberID, newapiUserID int64
	teamID                        *int64
	model                         string
	bucket                        time.Time
	consumed                      int64
	maxTS                         int64
}

// RunSettlement 是计费结算的单次扫描(leader 单写者,03 §3.1):
// 读 new-api 消费 logs(小窗口增量,绝不全表)→ 按 (org,user,model,小时桶) 聚合 →
// usage_ledger 去重落账(命中去重键不重复扣)→ 扣对应组织 company_balance(乐观锁)→
// 守恒断言 → 推进水位 → 余额到 0 且开了硬停才硬停。**只结算开了 billing_enabled 的组织。**
// 返回本次新落账的总消耗(quota)。
func (s *Service) RunSettlement(ctx context.Context) (int64, error) {
	cur, err := s.store.GetOrCreateCursor(ctx, 0) // org_id=0 全局 leader 水位
	if err != nil {
		return 0, err
	}
	since := cur.LastSettledTS
	until := s.now().Unix() - settlementLagSec
	if until <= since {
		return 0, nil // 窗口为空
	}

	// 1) 分页读窗口内消费 logs,按桶聚合(只收 billing_enabled 组织的成员)。
	// log-id 级去重(修少收 bug):只处理 id > 水位的日志,每条只扣一次;边界重读(同 ts)靠此跳过。
	aggs := map[string]*bucketAgg{}
	var maxProcessedTS int64 = since
	maxLogID := cur.LastSettledLogID
	drained := false
	flagCache := map[int64]bool{} // orgID -> billing_enabled
	memberCache := map[int64]*model.Member{}

	for page := 1; page <= settlementMaxPages; page++ {
		entries, total, err := s.upstream.ReadConsumptionLogs(ctx, since, until, page, 100)
		if err != nil {
			return 0, mapUpstream(err)
		}
		for _, e := range entries {
			if e.CreatedAt > maxProcessedTS {
				maxProcessedTS = e.CreatedAt
			}
			if e.ID <= cur.LastSettledLogID {
				continue // 已结算过(水位去重),绝不重扣
			}
			if e.ID > maxLogID {
				maxLogID = e.ID
			}
			if e.Quota <= 0 {
				continue
			}
			m := memberCache[int64(e.UserID)]
			if m == nil {
				mm, merr := s.store.GetMemberByNewapiUserID(ctx, int64(e.UserID))
				if errors.Is(merr, repo.ErrNotFound) {
					memberCache[int64(e.UserID)] = &model.Member{} // 标记非平台成员,跳过
					continue
				}
				if merr != nil {
					return 0, mapUpstream(merr)
				}
				memberCache[int64(e.UserID)] = mm
				m = mm
			}
			if m.ID == 0 {
				continue // 非平台成员
			}
			billing, ok := flagCache[m.OrgID]
			if !ok {
				f, ferr := s.store.GetOrgBillingFlags(ctx, m.OrgID)
				if ferr != nil {
					return 0, ferr
				}
				billing = f.BillingEnabled
				flagCache[m.OrgID] = billing
			}
			if !billing {
				continue // 该组织未开计费,读到但不扣
			}
			bkt := hourBucket(e.CreatedAt)
			key := fmt.Sprintf("%d|%d|%s|%d", m.OrgID, e.UserID, e.ModelName, bkt.Unix())
			a := aggs[key]
			if a == nil {
				a = &bucketAgg{orgID: m.OrgID, memberID: m.ID, newapiUserID: int64(e.UserID), teamID: m.TeamID, model: e.ModelName, bucket: bkt}
				aggs[key] = a
			}
			a.consumed += e.Quota
			if e.CreatedAt > a.maxTS {
				a.maxTS = e.CreatedAt
			}
		}
		if page*100 >= total {
			drained = true
			break
		}
	}

	// 2) 落账(桶累计)+ 累计每组织新增消耗。本轮聚合的都是 id>水位 的新日志,每条只扣一次 → 全额计扣。
	perOrg := map[int64]int64{}
	var settled []*bucketAgg
	for _, a := range aggs {
		if err := s.store.AddToLedgerBucket(ctx, &repo.LedgerBucket{
			OrgID: a.orgID, MemberID: a.memberID, NewapiUserID: a.newapiUserID, TeamID: a.teamID,
			ModelName: a.model, TimeBucket: a.bucket, ConsumedQuota: a.consumed,
			LogMaxTS: time.Unix(a.maxTS, 0).UTC(),
		}); err != nil {
			return 0, err
		}
		perOrg[a.orgID] += a.consumed
		settled = append(settled, a)
	}

	// 3) 逐组织扣余额 + 守恒断言 + 状态/硬停。
	var totalDeducted int64
	for orgID, amount := range perOrg {
		if amount <= 0 {
			continue
		}
		bal, err := s.store.DeductBalance(ctx, orgID, amount)
		if errors.Is(err, repo.ErrOptimisticLock) {
			s.log.Warn("结算扣余额乐观锁冲突,下轮重试", "org_id", orgID)
			continue
		}
		if err != nil {
			return totalDeducted, err
		}
		totalDeducted += amount
		// 守恒断言:balance 必须 == total_recharged - total_refunded - total_consumed(R2-S1)。
		if bal.Balance != bal.TotalRecharged-bal.TotalRefunded-bal.TotalConsumed {
			s.log.Error("守恒断言失败!", "org_id", orgID, "balance", bal.Balance,
				"recharged", bal.TotalRecharged, "refunded", bal.TotalRefunded, "consumed", bal.TotalConsumed)
		}
		if err := s.recomputeOrgStatus(ctx, orgID, bal); err != nil {
			s.log.Error("结算后重算组织状态失败", "org_id", orgID, "err", err)
		}
		s.auditSystem(ctx, orgID, "settlement_deduct", "balance", &orgID, map[string]any{
			"deducted": amount, "balance_after": bal.Balance,
		}, "ok")
	}

	// 3.5) 单模型日上限软限额检测(E4:跨阈值则告警;默认仅告警,收权限可配)。
	for _, a := range settled {
		s.checkModelSoftLimit(ctx, a)
	}

	// 4) 推进水位:ts 全部读尽 → 推到 until;否则推到已处理的最大 ts;log 水位推到本轮处理的最大 id
	//    (边界同 ts 重读靠 log 水位去重,不重扣)。
	target := maxProcessedTS
	if drained {
		target = until
	}
	if target > since || maxLogID > cur.LastSettledLogID {
		if _, err := s.store.AdvanceCursor(ctx, 0, target, maxLogID, cur.Version); err != nil {
			s.log.Error("推进结算水位失败", "err", err)
		}
	}
	return totalDeducted, nil
}

// hardStopOrg 硬停:把组织内全部就绪成员 quota override 为 0(逐组织开关已开时才调,03 §3.1)。
func (s *Service) hardStopOrg(ctx context.Context, orgID int64) error {
	members, err := s.store.ListActiveOverridableMembers(ctx, orgID)
	if err != nil {
		return err
	}
	for _, m := range members {
		if err := s.upstream.ManageUserQuota(ctx, int(m.NewapiUserID), newapi.QuotaOverride, 0); err != nil {
			s.log.Error("硬停 override 0 失败", "member_id", m.ID, "err", err)
		}
	}
	s.auditSystem(ctx, orgID, "hard_stop", "organization", &orgID, map[string]any{"members": len(members)}, "ok")
	return nil
}

// restoreOrgQuotas 解硬停:充值后把成员 quota 重算下发恢复(03 §3.3)。
func (s *Service) restoreOrgQuotas(ctx context.Context, orgID int64) error {
	members, err := s.store.ListActiveOverridableMembers(ctx, orgID)
	if err != nil {
		return err
	}
	for _, m := range members {
		if _, err := s.applyMemberOverride(ctx, m); err != nil {
			s.log.Error("恢复成员 quota 失败", "member_id", m.ID, "err", err)
		}
	}
	s.auditSystem(ctx, orgID, "restore_quota", "organization", &orgID, map[string]any{"members": len(members)}, "ok")
	return nil
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
	sumToday, err := s.store.SumMemberModelToday(ctx, a.orgID, a.newapiUserID, a.model, dayStart)
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
			m := memberCache[int64(e.UserID)]
			if m == nil {
				mm, merr := s.store.GetMemberByNewapiUserID(ctx, int64(e.UserID))
				if errors.Is(merr, repo.ErrNotFound) {
					memberCache[int64(e.UserID)] = &model.Member{}
					continue
				}
				if merr != nil {
					return mapUpstream(merr)
				}
				memberCache[int64(e.UserID)] = mm
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

// hourBucket 把 unix 秒取整到小时桶(UTC)。
func hourBucket(unixSec int64) time.Time {
	t := time.Unix(unixSec, 0).UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, time.UTC)
}
