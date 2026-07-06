package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	LogType   *int
	MemberID  *int64
	RequestID string
	Limit     int
	Offset    int
}

// RunLogMirrorSlice 把 new-api 全类型日志按组织镜像到本地排障表。
// 它只写 org_newapi_log / org_newapi_log_cursor,不参与结算、不扣余额。
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
	subUntil, total, overflow, err := s.pickLogMirrorUpperBound(ctx, src.Username, since, until)
	if err != nil {
		return err
	}
	if overflow {
		s.log.Error("new-api 完整日志镜像窗口超出安全分页上限,游标不推进",
			"org_id", src.OrgID, "username", src.Username, "since", since, "until", until, "sub_until", subUntil,
			"total", total, "page_cap", logMirrorPageCap)
		oid := src.OrgID
		s.auditSystem(ctx, src.OrgID, "log_mirror_overflow", "log_mirror", &oid, map[string]any{
			"username": src.Username, "since": since, "until": until, "sub_until": subUntil,
			"total": total, "page_cap": logMirrorPageCap, "cursor_ts": cur.CursorTS, "cursor_log_id": cur.CursorLogID,
		}, "blocked")
		_ = s.store.TouchLogMirrorCursor(ctx, src.OrgID)
		return fmt.Errorf("%w: org=%d username=%s total=%d cap=%d", errLogMirrorOverflow, src.OrgID, src.Username, total, logMirrorPageCap)
	}
	if total == 0 {
		return s.store.AdvanceLogMirrorCursor(ctx, src.OrgID, until, cur.CursorLogID)
	}

	var rows []repo.OrgNewapiLog
	var maxID int64
	for page := 1; page <= logMirrorMaxPages; page++ {
		items, total, err := s.upstream.ReadAllLogsByUsername(ctx, src.Username, since, subUntil, page, logMirrorPageSize)
		if err != nil {
			return mapUpstream(err)
		}
		if total > logMirrorPageCap {
			return fmt.Errorf("log mirror page total changed beyond cap: org=%d username=%s total=%d cap=%d", src.OrgID, src.Username, total, logMirrorPageCap)
		}
		for _, it := range items {
			r, err := s.toMirroredLog(ctx, src, it)
			if err != nil {
				s.log.Warn("new-api 日志归因失败,按未归因落镜像", "org_id", src.OrgID, "log_id", it.ID, "err", err)
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
	if _, err := s.store.InsertOrgNewapiLogs(ctx, rows); err != nil {
		return err
	}
	return s.store.AdvanceLogMirrorCursor(ctx, src.OrgID, subUntil, maxID)
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

func (s *Service) toMirroredLog(ctx context.Context, src repo.LogMirrorSource, e newapi.AllLogEntry) (repo.OrgNewapiLog, error) {
	r := repo.OrgNewapiLog{
		OrgID: src.OrgID, NewapiUserID: src.NewapiUserID, NewapiTokenID: e.TokenID,
		TokenName: e.TokenName, LogType: e.Type, ModelName: e.ModelName,
		ChannelID: e.ChannelID, ChannelName: e.ChannelName, GroupName: e.Group,
		RequestID: e.RequestID, Quota: e.Quota, PromptTokens: e.PromptTokens,
		CompletionTokens: e.CompletionTokens, UseTime: e.UseTime, IsStream: e.IsStream,
		Content: e.Content, IP: e.IP, Other: string(e.Other), NewapiLogID: e.ID,
		LogTS: time.Unix(e.CreatedAt, 0).UTC(),
	}
	if e.TokenID <= 0 {
		return r, nil
	}
	m, keyID, found, err := s.store.GetMemberByNewapiTokenID(ctx, e.TokenID)
	if err != nil {
		return r, err
	}
	if found && m.OrgID == src.OrgID {
		r.MemberID = m.ID
		r.KeyID = keyID
	}
	return r, nil
}

func (s *Service) ListOrgNewapiLogs(ctx context.Context, c session.Claims, orgID int64, f OrgNewapiLogFilter) ([]repo.OrgNewapiLog, int, error) {
	if err := assertRole(c, session.RoleOperator); err != nil {
		return nil, 0, err
	}
	if _, err := s.store.GetOrganization(ctx, orgID); errors.Is(err, repo.ErrNotFound) {
		return nil, 0, apperr.NotFound("组织不存在")
	} else if err != nil {
		return nil, 0, apperr.Internal("").WithCause(err)
	}
	items, total, err := s.store.ListOrgNewapiLogs(ctx, repo.OrgNewapiLogFilter{
		OrgID: orgID, LogType: f.LogType, MemberID: f.MemberID, RequestID: f.RequestID,
		Limit: f.Limit, Offset: f.Offset,
	})
	if err != nil {
		return nil, 0, err
	}
	for i := range items {
		items[i].Content = truncateRunes(items[i].Content, 240)
		items[i].Other = truncateRunes(items[i].Other, 240)
		items[i].IP = maskLogIP(items[i].IP)
	}
	return items, total, nil
}

func (s *Service) ListMemberTokenMappings(ctx context.Context, c session.Claims, orgID int64) ([]repo.MemberTokenMapping, error) {
	if err := assertRole(c, session.RoleOperator); err != nil {
		return nil, err
	}
	if _, err := s.store.GetOrganization(ctx, orgID); errors.Is(err, repo.ErrNotFound) {
		return nil, apperr.NotFound("组织不存在")
	} else if err != nil {
		return nil, apperr.Internal("").WithCause(err)
	}
	return s.store.ListMemberTokenMappings(ctx, orgID)
}

func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	rs := []rune(s)
	if len(rs) <= max {
		return s
	}
	return string(rs[:max]) + "..."
}

func maskLogIP(ip string) string {
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return ""
	}
	if strings.Contains(ip, ",") {
		parts := strings.Split(ip, ",")
		for i := range parts {
			parts[i] = maskLogIP(parts[i])
		}
		return strings.Join(parts, ", ")
	}
	if strings.Count(ip, ".") == 3 {
		parts := strings.Split(ip, ".")
		parts[3] = "*"
		return strings.Join(parts, ".")
	}
	if strings.Contains(ip, ":") {
		parts := strings.Split(ip, ":")
		if len(parts) > 2 {
			return strings.Join(parts[:2], ":") + ":***"
		}
	}
	if len([]rune(ip)) > 6 {
		return truncateRunes(ip, 6) + "*"
	}
	return "*"
}
