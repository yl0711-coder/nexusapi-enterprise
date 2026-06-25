// Package service 是业务逻辑层(10 §4.3):RBAC 校验 + 事务编排 + 幂等,
// 是唯一可编排 repo / adapter 的层。RBAC、密钥加解密均在此层强制,不下放 handler。
package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/nexusapi-platform/enterprise/adapter/newapi"
	"github.com/nexusapi-platform/enterprise/pkg/crypto"
	"github.com/nexusapi-platform/enterprise/pkg/lock"
	"github.com/nexusapi-platform/enterprise/pkg/session"
	"github.com/nexusapi-platform/enterprise/repo"
)

// Adapter 是 service 依赖的 new-api 能力(newapi.NewapiAdapter 的子集,便于测试替身)。
type Adapter = newapi.NewapiAdapter

// Service 聚合各资源用例,持有依赖。
type Service struct {
	store    *repo.Store
	upstream Adapter
	keyring  *crypto.Keyring
	signer   *session.Signer
	log      *slog.Logger

	// quotaLocker 串行化同一组织的 override 下发(GZ-04 返工·方案①):消除 settlement converge 与
	// quota-worker 两 goroutine 在 gateByOrgStatus"读状态→决策→下发"上的 TOCTOU。进程内锁,多节点需换分布式锁。
	quotaLocker lock.KeyedLocker

	// observeMode = MVP 观测模式(改动⑤/⑥):落账不扣钱、不停服。
	observeMode bool

	// memberRole 是开通成员时给 new-api 用户的角色(普通用户)。
	memberRole string
}

// Deps 是构造 Service 的依赖集合。
type Deps struct {
	Store    *repo.Store
	Upstream Adapter
	Keyring  *crypto.Keyring
	Signer   *session.Signer
	Logger   *slog.Logger
	// ObserveMode = MVP 观测模式(改动⑤/⑥):结算照常落账供看板,但跳过扣 company_balance / 硬停 / 守恒断言;
	// 对账只跑 ReconcileBilling(logs↔ledger),跳过 ReconcileBalanceLedger / ReconcileDiscounts。本期不碰钱。
	ObserveMode bool
}

// New 构造 Service。
func New(d Deps) *Service {
	log := d.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		store:       d.Store,
		upstream:    d.Upstream,
		keyring:     d.Keyring,
		signer:      d.Signer,
		log:         log,
		quotaLocker: lock.NewInProcessLocker(),
		observeMode: d.ObserveMode,
		memberRole:  "", // new-api 普通用户角色,空 = 默认普通用户
	}
}

// now 便于将来注入测试时钟。
func (s *Service) now() time.Time { return time.Now().UTC() }

// Ping 探活底层库(就绪探针用,R2-S5:readyz 真探 DB)。
func (s *Service) Ping(ctx context.Context) error { return s.store.DB().PingContext(ctx) }

// withTimeout 给上游/库调用统一兜一个短超时上界(防 handler 无界等待)。
func withTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, d)
}
