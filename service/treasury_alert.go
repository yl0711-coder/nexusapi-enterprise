// 架构B 阶段1 BE③(29-PRD §4.9 金库低预警,只读):
// 探针读各组织金库 user.quota(实时 DB 真值),低于平台阈值 treasury_low_watermark_raw →
// 站内通知组织管理员 + 运营方(复用 notification)+ 审计。只读只报,绝不写 quota、绝不停服(fail-open)。
//
// 去抖:按"跨越"告警(高于阈值→跌破那一拍告警一次;回升后再跌破才再告)。状态在进程内存,
// 重启后首轮"已低于"会再告一次(可接受:重启后重新确认低水位比漏报安全)。
// 接线(阶段2 已接):ReconcileWorker tick 周期调用(与 VerifyQuotaPerUnit 同批,33 §11 带入项);多节点由 leader gate 单跑。
package service

import (
	"context"
	"fmt"

	"github.com/nexusapi-platform/enterprise/repo"
)

const treasuryLowWatermarkKey = "treasury_low_watermark_raw"

// CheckTreasuryLowWatermarks 金库低预警探针单轮(leader-only;阈值<=0=未启用,静默跳过)。
// 单组织读失败只记日志继续(fail-open,不因一家上游抖动漏掉全平台巡检)。
func (s *Service) CheckTreasuryLowWatermarks(ctx context.Context) error {
	if ok, _, err := s.leadership.CanRunTick(ctx); err != nil || !ok {
		return err
	}
	watermark, err := s.store.GetSettingInt64(ctx, treasuryLowWatermarkKey, 0)
	if err != nil {
		return err
	}
	if watermark <= 0 {
		return nil
	}
	orgs, err := s.store.ListOrgTreasuries(ctx)
	if err != nil {
		return err
	}
	for _, o := range orgs {
		quota, gerr := s.upstream.GetUserQuota(ctx, int(o.NewapiUserID))
		if gerr != nil {
			s.log.Warn("金库低预警:读金库 quota 失败(fail-open,跳过本组织本轮)", "org_id", o.OrgID, "err", gerr)
			continue
		}
		alert, nowBelow := treasuryAlertDecision(s.treasuryBelowState(o.OrgID), quota, watermark)
		s.setTreasuryBelowState(o.OrgID, nowBelow)
		if !alert {
			continue
		}
		s.alertTreasuryLow(ctx, o, quota, watermark)
	}
	return nil
}

// treasuryAlertDecision 跨越告警纯决策(单测):仅在"上一拍不低于、本拍低于"时告警。
func treasuryAlertDecision(prevBelow bool, quotaRaw, watermarkRaw int64) (alert, nowBelow bool) {
	nowBelow = quotaRaw < watermarkRaw
	return nowBelow && !prevBelow, nowBelow
}

func (s *Service) treasuryBelowState(orgID int64) bool {
	s.treasuryAlertMu.Lock()
	defer s.treasuryAlertMu.Unlock()
	return s.treasuryBelow[orgID]
}

func (s *Service) setTreasuryBelowState(orgID int64, below bool) {
	s.treasuryAlertMu.Lock()
	defer s.treasuryAlertMu.Unlock()
	if s.treasuryBelow == nil {
		s.treasuryBelow = map[int64]bool{}
	}
	s.treasuryBelow[orgID] = below
}

// alertTreasuryLow 落告警:日志 + 审计 + 站内通知(本组织全部管理员 + 全体运营方)。
// 文案只报事实与动作(请充值/联系运营方代充),不带倍率/内部数值口径换算。
func (s *Service) alertTreasuryLow(ctx context.Context, o repo.TreasuryRef, quotaRaw, watermarkRaw int64) {
	s.log.Error("金库低预警:组织金库余额低于平台阈值(请安排代充,增量代充纪律见 33 §8 运维注记)",
		"org_id", o.OrgID, "org_name", o.Name, "treasury_raw", quotaRaw, "watermark_raw", watermarkRaw)
	oid := o.OrgID
	s.auditSystem(ctx, o.OrgID, "treasury_low_watermark", "organization", &oid, map[string]any{
		"treasury_raw": quotaRaw, "watermark_raw": watermarkRaw,
	}, "alert")
	title := "组织金库余额不足"
	body := fmt.Sprintf("组织「%s」金库余额已低于预警阈值,请尽快联系运营方充值,以免成员额度发放受阻。", o.Name)
	if admins, err := s.store.ListOrgAdminIDs(ctx, o.OrgID); err == nil {
		for _, aid := range admins {
			s.notify(ctx, o.OrgID, aid, "treasury_low", title, body)
		}
	}
	if ops, err := s.store.ListOperatorMembers(ctx); err == nil {
		opBody := fmt.Sprintf("组织「%s」(id=%d)金库余额低于预警阈值,请按增量代充纪律处理。", o.Name, o.OrgID)
		for _, op := range ops {
			s.notify(ctx, op.OrgID, op.MemberID, "treasury_low", title, opBody)
		}
	}
}
