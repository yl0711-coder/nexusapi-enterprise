// 架构B 阶段1 BE③(31-ADR §15 账本可见性,只读):分配账本三级可见。
//   超管全可见;组织管理员可见本组织流水;成员仅可见给自己的到账(to_user=本人)。
// repo.ListTransfers(BE② 地基)已留可见性入参,本文件只做 RBAC 收敛 + 视图脱敏。
// 只读:绝不写 ledger_transfer(写路径归 BE② service/ledger.go)。
package service

import (
	"context"
	"errors"
	"time"

	"github.com/nexusapi-platform/enterprise/pkg/apperr"
	"github.com/nexusapi-platform/enterprise/pkg/session"
	"github.com/nexusapi-platform/enterprise/repo"
)

// 账本方向(组长契约裁定:显式回 direction,从 from/to 相对金库 uid 派生,不让前端靠金额符号猜)。
const (
	LedgerDirCredit = "credit" // 金库→成员 入账(划拨/补满/恢复)
	LedgerDirDebit  = "debit"  // 成员→金库 退额(离职/回收)
)

// LedgerEntryView 账本行视图(按角色脱敏:new-api 内部 user id 与幂等键只给超管,31-ADR §8 内部 id 原则)。
type LedgerEntryView struct {
	ID             int64      `json:"id"`
	OrgID          int64      `json:"org_id"`
	MemberID       int64      `json:"member_id,omitempty"`
	Direction      string     `json:"direction"` // credit / debit(相对金库派生;金库不明时空串)
	AmountRaw      int64      `json:"amount_raw"`
	Status         string     `json:"status"` // pending / applied / failed
	Reason         string     `json:"reason"`
	CreatedBy      string     `json:"created_by,omitempty"`      // 超管+组织管理员
	FromUserID     int64      `json:"from_user_id,omitempty"`    // 仅超管
	ToUserID       int64      `json:"to_user_id,omitempty"`      // 仅超管
	IdempotencyKey string     `json:"idempotency_key,omitempty"` // 仅超管
	CreatedAt      time.Time  `json:"created_at"`
	AppliedAt      *time.Time `json:"applied_at,omitempty"`
}

// ListOrgLedger 本组织划账流水(GET /organizations/:id/ledger;operator + org_admin,分页)。
func (s *Service) ListOrgLedger(ctx context.Context, c session.Claims, orgID int64, limit, offset int) ([]LedgerEntryView, int, error) {
	if err := assertOrgScope(c, orgID); err != nil {
		return nil, 0, err
	}
	if err := assertRole(c, session.RoleOperator, session.RoleOrgAdmin); err != nil {
		return nil, 0, err
	}
	if _, err := s.store.GetOrganization(ctx, orgID); errors.Is(err, repo.ErrNotFound) {
		return nil, 0, apperr.NotFound("组织不存在")
	} else if err != nil {
		return nil, 0, apperr.Internal("").WithCause(err)
	}
	rows, total, err := s.store.ListTransfers(ctx, repo.LedgerFilter{OrgID: orgID, Limit: limit, Offset: offset})
	if err != nil {
		return nil, 0, apperr.Internal("").WithCause(err)
	}
	views, err := s.ledgerViews(ctx, c.Role, rows)
	return views, total, err
}

// ListMyLedger 成员看给自己的到账(GET /me/ledger;33 §3.5:仅 to_user=本人)。
// operator 无成员身份 → 403。成员未开通服务账号 → 空列表(还没有账可到)。
func (s *Service) ListMyLedger(ctx context.Context, c session.Claims, limit, offset int) ([]LedgerEntryView, int, error) {
	if err := assertRole(c, session.RoleOrgAdmin, session.RoleTeamLeader, session.RoleMember); err != nil {
		return nil, 0, err
	}
	m, err := s.store.GetMember(ctx, c.OrgID, c.MemberID)
	if errors.Is(err, repo.ErrNotFound) {
		return nil, 0, apperr.NotFound("成员不存在")
	}
	if err != nil {
		return nil, 0, apperr.Internal("").WithCause(err)
	}
	if m.NewapiUserID == nil || *m.NewapiUserID == 0 {
		return []LedgerEntryView{}, 0, nil
	}
	// 双重收敛:member_id=本人(账本可见性冗余列,0031)且 to_user=本人(只看到账,不看退回)。
	rows, total, err := s.store.ListTransfers(ctx, repo.LedgerFilter{
		OrgID: c.OrgID, MemberID: c.MemberID, ToUserID: *m.NewapiUserID, Limit: limit, Offset: offset,
	})
	if err != nil {
		return nil, 0, apperr.Internal("").WithCause(err)
	}
	views, err := s.ledgerViews(ctx, c.Role, rows)
	return views, total, err
}

// ListAllLedger 全平台流水(GET /ledger;仅运营方,可筛 org)。
func (s *Service) ListAllLedger(ctx context.Context, c session.Claims, orgID int64, limit, offset int) ([]LedgerEntryView, int, error) {
	if err := assertRole(c, session.RoleOperator); err != nil {
		return nil, 0, err
	}
	rows, total, err := s.store.ListTransfers(ctx, repo.LedgerFilter{OrgID: orgID, Limit: limit, Offset: offset})
	if err != nil {
		return nil, 0, apperr.Internal("").WithCause(err)
	}
	views, err := s.ledgerViews(ctx, c.Role, rows)
	return views, total, err
}

// ledgerViews 账本行→视图:派生 direction(相对金库 uid)+ 按角色脱敏。金库 uid 按 org 缓存一轮。
func (s *Service) ledgerViews(ctx context.Context, role session.Role, rows []*repo.LedgerTransfer) ([]LedgerEntryView, error) {
	treasury := map[int64]int64{} // orgID -> 金库 newapi_user_id(0=未知)
	out := make([]LedgerEntryView, 0, len(rows))
	for _, t := range rows {
		uid, ok := treasury[t.OrgID]
		if !ok {
			uid = 0
			if org, err := s.store.GetOrganization(ctx, t.OrgID); err == nil && org.NewapiUserID != nil {
				uid = *org.NewapiUserID
			}
			treasury[t.OrgID] = uid
		}
		out = append(out, ledgerEntryView(t, uid, role))
	}
	return out, nil
}

// ledgerEntryView 单行视图(纯函数,单测):direction 派生 + 角色脱敏。
//   - direction:from=金库 → credit(给成员入账);to=金库 → debit(成员退回);金库未知 → 空串。
//   - 脱敏:from/to_user_id、idempotency_key 仅超管;created_by 超管+组织管理员(成员不给操作者标识)。
func ledgerEntryView(t *repo.LedgerTransfer, treasuryUID int64, role session.Role) LedgerEntryView {
	v := LedgerEntryView{
		ID: t.ID, OrgID: t.OrgID, MemberID: t.MemberID,
		AmountRaw: t.AmountRaw, Status: t.Status, Reason: t.Reason,
		CreatedAt: t.CreatedAt, AppliedAt: t.AppliedAt,
	}
	switch {
	case treasuryUID != 0 && t.FromUserID == treasuryUID:
		v.Direction = LedgerDirCredit
	case treasuryUID != 0 && t.ToUserID == treasuryUID:
		v.Direction = LedgerDirDebit
	}
	if role == session.RoleOperator {
		v.FromUserID = t.FromUserID
		v.ToUserID = t.ToUserID
		v.IdempotencyKey = t.IdempotencyKey
	}
	if role == session.RoleOperator || role == session.RoleOrgAdmin {
		v.CreatedBy = t.CreatedBy
	}
	return v
}
