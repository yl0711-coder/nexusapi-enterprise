-- 架构B(31-ADR §4/33-§2-0031):分配账本 = 划账守恒真相(金库→成员/成员→金库,复式记账)。
-- 幂等不靠单次调用,靠 idempotency_key 唯一键 + 对账环"读-核-补"(pending→applied/failed)。
-- 全 raw quota(bigint,与 new-api 同单位)、DATETIME(3)(0025 的 2038 坑不复制)。保留 ≥1 年(ADR §15)。
-- 可见性:超管全部/组织本组织/成员仅给自己的到账(member_id 冗余列即为此查询服务)。
-- 回滚:DROP TABLE ledger_transfer;
CREATE TABLE IF NOT EXISTS ledger_transfer (
  id               BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  org_id           BIGINT UNSIGNED NOT NULL,
  from_user_id     BIGINT          NOT NULL,              -- new-api user id(出账方:金库或成员)
  to_user_id       BIGINT          NOT NULL,              -- new-api user id(入账方)
  member_id        BIGINT UNSIGNED NOT NULL DEFAULT 0,    -- 冗余:关联成员(账本可见性查询;组织级动作=0)
  amount_raw       BIGINT          NOT NULL,              -- raw quota,恒正;方向由 from/to 表达
  idempotency_key  VARCHAR(128)    NOT NULL,              -- 幂等键(重放命中读状态直接返回)
  status           ENUM('pending','applied','failed') NOT NULL DEFAULT 'pending',
  reason           VARCHAR(64)     NOT NULL DEFAULT '',   -- initial_grant/topup/offboard_refund/subscription_topup/reconcile_fix
  created_by       VARCHAR(64)     NOT NULL DEFAULT '',   -- 操作者(actor 标识,审计)
  created_at       DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  applied_at       DATETIME(3)     NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_ledger_idem (idempotency_key),
  KEY idx_ledger_org_time (org_id, created_at),
  KEY idx_ledger_member_time (member_id, created_at),
  KEY idx_ledger_status (status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
