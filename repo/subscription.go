// 架构B 阶段1(BE②):订阅补满 worker 与对账恒等式扫描的只读查询。
// 独立文件不动 member.go/tier.go(模块文件边界,34 §1.3);全部 keyset 分页,按每组织数百成员设计(31-ADR §12-9)。
package repo

import (
	"context"
	"database/sql"
	"errors"

	"github.com/nexusapi-platform/enterprise/model"
)

// TopupCandidate 订阅补满候选:active+done 成员 × subscription 档位 × 未硬停组织,一次连接查全,
// worker 不逐成员回表。金额/额度全 raw。
type TopupCandidate struct {
	MemberID    int64
	OrgID       int64
	MemberUID   int    // 成员 new-api user id
	TreasuryUID int    // 金库 new-api user id
	TargetRaw   int64  // tier.amount_raw(补满目标)
	ResetPeriod string // daily | weekly | monthly
	OrgTimezone string // 组织时区(周期桶自然边界)
}

// ListSubscriptionTopupCandidates 列订阅补满候选(keyset:member.id > after,按 id 升序)。
// 过滤即护栏(31-ADR §4.3):跳过 disabled/offboarded/expired/pending/provisioning(status='active')、
// quarantined(bootstrap_state='done' 天然排除)、软删(deleted_at)、未开通(newapi_user_id NULL)、
// 非订阅档/坏档位、已归档/停用/硬停组织('low'=余额预警仍在营,照补)。
func (s *Store) ListSubscriptionTopupCandidates(ctx context.Context, afterMemberID int64, limit int) ([]TopupCandidate, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT m.id, m.org_id, m.newapi_user_id, o.newapi_user_id, t.amount_raw, t.reset_period, o.timezone
		   FROM member m
		   JOIN tier t         ON t.id = m.tier_id AND t.org_id = m.org_id
		   JOIN organization o ON o.id = m.org_id
		  WHERE m.id > ?
		    AND m.status = ? AND m.bootstrap_state = ? AND m.deleted_at IS NULL AND m.newapi_user_id IS NOT NULL
		    AND t.quota_type = 'subscription' AND t.status = ? AND t.amount_raw IS NOT NULL AND t.amount_raw > 0 AND t.reset_period IS NOT NULL
		    AND o.deleted_at IS NULL AND o.archived_at IS NULL AND o.status IN (?, ?) AND o.newapi_user_id IS NOT NULL
		  ORDER BY m.id ASC LIMIT ?`,
		afterMemberID, model.MemberStatusActive, model.BootstrapDone, model.StatusActive,
		model.OrgStatusActive, model.OrgStatusLow, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TopupCandidate
	for rows.Next() {
		var c TopupCandidate
		if err := rows.Scan(&c.MemberID, &c.OrgID, &c.MemberUID, &c.TreasuryUID, &c.TargetRaw, &c.ResetPeriod, &c.OrgTimezone); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// MemberUserRef 恒等式扫描的成员引用(**全量**:含 disabled/offboarded/quarantined/软删——
// 守恒审计不放过任何持 new-api user 的成员;username 供 /api/log/stat 权威消耗查询)。
type MemberUserRef struct {
	MemberID       int64
	OrgID          int64
	NewapiUserID   int64
	NewapiUsername string
}

// ListMembersWithNewapiUser keyset 遍历所有已开通 new-api user 的成员(不过滤状态/软删)。
func (s *Store) ListMembersWithNewapiUser(ctx context.Context, afterMemberID int64, limit int) ([]MemberUserRef, error) {
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, org_id, newapi_user_id, COALESCE(newapi_username, '')
		   FROM member WHERE id > ? AND newapi_user_id IS NOT NULL ORDER BY id ASC LIMIT ?`,
		afterMemberID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MemberUserRef
	for rows.Next() {
		var r MemberUserRef
		if err := rows.Scan(&r.MemberID, &r.OrgID, &r.NewapiUserID, &r.NewapiUsername); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetMemberAnyState 按 id 取成员**不滤软删**(离职退额原语用:offboarded 成员 deleted_at 已置,
// GetMember 查不到;钱面动作必须能触达离职行)。
func (s *Store) GetMemberAnyState(ctx context.Context, orgID, id int64) (*model.Member, error) {
	row := s.db.QueryRowContext(ctx, memberSelect+` WHERE id = ? AND org_id = ?`, id, orgID)
	return scanMember(row)
}

// TreasuryOrg 恒等式扫描的金库引用(金库低水位/被建 token 消费检测)。
type TreasuryOrg struct {
	OrgID            int64
	TreasuryUID      int64
	TreasuryUsername string
}

// ListTreasuryOrgs 列所有持金库 user 的组织(keyset)。
func (s *Store) ListTreasuryOrgs(ctx context.Context, afterOrgID int64, limit int) ([]TreasuryOrg, error) {
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, newapi_user_id, COALESCE(newapi_username, '')
		   FROM organization WHERE id > ? AND newapi_user_id IS NOT NULL AND deleted_at IS NULL
		  ORDER BY id ASC LIMIT ?`,
		afterOrgID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TreasuryOrg
	for rows.Next() {
		var t TreasuryOrg
		if err := rows.Scan(&t.OrgID, &t.TreasuryUID, &t.TreasuryUsername); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetUsernameByNewapiUserID 按 new-api user id 反查用户名(对账环日志取证用:manage 日志按 username 查)。
// 先查成员再查组织金库;found=false 表示非平台管辖 user(账本不该出现,调用方告警)。
func (s *Store) GetUsernameByNewapiUserID(ctx context.Context, uid int64) (username string, found bool, err error) {
	var u sql.NullString
	err = s.db.QueryRowContext(ctx,
		`SELECT newapi_username FROM member WHERE newapi_user_id = ? LIMIT 1`, uid).Scan(&u)
	if err == nil && u.Valid && u.String != "" {
		return u.String, true, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", false, err
	}
	err = s.db.QueryRowContext(ctx,
		`SELECT newapi_username FROM organization WHERE newapi_user_id = ? AND deleted_at IS NULL LIMIT 1`, uid).Scan(&u)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if !u.Valid || u.String == "" {
		return "", false, nil
	}
	return u.String, true, nil
}

// SumConsumedByNewapiUser usage_ledger(自建 bigint 台账,0004 起含 newapi_user_id)按 new-api user 求总消耗。
// 恒等式扫描的**诊断辅助值**(镜像口径,随结算水位滞后;判定主口径走 /api/log/stat 权威值)。
func (s *Store) SumConsumedByNewapiUser(ctx context.Context, newapiUserID int64) (int64, error) {
	var v sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT SUM(consumed_quota) FROM usage_ledger WHERE newapi_user_id = ?`, newapiUserID).Scan(&v)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	return v.Int64, nil
}
