-- 企业管理平台 · 迁移 0007(补全:审批阈值可配 E13 + 配额策略 quota_policy 09§6)

-- organization 加可配审批阈值(E13;之前硬编,现落库可配)。
ALTER TABLE organization
  ADD COLUMN approval_auto_max_quota BIGINT NOT NULL DEFAULT 50000000,   -- ① 自动通过额度上限
  ADD COLUMN approval_auto_max_days  INT    NOT NULL DEFAULT 1,          -- ① 自动通过时长上限(天)
  ADD COLUMN approval_l1_max_quota   BIGINT NOT NULL DEFAULT 150000000;  -- ② 一审上限

-- quota_policy(配额策略)—— 09 §6。供周期重置 worker 算下次重置点。
CREATE TABLE IF NOT EXISTS quota_policy (
  id           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  org_id       BIGINT UNSIGNED NOT NULL,
  scope        VARCHAR(16)     NOT NULL,                      -- org/team/member
  scope_id     BIGINT UNSIGNED NOT NULL,
  period       VARCHAR(16)     NOT NULL,                      -- daily/weekly/monthly
  limit_quota  BIGINT          NOT NULL,
  reset_anchor VARCHAR(32)     NOT NULL DEFAULT '00:00',
  status       VARCHAR(16)     NOT NULL DEFAULT 'active',     -- active/disabled
  created_at   DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at   DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uk_policy_scope_period (org_id, scope, scope_id, period),
  KEY idx_policy_org (org_id, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
