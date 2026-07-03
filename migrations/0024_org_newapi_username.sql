-- 迁移 0024(v1.1 项B):门A 组织的 new-api 用户名改为"随机名存库",不再用可猜的 org<orgID>。
-- 背景:共享 new-api 实例上 org2/org3 又短又可猜、无保留 → 撞主站客户/被抢注,且撞名后无归属校验就接管(可能误伤)。
-- 修法:首次 provision 生成高熵随机名(ent_<base32>,≤20 满足 rc.4 username 约束)落此列;
--       重开/自愈从此列读名;adopt 只允许"名==本组织存的名"(见 service/adapter 归属校验)。
-- 门B 关联组织用企业自己的 new-api 用户名(不走此列,保持 NULL,不受影响)。
ALTER TABLE organization
  ADD COLUMN newapi_username VARCHAR(32) NULL AFTER newapi_user_id;
