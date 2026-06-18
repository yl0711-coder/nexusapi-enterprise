package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/nexusapi-platform/enterprise/model"
)

// CreateApproval 建申请,返回 id。
func (s *Store) CreateApproval(ctx context.Context, a *model.Approval) (int64, error) {
	payload, _ := json.Marshal(a.Payload)
	if a.State == "" {
		a.State = model.ApprovalPending
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO approval (org_id, applicant_id, team_id, request_type, payload, state, is_level2)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		a.OrgID, a.ApplicantID, a.TeamID, a.RequestType, string(payload), a.State, a.IsLevel2)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// GetApproval 取申请(强制 org 谓词)。
func (s *Store) GetApproval(ctx context.Context, orgID, id int64) (*model.Approval, error) {
	row := s.db.QueryRowContext(ctx, approvalSelect+` WHERE id = ? AND org_id = ?`, id, orgID)
	return scanApproval(row)
}

// ListApprovals 列申请(可按 state / team 过滤,分页)。
func (s *Store) ListApprovals(ctx context.Context, orgID int64, state string, teamID *int64, applicantID *int64, limit, offset int) ([]*model.Approval, int, error) {
	where := []string{"org_id = ?"}
	args := []any{orgID}
	if state != "" {
		where = append(where, "state = ?")
		args = append(args, state)
	}
	if teamID != nil {
		where = append(where, "team_id = ?")
		args = append(args, *teamID)
	}
	if applicantID != nil {
		where = append(where, "applicant_id = ?")
		args = append(args, *applicantID)
	}
	cond := strings.Join(where, " AND ")
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM approval WHERE `+cond, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	pageArgs := append(append([]any{}, args...), limit, offset)
	// T9:列表 join 成员名(COALESCE(display_name, login_email)),申请人显示姓名而非 #id。
	// approval 列加 a. 前缀避免与 join 表歧义;cond/排序里的列同理。
	condQ := strings.ReplaceAll(cond, "org_id", "a.org_id")
	condQ = strings.ReplaceAll(condQ, "team_id", "a.team_id")
	condQ = strings.ReplaceAll(condQ, "applicant_id", "a.applicant_id")
	condQ = strings.ReplaceAll(condQ, "state", "a.state")
	q := `SELECT a.id, a.org_id, a.applicant_id, a.team_id, a.request_type, a.payload, a.state, a.is_level2,
		a.l1_reviewer_id, a.l2_reviewer_id, a.reject_reason, a.created_at,
		COALESCE(NULLIF(m.display_name, ''), m.login_email, '') AS applicant_name
		FROM approval a LEFT JOIN member m ON m.id = a.applicant_id
		WHERE ` + condQ + ` ORDER BY a.id DESC LIMIT ? OFFSET ?`
	rows, err := s.db.QueryContext(ctx, q, pageArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []*model.Approval
	for rows.Next() {
		var a model.Approval
		var payload string
		if err := rows.Scan(&a.ID, &a.OrgID, &a.ApplicantID, &a.TeamID, &a.RequestType, &payload, &a.State, &a.IsLevel2,
			&a.L1ReviewerID, &a.L2ReviewerID, &a.RejectReason, &a.CreatedAt, &a.ApplicantName); err != nil {
			return nil, 0, err
		}
		_ = json.Unmarshal([]byte(payload), &a.Payload)
		out = append(out, &a)
	}
	return out, total, rows.Err()
}

// AdvanceApproval 乐观推进申请状态(仅当当前 state == fromState 才改),防并发重复裁决。
// reviewerCol ∈ {"l1","l2",""};reject 时传 reason。返回是否成功推进。
func (s *Store) AdvanceApproval(ctx context.Context, id int64, fromState, toState, reviewerCol string, reviewerID int64, reason *string) (bool, error) {
	set := "state = ?"
	args := []any{toState}
	switch reviewerCol {
	case "l1":
		set += ", l1_reviewer_id = ?, l1_reviewed_at = CURRENT_TIMESTAMP(3)"
		args = append(args, reviewerID)
	case "l2":
		set += ", l2_reviewer_id = ?, l2_reviewed_at = CURRENT_TIMESTAMP(3)"
		args = append(args, reviewerID)
	}
	if reason != nil {
		set += ", reject_reason = ?"
		args = append(args, *reason)
	}
	args = append(args, id, fromState)
	res, err := s.db.ExecContext(ctx, `UPDATE approval SET `+set+` WHERE id = ? AND state = ?`, args...)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

const approvalSelect = `SELECT id, org_id, applicant_id, team_id, request_type, payload, state, is_level2,
	l1_reviewer_id, l2_reviewer_id, reject_reason, created_at FROM approval`

func scanApproval(r rowScanner) (*model.Approval, error) {
	var a model.Approval
	var payload string
	err := r.Scan(&a.ID, &a.OrgID, &a.ApplicantID, &a.TeamID, &a.RequestType, &payload, &a.State, &a.IsLevel2,
		&a.L1ReviewerID, &a.L2ReviewerID, &a.RejectReason, &a.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(payload), &a.Payload)
	return &a, nil
}
