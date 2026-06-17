package repo

import (
	"context"

	"github.com/nexusapi-platform/enterprise/model"
)

// CreateNotification 追加一条站内通知。
func (s *Store) CreateNotification(ctx context.Context, n *model.Notification) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO notification (org_id, member_id, type, title, body) VALUES (?, ?, ?, ?, ?)`,
		n.OrgID, n.MemberID, n.Type, n.Title, n.Body)
	return err
}

// ListNotifications 列某成员的通知(分页,倒序)+ 未读数。
func (s *Store) ListNotifications(ctx context.Context, orgID, memberID int64, limit, offset int) ([]*model.Notification, int, int, error) {
	var total, unread int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(is_read=0),0) FROM notification WHERE org_id = ? AND member_id = ?`,
		orgID, memberID).Scan(&total, &unread); err != nil {
		return nil, 0, 0, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, org_id, member_id, type, title, body, is_read, created_at
		 FROM notification WHERE org_id = ? AND member_id = ? ORDER BY id DESC LIMIT ? OFFSET ?`,
		orgID, memberID, limit, offset)
	if err != nil {
		return nil, 0, 0, err
	}
	defer rows.Close()
	var out []*model.Notification
	for rows.Next() {
		var n model.Notification
		if err := rows.Scan(&n.ID, &n.OrgID, &n.MemberID, &n.Type, &n.Title, &n.Body, &n.IsRead, &n.CreatedAt); err != nil {
			return nil, 0, 0, err
		}
		out = append(out, &n)
	}
	return out, total, unread, rows.Err()
}

// MarkNotificationRead 标某条(或全部:id=0)已读,强制成员谓词。
func (s *Store) MarkNotificationRead(ctx context.Context, orgID, memberID, id int64) error {
	if id == 0 {
		_, err := s.db.ExecContext(ctx,
			`UPDATE notification SET is_read = 1 WHERE org_id = ? AND member_id = ? AND is_read = 0`, orgID, memberID)
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE notification SET is_read = 1 WHERE id = ? AND org_id = ? AND member_id = ?`, id, orgID, memberID)
	return err
}
