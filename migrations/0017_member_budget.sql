-- 企业管理平台 · 迁移 0017(v2 M0:成员周期额度 member_budget)
--
-- v2 周期发放模型(13 §4.3 / 14 §3.4):每成员同一时刻只挂一种周期 cap(日/周/月,三选一)。
--   cap          = 每周期发放上限(天花板,非保证)
--   granted      = 本期已发放(占用池子的量;钱阶段重置时 = min(cap, 池子可分配))
--   period_anchor= 当前周期锚点(UTC+8 自然日/周/月边界,数学确定)
--   pending_*    = 改 cap/周期 后下个周期边界才生效(本期不动),空=无待生效变更
--   spend_cache  = 派生消费缓存(展示加速,真相仍来自日志,可重算)
--   last_reset_at= 上次重置时间(配合单调 CAS 幂等,钱阶段用)
--
-- 本期(观测)只"分配/配置",不执行扣停;发放/重置逻辑在 M4 接缝、flag 关。本迁移只建表。

CREATE TABLE IF NOT EXISTS member_budget (
  id                  BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  org_id              BIGINT UNSIGNED NOT NULL,
  member_id           BIGINT UNSIGNED NOT NULL,
  period_type         VARCHAR(8)      NOT NULL DEFAULT 'month',  -- day/week/month(单一周期)
  cap                 BIGINT          NOT NULL DEFAULT 0,        -- 周期发放上限(quota)
  granted             BIGINT          NOT NULL DEFAULT 0,        -- 本期已发放(quota;占用池子)
  period_anchor       DATETIME(3)     NULL,                      -- 当前周期锚点(UTC+8 边界)
  pending_cap         BIGINT          NULL,                      -- 待下周期生效的新 cap(NULL=无)
  pending_period_type VARCHAR(8)      NULL,                      -- 待下周期生效的新周期类型(NULL=无)
  spend_cache         BIGINT          NOT NULL DEFAULT 0,        -- 派生消费缓存(可重算)
  last_reset_at       DATETIME(3)     NULL,                      -- 上次重置时间(单调 CAS)
  created_at          DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at          DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uk_budget_member (member_id),                       -- 每成员一行(单一周期)
  KEY idx_budget_org (org_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
