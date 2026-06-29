-- 企业管理平台 · 迁移 0018(v2 M0:组织池"已发放占用" committed)
--
-- v2 不超卖(周期发放模型,14 §3.5):组织池在 company_balance 上明确三量——
--   total_recharged(累计预付,已有)
--   committed      (Σ当前已发放占用 = Σ成员 granted;本列新增,不超卖与"可分配"派生用)
--   真实消费       (派生自日志,不落库为可变镜像)
-- 可分配 = total_recharged - committed。committed 的增减必须与成员 granted 在同一事务(14 §3.5)。
--
-- 本期(观测)committed 恒为 0(不发放);本迁移只加列,发放/回收逻辑在 M4 接缝、flag 关。

ALTER TABLE company_balance
  ADD COLUMN committed BIGINT NOT NULL DEFAULT 0 AFTER total_refunded;
