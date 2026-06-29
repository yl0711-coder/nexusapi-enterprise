package repo

import (
	"context"
	"database/sql"
	"errors"
)

// member_key 维护(v2 M0/M1):member_key_slot(稳定 key 槽,id=平台 key_id)+ member_key_token
// (物理 new-api 令牌,append-only)。member.newapi_token_id 仍是"当前主令牌指针"(兼容),
// 这两张表额外承载 1:N 与"轮换不断历史"(旧令牌行留存,历史日志按旧 token_id 仍映射回同一 key_id)。

// ensurePrimaryKeyTx 建成员主 key 槽 + 当前令牌,返回 key_id(slot.id)。开通成员首次落 key 用。
func (s *Store) ensurePrimaryKeyTx(ctx context.Context, x dbtx, orgID, memberID, newapiTokenID int64, tokenName, keyMasked string, rotation int) (int64, error) {
	res, err := x.ExecContext(ctx,
		`INSERT INTO member_key_slot (org_id, member_id, is_primary, status) VALUES (?, ?, 1, 'active')`,
		orgID, memberID)
	if err != nil {
		return 0, err
	}
	keyID, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err := insertKeyTokenTx(ctx, x, keyID, orgID, memberID, newapiTokenID, tokenName, keyMasked, rotation); err != nil {
		return 0, err
	}
	return keyID, nil
}

// insertKeyTokenTx 在指定 key 槽插一条 current 令牌行。
func insertKeyTokenTx(ctx context.Context, x dbtx, keyID, orgID, memberID, newapiTokenID int64, tokenName, keyMasked string, rotation int) error {
	_, err := x.ExecContext(ctx,
		`INSERT INTO member_key_token
		   (key_id, org_id, member_id, newapi_token_id, token_name, is_current, key_masked, rotation, status)
		 VALUES (?, ?, ?, ?, ?, 1, ?, ?, 'active')`,
		keyID, orgID, memberID, newapiTokenID, tokenName, keyMasked, rotation)
	return err
}

// rotatePrimaryKeyTx 在成员主槽上轮换令牌:旧 current 行置 is_current=0/superseded(留作历史归因),
// 再插新 current 行;若该成员尚无主槽(观测期首次自助建 key)则新建主槽。返回 key_id。
func (s *Store) rotatePrimaryKeyTx(ctx context.Context, x dbtx, orgID, memberID, newapiTokenID int64, tokenName, keyMasked string, rotation int) (int64, error) {
	var keyID int64
	err := x.QueryRowContext(ctx,
		`SELECT id FROM member_key_slot WHERE member_id = ? AND org_id = ? AND is_primary = 1 AND status = 'active' ORDER BY id LIMIT 1`,
		memberID, orgID).Scan(&keyID)
	if errors.Is(err, sql.ErrNoRows) {
		return s.ensurePrimaryKeyTx(ctx, x, orgID, memberID, newapiTokenID, tokenName, keyMasked, rotation)
	}
	if err != nil {
		return 0, err
	}
	if _, err := x.ExecContext(ctx,
		`UPDATE member_key_token SET is_current = 0, status = 'superseded' WHERE key_id = ? AND is_current = 1`, keyID); err != nil {
		return 0, err
	}
	if err := insertKeyTokenTx(ctx, x, keyID, orgID, memberID, newapiTokenID, tokenName, keyMasked, rotation); err != nil {
		return 0, err
	}
	return keyID, nil
}

// CreateAdditionalKey 为成员新建一个额外 key 槽(非主)+ 当前令牌,返回新 key_id(slot.id)。
// 真 1:N:一个成员可有多把独立 key,各自稳定 key_id,日志按 token_id 分别归因(主令牌指针 member.newapi_token_id
// 仍指向主槽令牌,额外 key 不动它)。
func (s *Store) CreateAdditionalKey(ctx context.Context, orgID, memberID, newapiTokenID int64, tokenName, keyMasked string, rotation int) (int64, error) {
	var keyID int64
	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		res, e := tx.ExecContext(ctx,
			`INSERT INTO member_key_slot (org_id, member_id, is_primary, status) VALUES (?, ?, 0, 'active')`, orgID, memberID)
		if e != nil {
			return e
		}
		keyID, e = res.LastInsertId()
		if e != nil {
			return e
		}
		return insertKeyTokenTx(ctx, tx, keyID, orgID, memberID, newapiTokenID, tokenName, keyMasked, rotation)
	})
	return keyID, err
}

// GetKeyIDByNewapiTokenID 按 new-api token id 反查平台稳定 key_id(结算归因用,M0-S2)。
// 无 org 谓词(leader 跨租户结算);命不中返 found=false(归因落到 key_id=0=未归因)。
func (s *Store) GetKeyIDByNewapiTokenID(ctx context.Context, newapiTokenID int64) (int64, bool, error) {
	var keyID int64
	err := s.db.QueryRowContext(ctx,
		`SELECT key_id FROM member_key_token WHERE newapi_token_id = ?`, newapiTokenID).Scan(&keyID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return keyID, true, nil
}
