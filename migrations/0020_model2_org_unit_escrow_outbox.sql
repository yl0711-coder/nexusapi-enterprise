-- 迁移 0020(模型2 R0 地基·增量部分):组织树 org_unit + 托管多桶 escrow_bucket + 开通 outbox + organization 加池子锚/凭证列。
-- 纯增量(新表 + 加列),不动 member 旧身份字段——身份翻转(去 member.newapi_user_id 等)在后续迁移单独做,使本步编译/迁移零破坏。
-- 范式见 10-ADR / 14-技术方案 §2。约定:InnoDB + utf8mb4_0900_ai_ci;VARCHAR 枚举(不用 MySQL ENUM);DATETIME(3) UTC;org_id 前缀索引;无外键。

-- 1. org_unit:组织内部任意深度树(邻接 parent_id + 物化路径 path)。每企业一棵独立树,企业本身=根节点(parent_id=NULL)。
CREATE TABLE IF NOT EXISTS org_unit (
  id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  org_id     BIGINT UNSIGNED NOT NULL,
  parent_id  BIGINT UNSIGNED NULL,                          -- 根节点为 NULL
  path       VARCHAR(255)    NOT NULL DEFAULT '',           -- 物化路径 "/1/7/22/"(含首尾斜杠),子树前缀匹配/rollup 用
  name       VARCHAR(128)    NOT NULL,
  type       VARCHAR(16)     NOT NULL DEFAULT 'department',  -- 自由标签 division/department/team/group,不强制层数
  status     VARCHAR(16)     NOT NULL DEFAULT 'active',      -- active/archived
  created_at DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  deleted_at DATETIME(3)     NULL,                           -- 软删(历史归属可解析:节点删了报表仍能解释旧记录)
  PRIMARY KEY (id),
  KEY idx_org_unit_org_parent (org_id, parent_id),
  KEY idx_org_unit_org_path (org_id, path)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- 2. escrow_bucket:托管多桶(治 user.quota int32 ~$4294 上限)。seq=1/active=镜像进 org user.quota 的可花窗口;其余=平台库托管。
--    续充=桶1 低于 threshold 时把下一桶 add 进 org user.quota(绝不 override、合并后 ≤ int32)。详见 14 §3.3/§3.7。
CREATE TABLE IF NOT EXISTS escrow_bucket (
  id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  org_id     BIGINT UNSIGNED NOT NULL,
  seq        INT             NOT NULL,                       -- 桶序(1=active 窗口)
  amount     BIGINT          NOT NULL DEFAULT 0,             -- 桶内额度(quota)
  status     VARCHAR(16)     NOT NULL DEFAULT 'holding',     -- active(镜像进窗口)/holding(平台库托管)/merged(已并入窗口)
  threshold  BIGINT          NOT NULL DEFAULT 0,             -- 触发续充阈值(必须 > 单笔最大请求成本)
  created_at DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uk_escrow_org_seq (org_id, seq),
  KEY idx_escrow_org_status (org_id, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- 3. outbox:开通意图(Transactional Outbox,崩溃安全)。开通请求与平台库记录同一本地事务写 outbox,worker 拉取执行。
--    idempotency_key 唯一=显式幂等键(重试不在 new-api 建重复 user/token);前向恢复(重试到成功),不补偿回滚。
CREATE TABLE IF NOT EXISTS outbox (
  id              BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  aggregate_type  VARCHAR(32)     NOT NULL,                  -- organization / member
  aggregate_id    BIGINT UNSIGNED NOT NULL,
  payload         JSON            NULL,
  status          VARCHAR(16)     NOT NULL DEFAULT 'pending', -- pending/done/failed
  idempotency_key VARCHAR(128)    NOT NULL,
  attempts        INT             NOT NULL DEFAULT 0,
  last_error      VARCHAR(512)    NULL,
  created_at      DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at      DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uk_outbox_idem (idempotency_key),
  KEY idx_outbox_status (status, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- 4. organization 加池子锚 + 组织 user 凭证(应用层加密存)。模型2:组织=一个 new-api user(持 user.quota=池子)。
--    access_token 供日常以 org 身份建员工 token;password 供 access_token 失效时重登录自愈(类比 member.member_password_enc)。
ALTER TABLE organization ADD COLUMN newapi_user_id BIGINT NULL AFTER slug;
ALTER TABLE organization ADD COLUMN newapi_access_token_enc VARBINARY(1024) NULL AFTER newapi_user_id;
ALTER TABLE organization ADD COLUMN newapi_password_enc VARBINARY(1024) NULL AFTER newapi_access_token_enc;
