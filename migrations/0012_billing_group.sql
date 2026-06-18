-- T17 计费分组:令牌计价分组接 tier(D1 两级)。
-- organization.default_token_group:组织级默认令牌计价分组(D1)。与"用户分组 org_id"(代码现算、
--   不入库)区分;也不复用 newapi_group 死列,新加明确列免歧义。
-- member.newapi_group:开通时解析出的令牌分组快照(T17-1/Q2),轮换/白名单直接读、防丢档;
--   是快照非真相源,切档时同步刷新(Q5)。
-- tier.newapi_group 列已在 0001 存在(承载层级计价分组),本次开始真正接入,无需加列。
ALTER TABLE organization ADD COLUMN default_token_group VARCHAR(64) NULL AFTER newapi_group;
ALTER TABLE member ADD COLUMN newapi_group VARCHAR(64) NULL AFTER tier_id;
