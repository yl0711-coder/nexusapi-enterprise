-- 迁移 0022(v1 观测管理版,20-§2/§13):组织正交属性 + 门B 关联唯一约束。
-- 一种组织、按正交属性工作,不按场景分支(场景仅决定初值):
--   funding_mode:钱谁管(v1 恒 self_funded;platform_funded=v2 平台经手钱/托管桶)
--   newapi_user_created_by_platform:provenance(门A=1 可清理资产/可自愈;门B=0 绝不删企业资产/运维重粘)
--     注:20-§13 还列了 newapi_link_mode(created|associated),与本布尔同义冗余(16 时代遗产),按零技术债只建布尔。
--   member_cap_mode:成员额度模式(v1 恒 shared 全员共享池子;quota 按人硬分=v2)
--   billing_kind:new-api 侧计费口径(wallet=读求和余额;subscription=订阅计费,余额页显示"订阅计费")
-- A3(五路验收):本文件只留**单条原子 ALTER**(4 列一次加);唯一索引拆到 0023 独立文件——
-- 避免"列已加但同文件后续 CREATE INDEX 失败→整文件未记→重启撞 Duplicate column"卡死。
ALTER TABLE organization
  ADD COLUMN funding_mode                    VARCHAR(16) NOT NULL DEFAULT 'self_funded' AFTER newapi_password_enc,
  ADD COLUMN newapi_user_created_by_platform TINYINT(1)  NOT NULL DEFAULT 1             AFTER funding_mode,
  ADD COLUMN member_cap_mode                 VARCHAR(8)  NOT NULL DEFAULT 'shared'      AFTER newapi_user_created_by_platform,
  ADD COLUMN billing_kind                    VARCHAR(16) NOT NULL DEFAULT 'wallet'      AFTER member_cap_mode;
