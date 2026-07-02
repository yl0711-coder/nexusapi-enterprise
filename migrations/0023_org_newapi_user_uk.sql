-- 迁移 0023(A3 拆分自 0022):一个 new-api 用户只能属一个组织(门B 防重复关联/串数据)。
-- 单条原子语句独立成文件:即便建索引失败(理论上仅当已存重复 newapi_user_id——v1 首次部署 organization 为空、
-- 不可能有 dup;仅在既有部署升级时才需防),0022 的列已安全记录,重跑只重试本文件。
-- NULL 不参与唯一(未开通组织 newapi_user_id 为 NULL,多个 NULL 不冲突)。
-- 【上线前置(检查单)】升级既有部署前先跑:
--   SELECT newapi_user_id, COUNT(*) FROM organization WHERE newapi_user_id IS NOT NULL AND deleted_at IS NULL
--     GROUP BY newapi_user_id HAVING COUNT(*) > 1;   -- 必须空集,否则先人工去重再迁移。
CREATE UNIQUE INDEX uk_org_newapi_user ON organization (newapi_user_id);
