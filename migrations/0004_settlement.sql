-- 企业管理平台 · 迁移 0004(里程碑 3b:读 logs 扣余额 + 硬停 — 钱出,逐组织灰度默认关)
--
-- usage_ledger(09 §9)去重键 +(09 §10)settlement_cursor 增量水位,双保险防重复扣。
-- organization 加两个逐组织灰度开关(用户拍板 2026-06-17,默认全关):
--   billing_enabled   是否对该组织读 logs 扣费(默认关:不扣,成员照常用)
--   hard_stop_enabled 余额到 0 是否硬停(override 成员 quota=0;默认关:只翻状态不切服务)
--
-- 本迁移用 ALTER 加列(非幂等),依赖 Migrate 的"只应用一次"机制(schema_migration)。

-- 1. usage_ledger(结算用量台账)—— 09 §9。去重键防 logs 重复读后重复累加。只追加。
CREATE TABLE IF NOT EXISTS usage_ledger (
  id                BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  org_id            BIGINT UNSIGNED NOT NULL,
  member_id         BIGINT UNSIGNED NOT NULL,
  newapi_user_id    BIGINT          NOT NULL,
  team_id           BIGINT UNSIGNED NULL,
  project_id        BIGINT UNSIGNED NULL,
  model_name        VARCHAR(128)    NOT NULL,
  time_bucket       DATETIME(3)     NOT NULL,                  -- 时间桶(去重维度,按小时取整)
  prompt_tokens     BIGINT          NOT NULL DEFAULT 0,
  completion_tokens BIGINT          NOT NULL DEFAULT 0,
  consumed_quota    BIGINT          NOT NULL DEFAULT 0,        -- 该桶结算消耗(quota)= Σ logs.quota
  log_max_ts        DATETIME(3)     NOT NULL,                  -- 该桶纳入的最大 log 时间戳
  log_max_id        BIGINT          NULL,
  created_at        DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uk_ledger_dedup (org_id, newapi_user_id, model_name, time_bucket),
  KEY idx_ledger_org_time (org_id, time_bucket),
  KEY idx_ledger_org_member (org_id, member_id, time_bucket)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- 2. settlement_cursor(结算水位游标)—— 09 §10。org_id=0 为全局 leader 水位(MVP 单 leader)。
CREATE TABLE IF NOT EXISTS settlement_cursor (
  id                  BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  org_id              BIGINT UNSIGNED NOT NULL,               -- 0 = 全局 leader 水位
  last_settled_ts     BIGINT          NOT NULL DEFAULT 0,     -- 已结算到的 log unix 时间戳水位
  last_settled_log_id BIGINT          NULL,
  last_run_at         DATETIME(3)     NULL,
  version             BIGINT          NOT NULL DEFAULT 0,     -- 乐观锁,防 leader 切换并发推进
  PRIMARY KEY (id),
  UNIQUE KEY uk_cursor_org (org_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- 3. organization 加逐组织灰度开关(默认全关:不扣费、不硬停)。
ALTER TABLE organization
  ADD COLUMN billing_enabled   TINYINT(1) NOT NULL DEFAULT 0,
  ADD COLUMN hard_stop_enabled TINYINT(1) NOT NULL DEFAULT 0;
