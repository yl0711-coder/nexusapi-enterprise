-- 企业管理平台 · 迁移 0015(v2 M0 地基:成员↔token 1:N + 平台稳定 key_id)
--
-- 现状:成员↔token 1:1(member.newapi_token_id)。v2 要求一成员可挂多把 key,且报表/账本
-- 按"平台稳定 key_id"归因(轮换换底层 new-api token_id 也不断历史,14 §3.2)。
--
-- 拆两张表:
--   member_key_slot  = 稳定 key 槽(id = 平台 key_id),1:N 挂成员;轮换不变。
--   member_key_token = 物理 new-api token(append-only):轮换=插新行(新 token_id/递增 rotation)+
--                      旧行 is_current=0,绝不删 → 历史日志按旧 token_id/token_name 仍能映射回同一 key_id。
--
-- 归因映射:结算读日志的 token_id/token_name(new-api Log 两字段都有),按 member_key_token
-- 查回 key_id(slot)。token_name 形如 nexus_m{member_id}_v{rotation}(deriveTokenName 确定性)。
--
-- 本迁移含回填:把现有"已开通且有令牌"的成员各建 1 个主 slot + 1 条 current token,
-- token_name 按现有 (member.id, key_rotation) 重建,与 deriveTokenName 完全一致。

-- 1. member_key_slot(稳定 key 槽)。id = 平台稳定 key_id。
CREATE TABLE IF NOT EXISTS member_key_slot (
  id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  org_id     BIGINT UNSIGNED NOT NULL,
  member_id  BIGINT UNSIGNED NOT NULL,
  is_primary TINYINT(1)      NOT NULL DEFAULT 0,            -- 成员主槽(兼容现"当前主 token"指针)
  status     VARCHAR(16)     NOT NULL DEFAULT 'active',     -- active/revoked
  created_at DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  KEY idx_key_slot_org_member (org_id, member_id),
  KEY idx_key_slot_member (member_id, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- 2. member_key_token(物理 new-api 令牌,append-only)。轮换插新行、旧行置 is_current=0。
CREATE TABLE IF NOT EXISTS member_key_token (
  id              BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  key_id          BIGINT UNSIGNED NOT NULL,                -- 所属稳定 key 槽(member_key_slot.id)
  org_id          BIGINT UNSIGNED NOT NULL,
  member_id       BIGINT UNSIGNED NOT NULL,
  newapi_token_id BIGINT          NULL,                    -- 物理 new-api token id(归因主键;NULL=尚未建成)
  token_name      VARCHAR(64)     NOT NULL,                -- 确定性令牌名 nexus_m{member}_v{rotation}(归因兜底键)
  is_current      TINYINT(1)      NOT NULL DEFAULT 1,      -- 该槽当前在用令牌(轮换后旧行置 0)
  key_masked      VARCHAR(64)     NULL,                    -- 脱敏 key
  rotation        INT             NOT NULL DEFAULT 0,
  status          VARCHAR(16)     NOT NULL DEFAULT 'active',-- active/revoked/superseded
  created_at      DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uk_key_token_newapi (newapi_token_id),        -- 归因:log.token_id 唯一映射回一行(NULL 允许多行)
  KEY idx_key_token_keyid (key_id),
  KEY idx_key_token_org_member (org_id, member_id),
  KEY idx_key_token_name (token_name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- 3. 回填:现有"已开通且有令牌"的成员各建 1 主 slot。
--    幂等(D 修复 2026-06-30):NOT EXISTS 防中断重跑重复建主槽(MySQL 不支持 ALTER/INSERT IF NOT EXISTS,故用子查询)。
INSERT INTO member_key_slot (org_id, member_id, is_primary, status)
SELECT m.org_id, m.id, 1, 'active'
  FROM member m
 WHERE m.newapi_token_id IS NOT NULL AND m.deleted_at IS NULL
   AND NOT EXISTS (SELECT 1 FROM member_key_slot s WHERE s.member_id = m.id AND s.is_primary = 1);

-- 4. 回填:为上面每个 slot 建 1 条 current token,token_name 按现 (member.id, key_rotation) 重建
--    (与 deriveTokenName 的 nexus_m%d_v%d 完全一致),newapi_token_id 取现有 member.newapi_token_id。
--    幂等:INSERT IGNORE 靠 uk_key_token_newapi(newapi_token_id 唯一)兜重跑,不重复落令牌。
INSERT IGNORE INTO member_key_token
  (key_id, org_id, member_id, newapi_token_id, token_name, is_current, key_masked, rotation, status)
SELECT s.id, m.org_id, m.id, m.newapi_token_id,
       CONCAT('nexus_m', m.id, '_v', m.key_rotation), 1, m.key_masked, m.key_rotation, 'active'
  FROM member m
  JOIN member_key_slot s ON s.member_id = m.id AND s.org_id = m.org_id AND s.is_primary = 1
 WHERE m.newapi_token_id IS NOT NULL AND m.deleted_at IS NULL;
