// 架构B 阶段1 BE③(33 §3.3,只读):读求和余额 / 归因 / 报表所需的只读查询。
// 本文件绝不写 quota、绝不写钱表;usage_ledger 自 0004 起即有 newapi_user_id 列(33 §12 组长复核)。
// 注:SumConsumedByNewapiUser 与 BE② 各自建(裁定),阶段2 组长合并去重。
package repo

import (
	"context"
	"database/sql"
	"errors"

	"github.com/nexusapi-platform/enterprise/model"
)

// GetMemberByNewapiUserID 架构B归因主键:按成员自己的 new-api user id 反查成员(33 §3.3 AttributeLog)。
// 无 org 谓词(leader 跨租户结算);**不过滤 deleted_at**(时点归因:离职成员的迟同步/历史消费仍归原成员)。
// 命不中(非平台成员 user,如金库/主站客户)返 found=false。
func (s *Store) GetMemberByNewapiUserID(ctx context.Context, newapiUserID int64) (*model.Member, bool, error) {
	row := s.db.QueryRowContext(ctx, memberSelect+` WHERE newapi_user_id = ?`, newapiUserID)
	m, err := scanMember(row)
	if errors.Is(err, ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return m, true, nil
}

// MemberBalanceSource 读求和余额的成员锚点(成员=各自 new-api user,user.quota=成员硬限额剩余)。
type MemberBalanceSource struct {
	MemberID     int64
	NewapiUserID int64
}

// ListMemberBalanceSources 列组织内参与读求和的成员(有服务账号;跳过 quarantined 孤儿——
// 33 §3.2:worker/统计一律跳过 quarantined;软删/离职成员已退额,quota≈0,不排除也不失真,
// 但离职语义上已不属组织额度,一并排除)。
func (s *Store) ListMemberBalanceSources(ctx context.Context, orgID int64) ([]MemberBalanceSource, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, newapi_user_id FROM member
		  WHERE org_id = ? AND deleted_at IS NULL AND newapi_user_id IS NOT NULL
		    AND bootstrap_state <> 'quarantined'
		  ORDER BY id ASC`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]MemberBalanceSource, 0)
	for rows.Next() {
		var m MemberBalanceSource
		if err := rows.Scan(&m.MemberID, &m.NewapiUserID); err != nil {
			return nil, err
		}
		if m.NewapiUserID == 0 {
			continue
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SumConsumedByNewapiUser 按成员 new-api user id 求 usage_ledger 累计消耗(成员"已用"口径,只读报表账)。
func (s *Store) SumConsumedByNewapiUser(ctx context.Context, newapiUserID int64) (int64, error) {
	var sum sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		`SELECT SUM(consumed_quota) FROM usage_ledger WHERE newapi_user_id = ?`, newapiUserID).Scan(&sum); err != nil {
		return 0, err
	}
	return sum.Int64, nil
}

// TreasuryRef 金库巡检锚点(金库低预警探针用,29-PRD §4.9)。
type TreasuryRef struct {
	OrgID        int64
	Name         string
	NewapiUserID int64
}

// ListOrgTreasuries 列全部未删组织的金库 user(newapi_user_id 语义=金库,0034)。
func (s *Store) ListOrgTreasuries(ctx context.Context) ([]TreasuryRef, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, newapi_user_id FROM organization
		  WHERE deleted_at IS NULL AND newapi_user_id IS NOT NULL AND archived_at IS NULL
		  ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]TreasuryRef, 0)
	for rows.Next() {
		var t TreasuryRef
		if err := rows.Scan(&t.OrgID, &t.Name, &t.NewapiUserID); err != nil {
			return nil, err
		}
		if t.NewapiUserID == 0 {
			continue
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// OperatorRef 运营方成员(站内通知投递对象;通知表按 org_id+member_id 定位)。
type OperatorRef struct {
	MemberID int64
	OrgID    int64
}

// ListOperatorMembers 列全部在职运营方成员(金库低预警"通知运营方"投递用)。
func (s *Store) ListOperatorMembers(ctx context.Context) ([]OperatorRef, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, org_id FROM member WHERE role = 'operator' AND deleted_at IS NULL AND status = 'active'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]OperatorRef, 0)
	for rows.Next() {
		var o OperatorRef
		if err := rows.Scan(&o.MemberID, &o.OrgID); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
