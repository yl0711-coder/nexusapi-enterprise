-- 架构B(31-ADR §3/33-§2-0033):平台配置 KV(超管可配)。QuotaPerUnit 不硬编码的落点。
-- money_freeze:全局钱动作急停(34 §3-⑤)——与已废弃的 observe 短路是两回事:急停=灰度事故一把止血,
-- 平时恒 false,非业务分支;Transfer 家族三入口(Transfer/RunSubscriptionTopup/ReconcileTransfers 的写)统一管辖,
-- reconcile 的读/检测/告警不受 freeze 影响(33 §10 forward-note-1)。
-- 回滚:DROP TABLE platform_setting;
CREATE TABLE IF NOT EXISTS platform_setting (
  k          VARCHAR(64)  NOT NULL,
  v          VARCHAR(255) NOT NULL,
  updated_by VARCHAR(64)  NOT NULL DEFAULT '',
  updated_at DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (k)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

INSERT IGNORE INTO platform_setting (k, v) VALUES
  ('quota_per_unit', '500000'),                    -- 与所连 new-api 实例对齐;启动 VerifyQuotaPerUnit 自检,不一致拒启动
  ('member_token_limit', '2'),                     -- 每成员令牌数上限(超管可配)
  ('member_quota_cap_raw', '500000000'),           -- 成员额度帽,默认 $1000=5亿 raw(int32 安全区)
  ('treasury_low_watermark_raw', '50000000'),      -- 金库低预警阈值,默认 $100(绝对值,可配)
  ('money_freeze', 'false');                       -- 全局钱动作急停(事故止血,平时恒 false)
