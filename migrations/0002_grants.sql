-- 企业管理平台 · 迁移 0002(里程碑 2:额度执行 — 调额 / 临时权限 / grant 到期回退)
--
-- 依据 09 §11 grant 表。【对 09 的落地命名变更,需回填 09】:09 表名为 `grant`,
-- 是 MySQL 保留字(到处要反引号、易错),落地改名 member_grant,字段/语义不变。
--
-- grant_type(09 §14 + 08 §3.4 状态机口径合并):
--   quota_add / quota_sub  临时增 / 减额(US-03 / US-04c;payload.delta 带符号)
--   model_add              临时放开模型(US-04b;payload.model);本期记录,模型硬隔离见 03 §3.4.1
--   account_ttl            临时账号有效期(US-04a;到期 worker disable 用户、member→expired)
-- status:active / expired / revoked(09 §14)。

CREATE TABLE IF NOT EXISTS member_grant (
  id           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  org_id       BIGINT UNSIGNED NOT NULL,
  member_id    BIGINT UNSIGNED NOT NULL,
  grant_type   VARCHAR(24)     NOT NULL,                      -- quota_add/quota_sub/model_add/account_ttl
  payload      JSON            NOT NULL,                      -- {delta,duration,model,...}
  reason       VARCHAR(512)    NULL,
  operator     VARCHAR(128)    NOT NULL,                      -- 授予人(审计)
  effective_at DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  expire_at    DATETIME(3)     NOT NULL,                      -- 到期 worker 反向应用
  status       VARCHAR(16)     NOT NULL DEFAULT 'active',     -- active/expired/revoked
  reverted_at  DATETIME(3)     NULL,                          -- 实际反向应用时间
  created_at   DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at   DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  KEY idx_grant_org_member (org_id, member_id, status),
  KEY idx_grant_expire (status, expire_at)                    -- worker 扫 status=active AND expire_at<=now
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
