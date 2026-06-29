package service

import (
	"context"
	"sort"
	"time"

	"github.com/nexusapi-platform/enterprise/model"
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

// BudgetRef 是「额度参考条」最小只读数据(#4·总监裁定走 B):只两个美元口径数,不带任何价/控字段。
type BudgetRef struct {
	ConsumedQuota  int64 `json:"consumed_quota"`  // 已用(usage_ledger 累计 SUM;非冻结的 company_balance.total_consumed)
	RechargedQuota int64 `json:"recharged_quota"` // 预付总额(company_balance.total_recharged)
}

// OrgBudgetRef 额度参考条(#4·B 方案):给客户/运营看「已用$ / 预付$」两数,辅助成本感知与垫钱敞口。
//
// 藏价红线(总监定):
//   - 这是藏价的"有意例外":客户看自己美元账单天经地义、不泄倍率,故本端点**不挂 mvpHidePrice**
//     (GetBalance/pricing/billing-settings 维持对客户 observe 下 404 不变,不开口子)。
//   - 只返两数,绝不带 ratio/折扣/低位阈值/退款明细/计费开关。
//   - 已用必须从 usage_ledger 求和(与看板消耗$同源):observe 下 company_balance.total_consumed 冻结,
//     读它会得 0/旧值;预付读 total_recharged(充值仍更新它,不冻结)。
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
	bal, err := s.store.GetOrCreateBalance(ctx, orgID)
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	return &BudgetRef{ConsumedQuota: consumed, RechargedQuota: bal.TotalRecharged}, nil
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
	uid := m.NewapiUserID
	return s.aggregateUsage(ctx, sinceHours, &orgID, &uid, nil)
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
	uid := m.NewapiUserID
	return s.usageTimeSeries(ctx, orgID, sinceHours, granularity, &uid)
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
	if sinceHours <= 0 || sinceHours > 24*92 {
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
func (s *Service) usageTimeSeries(ctx context.Context, orgID int64, sinceHours int, granularity string, userFilter *int64) ([]UsageSeriesPoint, error) {
	if sinceHours <= 0 || sinceHours > 24*92 {
		sinceHours = 24
	}
	since := time.Unix(s.now().Unix()-int64(sinceHours)*3600, 0).UTC()
	pts, err := s.store.AggregateUsageByTime(ctx, orgID, since, validGranularity(granularity), userFilter, nil)
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
func (s *Service) aggregateUsage(ctx context.Context, sinceHours int, orgFilter *int64, userFilter *int64, teamFilter *int64) (*UsageReport, error) {
	// 改动⑦:看板时间窗放到一季度(92 天),支持「近 90 天」选项;仍是只读聚合,无副作用。
	if sinceHours <= 0 || sinceHours > 24*92 {
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

	// 1) 已结算 ledger 为主。team 过滤走 JOIN member 变体(口径A);否则常规(可带 userFilter)。
	if orgFilter != nil {
		var lm map[string]int64
		var lu map[int64]int64
		var err error
		if teamFilter != nil {
			lm, lu, _, err = s.store.AggregateUsageLedgerByTeam(ctx, *orgFilter, time.Unix(since, 0).UTC(), *teamFilter)
		} else {
			lm, lu, _, err = s.store.AggregateUsageLedger(ctx, *orgFilter, time.Unix(since, 0).UTC(), userFilter)
		}
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
				if teamFilter != nil { // team 下钻:只算当前归属该团队(0=未分组)的成员
					tk := int64(0)
					if m.TeamID != nil {
						tk = *m.TeamID
					}
					if tk != *teamFilter {
						continue
					}
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
		// 改动④:给员工排行补显示名(user_id → 成员 display_name||login_email);查不到/非平台成员留空,前端回落显示 user_id。
		// 复用 memberCache(上面 live-logs 段已填部分),ledger-only 的成员在此补查;归账口径与结算一致(GetMemberByNewapiUserID)。
		// F3:同一循环顺带按团队累加(口径A,白嫖现成 member 反查的 TeamID),未分组归 key=0。仅整组织看板(无 team 过滤)建 by_team。
		byTeam := map[int64]int64{}
		for _, b := range byMember {
			uid := atoi64(b.Key)
			m := memberCache[uid]
			if m == nil {
				if mm, merr := s.store.GetMemberByNewapiUserID(ctx, uid); merr == nil {
					m = mm
					memberCache[uid] = mm
				} else {
					memberCache[uid] = &model.Member{}
				}
			}
			teamKey := int64(0) // 0=未分组(含查不到/非平台成员),保证 Σby_team == Σby_member(AC-F3-5)
			if m != nil && m.ID != 0 {
				b.MemberID = m.ID // #5:供前端排行下钻(点员工→/members/{id}/usage 拉其 by_model 明细)
				if m.DisplayName != nil && *m.DisplayName != "" {
					b.Label = *m.DisplayName
				} else {
					b.Label = m.LoginEmail
				}
				if m.TeamID != nil {
					teamKey = *m.TeamID
				}
			}
			byTeam[teamKey] += b.ConsumedQuota
		}
		rep.ByMember = sortBuckets(byMember)
		if teamFilter == nil && orgFilter != nil { // 仅整组织看板填 by_team(团队下钻自身不再分团队)
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
