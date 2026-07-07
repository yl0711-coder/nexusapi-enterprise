// 架构B 阶段0(0030/33 §3.2):成员服务账号存取。
// 凭证密文(access_token/password,AES-256-GCM)不进 model.Member 常规读——单独方法取,防密文到处传。
package repo

import (
	"context"
	"database/sql"
	"errors"
)

// SetMemberServiceAccount 回填成员 new-api 服务账号(Provision saga 成功步骤后调;幂等可重写)。
func (s *Store) SetMemberServiceAccount(ctx context.Context, orgID, memberID int64, newapiUserID int64, username string, accessTokenEnc, passwordEnc []byte) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE member SET newapi_user_id = ?, newapi_username = ?, newapi_access_token_enc = ?, newapi_password_enc = ?
		  WHERE id = ? AND org_id = ?`,
		newapiUserID, username, accessTokenEnc, passwordEnc, memberID, orgID)
	return err
}

// UpdateMemberAccessToken 401 自愈后原子回存新 access_token(旋转语义:旧 token 已作废,必须立即落库)。
func (s *Store) UpdateMemberAccessToken(ctx context.Context, memberID int64, accessTokenEnc []byte) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE member SET newapi_access_token_enc = ? WHERE id = ?`, accessTokenEnc, memberID)
	return err
}

// MemberServiceCred 成员服务账号凭证(密文形态;解密在 service 层 keyring 做)。
type MemberServiceCred struct {
	MemberID       int64
	OrgID          int64
	NewapiUserID   int64
	NewapiUsername string
	AccessTokenEnc []byte
	PasswordEnc    []byte
}

// GetMemberServiceCred 取成员服务账号凭证密文。未开通(newapi_user_id NULL)返回 ok=false。
func (s *Store) GetMemberServiceCred(ctx context.Context, memberID int64) (*MemberServiceCred, bool, error) {
	var c MemberServiceCred
	var uid sql.NullInt64
	var uname sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT id, org_id, newapi_user_id, COALESCE(newapi_username,''), newapi_access_token_enc, newapi_password_enc
		   FROM member WHERE id = ?`, memberID).
		Scan(&c.MemberID, &c.OrgID, &uid, &uname, &c.AccessTokenEnc, &c.PasswordEnc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, ErrNotFound
	}
	if err != nil {
		return nil, false, err
	}
	if !uid.Valid || uid.Int64 == 0 {
		return nil, false, nil
	}
	c.NewapiUserID = uid.Int64
	c.NewapiUsername = uname.String
	return &c, true, nil
}

// MarkMemberQuarantined 孤儿隔离(34 §3-③):CreateUser 成功后续步骤失败,new-api 无干净删 user 能力 →
// disable(调用方已做)+ 本标记;worker/统计一律跳过 quarantined;可重试(重入走确定性 adopt 或新名重建)。
func (s *Store) MarkMemberQuarantined(ctx context.Context, orgID, memberID int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE member SET bootstrap_state = 'quarantined' WHERE id = ? AND org_id = ?`, memberID, orgID)
	return err
}

// ListMembersWithServiceAccount 列组织内已开通服务账号的成员(id+newapi_user_id;硬停 fan-out/上线闸遍历用)。
func (s *Store) ListMembersWithServiceAccount(ctx context.Context, orgID int64) ([]MemberServiceCred, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, org_id, newapi_user_id, COALESCE(newapi_username,''), newapi_access_token_enc, newapi_password_enc
		   FROM member WHERE org_id = ? AND newapi_user_id IS NOT NULL`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MemberServiceCred
	for rows.Next() {
		var c MemberServiceCred
		var uid sql.NullInt64
		var uname sql.NullString
		if err := rows.Scan(&c.MemberID, &c.OrgID, &uid, &uname, &c.AccessTokenEnc, &c.PasswordEnc); err != nil {
			return nil, err
		}
		c.NewapiUserID = uid.Int64
		c.NewapiUsername = uname.String
		out = append(out, c)
	}
	return out, rows.Err()
}
