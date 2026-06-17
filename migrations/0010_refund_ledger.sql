-- 企业管理平台 · 迁移 0010(R2-S1:退款冲正独立流水,不污染累计充值)
-- 守恒恒等式:balance = total_recharged - total_refunded - total_consumed。
-- 冲正只 total_refunded += amount(不动 total_recharged),并落独立 refund 流水。

ALTER TABLE company_balance ADD COLUMN total_refunded BIGINT NOT NULL DEFAULT 0;

CREATE TABLE IF NOT EXISTS refund (
  id           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  org_id       BIGINT UNSIGNED NOT NULL,
  amount       BIGINT          NOT NULL,                  -- 冲正额度(quota)
  reason       VARCHAR(512)    NOT NULL,
  operator     VARCHAR(128)    NOT NULL,                  -- 运营方
  created_at   DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  KEY idx_refund_org_time (org_id, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
