-- 架构B(31-ADR/33-§2-0030):成员=各自 new-api user(平台托管服务账号)。
-- member 加 3 列:成员 new-api user id + 每成员一份加密凭证(AES-256-GCM,主密钥后续迁 KMS/信封)。
-- 旧列 newapi_token_id/key_masked/key_rotation 保留不删(语义废弃,代码停引用)。
-- bootstrap_state 增加约定值 'quarantined'(VARCHAR 无需 DDL;孤儿隔离:CreateUser 成功后续失败,
-- new-api 无干净删 user 能力 → disable+隔离标记+可重试,不硬删,33-§3.2)。
-- 回滚:ALTER TABLE member DROP COLUMN newapi_user_id, DROP COLUMN newapi_access_token_enc, DROP COLUMN newapi_password_enc;
ALTER TABLE member ADD COLUMN newapi_user_id BIGINT NULL AFTER bootstrap_state;
ALTER TABLE member ADD COLUMN newapi_username VARCHAR(64) NULL AFTER newapi_user_id;
ALTER TABLE member ADD COLUMN newapi_access_token_enc VARBINARY(1024) NULL AFTER newapi_username;
ALTER TABLE member ADD COLUMN newapi_password_enc VARBINARY(1024) NULL AFTER newapi_access_token_enc;
ALTER TABLE member ADD UNIQUE KEY uk_member_newapi_user (newapi_user_id);
