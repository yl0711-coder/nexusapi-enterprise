// 架构B 阶段0(33 §2-0033):平台配置 KV(超管可配)。QuotaPerUnit 不硬编码的落点;
// money_freeze 全局钱动作急停(事故止血,平时恒 false,与已废弃的 observe 短路是两回事)。
package repo

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
)

// GetSetting 读平台配置;缺省键返回 (def, nil)(种子行由 0033 INSERT IGNORE 保证,防御性兜底)。
func (s *Store) GetSetting(ctx context.Context, key, def string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT v FROM platform_setting WHERE k = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return def, nil
	}
	if err != nil {
		return def, err
	}
	return v, nil
}

// SetSetting 写平台配置(超管;审计由 service 层负责)。
func (s *Store) SetSetting(ctx context.Context, key, value, updatedBy string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO platform_setting (k, v, updated_by) VALUES (?, ?, ?)
		 ON DUPLICATE KEY UPDATE v = VALUES(v), updated_by = VALUES(updated_by)`, key, value, updatedBy)
	return err
}

// GetSettingInt64 读整型配置(解析失败回 def,不炸——配置坏值不应停服,fail-open)。
func (s *Store) GetSettingInt64(ctx context.Context, key string, def int64) (int64, error) {
	v, err := s.GetSetting(ctx, key, strconv.FormatInt(def, 10))
	if err != nil {
		return def, err
	}
	n, perr := strconv.ParseInt(v, 10, 64)
	if perr != nil {
		return def, nil
	}
	return n, nil
}

// GetSettingBool 读布尔配置(仅 "true" 为真,其余含坏值一律 false)。
func (s *Store) GetSettingBool(ctx context.Context, key string) (bool, error) {
	v, err := s.GetSetting(ctx, key, "false")
	if err != nil {
		return false, err
	}
	return v == "true", nil
}
