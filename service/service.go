// Package service 是业务逻辑层(10 §4.3):RBAC 校验 + 事务编排 + 幂等,
// 是唯一可编排 repo / adapter 的层。RBAC、密钥加解密均在此层强制,不下放 handler。
package service

import (
	"context"
	"log/slog"
	"sync"
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

	// fundingEnabled v1 escrow 休眠总闸(20-§9,NEXUS_PLATFORM_FUNDING_ENABLED,默认 false):
	// 平台经手钱(入账/退款/续充/充值申请/escrow 对账/计费对账)全部禁用——v1 钱在 new-api,平台只看不碰。
	// v2 开 flag 即恢复,代码/表保留不删(§17 接缝)。
	fundingEnabled bool

	// memberRole 是开通成员时给 new-api 用户的角色(普通用户)。
	memberRole string

	// settlementMu 串行化所有 forward 结算(RunSettlement:settlement-worker + escrow-drain 两入口)——
	// forward 与 rescan-补漏在 600s 重叠带只靠 usage_detail 幂等去重(ledger 无按行去重),
	// 并发即 TOCTOU 双算(24-§3.3 命根子)。v1 单节点进程内锁即足;
	// v2 多节点由选主保证单节点跑 settlement worker(见 project_enterprise_platform_multinode_leader)。
	// (历史回填 RunBackfillSlice 曾共此锁,已随门B 整体退役清除,v2 收编按 doc24 重建。)
	settlementMu sync.Mutex

	// usageDetailRetentionDays 逐条明细保留天数(24-§6):0=永久保留(不清理,回填全历史随时可查的前提);
	// 设正整数 N 才清 N 天前。默认 0——量涨到千万行级再配天数启用(旋钮,不返工)。
	usageDetailRetentionDays int

	// leadership B5:leader-only 写工作(结算/回填/托管对账)的准入决策支点;v1=envLeadership,v2 换 leaseLeadership。
	leadership Leadership

	// ledger 钱核心进程内运行态(架构B 阶段1,BE②:订阅补满桶去重/恒等式扫描限频/负漂移双轮确认)。
	// 零值可用(懒初始化);单 leader 进程语义,多节点前随选主重造(见 ledger.go)。
	ledger ledgerRuntime
	// 金库低预警跨越去抖状态(BE③ treasury_alert.go;进程内,零值可用,多节点由 leader gate 单跑)。
	treasuryAlertMu sync.Mutex
	treasuryBelow   map[int64]bool
	// offboardQuiesceWait 架构B 离职「确认静默」的单次等待间隔(31-ADR §4.5:disable→读实时余额→退额;
	// 有界重读余额直至稳定)。默认 2s;测试注小值提速。
	offboardQuiesceWait time.Duration
}

// Deps 是构造 Service 的依赖集合。
type Deps struct {
	Store    *repo.Store
	Upstream Adapter
	Keyring  *crypto.Keyring
	Signer   *session.Signer
	Logger   *slog.Logger
	// FundingEnabled 平台经手钱总闸(v1 恒 false=escrow 休眠;v2 开)。
	FundingEnabled bool
	// UsageDetailRetentionDays 逐条明细保留天数(24-§6):0=永久保留(默认,不清理);正整数 N=清 N 天前。
	UsageDetailRetentionDays int
	// Leadership B5:leader-only 写工作准入(nil → 默认 envLeadership(true),即单节点/测试恒 leader)。
	Leadership Leadership
	// OffboardQuiesceWait 架构B 离职静默确认的重读间隔(<=0 默认 2s;测试可注小值)。
	OffboardQuiesceWait time.Duration
}

// New 构造 Service。
func New(d Deps) *Service {
	log := d.Logger
	if log == nil {
		log = slog.Default()
	}
	leadership := d.Leadership
	if leadership == nil {
		leadership = NewEnvLeadership(true) // 默认恒 leader(单节点/测试);多节点由 main.go 注入
	}
	quiesce := d.OffboardQuiesceWait
	if quiesce <= 0 {
		quiesce = 2 * time.Second
	}
	return &Service{
		store:                    d.Store,
		upstream:                 d.Upstream,
		keyring:                  d.Keyring,
		signer:                   d.Signer,
		log:                      log,
		quotaLocker:              lock.NewInProcessLocker(),
		fundingEnabled:           d.FundingEnabled,
		memberRole:               "", // new-api 普通用户角色,空 = 默认普通用户
		usageDetailRetentionDays: d.UsageDetailRetentionDays, // 默认 0 = 永久保留(不清理)
		leadership:               leadership,
		offboardQuiesceWait:      quiesce,
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
