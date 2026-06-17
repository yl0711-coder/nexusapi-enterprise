-- 企业管理平台 · 迁移 0003(里程碑 3a:充值入账 / 申请 / 余额 / 低位告警 — 钱进与只读)
--
-- 依据 09 §7/§8。company_balance 带乐观锁 version;recharge 以 transfer_no 唯一作入账幂等键。
-- 【对 09 的补充,需回填 09】:recharge_request —— 09 MVP 表无"申请充值/退款申请"载体
-- (§15.3 credit_requests 是二期授信),而 US-09/US-12 需要;故补此表(只发起申请、绝不改余额)。
--
-- 本迁移不含 usage_ledger / settlement_cursor(那是 3b 读 logs 扣费,逐组织灰度,下一轮建)。

-- 1. company_balance(公司预付余额)—— 09 §7。每组织一行,乐观锁扣减。
CREATE TABLE IF NOT EXISTS company_balance (
  id              BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  org_id          BIGINT UNSIGNED NOT NULL,
  total_recharged BIGINT          NOT NULL DEFAULT 0,         -- 累计预付(quota)
  total_consumed  BIGINT          NOT NULL DEFAULT 0,         -- 累计消耗(quota);3a 恒 0,3b 读 logs 累加
  balance         BIGINT          NOT NULL DEFAULT 0,         -- 当前余额 = total_recharged - total_consumed(冗余便查)
  low_watermark   BIGINT          NOT NULL DEFAULT 0,         -- 低位告警阈值(quota)
  version         BIGINT          NOT NULL DEFAULT 0,         -- 乐观锁,每次改 +1
  created_at      DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at      DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uk_balance_org (org_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- 2. recharge(充值 / 入账记录)—— 09 §8。transfer_no 唯一 = 入账幂等(防同一笔转账重复入账)。只追加。
CREATE TABLE IF NOT EXISTS recharge (
  id           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  org_id       BIGINT UNSIGNED NOT NULL,
  amount       BIGINT          NOT NULL,                      -- 入账额度(quota)
  amount_cny   BIGINT          NULL,                          -- 原始收款人民币(分),仅留痕,不参与余额
  transfer_no  VARCHAR(128)    NOT NULL,                      -- 入账幂等键:对公转账唯一号/银行流水号
  operator     VARCHAR(128)    NOT NULL,                      -- 入账操作人(运营方)
  note         VARCHAR(512)    NULL,
  recharged_at DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  created_at   DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uk_recharge_transfer (org_id, transfer_no),
  KEY idx_recharge_org_time (org_id, recharged_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- 3. recharge_request(申请充值 / 退款申请)—— 补 09 缺口。只发起申请、绝不改余额(US-09/US-12)。
CREATE TABLE IF NOT EXISTS recharge_request (
  id           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  org_id       BIGINT UNSIGNED NOT NULL,
  request_type VARCHAR(16)     NOT NULL,                      -- topup(申请充值)/ refund(退款申请)
  amount       BIGINT          NOT NULL,                      -- 申请额度(quota)
  note         VARCHAR(512)    NULL,
  applicant    VARCHAR(128)    NOT NULL,                      -- 申请人(组织管理员)
  status       VARCHAR(16)     NOT NULL DEFAULT 'pending',    -- pending/processed/rejected
  processed_by VARCHAR(128)    NULL,                          -- 处理人(运营方)
  processed_at DATETIME(3)     NULL,
  created_at   DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at   DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  KEY idx_req_org_status (org_id, status, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
