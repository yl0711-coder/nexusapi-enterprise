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
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO organization (name, slug, status, timezone, newapi_group, default_tier_id, billing_mode)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		o.Name, o.Slug, o.Status, o.Timezone, o.NewapiGroup, o.DefaultTierID, o.BillingMode)
	if err != nil {
		if isDupKey(err) {
			return 0, ErrConflict
		}
		return 0, err
	}
	return res.LastInsertId()
}

// GetOrganization 按 id 取组织(未删)。不存在 → ErrNotFound。
func (s *Store) GetOrganization(ctx context.Context, id int64) (*model.Organization, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, slug, status, timezone, newapi_group, default_tier_id, billing_mode, created_at, updated_at
		 FROM organization WHERE id = ? AND deleted_at IS NULL`, id)
	return scanOrg(row)
}

// GetOrganizationBySlug 按 slug 取组织(运营方组织引导用)。
func (s *Store) GetOrganizationBySlug(ctx context.Context, slug string) (*model.Organization, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, slug, status, timezone, newapi_group, default_tier_id, billing_mode, created_at, updated_at
		 FROM organization WHERE slug = ? AND deleted_at IS NULL`, slug)
	return scanOrg(row)
}

// ListOrganizations 列出全部组织(运营方视角,分页)。
func (s *Store) ListOrganizations(ctx context.Context, limit, offset int) ([]*model.Organization, int, error) {
	var total int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM organization WHERE deleted_at IS NULL`).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, slug, status, timezone, newapi_group, default_tier_id, billing_mode, created_at, updated_at
		 FROM organization WHERE deleted_at IS NULL ORDER BY id DESC LIMIT ? OFFSET ?`, limit, offset)
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

// OrgDiscount 是组织折扣镜像(09 §1,配置后单向写入 new-api,本表只读回显)。
type OrgDiscount struct {
	Mode          string
	NewapiGroup   *string
	GroupRatio    *float64
	SpecialRatios []byte // JSON
}

// GetOrgDiscount 读组织折扣配置。
func (s *Store) GetOrgDiscount(ctx context.Context, orgID int64) (*OrgDiscount, error) {
	var d OrgDiscount
	var special sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT discount_mode, newapi_group, group_ratio, special_ratios FROM organization WHERE id = ? AND deleted_at IS NULL`, orgID).
		Scan(&d.Mode, &d.NewapiGroup, &d.GroupRatio, &special)
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
func (s *Store) UpdateOrgDiscount(ctx context.Context, orgID int64, mode string, newapiGroup *string, groupRatio *float64, specialRatios []byte) error {
	var special any
	if len(specialRatios) > 0 {
		special = string(specialRatios)
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE organization SET discount_mode = ?, newapi_group = COALESCE(?, newapi_group), group_ratio = ?, special_ratios = ?
		 WHERE id = ? AND deleted_at IS NULL`, mode, newapiGroup, groupRatio, special, orgID)
	return err
}

// UpdateOrgStatus 改组织服务状态(active/low/stopped,余额水位驱动,09 §14)。
func (s *Store) UpdateOrgStatus(ctx context.Context, orgID int64, status string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE organization SET status = ? WHERE id = ? AND deleted_at IS NULL`, status, orgID)
	return err
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
	err := r.Scan(&o.ID, &o.Name, &o.Slug, &o.Status, &o.Timezone, &o.NewapiGroup,
		&o.DefaultTierID, &o.BillingMode, &o.CreatedAt, &o.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &o, nil
}
