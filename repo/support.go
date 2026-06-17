package repo

import (
	"context"
	"database/sql"
	"errors"

	"github.com/nexusapi-platform/enterprise/model"
)

// CreateSupportSession 建支持会话,返回 id。
func (s *Store) CreateSupportSession(ctx context.Context, ss *model.SupportSession) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO support_session (org_id, actor, on_behalf_of, scope, grant_type, state, expire_at)
		 VALUES (?, ?, ?, ?, ?, 'active', ?)`,
		ss.OrgID, ss.Actor, ss.OnBehalfOf, ss.Scope, ss.GrantType, ss.ExpireAt)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// GetSupportSession 取支持会话。
func (s *Store) GetSupportSession(ctx context.Context, id int64) (*model.SupportSession, error) {
	var ss model.SupportSession
	err := s.db.QueryRowContext(ctx,
		`SELECT id, org_id, actor, on_behalf_of, scope, grant_type, state, started_at, expire_at
		 FROM support_session WHERE id = ?`, id).Scan(
		&ss.ID, &ss.OrgID, &ss.Actor, &ss.OnBehalfOf, &ss.Scope, &ss.GrantType, &ss.State, &ss.StartedAt, &ss.ExpireAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &ss, nil
}

// RevokeSupportSession 吊销支持会话(运营方主动结束 / 客户撤销)。
func (s *Store) RevokeSupportSession(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE support_session SET state = 'revoked', revoked_at = CURRENT_TIMESTAMP(3) WHERE id = ? AND state = 'active'`, id)
	return err
}
