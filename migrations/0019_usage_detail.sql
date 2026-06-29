-- usage_detail(v2 M2-2):逐条用量明细,供报表下钻读本库(不再实时压 new-api /api/log/)。
-- 与 usage_ledger(小时桶聚合)互补:ledger 管趋势/对账,detail 管下钻明细 + 请求数/token 数指标。
-- 幂等:newapi_log_id 唯一(new-api logs.id 全局唯一)→ 同一条日志只落一次,结算重叠窗口/重试不重复。
-- 保留:按 log_ts 保 90 天(13 §4.4),reconcile worker 周期清理超期行(故 log_ts 单列索引供清理范围扫描)。
-- 落账与 usage_ledger 同事务原子写;observe 下也照写(报表数据,不涉钱)。
CREATE TABLE IF NOT EXISTS usage_detail (
  id                BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  org_id            BIGINT UNSIGNED NOT NULL,
  member_id         BIGINT UNSIGNED NOT NULL,
  newapi_user_id    BIGINT          NOT NULL,
  key_id            BIGINT UNSIGNED NOT NULL DEFAULT 0,        -- 平台稳定 key_id(0=未归因),与 ledger 同口径
  team_id           BIGINT UNSIGNED NULL,                      -- 落账时成员归属团队(快照;下钻团队口径见报表层)
  model_name        VARCHAR(128)    NOT NULL,
  newapi_log_id     BIGINT          NOT NULL,                  -- new-api logs.id(幂等唯一键)
  prompt_tokens     BIGINT          NOT NULL DEFAULT 0,
  completion_tokens BIGINT          NOT NULL DEFAULT 0,
  consumed_quota    BIGINT          NOT NULL DEFAULT 0,
  log_ts            DATETIME(3)     NOT NULL,                  -- 该次调用发生时刻(new-api CreatedAt)
  created_at        DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uk_detail_log (newapi_log_id),
  KEY idx_detail_org_member_ts (org_id, member_id, log_ts),
  KEY idx_detail_org_key_ts (org_id, key_id, log_ts),
  KEY idx_detail_org_ts (org_id, log_ts),
  KEY idx_detail_log_ts (log_ts)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
