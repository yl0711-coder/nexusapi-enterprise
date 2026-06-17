package service

import (
	"context"

	"github.com/nexusapi-platform/enterprise/pkg/apperr"
	"github.com/nexusapi-platform/enterprise/pkg/session"
)

// BulkRowResult 是批量操作的逐行结果(US-02:逐行可见,部分成功不回滚)。
type BulkRowResult struct {
	Name     string `json:"name,omitempty"`
	MemberID int64  `json:"member_id,omitempty"`
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
}

// bulkMaxRows 单批上限(防打爆 Executor / new-api,08 待拍板项,先给保守默认)。
const bulkMaxRows = 200

// BulkOpenMembers 批量开通成员(US-02):逐个限速执行 US-01(adapter 出口已限速),
// 逐行结果;部分成功不回滚;同批重名跳过。RBAC 复用 OpenMember(组织管理员/团队负责人)。
func (s *Service) BulkOpenMembers(ctx context.Context, c session.Claims, orgID int64, names []string, teamID, tierID *int64) ([]BulkRowResult, error) {
	if len(names) == 0 {
		return nil, apperr.InvalidParam("名单为空")
	}
	if len(names) > bulkMaxRows {
		return nil, apperr.InvalidParam("单批超过上限 200,请分批")
	}
	seen := map[string]bool{}
	out := make([]BulkRowResult, 0, len(names))
	for _, raw := range names {
		name := trimSpace(raw)
		if name == "" {
			continue
		}
		if seen[name] {
			out = append(out, BulkRowResult{Name: name, OK: false, Error: "同批重复,已跳过"})
			continue
		}
		seen[name] = true
		res, err := s.OpenMember(ctx, c, orgID, OpenMemberInput{Name: name, TeamID: teamID, TierID: tierID})
		if err != nil {
			e := apperr.Coerce(err)
			out = append(out, BulkRowResult{Name: name, OK: false, Error: e.Message})
			continue
		}
		out = append(out, BulkRowResult{Name: name, MemberID: res.MemberID, OK: true})
	}
	s.audit(ctx, c, orgID, "bulk_open_members", "member", nil, map[string]any{"total": len(names)})
	return out, nil
}

// BulkSetStatus 批量停用/恢复成员;逐个执行 + 逐行结果。
func (s *Service) BulkSetStatus(ctx context.Context, c session.Claims, orgID int64, memberIDs []int64, enabled bool) ([]BulkRowResult, error) {
	if len(memberIDs) == 0 {
		return nil, apperr.InvalidParam("未选择成员")
	}
	out := make([]BulkRowResult, 0, len(memberIDs))
	for _, mid := range memberIDs {
		if err := s.SetMemberStatus(ctx, c, orgID, mid, enabled); err != nil {
			out = append(out, BulkRowResult{MemberID: mid, OK: false, Error: apperr.Coerce(err).Message})
			continue
		}
		out = append(out, BulkRowResult{MemberID: mid, OK: true})
	}
	return out, nil
}

func trimSpace(s string) string {
	i, j := 0, len(s)
	for i < j && (s[i] == ' ' || s[i] == '\t' || s[i] == '\r' || s[i] == '\n') {
		i++
	}
	for j > i && (s[j-1] == ' ' || s[j-1] == '\t' || s[j-1] == '\r' || s[j-1] == '\n') {
		j--
	}
	return s[i:j]
}
