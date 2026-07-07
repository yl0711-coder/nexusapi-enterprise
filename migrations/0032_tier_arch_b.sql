-- 架构B(31-ADR §5/33-§2-0032):档位=额度型+额度值+周期+分组+可用模型,额度落成员 user.quota(非令牌)。
-- 去「无上限」:quota_type 只有 fixed(固定/单次,不重置)| subscription(订阅/周期,worker 补满到目标)。
-- 旧 daily/weekly/monthly_limit 保留废弃(代码停引用);amount_raw 为唯一额度值(raw quota)。
-- tier_grant 沿用 v3 已定义结构(档位授权到成员/团队;visibility='all' 的档位不需要 grant 行)。
-- 回滚:ALTER TABLE tier DROP COLUMN quota_type, DROP COLUMN amount_raw, DROP COLUMN reset_period, DROP COLUMN visibility; DROP TABLE tier_grant;
ALTER TABLE tier ADD COLUMN quota_type ENUM('fixed','subscription') NOT NULL DEFAULT 'fixed' AFTER model_cap;
ALTER TABLE tier ADD COLUMN amount_raw BIGINT NULL AFTER quota_type;
ALTER TABLE tier ADD COLUMN reset_period ENUM('daily','weekly','monthly') NULL AFTER amount_raw;
ALTER TABLE tier ADD COLUMN visibility ENUM('all','assigned') NOT NULL DEFAULT 'assigned' AFTER reset_period;

CREATE TABLE IF NOT EXISTS tier_grant (
  id           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  org_id       BIGINT UNSIGNED NOT NULL,
  tier_id      BIGINT UNSIGNED NOT NULL,
  target_type  ENUM('member','team') NOT NULL,
  target_id    BIGINT UNSIGNED NOT NULL,
  created_by   VARCHAR(64)     NOT NULL DEFAULT '',
  created_at   DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uk_tier_grant (org_id, tier_id, target_type, target_id),
  KEY idx_tier_grant_target (org_id, target_type, target_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
