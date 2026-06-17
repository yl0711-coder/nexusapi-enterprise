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
	aggs := map[string]*bucketAgg{}
	var maxProcessedTS int64 = since
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

	// 2) 去重落账 + 累计每组织新增消耗。
	perOrg := map[int64]int64{}
	for _, a := range aggs {
		inserted, err := s.store.UpsertLedgerBucket(ctx, &repo.LedgerBucket{
			OrgID: a.orgID, MemberID: a.memberID, NewapiUserID: a.newapiUserID, TeamID: a.teamID,
			ModelName: a.model, TimeBucket: a.bucket, ConsumedQuota: a.consumed,
			LogMaxTS: time.Unix(a.maxTS, 0).UTC(),
		})
		if err != nil {
			return 0, err
		}
		if inserted {
			perOrg[a.orgID] += a.consumed
		}
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
		// 守恒断言:balance 必须 == total_recharged - total_consumed。
		if bal.Balance != bal.TotalRecharged-bal.TotalConsumed {
			s.log.Error("守恒断言失败!", "org_id", orgID, "balance", bal.Balance,
				"recharged", bal.TotalRecharged, "consumed", bal.TotalConsumed)
		}
		if err := s.recomputeOrgStatus(ctx, orgID, bal); err != nil {
			s.log.Error("结算后重算组织状态失败", "org_id", orgID, "err", err)
		}
		s.auditSystem(ctx, orgID, "settlement_deduct", "balance", &orgID, map[string]any{
			"deducted": amount, "balance_after": bal.Balance,
		}, "ok")
	}

	// 4) 推进水位:全部读尽 → 推到 until;否则推到已处理的最大 ts(dedup 兜底重叠)。
	target := maxProcessedTS
	if drained {
		target = until
	}
	if target > since {
		if _, err := s.store.AdvanceCursor(ctx, 0, target, cur.Version); err != nil {
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

// hourBucket 把 unix 秒取整到小时桶(UTC)。
func hourBucket(unixSec int64) time.Time {
	t := time.Unix(unixSec, 0).UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, time.UTC)
}
