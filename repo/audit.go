package repo

import (
	"context"

	"github.com/nexusapi-platform/enterprise/model"
)

// WriteAudit 追加一条审计(09 §13;留痕是 AC 一等公民,08 §0.4)。
// detail 必须已脱敏(绝不含明文 key / 密文凭证),由 service 保证。
func (s *Store) WriteAudit(ctx context.Context, e *model.AuditEntry) error {
	if e.Result == "" {
		e.Result = "ok"
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_log (org_id, actor, on_behalf_of, support_session_id, action, target_type, target_id, detail, result, request_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.OrgID, e.Actor, e.OnBehalfOf, e.SupportSessionID, e.Action, e.TargetType, e.TargetID, e.Detail, e.Result, e.RequestID)
	return err
}

// ListAuditLogs 按 org 取审计(分页,倒序)。
func (s *Store) ListAuditLogs(ctx context.Context, orgID int64, limit, offset int) ([]*model.AuditEntry, int, error) {
	var total int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM audit_log WHERE org_id = ?`, orgID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT org_id, actor, on_behalf_of, support_session_id, action, target_type, target_id, detail, result, request_id
		 FROM audit_log WHERE org_id = ? ORDER BY id DESC LIMIT ? OFFSET ?`, orgID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []*model.AuditEntry
	for rows.Next() {
		var e model.AuditEntry
		if err := rows.Scan(&e.OrgID, &e.Actor, &e.OnBehalfOf, &e.SupportSessionID, &e.Action,
			&e.TargetType, &e.TargetID, &e.Detail, &e.Result, &e.RequestID); err != nil {
			return nil, 0, err
		}
		out = append(out, &e)
	}
	return out, total, rows.Err()
}
