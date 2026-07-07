// 架构B 阶段1 BE③(33 §3.3 OrgBalance,只读):读求和余额。
//
// 组织"总余额" = 金库 user.quota + Σ(成员 user.quota),全部实时读 new-api **DB 真值**
// (adapter GetUserQuota 走 GET /api/user/:id,fromDB;31-ADR §2 护栏:对账/求和绝不读缓存——
// 缓存与 DB 非事务、会漂移)。本文件一行 quota 都不写、绝不调 Transfer(34 §4 BE③ 护栏)。
package service

import (
	"context"
	"errors"
	"sync"

	"github.com/nexusapi-platform/enterprise/pkg/apperr"
	"github.com/nexusapi-platform/enterprise/pkg/session"
	"github.com/nexusapi-platform/enterprise/repo"
)

// balanceSumConcurrency Σ成员 GetUserQuota 的并发上界(按数百成员设计:有界并发,不串行也不打爆上游;
// adapter Executor 另有全局限速兜底)。
const balanceSumConcurrency = 8

// OrgBalanceView 组织读求和余额(字段名=组长契约裁定:treasury_raw/members_total_raw/total_raw/low)。
type OrgBalanceView struct {
	OrgID           int64 `json:"org_id"`
	TreasuryRaw     int64 `json:"treasury_raw"`      // 金库 user.quota(实时读 DB)
	MembersTotalRaw int64 `json:"members_total_raw"` // Σ成员 user.quota
	TotalRaw        int64 `json:"total_raw"`         // 金库 + Σ成员
	Low             bool  `json:"low"`               // 金库低于平台预警阈值 treasury_low_watermark_raw
	MemberCount     int   `json:"member_count"`      // 参与求和的成员数(跳过 quarantined/未开通)
}

// OrgBalance 读求和余额(GET /organizations/:id/balance;operator + org_admin)。
func (s *Service) OrgBalance(ctx context.Context, c session.Claims, orgID int64) (*OrgBalanceView, error) {
	if err := assertOrgScope(c, orgID); err != nil {
		return nil, err
	}
	if err := assertRole(c, session.RoleOperator, session.RoleOrgAdmin); err != nil {
		return nil, err
	}
	v, err := s.orgBalanceTotals(ctx, orgID)
	if err != nil {
		return nil, err
	}
	// 金库低预警旗标(29-PRD §4.9;阈值=平台配置,<=0 视为未启用)。
	if wm, werr := s.store.GetSettingInt64(ctx, "treasury_low_watermark_raw", 0); werr == nil && wm > 0 && v.TreasuryRaw < wm {
		v.Low = true
	}
	return v, nil
}

// orgBalanceTotals 读求和内部实现(RBAC 由调用方做):金库 + 有界并发 Σ成员。
// 任一读失败即整体失败(部分求和=虚假余额,涉钱读宁缺勿假)。
func (s *Service) orgBalanceTotals(ctx context.Context, orgID int64) (*OrgBalanceView, error) {
	org, err := s.store.GetOrganization(ctx, orgID)
	if errors.Is(err, repo.ErrNotFound) {
		return nil, apperr.NotFound("组织不存在")
	}
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	v := &OrgBalanceView{OrgID: orgID}
	if org.NewapiUserID != nil && *org.NewapiUserID != 0 {
		t, gerr := s.upstream.GetUserQuota(ctx, int(*org.NewapiUserID))
		if gerr != nil {
			return nil, mapUpstream(gerr)
		}
		v.TreasuryRaw = t
	}
	members, err := s.store.ListMemberBalanceSources(ctx, orgID)
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	sum, err := s.sumMemberQuotas(ctx, members)
	if err != nil {
		return nil, err
	}
	v.MembersTotalRaw = sum
	v.MemberCount = len(members)
	v.TotalRaw = v.TreasuryRaw + v.MembersTotalRaw
	return v, nil
}

// sumMemberQuotas 有界并发读全部成员 user.quota(全走 DB)并求和;任一失败返回错误。
func (s *Service) sumMemberQuotas(ctx context.Context, members []repo.MemberBalanceSource) (int64, error) {
	if len(members) == 0 {
		return 0, nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		total    int64
		firstErr error
	)
	sem := make(chan struct{}, balanceSumConcurrency)
	for _, m := range members {
		m := m
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if ctx.Err() != nil {
				return
			}
			q, err := s.upstream.GetUserQuota(ctx, int(m.NewapiUserID))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = mapUpstream(err)
					cancel() // 快速失败:后续读没有意义(部分求和不可用)
				}
				return
			}
			total += q
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return 0, firstErr
	}
	return total, nil
}

// memberExhaustedNotice 成员额度用尽的对客文案(29-PRD §4.9:错误文案映射,不暴露内部机制)。
const memberExhaustedNotice = "额度已用完,请联系管理员"

// MemberBalanceView 成员本人额度视图(GET /me/balance;29-PRD §4.7:额度/已用/剩余)。
type MemberBalanceView struct {
	Provisioned   bool   `json:"provisioned"`               // 是否已开通服务账号(false=下列数值无意义)
	RemainingRaw  int64  `json:"remaining_raw"`             // 剩余 = 成员 user.quota 实时真值(诚实口径,可能短暂略负)
	ConsumedRaw   int64  `json:"consumed_raw"`              // 已用 = usage_ledger 按成员 user 累计(报表口径)
	GrantedNetRaw int64  `json:"granted_net_raw"`           // 累计净划入 = Σ到账 − Σ退回(分配账本 applied 口径)
	Exhausted     bool   `json:"exhausted"`                 // 额度已用完(remaining<=0)
	Notice        string `json:"notice,omitempty"`          // 用尽时的对客提示文案
}

// MyBalance 成员看自己的额度/已用/剩余(镜像 new-api 普通用户可见性,31-ADR §9)。
// operator 无成员额度语义 → 403;org_admin/team_leader/member 都可看"自己"。
func (s *Service) MyBalance(ctx context.Context, c session.Claims) (*MemberBalanceView, error) {
	if err := assertRole(c, session.RoleOrgAdmin, session.RoleTeamLeader, session.RoleMember); err != nil {
		return nil, err
	}
	m, err := s.store.GetMember(ctx, c.OrgID, c.MemberID)
	if errors.Is(err, repo.ErrNotFound) {
		return nil, apperr.NotFound("成员不存在")
	}
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	v := &MemberBalanceView{}
	if m.NewapiUserID == nil || *m.NewapiUserID == 0 {
		return v, nil // 未开通服务账号:Provisioned=false,数值全 0
	}
	v.Provisioned = true
	remaining, gerr := s.upstream.GetUserQuota(ctx, int(*m.NewapiUserID))
	if gerr != nil {
		return nil, mapUpstream(gerr)
	}
	v.RemainingRaw = remaining
	consumed, cerr := s.store.SumConsumedByNewapiUser(ctx, *m.NewapiUserID)
	if cerr != nil {
		return nil, apperr.Internal("").WithCause(cerr)
	}
	v.ConsumedRaw = consumed
	net, nerr := s.store.SumAppliedNetByUser(ctx, *m.NewapiUserID)
	if nerr != nil {
		return nil, apperr.Internal("").WithCause(nerr)
	}
	v.GrantedNetRaw = net
	v.Exhausted, v.Notice = memberExhaustedState(remaining)
	return v, nil
}

// memberExhaustedState 额度用尽判定 + 文案映射(纯函数,单测):剩余<=0 即用尽
// (new-api 原生双扣不透支;在途请求可短暂略负,同样视为用尽)。
func memberExhaustedState(remainingRaw int64) (bool, string) {
	if remainingRaw <= 0 {
		return true, memberExhaustedNotice
	}
	return false, ""
}
