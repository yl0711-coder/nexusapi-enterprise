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
	Q      string // 关键词:匹配 display_name / login_email / key_masked
	OrgID  *int64
	TeamID *int64
	Status string
	Limit  int
	Offset int
}

type MemberOverview struct {
	ID             int64   `json:"id"`
	OrgID          int64   `json:"org_id"`
	OrgName        string  `json:"org_name"`
	TeamID         *int64  `json:"team_id"`
	TeamName       string  `json:"team_name,omitempty"`
	LoginEmail     string  `json:"login_email"`
	DisplayName    *string `json:"display_name"`
	Role           string  `json:"role"`
	TierID         *int64  `json:"tier_id"`
	TierName       string  `json:"tier_name,omitempty"`
	NewapiGroup    *string `json:"newapi_group"`
	Status         string  `json:"status"`
	KeyMasked      *string `json:"key_masked"`
	NewapiTokenID  *int64  `json:"newapi_token_id"`
	MonthlyLimit   *int64  `json:"monthly_limit_quota"`
	ConsumedQuota  int64   `json:"consumed_quota"`
	RemainingQuota *int64  `json:"remaining_quota,omitempty"`
	BootstrapState string  `json:"bootstrap_state"`
	CreatedAt      string  `json:"created_at"`
}

// CreateMemberProvisional 先建 provisioning 中间态成员行(尚无 new-api 用户),返回 member.id。
// service 用此 id 派生确定性 username 后再 bootstrap(10 §2.5),成功后调 FinalizeBootstrap。
func (s *Store) CreateMemberProvisional(ctx context.Context, m *model.Member) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO member (org_id, team_id, login_email, display_name, role, tier_id, newapi_group,
		    status, platform_password_hash, bootstrap_state)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.OrgID, m.TeamID, m.LoginEmail, m.DisplayName, m.Role, m.TierID, m.NewapiGroup,
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
// v2(M0/M1):若本次带令牌(NewapiTokenID!=nil),在**同一事务**内建主 key 槽 + 当前令牌(member_key),
// 使 member.newapi_token_id 指针与 member_key 一致;无令牌(观测期 SkipToken)则只回填成员、不建 key。
// tokenName 为该令牌的确定性名(nexus_m{member}_v{rotation},归因映射键);无令牌时传 ""。
func (s *Store) FinalizeBootstrap(ctx context.Context, m *model.Member, tokenName string) error {
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE member
			    SET newapi_token_id = ?, key_masked = ?, key_rotation = ?,
			        bootstrap_state = ?, status = ?, bootstrapped_at = CURRENT_TIMESTAMP(3)
			  WHERE id = ? AND org_id = ?`,
			m.NewapiTokenID, m.KeyMasked, m.KeyRotation,
			model.BootstrapDone, model.MemberStatusActive,
			m.ID, m.OrgID); err != nil {
			return err
		}
		if m.NewapiTokenID != nil {
			masked := ""
			if m.KeyMasked != nil {
				masked = *m.KeyMasked
			}
			if _, err := s.ensurePrimaryKeyTx(ctx, tx, m.OrgID, m.ID, *m.NewapiTokenID, tokenName, masked, m.KeyRotation); err != nil {
				return err
			}
		}
		return nil
	})
}

// MarkBootstrapFailedAndRelease 标 bootstrap 失败,并把 login_email 墓碑改写(前缀 failed-{id}-,LEFT 截到列宽 191)
// 以释放 uk_member_org_email 占用、允许同邮箱重开(GZ-03 缺陷3)。
// 模型2:member 不映射 new-api 用户,开通失败=员工 token 未建成——**无孤儿用户**(org user 共享、不动;残留 token 靠
// 确定性名 adopt 在重开时自愈),故去掉 model1 的 newapi_user_id 回写与 orphan-用户机制。status 维持 provisioning。
func (s *Store) MarkBootstrapFailedAndRelease(ctx context.Context, orgID, memberID int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE member
		    SET bootstrap_state = ?, status = ?,
		        login_email = LEFT(CONCAT('failed-', id, '-', login_email), 191)
		  WHERE id = ? AND org_id = ?`,
		model.BootstrapFailed, model.MemberStatusProvisioning, memberID, orgID)
	return err
}

// UpdateMemberKey 轮换/自助建后回填新令牌 id / 脱敏 key / 轮换计数(US-07)。
// v2(M0/M1):在**同一事务**内同步 member_key——主槽上把旧 current 令牌置 superseded、插新 current 令牌
// (无主槽则新建,覆盖观测期首次自助建 key)。旧令牌行留存 → 历史日志按旧 token_id 仍归因到同一 key_id。
// tokenName 为新令牌确定性名(nexus_m{member}_v{rotation})。
func (s *Store) UpdateMemberKey(ctx context.Context, orgID, memberID, tokenID int64, keyMasked, tokenName string, rotation int) error {
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE member SET newapi_token_id = ?, key_masked = ?, key_rotation = ? WHERE id = ? AND org_id = ?`,
			tokenID, keyMasked, rotation, memberID, orgID); err != nil {
			return err
		}
		_, err := s.rotatePrimaryKeyTx(ctx, tx, orgID, memberID, tokenID, tokenName, keyMasked, rotation)
		return err
	})
}

// ActivatePlatformAccount 把无代发 key 的平台账号(运营方/组织管理员)直接置 active、
// bootstrap_state=done(它们不映射 new-api 用户,newapi_user_id 留空)。
func (s *Store) ActivatePlatformAccount(ctx context.Context, orgID, memberID int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE member SET status = ?, bootstrap_state = ? WHERE id = ? AND org_id = ?`,
		model.MemberStatusActive, model.BootstrapDone, memberID, orgID)
	return err
}

// ClearMemberToken 模型2:清成员当前令牌指针(停用删 token 后调,防悬挂指针;员工恢复后自助重建新 key)。
func (s *Store) ClearMemberToken(ctx context.Context, orgID, memberID int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE member SET newapi_token_id = NULL, key_masked = NULL WHERE id = ? AND org_id = ?`, memberID, orgID)
	return err
}

// UpdateMemberStatus 改成员状态(US-05 停用/恢复、account_ttl 到期置 expired)。
func (s *Store) UpdateMemberStatus(ctx context.Context, orgID, memberID int64, status string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE member SET status = ? WHERE id = ? AND org_id = ?`, status, memberID, orgID)
	return err
}

// BumpMemberSessionEpoch 自增某成员会话代次(A3):禁用/改角色/改密后调用,令其所有旧平台 token 立即失效。
func (s *Store) BumpMemberSessionEpoch(ctx context.Context, orgID, memberID int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE member SET session_epoch = session_epoch + 1 WHERE id = ? AND org_id = ?`, memberID, orgID)
	return err
}

// BumpOrgMembersSessionEpoch 自增某组织**全部**成员会话代次(A3/WB-4):硬停时调用,令该组织所有成员平台会话立即失效。
func (s *Store) BumpOrgMembersSessionEpoch(ctx context.Context, orgID int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE member SET session_epoch = session_epoch + 1 WHERE org_id = ? AND deleted_at IS NULL`, orgID)
	return err
}

// OffboardMember 离职(软删):deleted_at=now + status=offboarded + 清 token 指针。ListMembers(deleted_at IS NULL)自动排除。
func (s *Store) OffboardMember(ctx context.Context, orgID, memberID int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE member SET deleted_at = CURRENT_TIMESTAMP(3), status = ?, newapi_token_id = NULL, key_masked = NULL
		   WHERE id = ? AND org_id = ? AND deleted_at IS NULL`, model.MemberStatusOffboarded, memberID, orgID)
	return err
}

// RestoreOffboardedMember 恢复入职:清软删 + 置 active(无 token,员工自助重建 key)。
func (s *Store) RestoreOffboardedMember(ctx context.Context, orgID, memberID int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE member SET deleted_at = NULL, status = ? WHERE id = ? AND org_id = ? AND deleted_at IS NOT NULL`,
		model.MemberStatusActive, memberID, orgID)
	return err
}

// ListOffboardedMembers 列离职成员(软删,资料/历史保留可恢复)。
func (s *Store) ListOffboardedMembers(ctx context.Context, orgID int64, limit, offset int) ([]*model.Member, int, error) {
	var total int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM member WHERE org_id = ? AND deleted_at IS NOT NULL`, orgID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx,
		memberSelect+` WHERE org_id = ? AND deleted_at IS NOT NULL ORDER BY id DESC LIMIT ? OFFSET ?`, orgID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []*model.Member
	for rows.Next() {
		m, serr := scanMember(rows)
		if serr != nil {
			return nil, 0, serr
		}
		out = append(out, m)
	}
	return out, total, rows.Err()
}

// UpdateMemberPassword 改成员平台登录密码哈希(个人设置·自助改密)。
func (s *Store) UpdateMemberPassword(ctx context.Context, orgID, memberID int64, passwordHash string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE member SET platform_password_hash = ? WHERE id = ? AND org_id = ?`, passwordHash, memberID, orgID)
	return err
}

// UpdateMemberDisplayName 改成员显示名(个人设置·自助改名)。
func (s *Store) UpdateMemberDisplayName(ctx context.Context, orgID, memberID int64, name string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE member SET display_name = ? WHERE id = ? AND org_id = ?`, name, memberID, orgID)
	return err
}

// UpdateMemberTeamTier 改成员团队/层级(nil=不改,PATCH /members/:id)。
func (s *Store) UpdateMemberTeamTier(ctx context.Context, orgID, memberID int64, teamID, tierID *int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE member SET team_id = COALESCE(?, team_id), tier_id = COALESCE(?, tier_id)
		 WHERE id = ? AND org_id = ?`, teamID, tierID, memberID, orgID)
	return err
}

// UpdateMemberRole 任命/变更成员角色(E17)。
func (s *Store) UpdateMemberRole(ctx context.Context, orgID, memberID int64, role string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE member SET role = ? WHERE id = ? AND org_id = ?`, role, memberID, orgID)
	return err
}

// ListOrgAdminIDs 列组织管理员的 member id(软限额/告警的通知对象)。
func (s *Store) ListOrgAdminIDs(ctx context.Context, orgID int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM member WHERE org_id = ? AND role = 'org_admin' AND deleted_at IS NULL`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ListMembersByTier 列出引用某层级的成员(改层级后重算 override 用,T10)。
func (s *Store) ListMembersByTier(ctx context.Context, orgID, tierID int64) ([]*model.Member, error) {
	rows, err := s.db.QueryContext(ctx, memberSelect+` WHERE org_id = ? AND tier_id = ? AND deleted_at IS NULL ORDER BY id`, orgID, tierID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Member
	for rows.Next() {
		m, err := scanMember(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetMemberNameByID 取成员显示名(姓名优先,回落登录名;跨 org,操作者名解析用,T14)。不存在返空。
func (s *Store) GetMemberNameByID(ctx context.Context, id int64) (string, error) {
	var name string
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(NULLIF(display_name, ''), login_email, '') FROM member WHERE id = ?`, id).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return name, err
}

// GetMemberAny 取成员**含软删行**(架构B 生命周期用:离职成员的幂等补退/恢复入职都要能读到软删行;
// 常规读一律用 GetMember,勿混用)。强制 org_id 谓词。
func (s *Store) GetMemberAny(ctx context.Context, orgID, id int64) (*model.Member, error) {
	row := s.db.QueryRowContext(ctx, memberSelect+` WHERE id = ? AND org_id = ?`, id, orgID)
	return scanMember(row)
}

// UpdateMemberTierGroup 恢复入职按新档位重设档位指针 + 分组快照(架构B RestoreMember)。
func (s *Store) UpdateMemberTierGroup(ctx context.Context, orgID, memberID, tierID int64, group string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE member SET tier_id = ?, newapi_group = ? WHERE id = ? AND org_id = ?`, tierID, group, memberID, orgID)
	return err
}

// GetMember 取成员,强制 org_id 谓词(跨 org → ErrNotFound)。
func (s *Store) GetMember(ctx context.Context, orgID, id int64) (*model.Member, error) {
	row := s.db.QueryRowContext(ctx, memberSelect+` WHERE id = ? AND org_id = ? AND deleted_at IS NULL`, id, orgID)
	return scanMember(row)
}

// GetMemberByNewapiTokenID 模型2 结算归因:按 new-api token id 反查平台成员 + 稳定 key_id。
// 成员共享 org user,归因只能走 token→member_key_token→member(绝不能按 user_id)。无 org 谓词(leader 跨租户);
// 命不中(非平台 token/无对应行)返 found=false。轮换后旧 token_id 仍命中(member_key_token append-only)。
// M4 时点归因(20-§6):member 读取**不过滤 deleted_at**——离职(软删)成员的迟同步/历史消费仍归原成员,不丢行不串人。
func (s *Store) GetMemberByNewapiTokenID(ctx context.Context, newapiTokenID int64) (*model.Member, int64, bool, error) {
	var orgID, memberID, keyID int64
	err := s.db.QueryRowContext(ctx,
		`SELECT org_id, member_id, key_id FROM member_key_token WHERE newapi_token_id = ?`, newapiTokenID).Scan(&orgID, &memberID, &keyID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, false, nil
	}
	if err != nil {
		return nil, 0, false, err
	}
	row := s.db.QueryRowContext(ctx, memberSelect+` WHERE id = ? AND org_id = ?`, memberID, orgID)
	m, merr := scanMember(row)
	if errors.Is(merr, ErrNotFound) {
		return nil, 0, false, nil
	}
	if merr != nil {
		return nil, 0, false, merr
	}
	return m, keyID, true, nil
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
	// 组长契约增补(33 §12-⑤):offboarded(软删)默认不进主列表;status=offboarded 显式查时切换谓词。
	deletedPred := "deleted_at IS NULL"
	if f.Status == model.MemberStatusOffboarded {
		deletedPred = "deleted_at IS NOT NULL"
	}
	where := []string{"org_id = ?", deletedPred}
	args := []any{orgID}
	if f.Q != "" {
		where = append(where, "(display_name LIKE ? OR login_email LIKE ? OR key_masked LIKE ?)")
		like := "%" + f.Q + "%"
		args = append(args, like, like, like)
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

func (s *Store) ListAllMembers(ctx context.Context, f MemberFilter) ([]MemberOverview, int, error) {
	// B3(28):管理类账号(运营方/组织管理员)过滤下沉 SQL——原前端过滤导致 total 与可见行错位(计数/翻页真 bug)。
	where := []string{"m.deleted_at IS NULL", "o.deleted_at IS NULL", "m.role NOT IN ('operator','org_admin')"}
	args := []any{}
	if f.OrgID != nil {
		where = append(where, "m.org_id = ?")
		args = append(args, *f.OrgID)
	}
	if f.Q != "" {
		where = append(where, "(m.display_name LIKE ? OR m.login_email LIKE ? OR m.key_masked LIKE ? OR o.name LIKE ?)")
		like := "%" + f.Q + "%"
		args = append(args, like, like, like, like)
	}
	if f.TeamID != nil {
		where = append(where, "m.team_id = ?")
		args = append(args, *f.TeamID)
	}
	if f.Status != "" {
		where = append(where, "m.status = ?")
		args = append(args, f.Status)
	}
	cond := strings.Join(where, " AND ")
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM member m JOIN organization o ON o.id = m.org_id WHERE `+cond, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	pageArgs := append(append([]any{}, args...), f.Limit, f.Offset)
	rows, err := s.db.QueryContext(ctx,
		`SELECT m.id, m.org_id, COALESCE(o.name,''), m.team_id, COALESCE(t.name,''),
		        m.login_email, m.display_name, m.role, m.tier_id, COALESCE(ti.name,''),
		        m.newapi_group, m.status, m.key_masked, m.newapi_token_id, ti.monthly_limit,
		        COALESCE(ul.consumed_quota,0), m.bootstrap_state, DATE_FORMAT(m.created_at, '%Y-%m-%dT%H:%i:%sZ')
		   FROM member m
		   JOIN organization o ON o.id = m.org_id
		   LEFT JOIN team t ON t.id = m.team_id AND t.org_id = m.org_id
		   LEFT JOIN tier ti ON ti.id = m.tier_id AND ti.org_id = m.org_id
		   LEFT JOIN (
		     SELECT org_id, member_id, SUM(consumed_quota) AS consumed_quota
		       FROM usage_ledger
		      GROUP BY org_id, member_id
		   ) ul ON ul.org_id = m.org_id AND ul.member_id = m.id
		  WHERE `+cond+`
		  ORDER BY m.org_id ASC, m.id DESC
		  LIMIT ? OFFSET ?`, pageArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := make([]MemberOverview, 0)
	for rows.Next() {
		var m MemberOverview
		if err := rows.Scan(&m.ID, &m.OrgID, &m.OrgName, &m.TeamID, &m.TeamName, &m.LoginEmail, &m.DisplayName,
			&m.Role, &m.TierID, &m.TierName, &m.NewapiGroup, &m.Status, &m.KeyMasked, &m.NewapiTokenID,
			&m.MonthlyLimit, &m.ConsumedQuota, &m.BootstrapState, &m.CreatedAt); err != nil {
			return nil, 0, err
		}
		if m.MonthlyLimit != nil {
			remaining := *m.MonthlyLimit - m.ConsumedQuota
			if remaining < 0 {
				remaining = 0
			}
			m.RemainingQuota = &remaining
		}
		out = append(out, m)
	}
	return out, total, rows.Err()
}

// memberSelect 模型2:不含 newapi_user_id/access_token_enc/member_password_enc(已从 member 移除,归 organization)。
const memberSelect = `SELECT id, org_id, team_id, login_email, display_name, role, tier_id, newapi_group,
	status, expire_at, platform_password_hash, bootstrapped_at,
	newapi_token_id, key_masked, key_rotation, bootstrap_state, session_epoch,
	newapi_user_id, newapi_username, created_at, updated_at FROM member`

func scanMember(r rowScanner) (*model.Member, error) {
	var m model.Member
	err := r.Scan(&m.ID, &m.OrgID, &m.TeamID, &m.LoginEmail, &m.DisplayName, &m.Role, &m.TierID, &m.NewapiGroup,
		&m.Status, &m.ExpireAt, &m.PlatformPasswordHash, &m.BootstrappedAt,
		&m.NewapiTokenID, &m.KeyMasked, &m.KeyRotation, &m.BootstrapState, &m.SessionEpoch,
		&m.NewapiUserID, &m.NewapiUsername, &m.CreatedAt, &m.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}
