-- 企业管理平台 · 迁移 0008(补全:周期重置 worker 记录上次重置点)
ALTER TABLE quota_policy ADD COLUMN last_reset_at DATETIME(3) NULL;
