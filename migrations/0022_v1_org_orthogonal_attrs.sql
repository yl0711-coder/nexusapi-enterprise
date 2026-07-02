-- 迁移 0022(v1 观测管理版,20-§2/§13):组织正交属性 + 门B 关联唯一约束。
-- 一种组织、按正交属性工作,不按场景分支(场景仅决定初值):
--   funding_mode:钱谁管(v1 恒 self_funded;platform_funded=v2 平台经手钱/托管桶)
--   newapi_user_created_by_platform:provenance(门A=1 可清理资产/可自愈;门B=0 绝不删企业资产/运维重粘)
--     注:20-§13 还列了 newapi_link_mode(created|associated),与本布尔同义冗余(16 时代遗产),按零技术债只建布尔。
--   member_cap_mode:成员额度模式(v1 恒 shared 全员共享池子;quota 按人硬分=v2)
--   billing_kind:new-api 侧计费口径(wallet=读求和余额;subscription=订阅计费,余额页显示"订阅计费")
ALTER TABLE organization
  ADD COLUMN funding_mode                    VARCHAR(16) NOT NULL DEFAULT 'self_funded' AFTER newapi_password_enc,
  ADD COLUMN newapi_user_created_by_platform TINYINT(1)  NOT NULL DEFAULT 1             AFTER funding_mode,
  ADD COLUMN member_cap_mode                 VARCHAR(8)  NOT NULL DEFAULT 'shared'      AFTER newapi_user_created_by_platform,
  ADD COLUMN billing_kind                    VARCHAR(16) NOT NULL DEFAULT 'wallet'      AFTER member_cap_mode;

-- 一个 new-api 用户只能属一个组织(门B 防重复关联/串数据;NULL 不冲突,未开通组织不受影响)。
CREATE UNIQUE INDEX uk_org_newapi_user ON organization (newapi_user_id);
