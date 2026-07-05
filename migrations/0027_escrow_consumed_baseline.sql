-- 迁移 0027(深审 B1):escrow 窗口纠偏的"已消费基线"。
-- funding 激活前(v1 观测期)的历史消费不应计入 v2 escrow 窗口纠偏与余额-台账对账,否则:
--   ① v2 首次充值的窗口被整段观测期消费冲成 0(客户真亏);② ReconcileBalanceLedger 每轮误报 D4 不一致。
-- 首次对账时快照 SUM(usage_ledger) 为基线,此后窗口纠偏/对账只算 (SUM(ledger) − baseline)。NULL = 尚未快照。
ALTER TABLE org_escrow_config ADD COLUMN consumed_baseline BIGINT NULL AFTER threshold_manual_override;
