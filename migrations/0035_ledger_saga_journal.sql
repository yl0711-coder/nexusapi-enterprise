-- 架构B 阶段1(BE②,组长批复 33-§12-1):ledger_transfer 增加 saga 步骤日志——精确补齐的信息基础。
-- 背景:new-api ManageUser add/subtract 非幂等、无请求 ID,pending 行无法从"双方当前 quota"唯一还原进度
-- (成员在并发消费/金库在被代充)。业界标准解法 = saga step journal:每完成一步上游写,立刻把进度落回账本行,
-- 三态判定(未扣/已扣未加/已加未记)从"猜"变成"读"。
--   phase:               recorded=意图已记、出账未确认;debited=出账已确认;credited=入账已确认
--   debited_raw:         出账实扣值(quota_guard clamp 可能 < amount_raw;sweep 退额模式下合法)
--   from_balance_before: Transfer 步骤①在 org 锁内读到的出账方余额(recorded 行"相对取证法"的锚点)
--   fail_reason:         判死原因(debit_not_landed/partial_debit/refunding/refunded/sweep_empty)
-- 对 BE③ 只读方无影响(纯增列)。
-- 回滚:ALTER TABLE ledger_transfer DROP COLUMN phase, DROP COLUMN debited_raw, DROP COLUMN from_balance_before, DROP COLUMN fail_reason;
ALTER TABLE ledger_transfer ADD COLUMN debited_raw BIGINT NOT NULL DEFAULT 0 AFTER amount_raw;
ALTER TABLE ledger_transfer ADD COLUMN phase ENUM('recorded','debited','credited') NOT NULL DEFAULT 'recorded' AFTER status;
ALTER TABLE ledger_transfer ADD COLUMN from_balance_before BIGINT NULL AFTER phase;
ALTER TABLE ledger_transfer ADD COLUMN fail_reason VARCHAR(128) NOT NULL DEFAULT '' AFTER reason;

-- 存量回填(幂等语义:仅一次性迁移执行;线上无真实数据,兜测试库存量):
-- 已 applied 的行双边必已完成 → phase='credited'、debited_raw=amount_raw。failed/pending 保持 recorded 缺省。
UPDATE ledger_transfer SET phase = 'credited', debited_raw = amount_raw WHERE status = 'applied';
