-- 企业管理平台 · 建库迁移 0001(里程碑 1:identity + org + RBAC + 开通成员)
--
-- 依据 09-数据字典与计量口径.md 的全表 DDL。本迁移落里程碑 1 实际用到的表
-- (organization / team / tier / member / audit_log)+ 平台工程必需补充表
-- (platform_idempotency)。09 的其余 MVP 表(quota_policy / company_balance /
-- recharge / usage_ledger / settlement_cursor / grant / approval /
-- support_session / project)在各自里程碑的后续迁移中建,不在本迁移堆砌未用表。
--
-- 通用约定(09 §0):InnoDB + utf8mb4_0900_ai_ci;每张业务表首业务列 org_id +
-- 以 org_id 起头的复合索引;时间列 DATETIME(3) UTC;软删除 deleted_at;
-- 枚举用 VARCHAR + 应用层校验(不用 MySQL ENUM)。
--
-- 【对 09 的补充,需回填 09】:member.platform_password_hash —— 09 的 member 表
-- 只有 login_email + member_password_enc(new-api 密码密文,重 bootstrap 兜底),
-- 没有平台自身登录的密码哈希列;而决策记录 §5 明确 MVP = 平台自有账号登录。
-- 故此处补一列 platform_password_hash(bcrypt,非密文/单向),供平台账号登录校验。

SET NAMES utf8mb4;

-- 1. organization(组织 / 公司)—— 09 §1
CREATE TABLE IF NOT EXISTS organization (
  id                    BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  name                  VARCHAR(128)    NOT NULL,
  slug                  VARCHAR(64)     NOT NULL,
  status                VARCHAR(16)     NOT NULL DEFAULT 'active',   -- active/low/stopped
  timezone              VARCHAR(64)     NOT NULL DEFAULT 'Asia/Shanghai',
  newapi_group          VARCHAR(64)     NULL,
  default_tier_id       BIGINT UNSIGNED NULL,
  -- 折扣镜像(配置后单向写入 new-api,本表只读回显;本期建列,计费在后续里程碑接)
  discount_mode         VARCHAR(16)     NOT NULL DEFAULT 'none',     -- none/total/per_model/fixed
  group_ratio           DECIMAL(10,4)   NULL,
  special_ratios        JSON            NULL,
  -- 后付费二期预留(本期建列即带,默认空 / prepaid,不进业务路径)
  billing_mode          VARCHAR(16)     NOT NULL DEFAULT 'prepaid',  -- prepaid/postpaid
  credit_limit          BIGINT          NOT NULL DEFAULT 0,
  temp_credit           BIGINT          NOT NULL DEFAULT 0,
  temp_credit_expire_at DATETIME(3)     NULL,
  billing_cycle         VARCHAR(16)     NULL,
  current_period_start  DATETIME(3)     NULL,
  grace_days            INT             NOT NULL DEFAULT 0,
  overdue_action        VARCHAR(16)     NULL,
  overdue_action_days   INT             NULL,
  buffer_ratio_soft     INT             NOT NULL DEFAULT 100,
  buffer_ratio_hard     INT             NOT NULL DEFAULT 100,
  trust_tier            VARCHAR(16)     NULL,
  created_at            DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at            DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  deleted_at            DATETIME(3)     NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_org_slug (slug),
  KEY idx_org_status (status),
  KEY idx_org_billing_mode (billing_mode)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- 2. team(团队 / 部门)—— 09 §2
CREATE TABLE IF NOT EXISTS team (
  id               BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  org_id           BIGINT UNSIGNED NOT NULL,
  name             VARCHAR(128)    NOT NULL,
  leader_member_id BIGINT UNSIGNED NULL,
  default_tier_id  BIGINT UNSIGNED NULL,
  status           VARCHAR(16)     NOT NULL DEFAULT 'active',        -- active/archived
  created_at       DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at       DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  deleted_at       DATETIME(3)     NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_team_org_name (org_id, name),
  KEY idx_team_org (org_id, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- 3. tier(层级)—— 09 §5
CREATE TABLE IF NOT EXISTS tier (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  org_id        BIGINT UNSIGNED NOT NULL,
  name          VARCHAR(128)    NOT NULL,
  model_set     JSON            NULL,                                -- 允许的模型集合
  daily_limit   BIGINT          NULL,
  weekly_limit  BIGINT          NULL,
  monthly_limit BIGINT          NULL,
  model_cap     JSON            NULL,
  newapi_group  VARCHAR(64)     NULL,                                -- 该层级映射的 new-api 分组(承载计价/模型集)
  is_default    TINYINT(1)      NOT NULL DEFAULT 0,                  -- 组织默认层级(全组织唯一,US-10)
  status        VARCHAR(16)     NOT NULL DEFAULT 'active',           -- active/archived
  created_at    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  deleted_at    DATETIME(3)     NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_tier_org_name (org_id, name),
  KEY idx_tier_org (org_id, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- 4. member(成员)—— 模型2(组织=newapi user、员工=该 user 下的 token)。
-- 模型2 改:member **不再持** newapi_user_id/access_token_enc/member_password_enc(那是 organization 的,见 0020);
-- 员工 token 在 org user 下建,member 仅留"当前令牌指针"(newapi_token_id/key_masked/key_rotation,member_key 表存全历史)。
-- team_id 暂留(兼容层,team→org_unit 切换在 R2);login_email 暂沿用 per-org 唯一(email 全局唯一化另起一刀)。
CREATE TABLE IF NOT EXISTS member (
  id                     BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  org_id                 BIGINT UNSIGNED NOT NULL,
  team_id                BIGINT UNSIGNED NULL,                       -- 兼容层(R2 切 org_unit)
  login_email            VARCHAR(191)    NOT NULL,                   -- 平台登录标识
  display_name           VARCHAR(128)    NULL,
  role                   VARCHAR(24)     NOT NULL DEFAULT 'member',  -- operator/org_admin/team_leader/member
  tier_id                BIGINT UNSIGNED NULL,
  status                 VARCHAR(16)     NOT NULL DEFAULT 'active',  -- active/disabled/expired/pending/provisioning
  expire_at              DATETIME(3)     NULL,
  -- 平台登录凭证(bcrypt 单向哈希,非密文)
  platform_password_hash VARCHAR(100)    NULL,
  bootstrapped_at        DATETIME(3)     NULL,
  -- 代发 key 产物(脱敏展示用;明文不落库,见 10 §3.1)。token 在 org user 下,这里是该成员当前令牌指针。
  newapi_token_id        BIGINT          NULL,                       -- 该成员当前令牌的 new-api token id(挂在 org user 下)
  key_masked             VARCHAR(64)     NULL,                       -- 脱敏 key(列表/详情回显)
  key_rotation           INT             NOT NULL DEFAULT 0,         -- 令牌轮换计数(派生确定性 token name)
  bootstrap_state        VARCHAR(16)     NOT NULL DEFAULT 'pending', -- pending/done/failed
  created_at             DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at             DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  deleted_at             DATETIME(3)     NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_member_org_email (org_id, login_email),
  KEY idx_member_org_team (org_id, team_id),
  KEY idx_member_org_status (org_id, status),
  KEY idx_member_org_role (org_id, role)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- 5. audit_log(审计日志)—— 09 §13。留痕是 AC 一等公民(08 §0.4),只追加。
CREATE TABLE IF NOT EXISTS audit_log (
  id                 BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  org_id             BIGINT UNSIGNED NOT NULL,
  actor              VARCHAR(128)    NOT NULL,                       -- 真实操作人
  on_behalf_of       VARCHAR(128)    NULL,                           -- 被代操作的客户身份(支持态双身份)
  support_session_id BIGINT UNSIGNED NULL,
  action             VARCHAR(64)     NOT NULL,                       -- 建成员/调额/停用 等
  target_type        VARCHAR(32)     NULL,                           -- member/balance/tier ...
  target_id          BIGINT UNSIGNED NULL,
  detail             JSON            NULL,                           -- 绝不写明文 key/密文凭证
  result             VARCHAR(16)     NOT NULL DEFAULT 'ok',          -- ok/failed
  request_id         VARCHAR(64)     NULL,                           -- 全链路追踪 id(10 §4.2)
  created_at         DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  KEY idx_audit_org_time (org_id, created_at),
  KEY idx_audit_org_actor (org_id, actor, created_at),
  KEY idx_audit_session (support_session_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- 6. platform_idempotency(写端点幂等)—— 10 §1.6 / §4.5。
-- 以 (org_id, endpoint, idempotency_key) 唯一;命中已成功记录回放原响应、不重复执行;
-- 命中进行中返回 409。response_snapshot 存成功时的信封 JSON 供回放。
CREATE TABLE IF NOT EXISTS platform_idempotency (
  id                BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  org_id            BIGINT UNSIGNED NOT NULL,
  endpoint          VARCHAR(128)    NOT NULL,                        -- 逻辑端点标识(method+path 模板)
  idempotency_key   VARCHAR(128)    NOT NULL,                        -- 客户端 UUID
  state             VARCHAR(16)     NOT NULL DEFAULT 'in_progress',  -- in_progress/done
  http_status       INT             NULL,                            -- 成功时回放的 HTTP 码
  response_snapshot JSON            NULL,                            -- 成功时回放的响应信封
  created_at        DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at        DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uk_idemp (org_id, endpoint, idempotency_key)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
