package service

import (
	"context"
	"sort"
	"time"

	"github.com/nexusapi-platform/enterprise/pkg/apperr"
	"github.com/nexusapi-platform/enterprise/pkg/session"
	"github.com/nexusapi-platform/enterprise/repo"
)

// usageMaxPages 单次用量查询最多读多少页 logs(小窗口,绝不全表)。
const usageMaxPages = 20

// UsageBucket 是一个聚合项(按模型或成员)。
type UsageBucket struct {
	Key           string `json:"key"`                 // 模型名 / 成员标识(byMember 时=new-api user_id)
	Label         string `json:"label,omitempty"`     // 改动④:byMember 的员工显示名(display_name||login_email);空=前端回落显示 Key
	MemberID      int64  `json:"member_id,omitempty"` // #5:byMember 的平台 member_id,供前端排行下钻调 /members/{id}/usage;非平台成员留空
	ConsumedQuota int64  `json:"consumed_quota"`      // 消耗 quota
	Count         int    `json:"count"`               // 调用次数
}

// UsageReport 是用量分析(看板,03 §3.1 读 logs 小窗口)。
type UsageReport struct {
	SinceHours int           `json:"since_hours"`
	TotalQuota int64         `json:"total_quota"`
	ByModel    []UsageBucket `json:"by_model"`
	ByMember   []UsageBucket `json:"by_member,omitempty"`
	ByTeam     []UsageBucket `json:"by_team,omitempty"` // F3:按团队(key=team_id,"0"=未分组);仅整组织看板(无 user/team 过滤时)填
}

// BudgetRef 是「额度参考条」最小只读数据(#4):只两个 raw 口径数,不带任何价/控字段。
// 架构B(BE③ 读口径改造,33 §5):预付口径从 company_balance.total_recharged(第二账,已退役)
// 改为**读求和余额**(金库+Σ成员实时读 new-api,单一真相);字段名随语义换为 balance_raw(FE 契约已广播)。
type BudgetRef struct {
	ConsumedQuota int64 `json:"consumed_quota"` // 已用(usage_ledger 累计 SUM,报表口径)
	BalanceRaw    int64 `json:"balance_raw"`    // 当前余额(读求和:金库 user.quota + Σ成员 user.quota)
}

// OrgBudgetRef 额度参考条(#4):给客户/运营看「已用 / 当前余额」两数,辅助成本感知。
//   - 只返两数,绝不带 ratio/折扣/低位阈值/计费开关。
//   - 已用从 usage_ledger 求和(与看板消耗同源);余额=读求和(orgBalanceTotals,全走 DB 不读缓存)。
//   - 藏价机制已废除(33 §12-7,ADR §9 镜像可见性):不再挂 mvpHidePrice/字段裁剪。
//   - org 作用域(assertOrgScope),operator + org_admin。
func (s *Service) OrgBudgetRef(ctx context.Context, c session.Claims, orgID int64) (*BudgetRef, error) {
	if err := assertOrgScope(c, orgID); err != nil {
		return nil, err
	}
	if err := assertRole(c, session.RoleOperator, session.RoleOrgAdmin); err != nil {
		return nil, err
	}
	// 已用:usage_ledger 全期累计(since=epoch),与看板消耗$同源;丢弃 by-model/by-user 明细只取 total。
	_, _, consumed, err := s.store.AggregateUsageLedger(ctx, orgID, time.Unix(0, 0).UTC(), nil)
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	bal, err := s.orgBalanceTotals(ctx, orgID)
	if err != nil {
		return nil, err
	}
	return &BudgetRef{ConsumedQuota: consumed, BalanceRaw: bal.TotalRaw}, nil
}

// OrgUsage 组织用量分析(O/A):读窗口内消费 logs,按模型 + 成员聚合(只算本 org 成员)。
func (s *Service) OrgUsage(ctx context.Context, c session.Claims, orgID int64, sinceHours int) (*UsageReport, error) {
	if err := assertOrgScope(c, orgID); err != nil {
		return nil, err
	}
	if err := assertRole(c, session.RoleOperator, session.RoleOrgAdmin); err != nil {
		return nil, err
	}
	return s.aggregateUsage(ctx, sinceHours, &orgID, nil, nil)
}

// TeamUsage 团队下钻用量(F3·org_admin/operator):某团队当前成员的 by_member + by_model + 总量(口径A)。
// teamID==0 → 未分组桶。仅本 org 任意 tid(assertOrgScope + assertRole);本期无 team_leader 作用域。
func (s *Service) TeamUsage(ctx context.Context, c session.Claims, orgID, teamID int64, sinceHours int) (*UsageReport, error) {
	if err := assertOrgScope(c, orgID); err != nil {
		return nil, err
	}
	if err := assertRole(c, session.RoleOperator, session.RoleOrgAdmin); err != nil {
		return nil, err
	}
	if teamID != 0 { // 指定团队须存在于本 org(未分组 0 不校验);跨 org → 404
		if _, err := s.store.GetTeam(ctx, orgID, teamID); err != nil {
			return nil, apperr.NotFound("团队不存在")
		}
	}
	tf := teamID
	return s.aggregateUsage(ctx, sinceHours, &orgID, nil, &tf)
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
	mid := m.ID // 模型2:按 member_id 归因(成员共享 org user)
	return s.aggregateUsage(ctx, sinceHours, &orgID, &mid, nil)
}

// UsageSeriesPoint 用量时间序列的一个点(折线图;period 标签 + 该期消耗 quota)。
type UsageSeriesPoint struct {
	Period        string `json:"period"`
	ConsumedQuota int64  `json:"consumed_quota"`
}

// validGranularity 收敛时间粒度白名单(day/week/month,非法回落 day)。
func validGranularity(g string) string {
	switch g {
	case "week", "month":
		return g
	default:
		return "day"
	}
}

// OrgUsageTimeSeries 组织用量时间序列(折线图,O/A;按 UTC+8 自然 day/week/month 上卷)。
func (s *Service) OrgUsageTimeSeries(ctx context.Context, c session.Claims, orgID int64, sinceHours int, granularity string) ([]UsageSeriesPoint, error) {
	if err := assertOrgScope(c, orgID); err != nil {
		return nil, err
	}
	if err := assertRole(c, session.RoleOperator, session.RoleOrgAdmin); err != nil {
		return nil, err
	}
	return s.usageTimeSeries(ctx, orgID, sinceHours, granularity, nil)
}

// MemberUsageTimeSeries 成员用量时间序列(本人/上级/管理员)。
func (s *Service) MemberUsageTimeSeries(ctx context.Context, c session.Claims, orgID, memberID int64, sinceHours int, granularity string) ([]UsageSeriesPoint, error) {
	if err := assertOrgScope(c, orgID); err != nil {
		return nil, err
	}
	m, err := s.store.GetMember(ctx, orgID, memberID)
	if err != nil {
		return nil, apperr.NotFound("成员不存在")
	}
	if err := assertSelf(c, memberID); err != nil {
		return nil, err
	}
	if c.Role == session.RoleTeamLeader {
		if err := assertTeamScope(c, m.TeamID); err != nil {
			return nil, err
		}
	}
	mid := m.ID // 模型2:按 member_id 归因(成员共享 org user)
	return s.usageTimeSeries(ctx, orgID, sinceHours, granularity, &mid)
}

// UsageDetailRecord 下钻明细一条(JSON;读 usage_detail,不查 new-api)。
type UsageDetailRecord struct {
	LogTS            string `json:"log_ts"` // RFC3339(该次调用发生时刻)
	ModelName        string `json:"model_name"`
	KeyID            int64  `json:"key_id"`
	MemberID         int64  `json:"member_id,omitempty"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	ConsumedQuota    int64  `json:"consumed_quota"`
}

// UsageDetailPage 下钻明细分页结果。
type UsageDetailPage struct {
	Records  []UsageDetailRecord `json:"records"`
	Total    int64               `json:"total"`
	Page     int                 `json:"page"`
	PageSize int                 `json:"page_size"`
}

const usageDetailMaxPageSize = 200

// maxUsageWindowHours 用量报表时间窗上限:≈30 年,支持"全部历史"档(前端 WIN_ALL=200000h≈22.8 年)。
// 原为 24*92(92天,当年绑 90 天 detail 保留);24-§6 detail 改永久保留 + 门B 全历史回填后,窗口须能覆盖老历史,
// 否则回填的历史被时间窗挡在外(客户第一眼以为没数据)。超此上限仍打回 24h(防呆/防滥用)。
const maxUsageWindowHours = 24 * 366 * 30

func clampDetailPage(page, pageSize int) (int, int) {
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 50
	}
	if pageSize > usageDetailMaxPageSize {
		pageSize = usageDetailMaxPageSize
	}
	return page, pageSize
}

func (s *Service) detailSince(sinceHours int) time.Time {
	if sinceHours <= 0 || sinceHours > maxUsageWindowHours {
		sinceHours = 24
	}
	return time.Unix(s.now().Unix()-int64(sinceHours)*3600, 0).UTC()
}

// OrgUsageDetail 组织下钻明细(O/A;可选 member/key/model 过滤;读本库逐条调用)。
func (s *Service) OrgUsageDetail(ctx context.Context, c session.Claims, orgID int64, sinceHours int, memberFilter, keyFilter *int64, model string, page, pageSize int) (*UsageDetailPage, error) {
	if err := assertOrgScope(c, orgID); err != nil {
		return nil, err
	}
	if err := assertRole(c, session.RoleOperator, session.RoleOrgAdmin); err != nil {
		return nil, err
	}
	return s.usageDetail(ctx, repo.DetailFilter{OrgID: orgID, MemberID: memberFilter, KeyID: keyFilter, Model: model, Since: s.detailSince(sinceHours)}, page, pageSize)
}

// MemberUsageDetail 成员下钻明细(本人/上级/管理员;锁定该 member)。
func (s *Service) MemberUsageDetail(ctx context.Context, c session.Claims, orgID, memberID int64, sinceHours, page, pageSize int) (*UsageDetailPage, error) {
	if err := assertOrgScope(c, orgID); err != nil {
		return nil, err
	}
	m, err := s.store.GetMember(ctx, orgID, memberID)
	if err != nil {
		return nil, apperr.NotFound("成员不存在")
	}
	if err := assertSelf(c, memberID); err != nil {
		return nil, err
	}
	if c.Role == session.RoleTeamLeader {
		if err := assertTeamScope(c, m.TeamID); err != nil {
			return nil, err
		}
	}
	mf := memberID
	return s.usageDetail(ctx, repo.DetailFilter{OrgID: orgID, MemberID: &mf, Since: s.detailSince(sinceHours)}, page, pageSize)
}

func (s *Service) usageDetail(ctx context.Context, f repo.DetailFilter, page, pageSize int) (*UsageDetailPage, error) {
	page, pageSize = clampDetailPage(page, pageSize)
	total, err := s.store.CountUsageDetail(ctx, f)
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	recs, err := s.store.ListUsageDetail(ctx, f, pageSize, (page-1)*pageSize)
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	out := make([]UsageDetailRecord, 0, len(recs))
	for _, r := range recs {
		out = append(out, UsageDetailRecord{
			LogTS: r.LogTS.UTC().Format(time.RFC3339), ModelName: r.ModelName, KeyID: r.KeyID, MemberID: r.MemberID,
			PromptTokens: r.PromptTokens, CompletionTokens: r.CompletionTokens, ConsumedQuota: r.ConsumedQuota,
		})
	}
	return &UsageDetailPage{Records: out, Total: total, Page: page, PageSize: pageSize}, nil
}

// usageTimeSeries 时间序列内部聚合(身份过滤已由调用方做);只读 usage_ledger,无 live-logs(看板趋势用已结算数据即可)。
func (s *Service) usageTimeSeries(ctx context.Context, orgID int64, sinceHours int, granularity string, memberFilter *int64) ([]UsageSeriesPoint, error) {
	if sinceHours <= 0 || sinceHours > maxUsageWindowHours {
		sinceHours = 24
	}
	since := time.Unix(s.now().Unix()-int64(sinceHours)*3600, 0).UTC()
	pts, err := s.store.AggregateUsageByTime(ctx, orgID, since, validGranularity(granularity), memberFilter, nil)
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	out := make([]UsageSeriesPoint, 0, len(pts))
	for _, p := range pts {
		out = append(out, UsageSeriesPoint{Period: p.Period, ConsumedQuota: p.Consumed})
	}
	return out, nil
}

// aggregateUsage 用量聚合(B4:已结算 usage_ledger 为主 + 当期未结小窗口 logs 为辅)。
// orgFilter 必给(看板按组织);userFilter!=nil 只算该 new-api user。历史读 ledger(分页安全、不压
// new-api);只对"结算游标→now"小窗口实时读 logs 补当期(限 2 页,绝不长段全量)。
// teamFilter:nil=不按团队过滤;*==0=未分组(team_id NULL);*>0=指定团队(口径A:成员当前 team_id)。
func (s *Service) aggregateUsage(ctx context.Context, sinceHours int, orgFilter *int64, memberFilter *int64, teamFilter *int64) (*UsageReport, error) {
	if sinceHours <= 0 || sinceHours > maxUsageWindowHours {
		sinceHours = 24
	}
	rep := &UsageReport{SinceHours: sinceHours}
	if orgFilter == nil {
		return rep, nil
	}
	since := time.Unix(s.now().Unix()-int64(sinceHours)*3600, 0).UTC()
	// 模型2:报表纯读已结算 ledger(settlement 已正确归因 member_id/key_id);不再补 live-logs——
	// 成员共享 org user,live-logs 无法按成员归因;settlement 每数秒一轮,滞后极小。
	var byModelQ map[string]int64
	var byMemberQ map[int64]int64 // member_id -> quota
	var err error
	if teamFilter != nil {
		byModelQ, byMemberQ, _, err = s.store.AggregateUsageLedgerByTeam(ctx, *orgFilter, since, *teamFilter)
	} else {
		byModelQ, byMemberQ, _, err = s.store.AggregateUsageLedger(ctx, *orgFilter, since, memberFilter)
	}
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	// A1(五路验收):调用次数 = usage_detail 底层请求数(每请求一行),绝非 ledger 桶行数(N 次会报成 1)。
	// team/member 视图按已聚合出的成员集限定 count(与 quota 口径一致);usage_detail 90 天保留,更早窗口 count 缺省 0。
	var cntMemberIDs []int64
	if teamFilter != nil || memberFilter != nil {
		for mid := range byMemberQ {
			cntMemberIDs = append(cntMemberIDs, mid)
		}
		if memberFilter != nil {
			cntMemberIDs = append(cntMemberIDs, *memberFilter)
		}
	}
	cntModel, cntMember, cerr := s.store.CountUsageDetailByDim(ctx, *orgFilter, since, cntMemberIDs)
	if cerr != nil {
		return nil, apperr.Internal("").WithCause(cerr)
	}
	byModel := map[string]*UsageBucket{}
	var total int64
	for m, q := range byModelQ {
		byModel[m] = &UsageBucket{Key: m, ConsumedQuota: q, Count: int(cntModel[m])}
		total += q
	}
	rep.TotalQuota = total
	rep.ByModel = sortBuckets(byModel)

	if memberFilter == nil {
		// by_member(按 member_id)补显示名 + 团队累加(口径A:成员当前 team_id);仅整组织看板(无 team 过滤)建 by_team。
		byMember := map[string]*UsageBucket{}
		byTeam := map[int64]int64{}
		for mid, q := range byMemberQ {
			b := &UsageBucket{Key: itoa(mid), ConsumedQuota: q, Count: int(cntMember[mid]), MemberID: mid}
			teamKey := int64(0) // 0=未分组,保证 Σby_team == Σby_member
			if m, merr := s.store.GetMember(ctx, *orgFilter, mid); merr == nil {
				if m.DisplayName != nil && *m.DisplayName != "" {
					b.Label = *m.DisplayName
				} else {
					b.Label = m.LoginEmail
				}
				if m.TeamID != nil {
					teamKey = *m.TeamID
				}
			}
			byMember[b.Key] = b
			byTeam[teamKey] += q
		}
		rep.ByMember = sortBuckets(byMember)
		if teamFilter == nil {
			rep.ByTeam = s.labelTeams(ctx, *orgFilter, byTeam)
		}
	}
	return rep, nil
}

// labelTeams 把 byTeam(team_id→quota)转成带团队名的桶;key=0→"未分组";查 ListTeams 一次解析名(归档/删名缺失回落"团队#id")。
func (s *Service) labelTeams(ctx context.Context, orgID int64, byTeam map[int64]int64) []UsageBucket {
	names := map[int64]string{}
	if teams, err := s.store.ListTeams(ctx, orgID); err == nil {
		for _, t := range teams {
			names[t.ID] = t.Name
		}
	}
	out := make([]UsageBucket, 0, len(byTeam))
	for tid, q := range byTeam {
		label := "未分组"
		if tid != 0 {
			if n, ok := names[tid]; ok {
				label = n
			} else {
				label = "团队#" + itoa(tid)
			}
		}
		out = append(out, UsageBucket{Key: itoa(tid), Label: label, ConsumedQuota: q})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ConsumedQuota > out[j].ConsumedQuota })
	return out
}

func sortBuckets(m map[string]*UsageBucket) []UsageBucket {
	out := make([]UsageBucket, 0, len(m))
	for _, v := range m {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ConsumedQuota > out[j].ConsumedQuota })
	return out
}
