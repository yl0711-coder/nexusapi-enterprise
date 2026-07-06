package service

import (
	"context"
	"errors"
	"time"

	"github.com/nexusapi-platform/enterprise/adapter/newapi"
	"github.com/nexusapi-platform/enterprise/pkg/apperr"
	"github.com/nexusapi-platform/enterprise/pkg/session"
	"github.com/nexusapi-platform/enterprise/repo"
)

const (
	logMirrorLagSec         int64 = 5
	logMirrorOverlapSec     int64 = 600
	logMirrorMaxPages             = 10
	logMirrorPageSize             = 100
	logMirrorSourcesPerTick       = 20
)

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
	subUntil, total, err := s.pickLogMirrorUpperBound(ctx, src.Username, since, until)
	if err != nil {
		return err
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

func (s *Service) pickLogMirrorUpperBound(ctx context.Context, username string, since, until int64) (int64, int, error) {
	_, total, err := s.upstream.ReadAllLogsByUsername(ctx, username, since, until, 1, logMirrorPageSize)
	if err != nil {
		return 0, 0, mapUpstream(err)
	}
	if total <= logMirrorMaxPages*logMirrorPageSize {
		return until, total, nil
	}
	lo, hi := since, until
	best := since
	bestTotal := 0
	for lo <= hi {
		mid := lo + (hi-lo)/2
		if mid <= since {
			mid = since + 1
		}
		_, n, err := s.upstream.ReadAllLogsByUsername(ctx, username, since, mid, 1, logMirrorPageSize)
		if err != nil {
			return 0, 0, mapUpstream(err)
		}
		if n <= logMirrorMaxPages*logMirrorPageSize {
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
	return best, bestTotal, nil
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
	return s.store.ListOrgNewapiLogs(ctx, repo.OrgNewapiLogFilter{
		OrgID: orgID, LogType: f.LogType, MemberID: f.MemberID, RequestID: f.RequestID,
		Limit: f.Limit, Offset: f.Offset,
	})
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
