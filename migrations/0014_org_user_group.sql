-- 0014:组织用户分组(改动① 灰度MVP)——复用既有 organization.newapi_group 列作"组织专属 new-api 用户分组"
-- (隔离边界 + 唯一,红线④)。不加列(0001 已建 newapi_group);折扣年代曾把派生值写进该列、值不可信,
-- 故**无条件回填**为当前实际在用的派生值 org_{id}(冲掉脏值、对齐开通侧 orgUserGroup 派生),再建唯一索引。
-- 配套:折扣路径已解耦对该列的写(repo.UpdateOrgDiscount 不再写 newapi_group),用户分组从此只读不写。
UPDATE organization SET newapi_group = CONCAT('org_', id);
CREATE UNIQUE INDEX uq_org_user_group ON organization (newapi_group);
