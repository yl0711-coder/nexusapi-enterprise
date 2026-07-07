-- 架构B(31-ADR §2/33-§2-0034):语义变更留档(注释性迁移,无结构改动)。
-- organization.newapi_user_id 自本迁移起语义 = 【金库 new-api user】(持组织的钱池子):
--   · 充值:运营方只充金库,绝不直充成员(守恒;对账交叉恒等式专抓违规直充)。
--   · 组织总余额 = 金库 user.quota + Σ成员 user.quota(读 DB 不读缓存)。
--   · 组织硬停 = disable 金库 + fan-out disable 全部成员 user。
-- organization.newapi_access_token_enc/newapi_password_enc = 金库 user 凭证(沿用 0020 列)。
-- A 版「组织 user 下挂全部成员 token」语义作废;成员 token 自 0030 起挂各自成员 user 下。
-- 本文件无 DDL,仅登记 schema_migration 作语义变更锚点。
SELECT 1;
