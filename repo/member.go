package repo

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/nexusapi-platform/enterprise/model"
)

// MemberFilter 是成员列表的过滤条件(10 §1.5;对应原型 members 页 _mq/_mteam/_mstatus)。
type MemberFilter struct {
	Q      string // 关键词:匹配 display_name / login_email
	TeamID *int64
	Status string
	Limit  int
	Offset int
}

// CreateMemberProvisional 先建 provisioning 中间态成员行(尚无 new-api 用户),返回 member.id。
// service 用此 id 派生确定性 username 后再 bootstrap(10 §2.5),成功后调 FinalizeBootstrap。
func (s *Store) CreateMemberProvisional(ctx context.Context, m *model.Member) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO member (org_id, team_id, login_email, display_name, role, tier_id,
		    status, platform_password_hash, bootstrap_state)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.OrgID, m.TeamID, m.LoginEmail, m.DisplayName, m.Role, m.TierID,
		model.MemberStatusProvisioning, m.PlatformPasswordHash, model.BootstrapPending)
	if err != nil {
		if isDupKey(err) {
			return 0, ErrConflict
		}
		return 0, err
	}
	return res.LastInsertId()
}

// FinalizeBootstrap 在代发 key 成功后回填成员:new-api 用户/令牌、密文凭证、脱敏 key,
// 置 bootstrap_state=done、status=active。
func (s *Store) FinalizeBootstrap(ctx context.Context, m *model.Member) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE member
		    SET newapi_user_id = ?, access_token_enc = ?, member_password_enc = ?,
		        newapi_token_id = ?, key_masked = ?, key_rotation = ?,
		        bootstrap_state = ?, status = ?, bootstrapped_at = CURRENT_TIMESTAMP(3)
		  WHERE id = ? AND org_id = ?`,
		m.NewapiUserID, m.AccessTokenEnc, m.MemberPasswordEnc,
		m.NewapiTokenID, m.KeyMasked, m.KeyRotation,
		model.BootstrapDone, model.MemberStatusActive,
		m.ID, m.OrgID)
	return err
}

// MarkBootstrapFailed 把成员标记为 bootstrap 失败(补偿后:不展示半截账号,10 §2.3)。
func (s *Store) MarkBootstrapFailed(ctx context.Context, orgID, memberID int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE member SET bootstrap_state = ?, status = ? WHERE id = ? AND org_id = ?`,
		model.BootstrapFailed, model.MemberStatusProvisioning, memberID, orgID)
	return err
}

// UpdateMemberKey 轮换后回填新令牌 id / 脱敏 key / 轮换计数(US-07)。
func (s *Store) UpdateMemberKey(ctx context.Context, orgID, memberID, tokenID int64, keyMasked string, rotation int) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE member SET newapi_token_id = ?, key_masked = ?, key_rotation = ? WHERE id = ? AND org_id = ?`,
		tokenID, keyMasked, rotation, memberID, orgID)
	return err
}

// ActivatePlatformAccount 把无代发 key 的平台账号(运营方/组织管理员)直接置 active、
// bootstrap_state=done(它们不映射 new-api 用户,newapi_user_id 留空)。
func (s *Store) ActivatePlatformAccount(ctx context.Context, orgID, memberID int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE member SET status = ?, bootstrap_state = ? WHERE id = ? AND org_id = ?`,
		model.MemberStatusActive, model.BootstrapDone, memberID, orgID)
	return err
}

// UpdateMemberStatus 改成员状态(US-05 停用/恢复、account_ttl 到期置 expired)。
func (s *Store) UpdateMemberStatus(ctx context.Context, orgID, memberID int64, status string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE member SET status = ? WHERE id = ? AND org_id = ?`, status, memberID, orgID)
	return err
}

// GetMember 取成员,强制 org_id 谓词(跨 org → ErrNotFound)。
func (s *Store) GetMember(ctx context.Context, orgID, id int64) (*model.Member, error) {
	row := s.db.QueryRowContext(ctx, memberSelect+` WHERE id = ? AND org_id = ? AND deleted_at IS NULL`, id, orgID)
	return scanMember(row)
}

// GetMemberByEmail 按平台登录邮箱取成员(登录用)。
// MVP login_email 在 (org_id, login_email) 上唯一;跨 org 可能重名,这里取首条(运营方/管理员邮箱实际唯一)。
// 多 org 同邮箱属边角,待登录引入组织选择后细化。
func (s *Store) GetMemberByEmail(ctx context.Context, email string) (*model.Member, error) {
	row := s.db.QueryRowContext(ctx,
		memberSelect+` WHERE login_email = ? AND deleted_at IS NULL ORDER BY id LIMIT 1`, email)
	return scanMember(row)
}

// ListMembers 按过滤 + 分页列成员;返回行与过滤后总数(10 §1.5)。
func (s *Store) ListMembers(ctx context.Context, orgID int64, f MemberFilter) ([]*model.Member, int, error) {
	where := []string{"org_id = ?", "deleted_at IS NULL"}
	args := []any{orgID}
	if f.Q != "" {
		where = append(where, "(display_name LIKE ? OR login_email LIKE ?)")
		like := "%" + f.Q + "%"
		args = append(args, like, like)
	}
	if f.TeamID != nil {
		where = append(where, "team_id = ?")
		args = append(args, *f.TeamID)
	}
	if f.Status != "" {
		where = append(where, "status = ?")
		args = append(args, f.Status)
	}
	cond := strings.Join(where, " AND ")

	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM member WHERE `+cond, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if f.Limit <= 0 {
		f.Limit = 20
	}
	pageArgs := append(append([]any{}, args...), f.Limit, f.Offset)
	rows, err := s.db.QueryContext(ctx, memberSelect+` WHERE `+cond+` ORDER BY id DESC LIMIT ? OFFSET ?`, pageArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []*model.Member
	for rows.Next() {
		m, err := scanMember(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, m)
	}
	return out, total, rows.Err()
}

const memberSelect = `SELECT id, org_id, team_id, newapi_user_id, login_email, display_name, role, tier_id,
	status, expire_at, platform_password_hash, access_token_enc, member_password_enc, bootstrapped_at,
	newapi_token_id, key_masked, key_rotation, bootstrap_state, created_at, updated_at FROM member`

func scanMember(r rowScanner) (*model.Member, error) {
	var m model.Member
	var newapiUserID sql.NullInt64
	err := r.Scan(&m.ID, &m.OrgID, &m.TeamID, &newapiUserID, &m.LoginEmail, &m.DisplayName, &m.Role, &m.TierID,
		&m.Status, &m.ExpireAt, &m.PlatformPasswordHash, &m.AccessTokenEnc, &m.MemberPasswordEnc, &m.BootstrappedAt,
		&m.NewapiTokenID, &m.KeyMasked, &m.KeyRotation, &m.BootstrapState, &m.CreatedAt, &m.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if newapiUserID.Valid {
		m.NewapiUserID = newapiUserID.Int64
	}
	return &m, nil
}
