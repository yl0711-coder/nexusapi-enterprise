-- T12 组织轻量归档:archived_at(NULL=未归档)。归档=隐藏+可检索+可恢复,不物理删除,
-- 与 status(计费态 active/low/stopped)、deleted_at(预留硬删)互不冲突。
ALTER TABLE organization ADD COLUMN archived_at DATETIME(3) NULL AFTER status;
ALTER TABLE organization ADD KEY idx_org_archived (archived_at);
