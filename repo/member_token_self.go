// 架构B 阶段1(BE① 身份层):成员自助令牌的归属账 + 档位授权反查。
//
//   - 归属真相:member_key_token(append-only,token↔member;稳定 key_id 供日志归因续用);
//     new-api 侧 token 天然挂成员自己的 user 下(第二把真锁,31-ADR §10)。
//   - 令牌数上限:事务内「锁成员行 → count → insert」(33 §3.5),防并发建超;
//     上游 token 先建后记账,记账被拒由调用方补偿删除上游 token。
package repo

import (
	"context"
	"database/sql"
	"errors"
)

// ErrTokenLimit 令牌数达上限(事务内 count+insert 判定;调用方翻译为 409)。
var ErrTokenLimit = errors.New("member token limit reached")

// ListMemberAuthorizedGroups 反查成员被授权档位的分组集合(31-ADR §5 护栏:
// 分组下拉/校验只列该成员被授权档位的分组,不是全组织可用分组)。
// 授权并集 = visibility='all' 的档位 ∪ 成员自身绑定档位 ∪ tier_grant(member)∪ tier_grant(team)。
// 返回原始 newapi_group 值(NULL → 空串,由 service 层回落组织默认/default)。
func (s *Store) ListMemberAuthorizedGroups(ctx context.Context, orgID, memberID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT COALESCE(t.newapi_group, '')
		   FROM tier t
		  WHERE t.org_id = ? AND t.deleted_at IS NULL AND t.status = 'active' AND (
		        t.visibility = 'all'
		     OR EXISTS (SELECT 1 FROM tier_grant g WHERE g.org_id = t.org_id AND g.tier_id = t.id AND g.target_type = 'all')
		     OR t.id = (SELECT m.tier_id FROM member m WHERE m.id = ? AND m.org_id = ?)
		     OR EXISTS (SELECT 1 FROM tier_grant g
		                 WHERE g.org_id = t.org_id AND g.tier_id = t.id
		                   AND g.target_type = 'member' AND g.target_id = ?)
		     OR EXISTS (SELECT 1 FROM tier_grant g
		                  JOIN member m2 ON m2.id = ? AND m2.org_id = g.org_id AND m2.team_id IS NOT NULL
		                 WHERE g.org_id = t.org_id AND g.tier_id = t.id
		                   AND g.target_type = 'team' AND g.target_id = m2.team_id)
		  )`, orgID, memberID, orgID, memberID, memberID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// InsertMemberTokenWithLimit 记一枚成员自助令牌归属行(独立 key 槽 + current 令牌行),
// **事务内 count+insert**(33 §3.5):锁成员行(SELECT ... FOR UPDATE)串行化同成员并发建 →
// 统计 current+active 令牌数 → ≥limit 返回 ErrTokenLimit(整事务回滚)→ 否则插槽+行。
// 返回新 key_id。调用方须在本方法失败时补偿删除已建的上游 token(先建上游后记账)。
func (s *Store) InsertMemberTokenWithLimit(ctx context.Context, orgID, memberID int64, limit int, newapiTokenID int64, tokenName, keyMasked string) (int64, error) {
	var keyID int64
	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		// 行锁:同成员并发建令牌在此串行化(防两请求同时通过 count 预检绕过上限)。
		var lockID int64
		if err := tx.QueryRowContext(ctx,
			`SELECT id FROM member WHERE id = ? AND org_id = ? FOR UPDATE`, memberID, orgID).Scan(&lockID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		var n int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM member_key_token
			  WHERE org_id = ? AND member_id = ? AND is_current = 1 AND status = 'active'`, orgID, memberID).Scan(&n); err != nil {
			return err
		}
		if limit > 0 && n >= limit {
			return ErrTokenLimit
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO member_key_slot (org_id, member_id, is_primary, status) VALUES (?, ?, 0, 'active')`, orgID, memberID)
		if err != nil {
			return err
		}
		keyID, err = res.LastInsertId()
		if err != nil {
			return err
		}
		return insertKeyTokenTx(ctx, tx, keyID, orgID, memberID, newapiTokenID, tokenName, keyMasked, 1)
	})
	if err != nil {
		return 0, err
	}
	return keyID, nil
}

// MemberOwnsToken 归属校验:该 new-api token 是否为该成员的 current+active 自助令牌。
// (纵深:new-api 侧 token CRUD 本就绑成员 user;这里是平台侧第一道 404 闸,不暴露他人 token 存在性。)
func (s *Store) MemberOwnsToken(ctx context.Context, orgID, memberID, newapiTokenID int64) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM member_key_token
		  WHERE org_id = ? AND member_id = ? AND newapi_token_id = ? AND is_current = 1 AND status = 'active'`,
		orgID, memberID, newapiTokenID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// RevokeMemberToken 删令牌后的归属收尾(append-only:行置 revoked 不删,历史日志仍按旧 token_id 归因)。
func (s *Store) RevokeMemberToken(ctx context.Context, orgID, memberID, newapiTokenID int64) error {
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		var keyID int64
		err := tx.QueryRowContext(ctx,
			`SELECT key_id FROM member_key_token
			  WHERE org_id = ? AND member_id = ? AND newapi_token_id = ? AND is_current = 1`,
			orgID, memberID, newapiTokenID).Scan(&keyID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // 已收尾(幂等)
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE member_key_token SET is_current = 0, status = 'revoked'
			  WHERE key_id = ? AND newapi_token_id = ?`, keyID, newapiTokenID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE member_key_slot SET status = 'revoked' WHERE id = ?`, keyID)
		return err
	})
}

// CountMemberActiveTokens 成员当前 current+active 自助令牌数(成员详情「令牌数」展示;
// 上限强判仍走 InsertMemberTokenWithLimit 的事务内 count)。
func (s *Store) CountMemberActiveTokens(ctx context.Context, orgID, memberID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM member_key_token
		  WHERE org_id = ? AND member_id = ? AND is_current = 1 AND status = 'active'`, orgID, memberID).Scan(&n)
	return n, err
}
