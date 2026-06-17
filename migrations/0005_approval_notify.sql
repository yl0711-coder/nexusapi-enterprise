-- 企业管理平台 · 迁移 0005(里程碑 4:成员自助 + 申请-审批 US-06 + 通知 US-13)
--
-- approval 依据 09 §12(状态机以 08 §3.1 为准)。notification 为补 09 缺口(US-13 站内通知载体,回填 09)。

-- 1. approval(申请-审批)—— 09 §12。三档:auto_approved / 一审(团队负责人)/ 二审(组织管理员)。
CREATE TABLE IF NOT EXISTS approval (
  id             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  org_id         BIGINT UNSIGNED NOT NULL,
  applicant_id   BIGINT UNSIGNED NOT NULL,                  -- 申请人(member)
  team_id        BIGINT UNSIGNED NULL,
  request_type   VARCHAR(24)     NOT NULL,                  -- quota_raise / model_open
  payload        JSON            NOT NULL,                  -- {model, amount, duration, reason}
  state          VARCHAR(24)     NOT NULL DEFAULT 'pending',-- pending/l1_approved/approved/rejected/auto_approved/cancelled
  is_level2      TINYINT(1)      NOT NULL DEFAULT 0,        -- 是否二审(>阈值 或 开新模型)
  l1_reviewer_id BIGINT UNSIGNED NULL,
  l1_reviewed_at DATETIME(3)     NULL,
  l2_reviewer_id BIGINT UNSIGNED NULL,
  l2_reviewed_at DATETIME(3)     NULL,
  reject_reason  VARCHAR(512)    NULL,
  created_at     DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at     DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  KEY idx_approval_org_state (org_id, state),
  KEY idx_approval_org_applicant (org_id, applicant_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- 2. notification(站内通知)—— 补 09 缺口。只读列表、可标已读;成员只收与本人相关(脱敏,不含跨组织)。
CREATE TABLE IF NOT EXISTS notification (
  id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  org_id     BIGINT UNSIGNED NOT NULL,
  member_id  BIGINT UNSIGNED NOT NULL,                      -- 收件人(member)
  type       VARCHAR(32)     NOT NULL,                      -- approval_result/balance_low/quota_reset/grant_expire 等
  title      VARCHAR(191)    NOT NULL,
  body       VARCHAR(1024)   NULL,
  is_read    TINYINT(1)      NOT NULL DEFAULT 0,
  created_at DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  KEY idx_notif_member (org_id, member_id, is_read, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
