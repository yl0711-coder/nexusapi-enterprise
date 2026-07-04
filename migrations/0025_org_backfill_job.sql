-- 迁移 0025(交付批次·历史日志全量回填,文档 24-§7.1):门B 关联型组织的历史消费日志回填任务表。
-- 一组织一行。回填 worker(串行并入结算单写者,24-§4.5)据此从 boundary 往 0 方向逐子窗口灌 usage_ledger + usage_detail。
-- 边界语义(24-§3):关联瞬间原子快照全局 forward 水位 B=(boundary_ts, boundary_log_id);
--   回填只处理 (created_at,id) <= B、forward 只处理 > B,两流在日志级别绝不相交(ledger 无按行去重,喂两次=真多算)。
--   守卫(24-§3.2/§7.3):快照到 B_ts==0(结算 worker 尚未首跑)时,触发方把 boundary_ts 置为 now、令回填吃全量,避免静默丢历史。
-- 纯只读补报表:回填绝不碰 company_balance、绝不推进全局 settlement_cursor(24-§10 涉钱红线)。
CREATE TABLE IF NOT EXISTS org_backfill_job (
  org_id           BIGINT       NOT NULL PRIMARY KEY,       -- 一组织一行
  newapi_user_id   BIGINT       NOT NULL,
  newapi_username  VARCHAR(255) NOT NULL,                   -- /api/log/ username 精确过滤用(企业自己的 new-api 用户名)
  boundary_ts      BIGINT       NOT NULL,                   -- 关联时快照的 forward 水位 ts(B_ts)
  boundary_log_id  BIGINT       NOT NULL,                   -- 关联时快照的 forward 水位 log_id(B_id)
  cursor_ts        BIGINT       NOT NULL,                   -- 回填进度(往 0 递减);= boundary_ts 表示未开始
  earliest_seen_ts BIGINT       NULL,                       -- 实际见到的最早日志 ts(界面显示历史起点)
  status           VARCHAR(16)  NOT NULL DEFAULT 'pending', -- pending|running|done|failed
  rows_ingested    BIGINT       NOT NULL DEFAULT 0,
  last_error       TEXT         NULL,
  created_at       TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at       TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  KEY idx_backfill_status (status)                          -- worker 轮询未完成任务(pending/running)
);
