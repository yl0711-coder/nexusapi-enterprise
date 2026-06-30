package service

import (
	"context"
	"time"

	"github.com/nexusapi-platform/enterprise/model"
	"github.com/nexusapi-platform/enterprise/repo"
)

// ResetDuePolicies 是周期重置(03 §3.3,leader 单写者):按 quota_policy 的 period 算当期重置边界,
// 跨过边界且本期未重置的策略 → 对其 scope 内成员重算 override 下发(把当期上限设回基线)。
// 返回本次重置的成员数。时区精化(reset_anchor 的 HH:MM / 按组织时区)为后续项,本期按 UTC 期初。
func (s *Service) ResetDuePolicies(ctx context.Context) (int, error) {
	if s.observeMode {
		return 0, nil // MVP(观测)下 quota-worker 绝不碰 new-api 写/停服(与 ReverseExpiredGrants:19 同口径,放行前必做)
	}
	policies, err := s.store.ListActivePoliciesForReset(ctx)
	if err != nil {
		return 0, err
	}
	now := s.now()
	reset := 0
	locCache := map[int64]*time.Location{}
	for _, p := range policies {
		loc := locCache[p.OrgID]
		if loc == nil {
			loc = time.UTC
			if org, oerr := s.store.GetOrganization(ctx, p.OrgID); oerr == nil && org.Timezone != "" {
				if l, lerr := time.LoadLocation(org.Timezone); lerr == nil {
					loc = l
				}
			}
			locCache[p.OrgID] = loc
		}
		boundary := periodBoundary(p.Period, now, loc)
		if boundary.IsZero() {
			continue
		}
		if p.LastResetAt != nil && !p.LastResetAt.Before(boundary) {
			continue // 本期已重置
		}
		members, err := s.policyScopeMembers(ctx, p)
		if err != nil {
			s.log.Error("周期重置取 scope 成员失败", "policy_id", p.ID, "err", err)
			continue
		}
		for _, m := range members {
			if m.BootstrapState != model.BootstrapDone || m.NewapiTokenID == nil {
				continue // 模型2:无令牌的成员无可执行额度(额度落 token.remain_quota)
			}
			if _, err := s.applyMemberOverride(ctx, m); err != nil {
				s.log.Error("周期重置下发 override 失败", "member_id", m.ID, "err", err)
				continue
			}
			reset++
		}
		if err := s.store.MarkPolicyReset(ctx, p.ID, now); err != nil {
			s.log.Error("记录策略重置点失败", "policy_id", p.ID, "err", err)
		}
		s.auditSystem(ctx, p.OrgID, "quota_reset", "quota_policy", &p.ScopeID, map[string]any{
			"scope": p.Scope, "period": p.Period, "members": len(members),
		}, "ok")
	}
	return reset, nil
}

// policyScopeMembers 取策略 scope 内的成员。
func (s *Service) policyScopeMembers(ctx context.Context, p *repo.QuotaPolicy) ([]*model.Member, error) {
	switch p.Scope {
	case "member":
		m, err := s.store.GetMember(ctx, p.OrgID, p.ScopeID)
		if err != nil {
			return nil, nil // 成员不存在则跳过
		}
		return []*model.Member{m}, nil
	case "team":
		all, err := s.store.ListActiveOverridableMembers(ctx, p.OrgID)
		if err != nil {
			return nil, err
		}
		var out []*model.Member
		for _, m := range all {
			if m.TeamID != nil && *m.TeamID == p.ScopeID {
				out = append(out, m)
			}
		}
		return out, nil
	default: // org
		return s.store.ListActiveOverridableMembers(ctx, p.OrgID)
	}
}

// periodBoundary 当期重置边界(按组织时区 loc 的本地期初,B3:anchor 到日界 00:00):
// daily=今日 0 点;weekly=本周一 0 点;monthly=本月 1 日 0 点。HH:MM 级 anchor 可后续细化。
func periodBoundary(period string, now time.Time, loc *time.Location) time.Time {
	ln := now.In(loc)
	y, mo, d := ln.Date()
	switch period {
	case "daily":
		return time.Date(y, mo, d, 0, 0, 0, 0, loc)
	case "weekly":
		wd := int(ln.Weekday()) // Sun=0
		if wd == 0 {
			wd = 7
		}
		return time.Date(y, mo, d, 0, 0, 0, 0, loc).AddDate(0, 0, -(wd - 1))
	case "monthly":
		return time.Date(y, mo, 1, 0, 0, 0, 0, loc)
	default:
		return time.Time{}
	}
}
