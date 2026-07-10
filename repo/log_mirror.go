package repo

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// LogMirrorSource 是完整日志镜像的最小上游读取锚点。
// username 来自 organization.newapi_username 或门B关联时留下的 backfill username。
type LogMirrorSource struct {
	OrgID        int64
	NewapiUserID int64
	Username     string
}

type LogMirrorCursor struct {
	OrgID       int64
	CursorTS    int64
	CursorLogID int64
}

type OrgNewapiLog struct {
	ID               int64     `json:"id"`
	OrgID            int64     `json:"org_id"`
	OrgName          string    `json:"org_name,omitempty"`
	MemberID         int64     `json:"member_id"`
	MemberName       string    `json:"member_name,omitempty"`
	MemberEmail      string    `json:"member_email,omitempty"`
	KeyID            int64     `json:"key_id"`
	NewapiUserID     int64     `json:"newapi_user_id"`
	NewapiTokenID    int64     `json:"newapi_token_id"`
	TokenName        string    `json:"token_name"`
	LogType          int       `json:"log_type"`
	ModelName        string    `json:"model_name"`
	ChannelID        int       `json:"channel_id"`
	ChannelName      string    `json:"channel_name"`
	GroupName        string    `json:"group_name"`
	RequestID        string    `json:"request_id"`
	Quota            int64     `json:"quota"`
	PromptTokens     int64     `json:"prompt_tokens"`
	CompletionTokens int64     `json:"completion_tokens"`
	UseTime          int       `json:"use_time"`
	IsStream         bool      `json:"is_stream"`
	Content          string    `json:"content"`
	IP               string    `json:"ip"`
	Other            string    `json:"other"`
	NewapiLogID      int64     `json:"newapi_log_id"`
	LogTS            time.Time `json:"log_ts"`
	CreatedAt        time.Time `json:"created_at"`
}

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
	// View 读取视角(脱敏下沉 repo 层,31-ADR §8):零值=LogViewRestricted 最严(fail-closed),
	// 只有 service 层判定为超管时才显式传 LogViewFull。
	View LogView
}

func (s *Store) ListAllNewapiLogs(ctx context.Context, f OrgNewapiLogFilter) ([]OrgNewapiLog, int, error) {
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	where, args := []string{"o.deleted_at IS NULL"}, []any{}
	if f.OrgID > 0 {
		where = append(where, "l.org_id = ?")
		args = append(args, f.OrgID)
	}
	if f.LogType != nil {
		where = append(where, "l.log_type = ?")
		args = append(args, *f.LogType)
	}
	if f.MemberID != nil {
		where = append(where, "l.member_id = ?")
		args = append(args, *f.MemberID)
	}
	if f.StartTimestamp > 0 {
		where = append(where, "l.log_ts >= FROM_UNIXTIME(?)")
		args = append(args, f.StartTimestamp)
	}
	if f.EndTimestamp > 0 {
		where = append(where, "l.log_ts <= FROM_UNIXTIME(?)")
		args = append(args, f.EndTimestamp)
	}
	if f.TokenName != "" {
		where = append(where, "l.token_name LIKE ?")
		args = append(args, "%"+f.TokenName+"%")
	}
	if f.ModelName != "" {
		where = append(where, "l.model_name LIKE ?")
		args = append(args, "%"+f.ModelName+"%")
	}
	if f.ChannelID != nil {
		where = append(where, "l.channel_id = ?")
		args = append(args, *f.ChannelID)
	}
	if f.GroupName != "" {
		where = append(where, "l.group_name LIKE ?")
		args = append(args, "%"+f.GroupName+"%")
	}
	if f.RequestID != "" {
		where = append(where, "l.request_id = ?")
		args = append(args, f.RequestID)
	}
	cond := strings.Join(where, " AND ")
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM org_newapi_log l JOIN organization o ON o.id = l.org_id WHERE `+cond, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	qargs := append(append([]any{}, args...), f.Limit, f.Offset)
	rows, err := s.db.QueryContext(ctx,
		`SELECT l.id, l.org_id, COALESCE(o.name,''), l.member_id, COALESCE(m.display_name,''), COALESCE(m.login_email,''), l.key_id, l.newapi_user_id, l.newapi_token_id,
		        COALESCE(l.token_name,''), l.log_type, COALESCE(l.model_name,''), l.channel_id,
		        COALESCE(l.channel_name,''), COALESCE(l.group_name,''), COALESCE(l.request_id,''),
		        l.quota, l.prompt_tokens, l.completion_tokens, l.use_time, l.is_stream, COALESCE(l.content,''),
		        COALESCE(l.ip,''), COALESCE(l.other,''), l.newapi_log_id, l.log_ts, l.created_at
		   FROM org_newapi_log l
		   JOIN organization o ON o.id = l.org_id
		   LEFT JOIN member m ON m.id = l.member_id AND m.org_id = l.org_id
		  WHERE `+cond+`
		  ORDER BY l.log_ts DESC, l.newapi_log_id DESC
		  LIMIT ? OFFSET ?`, qargs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := make([]OrgNewapiLog, 0)
	for rows.Next() {
		var r OrgNewapiLog
		if err := rows.Scan(&r.ID, &r.OrgID, &r.OrgName, &r.MemberID, &r.MemberName, &r.MemberEmail, &r.KeyID, &r.NewapiUserID, &r.NewapiTokenID,
			&r.TokenName, &r.LogType, &r.ModelName, &r.ChannelID, &r.ChannelName, &r.GroupName, &r.RequestID,
			&r.Quota, &r.PromptTokens, &r.CompletionTokens, &r.UseTime, &r.IsStream, &r.Content, &r.IP, &r.Other,
			&r.NewapiLogID, &r.LogTS, &r.CreatedAt); err != nil {
			return nil, 0, err
		}
		out = append(out, r)
	}
	redactOrgNewapiLogs(out, f.View) // 脱敏下沉 repo 层(31-ADR §8):按视角出列,不靠读端点各自记得
	return out, total, rows.Err()
}

type MemberTokenMapping struct {
	KeyTokenID    int64     `json:"key_token_id"`
	KeyID         int64     `json:"key_id"`
	OrgID         int64     `json:"org_id"`
	MemberID      int64     `json:"member_id"`
	DisplayName   string    `json:"display_name"`
	LoginEmail    string    `json:"login_email"`
	MemberStatus  string    `json:"member_status"`
	MemberDeleted bool      `json:"member_deleted"`
	NewapiTokenID int64     `json:"newapi_token_id"`
	TokenName     string    `json:"token_name"`
	IsCurrent     bool      `json:"is_current"`
	KeyMasked     string    `json:"key_masked"`
	Rotation      int       `json:"rotation"`
	TokenStatus   string    `json:"token_status"`
	NewapiGroup   string    `json:"newapi_group"`
	TeamID        int64     `json:"team_id"`
	CreatedAt     time.Time `json:"created_at"`
}

func (s *Store) ListLogMirrorSources(ctx context.Context, limit int) ([]LogMirrorSource, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT o.id, o.newapi_user_id, COALESCE(o.newapi_username, b.newapi_username, '') AS username
		   FROM organization o
		   LEFT JOIN org_newapi_log_cursor c ON c.org_id = o.id
		  WHERE o.deleted_at IS NULL AND o.newapi_user_id IS NOT NULL
		  ORDER BY COALESCE(c.last_run_at, '1970-01-01') ASC, o.id ASC
		  LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]LogMirrorSource, 0)
	for rows.Next() {
		var src LogMirrorSource
		if err := rows.Scan(&src.OrgID, &src.NewapiUserID, &src.Username); err != nil {
			return nil, err
		}
		if strings.TrimSpace(src.Username) == "" {
			continue
		}
		out = append(out, src)
	}
	return out, rows.Err()
}

func (s *Store) GetOrCreateLogMirrorCursor(ctx context.Context, orgID int64) (*LogMirrorCursor, error) {
	var c LogMirrorCursor
	err := s.db.QueryRowContext(ctx,
		`SELECT org_id, cursor_ts, cursor_log_id FROM org_newapi_log_cursor WHERE org_id = ?`, orgID).
		Scan(&c.OrgID, &c.CursorTS, &c.CursorLogID)
	if err == nil {
		return &c, nil
	}
	if !errorsIsNoRows(err) {
		return nil, err
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO org_newapi_log_cursor (org_id, cursor_ts, cursor_log_id) VALUES (?, 0, 0)`, orgID); err != nil && !isDupKey(err) {
		return nil, err
	}
	return &LogMirrorCursor{OrgID: orgID}, nil
}

func (s *Store) AdvanceLogMirrorCursor(ctx context.Context, orgID, cursorTS, cursorLogID int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO org_newapi_log_cursor (org_id, cursor_ts, cursor_log_id, last_run_at)
		 VALUES (?, ?, ?, CURRENT_TIMESTAMP(3))
		 ON DUPLICATE KEY UPDATE
		   cursor_log_id = IF(VALUES(cursor_ts) > cursor_ts OR (VALUES(cursor_ts) = cursor_ts AND VALUES(cursor_log_id) > cursor_log_id), VALUES(cursor_log_id), cursor_log_id),
		   cursor_ts = IF(VALUES(cursor_ts) > cursor_ts OR (VALUES(cursor_ts) = cursor_ts AND VALUES(cursor_log_id) > cursor_log_id), VALUES(cursor_ts), cursor_ts),
		   last_run_at = CURRENT_TIMESTAMP(3)`,
		orgID, cursorTS, cursorLogID)
	return err
}

func (s *Store) TouchLogMirrorCursor(ctx context.Context, orgID int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO org_newapi_log_cursor (org_id, cursor_ts, cursor_log_id, last_run_at)
		 VALUES (?, 0, 0, CURRENT_TIMESTAMP(3))
		 ON DUPLICATE KEY UPDATE last_run_at = CURRENT_TIMESTAMP(3)`,
		orgID)
	return err
}

func (s *Store) InsertOrgNewapiLogs(ctx context.Context, rows []OrgNewapiLog) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	inserted := 0
	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx,
			`INSERT IGNORE INTO org_newapi_log
			  (org_id, member_id, key_id, newapi_user_id, newapi_token_id, token_name, log_type,
			   model_name, channel_id, channel_name, group_name, request_id, quota, prompt_tokens,
			   completion_tokens, use_time, is_stream, content, ip, other, newapi_log_id, log_ts)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, r := range rows {
			res, err := stmt.ExecContext(ctx,
				r.OrgID, r.MemberID, r.KeyID, r.NewapiUserID, r.NewapiTokenID, nullString(r.TokenName), r.LogType,
				nullString(r.ModelName), r.ChannelID, nullString(r.ChannelName), nullString(r.GroupName), nullString(r.RequestID),
				r.Quota, r.PromptTokens, r.CompletionTokens, r.UseTime, r.IsStream, nullString(r.Content), nullString(r.IP),
				nullString(r.Other), r.NewapiLogID, r.LogTS)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n > 0 {
				inserted++
			}
		}
		return nil
	})
	return inserted, err
}

func (s *Store) ListOrgNewapiLogs(ctx context.Context, f OrgNewapiLogFilter) ([]OrgNewapiLog, int, error) {
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	where, args := []string{"l.org_id = ?"}, []any{f.OrgID}
	if f.LogType != nil {
		where = append(where, "l.log_type = ?")
		args = append(args, *f.LogType)
	}
	if f.MemberID != nil {
		where = append(where, "l.member_id = ?")
		args = append(args, *f.MemberID)
	}
	if f.StartTimestamp > 0 {
		where = append(where, "l.log_ts >= FROM_UNIXTIME(?)")
		args = append(args, f.StartTimestamp)
	}
	if f.EndTimestamp > 0 {
		where = append(where, "l.log_ts <= FROM_UNIXTIME(?)")
		args = append(args, f.EndTimestamp)
	}
	if f.TokenName != "" {
		where = append(where, "l.token_name LIKE ?")
		args = append(args, "%"+f.TokenName+"%")
	}
	if f.ModelName != "" {
		where = append(where, "l.model_name LIKE ?")
		args = append(args, "%"+f.ModelName+"%")
	}
	if f.ChannelID != nil {
		where = append(where, "l.channel_id = ?")
		args = append(args, *f.ChannelID)
	}
	if f.GroupName != "" {
		where = append(where, "l.group_name LIKE ?")
		args = append(args, "%"+f.GroupName+"%")
	}
	if f.RequestID != "" {
		where = append(where, "l.request_id = ?")
		args = append(args, f.RequestID)
	}
	cond := strings.Join(where, " AND ")
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM org_newapi_log l WHERE `+cond, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	qargs := append(append([]any{}, args...), f.Limit, f.Offset)
	rows, err := s.db.QueryContext(ctx,
		`SELECT l.id, l.org_id, l.member_id, COALESCE(m.display_name,''), COALESCE(m.login_email,''), l.key_id, l.newapi_user_id, l.newapi_token_id,
		        COALESCE(l.token_name,''), l.log_type, COALESCE(l.model_name,''), l.channel_id,
		        COALESCE(l.channel_name,''), COALESCE(l.group_name,''), COALESCE(l.request_id,''),
		        l.quota, l.prompt_tokens, l.completion_tokens, l.use_time, l.is_stream, COALESCE(l.content,''),
		        COALESCE(l.ip,''), COALESCE(l.other,''), l.newapi_log_id, l.log_ts, l.created_at
		   FROM org_newapi_log l
		   LEFT JOIN member m ON m.id = l.member_id AND m.org_id = l.org_id
		  WHERE `+cond+`
		  ORDER BY l.log_ts DESC, l.newapi_log_id DESC
		  LIMIT ? OFFSET ?`, qargs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := make([]OrgNewapiLog, 0)
	for rows.Next() {
		var r OrgNewapiLog
		if err := rows.Scan(&r.ID, &r.OrgID, &r.MemberID, &r.MemberName, &r.MemberEmail, &r.KeyID, &r.NewapiUserID, &r.NewapiTokenID,
			&r.TokenName, &r.LogType, &r.ModelName, &r.ChannelID, &r.ChannelName, &r.GroupName, &r.RequestID,
			&r.Quota, &r.PromptTokens, &r.CompletionTokens, &r.UseTime, &r.IsStream, &r.Content, &r.IP, &r.Other,
			&r.NewapiLogID, &r.LogTS, &r.CreatedAt); err != nil {
			return nil, 0, err
		}
		out = append(out, r)
	}
	redactOrgNewapiLogs(out, f.View) // 脱敏下沉 repo 层(31-ADR §8)
	return out, total, rows.Err()
}

// MemberLogSource 架构B(BE③):组织日志镜像的成员级上游锚点(成员=各自 new-api user,按 username 拉)。
type MemberLogSource struct {
	MemberID     int64
	NewapiUserID int64
	Username     string
}

// ListMemberLogSources 列组织内可镜像日志的成员(有服务账号 + username;跳过 quarantined 孤儿与软删成员)。
// 组织日志聚合 = 金库 user + 这批成员 user(31-ADR §8:跨 N 成员按数百成员设计,镜像侧摊销上游读)。
func (s *Store) ListMemberLogSources(ctx context.Context, orgID int64) ([]MemberLogSource, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, newapi_user_id, COALESCE(newapi_username, '')
		   FROM member
		  WHERE org_id = ? AND deleted_at IS NULL AND newapi_user_id IS NOT NULL
		    AND bootstrap_state <> 'quarantined'
		  ORDER BY id ASC`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]MemberLogSource, 0)
	for rows.Next() {
		var m MemberLogSource
		if err := rows.Scan(&m.MemberID, &m.NewapiUserID, &m.Username); err != nil {
			return nil, err
		}
		if strings.TrimSpace(m.Username) == "" || m.NewapiUserID == 0 {
			continue
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ListMemberTokenMappings 员工↔token 映射(运营排障)。B4(28):分页,不再一次拉全量(历史令牌多的大客户首屏慢)。
func (s *Store) ListMemberTokenMappings(ctx context.Context, orgID int64, limit, offset int) ([]MemberTokenMapping, int, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var total int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM member_key_token WHERE org_id = ?`, orgID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT mkt.id, mkt.key_id, mkt.org_id, mkt.member_id,
		        COALESCE(m.display_name,''), COALESCE(m.login_email,''), COALESCE(m.status,''),
		        IF(m.deleted_at IS NULL, 0, 1),
		        mkt.newapi_token_id, COALESCE(mkt.token_name,''), IF(mkt.is_current = 1, 1, 0),
		        COALESCE(mkt.key_masked,''), mkt.rotation, COALESCE(mkt.status,''),
		        COALESCE(m.newapi_group,''), COALESCE(m.team_id,0), mkt.created_at
		   FROM member_key_token mkt
		   LEFT JOIN member m ON m.id = mkt.member_id AND m.org_id = mkt.org_id
		  WHERE mkt.org_id = ?
		  ORDER BY mkt.member_id ASC, mkt.key_id ASC, mkt.rotation DESC, mkt.id DESC
		  LIMIT ? OFFSET ?`, orgID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := make([]MemberTokenMapping, 0)
	for rows.Next() {
		var m MemberTokenMapping
		var memberDeleted, isCurrent int
		if err := rows.Scan(&m.KeyTokenID, &m.KeyID, &m.OrgID, &m.MemberID, &m.DisplayName, &m.LoginEmail,
			&m.MemberStatus, &memberDeleted, &m.NewapiTokenID, &m.TokenName, &isCurrent, &m.KeyMasked,
			&m.Rotation, &m.TokenStatus, &m.NewapiGroup, &m.TeamID, &m.CreatedAt); err != nil {
			return nil, 0, err
		}
		m.MemberDeleted = memberDeleted == 1
		m.IsCurrent = isCurrent == 1
		out = append(out, m)
	}
	return out, total, rows.Err()
}

func nullString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func errorsIsNoRows(err error) bool {
	return err == sql.ErrNoRows
}
