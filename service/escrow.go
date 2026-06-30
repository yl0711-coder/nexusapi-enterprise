package service

import (
	"context"
	"database/sql"
	"errors"

	"github.com/nexusapi-platform/enterprise/adapter/newapi"
	"github.com/nexusapi-platform/enterprise/model"
	"github.com/nexusapi-platform/enterprise/pkg/apperr"
	"github.com/nexusapi-platform/enterprise/pkg/session"
	"github.com/nexusapi-platform/enterprise/repo"
)

// escrowWindowCap 是桶1(镜像进 org user.quota 的可花窗口)上限 ≈ $4000(= 4000 × QuotaPerUnit 500000)。
// < int32 上限 ~$4294(0020/14 §3.3),留头寸防溢出;续充合并后桶1 断言 ≤ 此值。
const escrowWindowCap int64 = 2_000_000_000

// escrowDefaultThreshold 续充触发阈值默认 ≈ $200(必须 > 单笔最大请求成本;运维按消费速率×续充延迟调,14 §3.7)。
const escrowDefaultThreshold int64 = 100_000_000

// DerivedBalance 模型2 读穿余额(14 §3.4):不持第二本权威余额,展示=桶1读穿(newapi user.quota 实际剩余)+ 托管桶之和。
type DerivedBalance struct {
	WindowQuota    int64 `json:"window_quota"`    // 桶1 实际剩余(读穿 new-api org user.quota)
	HoldingQuota   int64 `json:"holding_quota"`   // 平台库托管桶之和(未进窗口)
	AvailableQuota int64 `json:"available_quota"` // = 窗口 + 托管(组织当前可用总额)
}

// GetDerivedBalance 读穿组织余额(O/A)。窗口读 new-api(桶1 实际剩余,含已消费递减);托管读平台库。
// 绝不持第二本权威余额——余额永远派生自 new-api 桶1 + 平台库托管。组织未开通池子 → 全 0。
func (s *Service) GetDerivedBalance(ctx context.Context, c session.Claims, orgID int64) (*DerivedBalance, error) {
	if err := assertOrgScope(c, orgID); err != nil {
		return nil, err
	}
	if err := assertRole(c, session.RoleOperator, session.RoleOrgAdmin); err != nil {
		return nil, err
	}
	uid, _, ok, err := s.store.GetOrgNewapiCred(ctx, orgID)
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	var window int64
	if ok {
		w, gerr := s.upstream.GetUserQuota(ctx, int(uid))
		if gerr != nil {
			return nil, mapUpstream(gerr)
		}
		window = w
	}
	holding, err := s.store.SumHoldingEscrow(ctx, orgID)
	if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	return &DerivedBalance{WindowQuota: window, HoldingQuota: holding, AvailableQuota: window + holding}, nil
}

// allocateRecharge 模型2 入账分配(由 Recharge 在记账成功后调):把本次充值额分进托管多桶并把"桶1 可进部分"add 进
// org user.quota。桶1 填到窗口上限(按 newapi 当前窗口读穿计算可进空间),余下入新 holding 桶。
// 涉钱安全:**add 不 override**;合并后桶1 断言 ≤ escrowWindowCap(防 int32 溢出);**DB-first 再 add**——
// 失败方向恒为"欠拨"(窗口少于已记账)、绝不超拨,reconcile(已释放−日志≈窗口)可发现,运维重试/人工 add 兜底。
func (s *Service) allocateRecharge(ctx context.Context, orgID, amount int64) error {
	org, err := s.store.GetOrganization(ctx, orgID)
	if err != nil {
		return apperr.Internal("").WithCause(err)
	}
	cred, err := s.EnsureOrgProvisioned(ctx, orgID, org.Name)
	if err != nil {
		return err
	}
	window, gerr := s.upstream.GetUserQuota(ctx, cred.NewapiUserID)
	if gerr != nil {
		return mapUpstream(gerr)
	}
	fit := amount
	if space := escrowWindowCap - window; fit > space {
		fit = space
	}
	if fit < 0 {
		fit = 0
	}
	remainder := amount - fit

	active, agerr := s.store.GetActiveEscrowBucket(ctx, orgID)
	firstTime := errors.Is(agerr, repo.ErrNotFound)
	if !firstTime && agerr != nil {
		return apperr.Internal("").WithCause(agerr)
	}
	maxSeq, mserr := s.store.MaxEscrowSeq(ctx, orgID)
	if mserr != nil {
		return apperr.Internal("").WithCause(mserr)
	}
	holdingSeq := maxSeq + 1
	if firstTime {
		holdingSeq = 2 // 桶1=seq1,托管=seq2
	}
	if err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
		if firstTime {
			if _, e := s.store.CreateEscrowBucketTx(ctx, tx, &model.EscrowBucket{OrgID: orgID, Seq: 1, Amount: fit, Status: model.EscrowActive, Threshold: escrowDefaultThreshold}); e != nil {
				return e
			}
		} else if e := s.store.UpdateEscrowBucketTx(ctx, tx, active.ID, active.Amount+fit, model.EscrowActive); e != nil {
			return e
		}
		if remainder > 0 {
			if _, e := s.store.CreateEscrowBucketTx(ctx, tx, &model.EscrowBucket{OrgID: orgID, Seq: holdingSeq, Amount: remainder, Status: model.EscrowHolding, Threshold: escrowDefaultThreshold}); e != nil {
				return e
			}
		}
		return nil
	}); err != nil {
		return apperr.Internal("").WithCause(err)
	}
	// 提交后 add fit 进 newapi org user.quota(绝不 override)。
	if fit > 0 {
		if window+fit > escrowWindowCap {
			return apperr.Internal("桶1 入账后超窗口上限,拒绝(防 int32 溢出)")
		}
		if err := s.upstream.ManageUserQuota(ctx, cred.NewapiUserID, newapi.QuotaAdd, fit); err != nil {
			return mapUpstream(err)
		}
	}
	return nil
}

// RefillWindow 手工续充(运营方,v1):把一个托管桶并入桶1 可花窗口(window<threshold 时调;v1 无自动 worker,ADR §9)。
// 可并入额 = min(该桶额, 窗口剩余空间);全并→该桶 merged、部分→减额仍 holding。
// 涉钱安全:**add 不 override**;**DB-first 再 add**(失败恒"欠拨"不超拨,重试不会重复并同一桶=幂等安全)。
func (s *Service) RefillWindow(ctx context.Context, c session.Claims, orgID int64) (*DerivedBalance, error) {
	if err := assertOrgScope(c, orgID); err != nil {
		return nil, err
	}
	if err := assertRole(c, session.RoleOperator); err != nil {
		return nil, err // 动钱:仅运营方
	}
	cred, err := s.orgCred(ctx, orgID)
	if err != nil {
		return nil, err
	}
	window, gerr := s.upstream.GetUserQuota(ctx, cred.NewapiUserID)
	if gerr != nil {
		return nil, mapUpstream(gerr)
	}
	h, herr := s.store.NextHoldingBucket(ctx, orgID)
	if errors.Is(herr, repo.ErrNotFound) {
		return s.GetDerivedBalance(ctx, c, orgID) // 无托管可续充
	}
	if herr != nil {
		return nil, apperr.Internal("").WithCause(herr)
	}
	merge := h.Amount
	if space := escrowWindowCap - window; merge > space {
		merge = space
	}
	if merge <= 0 {
		return nil, apperr.New(apperr.CodeInvalidParam, 409, "窗口已满,暂无可并入空间")
	}
	active, aerr := s.store.GetActiveEscrowBucket(ctx, orgID)
	if aerr != nil {
		return nil, apperr.Internal("").WithCause(aerr)
	}
	if err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
		if e := s.store.UpdateEscrowBucketTx(ctx, tx, active.ID, active.Amount+merge, model.EscrowActive); e != nil {
			return e
		}
		if merge >= h.Amount {
			return s.store.UpdateEscrowBucketTx(ctx, tx, h.ID, 0, model.EscrowMerged)
		}
		return s.store.UpdateEscrowBucketTx(ctx, tx, h.ID, h.Amount-merge, model.EscrowHolding)
	}); err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	if err := s.upstream.ManageUserQuota(ctx, cred.NewapiUserID, newapi.QuotaAdd, merge); err != nil {
		return nil, mapUpstream(err)
	}
	s.audit(ctx, c, orgID, "escrow_refill", "balance", &orgID, map[string]any{"merged": merge, "window_before": window})
	return s.GetDerivedBalance(ctx, c, orgID)
}
