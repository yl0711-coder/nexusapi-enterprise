-- 🔴 修结算少收 bug(灰度前阻断):同一小时桶第 2 笔起被 INSERT IGNORE 丢弃、永不扣。
-- 改为 log-id 级去重:cursor 记已结算到的最大 new-api 日志 id,每条日志只扣一次。
-- settlement_cursor.last_settled_log_id 在 0004 已建(BIGINT NULL,从未启用);本次启用:
--   回填 NULL→0,并收紧为 NOT NULL DEFAULT 0,供水位单调推进(new-api logs.id 自增)。
UPDATE settlement_cursor SET last_settled_log_id = 0 WHERE last_settled_log_id IS NULL;
ALTER TABLE settlement_cursor MODIFY COLUMN last_settled_log_id BIGINT NOT NULL DEFAULT 0;
