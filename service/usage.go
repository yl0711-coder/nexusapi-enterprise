package service

import (
	"context"
	"sort"

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

// aggregateUsage 读窗口内消费 logs 聚合。orgFilter!=nil 只算该 org;userFilter!=nil 只算该 new-api user。
func (s *Service) aggregateUsage(ctx context.Context, sinceHours int, orgFilter *int64, userFilter *int64) (*UsageReport, error) {
	if sinceHours <= 0 || sinceHours > 24*31 {
		sinceHours = 24 // 默认近 24h
	}
	until := s.now().Unix()
	since := until - int64(sinceHours)*3600

	byModel := map[string]*UsageBucket{}
	byMember := map[string]*UsageBucket{}
	memberCache := map[int64]*model.Member{}
	var total int64

	for page := 1; page <= usageMaxPages; page++ {
		entries, totalCnt, err := s.upstream.ReadConsumptionLogs(ctx, since, until, page, 100)
		if err != nil {
			return nil, mapUpstream(err)
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
			total += e.Quota
			bm := byModel[e.ModelName]
			if bm == nil {
				bm = &UsageBucket{Key: e.ModelName}
				byModel[e.ModelName] = bm
			}
			bm.ConsumedQuota += e.Quota
			bm.Count++
			mk := itoa(int64(e.UserID))
			bmem := byMember[mk]
			if bmem == nil {
				bmem = &UsageBucket{Key: mk}
				byMember[mk] = bmem
			}
			bmem.ConsumedQuota += e.Quota
			bmem.Count++
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
