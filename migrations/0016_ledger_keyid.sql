-- 企业管理平台 · 迁移 0016(v2 M0:usage_ledger 加 key_id 维度)
--
-- v2 报表要求按"平台稳定 key_id"归因(实体含 key 维度,13 §4.4)。给 usage_ledger 加 key_id 列,
-- 并把去重键纳入 key_id,使同一 (org,user,model,小时桶) 下不同 key 各成一行、互不覆盖。
--
-- 兼容:key_id 默认 0 = 未归因(历史行 / 非平台成员 / 尚未接 key_id 的结算)。本迁移只改 schema,
-- 结算写入仍走旧列(key_id 取默认 0),行为与现状完全一致;按 key_id 真归因在 M0-S2 接结算时启用。
-- 加常量列到唯一键不会产生冲突(历史行原本就在 (org,user,model,桶) 上唯一,补 key_id=0 仍唯一)。

ALTER TABLE usage_ledger
  ADD COLUMN key_id BIGINT UNSIGNED NOT NULL DEFAULT 0 AFTER newapi_user_id;

ALTER TABLE usage_ledger DROP INDEX uk_ledger_dedup;

ALTER TABLE usage_ledger
  ADD UNIQUE KEY uk_ledger_dedup (org_id, newapi_user_id, key_id, model_name, time_bucket);

ALTER TABLE usage_ledger
  ADD KEY idx_ledger_org_key (org_id, key_id, time_bucket);
