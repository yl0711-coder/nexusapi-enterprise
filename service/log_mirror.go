package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nexusapi-platform/enterprise/adapter/newapi"
	"github.com/nexusapi-platform/enterprise/pkg/apperr"
	"github.com/nexusapi-platform/enterprise/pkg/session"
	"github.com/nexusapi-platform/enterprise/repo"
)

const (
	logMirrorLagSec         int64 = 60
	logMirrorOverlapSec     int64 = 600
	logMirrorMaxPages             = 10
	logMirrorPageSize             = 100
	logMirrorSourcesPerTick       = 20
)

const logMirrorPageCap = logMirrorMaxPages * logMirrorPageSize

var errLogMirrorOverflow = errors.New("log mirror window overflow")

type OrgNewapiLogFilter struct {
	OrgID          int64
	LogType        *int
	MemberID       *int64
	StartTimestamp int64
	EndTimestamp   int64
	TokenName      string
	ModelName      string
	ChannelID      *int
	GroupName      string
	RequestID      string
	Limit          int
	Offset         int
}

// RunLogMirrorSlice 把 new-api 全类型日志按组织镜像到本地排障表。
// 它只写 org_newapi_log / org_newapi_log_cursor,不参与结算、不碰钱。
// 架构B(31-ADR §8):组织日志 = 金库 user + **名下全部成员 user** 聚合;上游读放大在镜像 worker 侧摊销
// (每成员按 username 拉),查询端(三视角端点)只打本地镜像表,一条 SQL,绝不 N 次串行打上游。
func (s *Service) RunLogMirrorSlice(ctx context.Context) error {
	ok, _, err := s.leadership.CanRunTick(ctx)
	if err != nil || !ok {
		return err
	}
	sources, err := s.store.ListLogMirrorSources(ctx, logMirrorSourcesPerTick)
	if err != nil {
		return err
	}
	for _, src := range sources {
		if err := s.mirrorOneOrgLogs(ctx, src); err != nil {
			s.log.Warn("new-api 完整日志镜像失败(下轮重试)", "org_id", src.OrgID, "username", src.Username, "err", err)
			if !errors.Is(err, errLogMirrorOverflow) {
				s.auditLogMirrorFailure(ctx, src, err)
			}
		}
	}
	return nil
}

// logMirrorTarget 一个组织内的镜像拉取目标(金库 user 或成员 user)。
type logMirrorTarget struct {
	username     string
	newapiUserID int64
	memberID     int64 // 0=金库
}

// mirrorOneOrgLogs 架构B 多成员聚合镜像:同一组织游标下,金库 + 全部成员 username 逐个拉取同一窗口。
// 共同安全上界 = min(各 username 的可整读上界);任一目标溢出 → 整组织游标不推进(与旧单 user 语义一致)。
// 任一目标读失败 → 本轮放弃不推游标(INSERT IGNORE + newapi_log_id 唯一键使重放幂等)。
func (s *Service) mirrorOneOrgLogs(ctx context.Context, src repo.LogMirrorSource) error {
	cur, err := s.store.GetOrCreateLogMirrorCursor(ctx, src.OrgID)
	if err != nil {
		return err
	}
	until := s.now().Unix() - logMirrorLagSec
	if until <= 0 {
		return nil
	}
	since := cur.CursorTS - logMirrorOverlapSec
	if since < 0 {
		since = 0
	}
	if since > until {
		return nil
	}

	// 拉取目标:金库 user(充值/管理日志)+ 全部成员 user(消费日志;跳过 quarantined/软删)。
	var targets []logMirrorTarget
	if src.Username != "" {
		targets = append(targets, logMirrorTarget{username: src.Username, newapiUserID: src.NewapiUserID})
	}
	memberSrcs, err := s.store.ListMemberLogSources(ctx, src.OrgID)
	if err != nil {
		return err
	}
	for _, m := range memberSrcs {
		targets = append(targets, logMirrorTarget{username: m.Username, newapiUserID: m.NewapiUserID, memberID: m.MemberID})
	}
	if len(targets) == 0 {
		return nil
	}

	// 共同安全上界:每个目标各自二分出可整读上界,取最小(保证所有目标都能在页上限内读尽该窗口)。
	commonUntil := until
	totalAll := 0
	for _, tg := range targets {
		subUntil, total, overflow, perr := s.pickLogMirrorUpperBound(ctx, tg.username, since, until)
		if perr != nil {
			return perr
		}
		if overflow {
			s.log.Error("new-api 完整日志镜像窗口超出安全分页上限,游标不推进",
				"org_id", src.OrgID, "username", tg.username, "member_id", tg.memberID,
				"since", since, "until", until, "sub_until", subUntil, "total", total, "page_cap", logMirrorPageCap)
			oid := src.OrgID
			s.auditSystem(ctx, src.OrgID, "log_mirror_overflow", "log_mirror", &oid, map[string]any{
				"username": tg.username, "member_id": tg.memberID, "since": since, "until": until,
				"sub_until": subUntil, "total": total, "page_cap": logMirrorPageCap,
				"cursor_ts": cur.CursorTS, "cursor_log_id": cur.CursorLogID,
			}, "blocked")
			_ = s.store.TouchLogMirrorCursor(ctx, src.OrgID)
			return fmt.Errorf("%w: org=%d username=%s total=%d cap=%d", errLogMirrorOverflow, src.OrgID, tg.username, total, logMirrorPageCap)
		}
		if subUntil < commonUntil {
			commonUntil = subUntil
		}
		totalAll += total
	}
	if totalAll == 0 {
		return s.store.AdvanceLogMirrorCursor(ctx, src.OrgID, until, cur.CursorLogID)
	}

	caches := newAttrCaches()
	var rows []repo.OrgNewapiLog
	maxID := cur.CursorLogID
	for _, tg := range targets {
		for page := 1; page <= logMirrorMaxPages; page++ {
			items, total, rerr := s.upstream.ReadAllLogsByUsername(ctx, tg.username, since, commonUntil, page, logMirrorPageSize)
			if rerr != nil {
				return mapUpstream(rerr)
			}
			if total > logMirrorPageCap {
				return fmt.Errorf("log mirror page total changed beyond cap: org=%d username=%s total=%d cap=%d", src.OrgID, tg.username, total, logMirrorPageCap)
			}
			for _, it := range items {
				r, terr := s.toMirroredLog(ctx, caches, src.OrgID, tg, it)
				if terr != nil {
					s.log.Warn("new-api 日志归因失败,按未归因落镜像", "org_id", src.OrgID, "log_id", it.ID, "err", terr)
				}
				rows = append(rows, r)
				if it.ID > maxID {
					maxID = it.ID
				}
			}
			if page*logMirrorPageSize >= total || len(items) == 0 {
				break
			}
		}
	}
	if _, err := s.store.InsertOrgNewapiLogs(ctx, rows); err != nil {
		return err
	}
	return s.store.AdvanceLogMirrorCursor(ctx, src.OrgID, commonUntil, maxID)
}

func (s *Service) pickLogMirrorUpperBound(ctx context.Context, username string, since, until int64) (int64, int, bool, error) {
	_, total, err := s.upstream.ReadAllLogsByUsername(ctx, username, since, until, 1, logMirrorPageSize)
	if err != nil {
		return 0, 0, false, mapUpstream(err)
	}
	if total <= logMirrorPageCap {
		return until, total, false, nil
	}
	if since >= until {
		return since, total, true, nil
	}
	lo, hi := since, until
	best := since
	bestTotal := -1
	for lo <= hi {
		mid := lo + (hi-lo)/2
		if mid <= since {
			mid = since + 1
		}
		_, n, err := s.upstream.ReadAllLogsByUsername(ctx, username, since, mid, 1, logMirrorPageSize)
		if err != nil {
			return 0, 0, false, mapUpstream(err)
		}
		if n <= logMirrorPageCap {
			best, bestTotal = mid, n
			lo = mid + 1
		} else {
			hi = mid - 1
		}
		if mid >= until {
			break
		}
	}
	if best <= since {
		best = since
	}
	if bestTotal < 0 {
		return since, total, true, nil
	}
	return best, bestTotal, false, nil
}

func (s *Service) auditLogMirrorFailure(ctx context.Context, src repo.LogMirrorSource, err error) {
	oid := src.OrgID
	result := "failed"
	detail := map[string]any{"username": src.Username, "error": err.Error()}
	if errors.Is(err, context.Canceled) {
		result = "canceled"
	} else if errors.Is(err, context.DeadlineExceeded) {
		result = "timeout"
	}
	var ae *apperr.Error
	if errors.As(err, &ae) {
		detail["code"] = ae.Code
		if ae.Code == newapi.CodeUpstreamAuth {
			result = "auth_failed"
		}
	}
	s.auditSystem(ctx, src.OrgID, "log_mirror_failed", "log_mirror", &oid, detail, result)
	_ = s.store.TouchLogMirrorCursor(ctx, src.OrgID)
}

// toMirroredLog 架构B归因:镜像按目标 username 拉取,成员归属天然=目标成员(user_id 主键);
// 再做 token 一致性断言(token→member 与 member.newapi_user_id→org 自洽),不一致落未归因(member_id=0)+告警。
func (s *Service) toMirroredLog(ctx context.Context, caches *attrCaches, orgID int64, tg logMirrorTarget, e newapi.AllLogEntry) (repo.OrgNewapiLog, error) {
	r := repo.OrgNewapiLog{
		OrgID: orgID, MemberID: tg.memberID, NewapiUserID: int64(e.UserID), NewapiTokenID: e.TokenID,
		TokenName: e.TokenName, LogType: e.Type, ModelName: e.ModelName,
		ChannelID: e.ChannelID, ChannelName: e.ChannelName, GroupName: e.Group,
		RequestID: e.RequestID, Quota: e.Quota, PromptTokens: e.PromptTokens,
		CompletionTokens: e.CompletionTokens, UseTime: e.UseTime, IsStream: e.IsStream,
		Content: e.Content, IP: e.IP, Other: string(e.Other), NewapiLogID: e.ID,
		LogTS: time.Unix(e.CreatedAt, 0).UTC(),
	}
	// 断言:按 username 拉取时 user_id 应恒等于目标 user;不等=上游异常/串台 → 未归因+告警。
	if e.UserID != 0 && int64(e.UserID) != tg.newapiUserID {
		r.MemberID = 0
		s.alertAttributionMismatch(ctx, caches, orgID, int64(e.UserID), e.TokenID, e.ID, "log_mirror")
		return r, nil
	}
	if e.TokenID <= 0 {
		return r, nil
	}
	tok, err := s.lookupTokenMember(ctx, caches, e.TokenID)
	if err != nil {
		return r, err // 归因查询失败:按 user 侧归属落镜像(key 未归因),调用方 warn
	}
	if !tok.found {
		return r, nil // 未登记令牌:保留 user 侧归属,key_id=0
	}
	switch {
	case tg.memberID != 0 && tok.member != nil && tok.member.ID == tg.memberID && tok.member.OrgID == orgID:
		r.KeyID = tok.keyID // 一致:成员 user 下自己的令牌
	case tg.memberID == 0 && tok.member != nil && tok.member.OrgID == orgID:
		r.MemberID = tok.member.ID // A 版遗留:成员令牌挂金库 user 下,org 断言已过,按 token 归因
		r.KeyID = tok.keyID
	default:
		r.MemberID = 0 // token→member 与 user→member/org 不一致:未归因桶 + 告警(防跨组织串台)
		r.KeyID = 0
		s.alertAttributionMismatch(ctx, caches, orgID, int64(e.UserID), e.TokenID, e.ID, "log_mirror")
	}
	return r, nil
}

// logViewForRole 三视角(31-ADR §8):超管=照抄 new-api 管理员(全量);其余角色=受限视角。
// 脱敏本体已下沉 repo 层(repo.redactOrgNewapiLogs,零值即最严 fail-closed),这里只做角色→视角映射。
func logViewForRole(c session.Claims) repo.LogView {
	if c.Role == session.RoleOperator {
		return repo.LogViewFull
	}
	return repo.LogViewRestricted
}

// ListOrgNewapiLogs 组织/成员日志(26 号文框架沿用,组长裁定:member 复用本端点,按 role 限权到本人):
//   - operator 进组织:全量(照抄 new-api 管理员);
//   - org_admin:聚合本组织全部成员(受限视角);
//   - member:服务端强制 member_id=本人(**含按令牌名筛选也只在本人日志内**,归属天然成立);
//     [v3 反转] token_name/group_name 对成员保留(成员=多令牌)。
// 查询只打本地镜像表(一条 SQL),跨 N 成员的上游读放大已在镜像 worker 侧摊销,绝不 N 次串行打上游。
func (s *Service) ListOrgNewapiLogs(ctx context.Context, c session.Claims, orgID int64, f OrgNewapiLogFilter) ([]repo.OrgNewapiLog, int, error) {
	if err := assertRole(c, session.RoleOperator, session.RoleOrgAdmin, session.RoleTeamLeader, session.RoleMember); err != nil {
		return nil, 0, err
	}
	if err := assertOrgScope(c, orgID); err != nil {
		return nil, 0, err
	}
	if c.Role == session.RoleMember || c.Role == session.RoleTeamLeader {
		f.MemberID = &c.MemberID // 成员(含团队负责人自视角)只看自己:服务端强制,忽略入参(隔离铁律)
	}
	if _, err := s.store.GetOrganization(ctx, orgID); errors.Is(err, repo.ErrNotFound) {
		return nil, 0, apperr.NotFound("组织不存在")
	} else if err != nil {
		return nil, 0, apperr.Internal("").WithCause(err)
	}
	items, total, err := s.store.ListOrgNewapiLogs(ctx, repo.OrgNewapiLogFilter{
		OrgID: orgID, LogType: f.LogType, MemberID: f.MemberID,
		StartTimestamp: f.StartTimestamp, EndTimestamp: f.EndTimestamp,
		TokenName: f.TokenName, ModelName: f.ModelName, ChannelID: f.ChannelID,
		GroupName: f.GroupName, RequestID: f.RequestID,
		Limit: f.Limit, Offset: f.Offset,
		View: logViewForRole(c), // 脱敏在 repo 层按视角出列(31-ADR §8,真隔离不靠前端藏)
	})
	if err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// ListAllNewapiLogs 运营方(超管)全局日志=照抄 new-api 管理员,什么都能看:
// other 全量、content 不截断、IP 不打码(LogViewFull 显式声明;repo 零值默认最严)。
func (s *Service) ListAllNewapiLogs(ctx context.Context, c session.Claims, f OrgNewapiLogFilter) ([]repo.OrgNewapiLog, int, error) {
	if err := assertRole(c, session.RoleOperator); err != nil {
		return nil, 0, err
	}
	items, total, err := s.store.ListAllNewapiLogs(ctx, repo.OrgNewapiLogFilter{
		OrgID: f.OrgID, LogType: f.LogType, MemberID: f.MemberID,
		StartTimestamp: f.StartTimestamp, EndTimestamp: f.EndTimestamp,
		TokenName: f.TokenName, ModelName: f.ModelName, ChannelID: f.ChannelID,
		GroupName: f.GroupName, RequestID: f.RequestID,
		Limit: f.Limit, Offset: f.Offset,
		View: repo.LogViewFull,
	})
	if err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

func (s *Service) ListMemberTokenMappings(ctx context.Context, c session.Claims, orgID int64, limit, offset int) ([]repo.MemberTokenMapping, int, error) {
	if err := assertRole(c, session.RoleOperator); err != nil {
		return nil, 0, err
	}
	if _, err := s.store.GetOrganization(ctx, orgID); errors.Is(err, repo.ErrNotFound) {
		return nil, 0, apperr.NotFound("组织不存在")
	} else if err != nil {
		return nil, 0, apperr.Internal("").WithCause(err)
	}
	return s.store.ListMemberTokenMappings(ctx, orgID, limit, offset)
}

// 日志脱敏(sanitizeLogsByRole/sanitizeOther/maskLogIP/truncateRunes)已**下沉 repo 层**
// (repo/log_redact.go,31-ADR §8 护栏:按角色出列,不靠各读端点各自记得脱敏;
// repo 零值视角=最严 fail-closed)。service 层只做角色→视角映射(logViewForRole)。
