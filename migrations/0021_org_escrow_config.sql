-- 迁移 0021(模型2 R5后步骤3):per-org escrow 策略表——续充阈值(补货点/reorder point)。
-- 生效阈值 = COALESCE(threshold_manual_override, threshold_auto):运维手动定优先,否则用每天按近7天日志重算的自动值。
-- 单独表(不塞 organization/escrow_bucket):为 escrow 策略后续扩展(窗口上限覆盖/续充额等)留位、不撑大 organization。
CREATE TABLE IF NOT EXISTS org_escrow_config (
  org_id                    BIGINT UNSIGNED NOT NULL,
  threshold_auto            BIGINT          NOT NULL DEFAULT 50000000,  -- 每天按补货点公式重算(新组织/历史<7天=DEFAULT_NEW ~$100)
  threshold_manual_override BIGINT          NULL,                        -- 运维单组织手动覆盖(优先);NULL=用 auto
  updated_at                DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (org_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
