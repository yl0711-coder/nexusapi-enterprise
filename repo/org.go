package repo

import (
	"context"
	"database/sql"
	"errors"

	"github.com/nexusapi-platform/enterprise/model"
)

// CreateOrganization 建组织,返回新 id。slug 冲突 → ErrConflict。
func (s *Store) CreateOrganization(ctx context.Context, o *model.Organization) (int64, error) {
	if o.Status == "" {
		o.Status = model.OrgStatusActive
	}
	if o.Timezone == "" {
		o.Timezone = "Asia/Shanghai"
	}
	if o.BillingMode == "" {
		o.BillingMode = "prepaid"
	}
	// v1 正交属性初值(0022):零值回落默认(self_funded/shared/wallet;CreatedByPlatform 由调用方按门显式设)。
	if o.FundingMode == "" {
		o.FundingMode = model.FundingSelfFunded
	}
	if o.MemberCapMode == "" {
		o.MemberCapMode = model.CapModeShared
	}
	if o.BillingKind == "" {
		o.BillingKind = model.BillingKindWallet
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO organization (name, slug, status, timezone, newapi_group, default_tier_id, billing_mode,
		                           funding_mode, newapi_user_created_by_platform, member_cap_mode, billing_kind)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		o.Name, o.Slug, o.Status, o.Timezone, o.NewapiUserGroup, o.DefaultTierID, o.BillingMode,
		o.FundingMode, o.CreatedByPlatform, o.MemberCapMode, o.BillingKind)
	if err != nil {
		if isDupKey(err) {
			return 0, ErrConflict
		}
		return 0, err
	}
	return res.LastInsertId()
}

// orgCols 组织查询列清单(与 scanOrg 一一对应,唯一来源防三处漂移)。
const orgCols = `id, name, slug, status, timezone, newapi_group, default_tier_id, billing_mode, default_token_group, archived_at,
	funding_mode, newapi_user_created_by_platform, member_cap_mode, billing_kind, created_at, updated_at`

// GetOrganization 按 id 取组织(未删)。不存在 → ErrNotFound。
func (s *Store) GetOrganization(ctx context.Context, id int64) (*model.Organization, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+orgCols+` FROM organization WHERE id = ? AND deleted_at IS NULL`, id)
	return scanOrg(row)
}

// GetOrganizationBySlug 按 slug 取组织(运营方组织引导用)。
func (s *Store) GetOrganizationBySlug(ctx context.Context, slug string) (*model.Organization, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+orgCols+` FROM organization WHERE slug = ? AND deleted_at IS NULL`, slug)
	return scanOrg(row)
}

// SetOrgNewapiUser 模型2(0020):记录组织的 new-api user 池子锚 + 加密 access_token + 加密密码(开通组织时写)。
// access_token/password 由 service 层加密后传入(密钥不进库);password 供 access_token 失效时重登录自愈。
// **WHERE newapi_user_id IS NULL 守卫**(R5 OBS-3):仅首写生效,返回 wrote=是否本次写入。并发/多节点首开
// 同组织时后到者落空(wrote=false)→ 调用方改读已落库凭证,防把"已被旋转作废的 access_token"覆盖掉有效凭证。
func (s *Store) SetOrgNewapiUser(ctx context.Context, orgID, newapiUserID int64, accessTokenEnc, passwordEnc []byte) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE organization SET newapi_user_id = ?, newapi_access_token_enc = ?, newapi_password_enc = ?
		   WHERE id = ? AND deleted_at IS NULL AND newapi_user_id IS NULL`,
		newapiUserID, accessTokenEnc, passwordEnc, orgID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// GetOrgNewapiCred 模型2(0020):读组织的 new-api user_id + 加密 access_token(建员工 token 用,R1)。
// ok=false 表示该组织尚未开通 new-api user(newapi_user_id 为 NULL);组织不存在 → ErrNotFound。
func (s *Store) GetOrgNewapiCred(ctx context.Context, orgID int64) (newapiUserID int64, accessTokenEnc []byte, ok bool, err error) {
	var uid sql.NullInt64
	var enc []byte
	row := s.db.QueryRowContext(ctx,
		`SELECT newapi_user_id, newapi_access_token_enc FROM organization WHERE id = ? AND deleted_at IS NULL`, orgID)
	if e := row.Scan(&uid, &enc); e != nil {
		if errors.Is(e, sql.ErrNoRows) {
			return 0, nil, false, ErrNotFound
		}
		return 0, nil, false, e
	}
	if !uid.Valid {
		return 0, nil, false, nil // 尚未开通 org user
	}
	return uid.Int64, enc, true, nil
}

// GetOrgNewapiPassword 读组织 new-api user 的加密密码(401 自愈:access_token 失效时用它重登派生新 token)。
func (s *Store) GetOrgNewapiPassword(ctx context.Context, orgID int64) ([]byte, error) {
	var enc []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT newapi_password_enc FROM organization WHERE id = ? AND deleted_at IS NULL`, orgID).Scan(&enc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return enc, err
}

// UpdateOrgAccessToken 刷新组织 access_token(401 自愈重登后落库;无 IS NULL 守卫,就是要覆盖旧的失效值)。
func (s *Store) UpdateOrgAccessToken(ctx context.Context, orgID int64, accessTokenEnc []byte) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE organization SET newapi_access_token_enc = ? WHERE id = ? AND deleted_at IS NULL`, accessTokenEnc, orgID)
	return err
}

// ListOrganizations 列出组织(运营方视角,分页)。includeArchived=false 时默认隐藏已归档(T12)。
func (s *Store) ListOrganizations(ctx context.Context, limit, offset int, includeArchived bool) ([]*model.Organization, int, error) {
	cond := "deleted_at IS NULL"
	if !includeArchived {
		cond += " AND archived_at IS NULL"
	}
	var total int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM organization WHERE `+cond).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+orgCols+` FROM organization WHERE `+cond+` ORDER BY id DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []*model.Organization
	for rows.Next() {
		o, err := scanOrg(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, o)
	}
	return out, total, rows.Err()
}

// SetOrgArchived 归档/取消归档组织(T12:软隐藏,不物理删除)。archived=true 置 archived_at=now。
func (s *Store) SetOrgArchived(ctx context.Context, orgID int64, archived bool) error {
	var q string
	if archived {
		q = `UPDATE organization SET archived_at = CURRENT_TIMESTAMP(3) WHERE id = ? AND deleted_at IS NULL`
	} else {
		q = `UPDATE organization SET archived_at = NULL WHERE id = ? AND deleted_at IS NULL`
	}
	res, err := s.db.ExecContext(ctx, q, orgID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// OrgDiscount 是组织折扣镜像(09 §1,配置后单向写入 new-api,本表只读回显)。
type OrgDiscount struct {
	Mode          string
	GroupRatio    *float64
	SpecialRatios []byte // JSON
}

// GetOrgDiscount 读组织折扣配置。
func (s *Store) GetOrgDiscount(ctx context.Context, orgID int64) (*OrgDiscount, error) {
	var d OrgDiscount
	var special sql.NullString
	// 改动①解耦(红线④):折扣不再读组织用户分组列(newapi_group);用户分组由 orgUserGroup 读字段、折扣不碰。
	err := s.db.QueryRowContext(ctx,
		`SELECT discount_mode, group_ratio, special_ratios FROM organization WHERE id = ? AND deleted_at IS NULL`, orgID).
		Scan(&d.Mode, &d.GroupRatio, &special)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if special.Valid {
		d.SpecialRatios = []byte(special.String)
	}
	return &d, nil
}

// UpdateOrgDiscount 写组织折扣配置(镜像 new-api,单向)。
// 改动①解耦(红线④):**不再写 newapi_group 列**——配折扣绝不能覆盖组织用户分组(隔离边界)。用户分组只由建组织写。
func (s *Store) UpdateOrgDiscount(ctx context.Context, orgID int64, mode string, groupRatio *float64, specialRatios []byte) error {
	var special any
	if len(specialRatios) > 0 {
		special = string(specialRatios)
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE organization SET discount_mode = ?, group_ratio = ?, special_ratios = ?
		 WHERE id = ? AND deleted_at IS NULL`, mode, groupRatio, special, orgID)
	return err
}

// UpdateOrgSettings 改组织设置(名称/时区/默认层级;nil=不改,E19)。
func (s *Store) UpdateOrgSettings(ctx context.Context, orgID int64, name, timezone *string, defaultTierID *int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE organization SET name = COALESCE(?, name), timezone = COALESCE(?, timezone),
		    default_tier_id = COALESCE(?, default_tier_id)
		 WHERE id = ? AND deleted_at IS NULL`, name, timezone, defaultTierID, orgID)
	return err
}

// ListDiscountedOrgIDs 列出已配折扣(mode != none)的组织 id,供折扣对账扫描(G,只读)。
func (s *Store) ListDiscountedOrgIDs(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM organization WHERE discount_mode IS NOT NULL AND discount_mode <> 'none' AND deleted_at IS NULL ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ApprovalRules 是可配审批阈值(E13)。
type ApprovalRules struct {
	AutoMaxQuota int64
	AutoMaxDays  int
	L1MaxQuota   int64
}

// GetApprovalRules 取组织审批阈值。
func (s *Store) GetApprovalRules(ctx context.Context, orgID int64) (*ApprovalRules, error) {
	var r ApprovalRules
	err := s.db.QueryRowContext(ctx,
		`SELECT approval_auto_max_quota, approval_auto_max_days, approval_l1_max_quota
		 FROM organization WHERE id = ? AND deleted_at IS NULL`, orgID).Scan(&r.AutoMaxQuota, &r.AutoMaxDays, &r.L1MaxQuota)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// SetApprovalRules 设组织审批阈值(nil=不改)。
func (s *Store) SetApprovalRules(ctx context.Context, orgID int64, autoMax, l1Max *int64, autoDays *int) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE organization SET approval_auto_max_quota = COALESCE(?, approval_auto_max_quota),
		    approval_l1_max_quota = COALESCE(?, approval_l1_max_quota),
		    approval_auto_max_days = COALESCE(?, approval_auto_max_days)
		 WHERE id = ? AND deleted_at IS NULL`, autoMax, l1Max, autoDays, orgID)
	return err
}

// UpdateOrgStatus 改组织服务状态(active/low/stopped,余额水位驱动,09 §14)。
func (s *Store) UpdateOrgStatus(ctx context.Context, orgID int64, status string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE organization SET status = ? WHERE id = ? AND deleted_at IS NULL`, status, orgID)
	return err
}

// SetOrgDefaultTokenGroup 设组织级默认令牌计价分组(D1 两级;nil 入参不改用 COALESCE 不便,直接置)。
func (s *Store) SetOrgDefaultTokenGroup(ctx context.Context, orgID int64, group *string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE organization SET default_token_group = ? WHERE id = ? AND deleted_at IS NULL`, group, orgID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetOrgDefaultTier 设组织默认层级(US-01 套默认层级用)。
func (s *Store) SetOrgDefaultTier(ctx context.Context, orgID, tierID int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE organization SET default_tier_id = ? WHERE id = ? AND deleted_at IS NULL`, tierID, orgID)
	return err
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanOrg(r rowScanner) (*model.Organization, error) {
	var o model.Organization
	err := r.Scan(&o.ID, &o.Name, &o.Slug, &o.Status, &o.Timezone, &o.NewapiUserGroup,
		&o.DefaultTierID, &o.BillingMode, &o.DefaultTokenGroup, &o.ArchivedAt,
		&o.FundingMode, &o.CreatedByPlatform, &o.MemberCapMode, &o.BillingKind, &o.CreatedAt, &o.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &o, nil
}
