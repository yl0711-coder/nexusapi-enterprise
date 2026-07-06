-- new-api 日志镜像:独立于 usage_detail / usage_ledger,只服务运营排障与成员-token 映射查看。
-- 红线:该表不参与计费、不反推余额、不影响 settlement_cursor。
CREATE TABLE IF NOT EXISTS org_newapi_log (
  id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
  org_id BIGINT NOT NULL,
  member_id BIGINT NOT NULL DEFAULT 0,
  key_id BIGINT NOT NULL DEFAULT 0,
  newapi_user_id BIGINT NOT NULL,
  newapi_token_id BIGINT NOT NULL DEFAULT 0,
  token_name VARCHAR(191) NULL,
  log_type INT NOT NULL DEFAULT 0,
  model_name VARCHAR(191) NULL,
  channel_id INT NOT NULL DEFAULT 0,
  channel_name VARCHAR(191) NULL,
  group_name VARCHAR(191) NULL,
  request_id VARCHAR(128) NULL,
  quota BIGINT NOT NULL DEFAULT 0,
  prompt_tokens BIGINT NOT NULL DEFAULT 0,
  completion_tokens BIGINT NOT NULL DEFAULT 0,
  use_time INT NOT NULL DEFAULT 0,
  is_stream TINYINT(1) NOT NULL DEFAULT 0,
  content TEXT NULL,
  ip VARCHAR(128) NULL,
  other MEDIUMTEXT NULL,
  newapi_log_id BIGINT NOT NULL,
  log_ts DATETIME(3) NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_org_newapi_log_id (newapi_log_id),
  KEY idx_org_log_ts (org_id, log_ts),
  KEY idx_org_type_ts (org_id, log_type, log_ts),
  KEY idx_org_member_ts (org_id, member_id, log_ts),
  KEY idx_org_token_ts (org_id, newapi_token_id, log_ts),
  KEY idx_org_request_id (org_id, request_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS org_newapi_log_cursor (
  org_id BIGINT NOT NULL PRIMARY KEY,
  cursor_ts BIGINT NOT NULL DEFAULT 0,
  cursor_log_id BIGINT NOT NULL DEFAULT 0,
  last_run_at DATETIME(3) NULL,
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
