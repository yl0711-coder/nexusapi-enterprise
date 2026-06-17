-- 企业管理平台 · 迁移 0009(修正审批阈值单位 bug:之前默认是 100元/300元,应为 10万元/30万元)
-- 单位 quota = 元×500000。10万元=50000000000;30万元=150000000000。
ALTER TABLE organization
  ALTER COLUMN approval_auto_max_quota SET DEFAULT 50000000000,
  ALTER COLUMN approval_l1_max_quota   SET DEFAULT 150000000000;

-- 修正存量(把按旧默认建的值抬到正确量级;已被运营改过的非默认值不动)。
UPDATE organization SET approval_auto_max_quota = 50000000000  WHERE approval_auto_max_quota = 50000000;
UPDATE organization SET approval_l1_max_quota   = 150000000000 WHERE approval_l1_max_quota   = 150000000;
