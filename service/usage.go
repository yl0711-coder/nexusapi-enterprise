package service

import (
	"context"
	"sort"
	"time"

	"github.com/nexusapi-platform/enterprise/model"
	"github.com/nexusapi-platform/enterprise/pkg/apperr"
	"github.com/nexusapi-platform/enterprise/pkg/session"
)

// usageMaxPages 单次用量查询最多读多少页 logs(小窗口,绝不全表)。
const usageMaxPages = 20

// UsageBucket 是一个聚合项(按模型或成员)。
type UsageBucket struct {
	Key           string `json:"key"`            // 模型名 / 成员标识
	ConsumedQuota int64  `json:"consumed_quota"` // 消耗 quota
	Count         int    `json:"count"`          // 调用次数
}

// UsageReport 是用量分析(看板,03 §3.1 读 logs 小窗口)。
type UsageReport struct {
	SinceHours int           `json:"since_hours"`
	TotalQuota int64         `json:"total_quota"`
	ByModel    []UsageBucket `json:"by_model"`
	ByMember   []UsageBucket `json:"by_member,omitempty"`
}

// OrgUsage 组织用量分析(O/A):读窗口内消费 logs,按模型 + 成员聚合(只算本 org 成员)。
func (s *Service) OrgUsage(ctx context.Context, c session.Claims, orgID int64, sinceHours int) (*UsageReport, error) {
	if err := assertOrgScope(c, orgID); err != nil {
		return nil, err
	}
	if err := assertRole(c, session.RoleOperator, session.RoleOrgAdmin); err != nil {
		return nil, err
	}
	return s.aggregateUsage(ctx, sinceHours, &orgID, nil)
}

// MemberUsage 成员用量(本人 / 上级 / 管理员)。
func (s *Service) MemberUsage(ctx context.Context, c session.Claims, orgID, memberID int64, sinceHours int) (*UsageReport, error) {
	if err := assertOrgScope(c, orgID); err != nil {
		return nil, err
	}
	m, err := s.store.GetMember(ctx, orgID, memberID)
	if err != nil {
		return nil, apperr.NotFound("成员不存在")
	}
	// 成员只能看本人;团队负责人本团队;管理员/运营方本 org。
	if err := assertSelf(c, memberID); err != nil {
		return nil, err
	}
	if c.Role == session.RoleTeamLeader {
		if err := assertTeamScope(c, m.TeamID); err != nil {
			return nil, err
		}
	}
	uid := m.NewapiUserID
	return s.aggregateUsage(ctx, sinceHours, &orgID, &uid)
}

// aggregateUsage 用量聚合(B4:已结算 usage_ledger 为主 + 当期未结小窗口 logs 为辅)。
// orgFilter 必给(看板按组织);userFilter!=nil 只算该 new-api user。历史读 ledger(分页安全、不压
// new-api);只对"结算游标→now"小窗口实时读 logs 补当期(限 2 页,绝不长段全量)。
func (s *Service) aggregateUsage(ctx context.Context, sinceHours int, orgFilter *int64, userFilter *int64) (*UsageReport, error) {
	if sinceHours <= 0 || sinceHours > 24*31 {
		sinceHours = 24
	}
	until := s.now().Unix()
	since := until - int64(sinceHours)*3600
	byModel := map[string]*UsageBucket{}
	byMember := map[string]*UsageBucket{}
	var total int64
	add := func(modelName string, uid, q int64) {
		total += q
		bm := byModel[modelName]
		if bm == nil {
			bm = &UsageBucket{Key: modelName}
			byModel[modelName] = bm
		}
		bm.ConsumedQuota += q
		bm.Count++
		mk := itoa(uid)
		bmem := byMember[mk]
		if bmem == nil {
			bmem = &UsageBucket{Key: mk}
			byMember[mk] = bmem
		}
		bmem.ConsumedQuota += q
		bmem.Count++
	}

	// 1) 已结算 ledger 为主。
	if orgFilter != nil {
		lm, lu, _, err := s.store.AggregateUsageLedger(ctx, *orgFilter, time.Unix(since, 0).UTC(), userFilter)
		if err != nil {
			return nil, apperr.Internal("").WithCause(err)
		}
		for m, q := range lm {
			bm := &UsageBucket{Key: m, ConsumedQuota: q, Count: 1}
			byModel[m] = bm
			total += q
		}
		for uid, q := range lu {
			byMember[itoa(uid)] = &UsageBucket{Key: itoa(uid), ConsumedQuota: q, Count: 1}
		}
	}

	// 2) 当期未结小窗口 logs(结算游标→now,限 2 页;读不到则看板降级只用 ledger)。
	cur, cerr := s.store.GetOrCreateCursor(ctx, 0)
	liveStart := since
	if cerr == nil && cur.LastSettledTS > liveStart {
		liveStart = cur.LastSettledTS
	}
	memberCache := map[int64]*model.Member{}
	for page := 1; page <= 2 && liveStart < until; page++ {
		entries, totalCnt, err := s.upstream.ReadConsumptionLogs(ctx, liveStart, until, page, 100)
		if err != nil {
			break
		}
		for _, e := range entries {
			if e.Quota <= 0 {
				continue
			}
			if userFilter != nil && int64(e.UserID) != *userFilter {
				continue
			}
			if orgFilter != nil {
				m := memberCache[int64(e.UserID)]
				if m == nil {
					mm, merr := s.store.GetMemberByNewapiUserID(ctx, int64(e.UserID))
					if merr != nil {
						memberCache[int64(e.UserID)] = &model.Member{}
						continue
					}
					memberCache[int64(e.UserID)] = mm
					m = mm
				}
				if m.ID == 0 || m.OrgID != *orgFilter {
					continue
				}
			}
			add(e.ModelName, int64(e.UserID), e.Quota)
		}
		if page*100 >= totalCnt {
			break
		}
	}

	rep := &UsageReport{SinceHours: sinceHours, TotalQuota: total, ByModel: sortBuckets(byModel)}
	if userFilter == nil {
		rep.ByMember = sortBuckets(byMember)
	}
	return rep, nil
}

func sortBuckets(m map[string]*UsageBucket) []UsageBucket {
	out := make([]UsageBucket, 0, len(m))
	for _, v := range m {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ConsumedQuota > out[j].ConsumedQuota })
	return out
}
