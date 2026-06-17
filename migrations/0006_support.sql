-- 企业管理平台 · 迁移 0006(里程碑 5:运营方三层支持 — support_session)
-- 依据 09 §14(状态机以 08 §3.2 为准)。只读态/协助态、授权/破玻璃、时限。
CREATE TABLE IF NOT EXISTS support_session (
  id                 BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  org_id             BIGINT UNSIGNED NOT NULL,                -- 绑定单一组织,跨 org 读写一律拒
  actor              VARCHAR(128)    NOT NULL,                -- 运营方真实身份
  on_behalf_of       VARCHAR(128)    NOT NULL,                -- 客户管理员身份
  scope              VARCHAR(16)     NOT NULL DEFAULT 'readonly', -- readonly/assist
  grant_type         VARCHAR(16)     NULL,                    -- authorized/break_glass(assist 必填)
  state              VARCHAR(16)     NOT NULL DEFAULT 'active',-- active/expired/revoked
  granted_by         VARCHAR(128)    NULL,
  started_at         DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  expire_at          DATETIME(3)     NOT NULL,
  revoked_at         DATETIME(3)     NULL,
  created_at         DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at         DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  KEY idx_session_org_state (org_id, state),
  KEY idx_session_expire (state, expire_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
