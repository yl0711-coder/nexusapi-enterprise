package service

import (
	"context"

	"github.com/nexusapi-platform/enterprise/model"
	"github.com/nexusapi-platform/enterprise/pkg/apperr"
	"github.com/nexusapi-platform/enterprise/pkg/session"
)

// notify 生成一条站内通知(US-13;脱敏,不含明文 key/跨组织信息)。
// 邮件(可配开关)本期留钩子:异步发、失败不阻塞主流程——MVP 仅站内,邮件 V-soon。
func (s *Service) notify(ctx context.Context, orgID, memberID int64, ntype, title, body string) {
	var b *string
	if body != "" {
		b = &body
	}
	if err := s.store.CreateNotification(ctx, &model.Notification{
		OrgID: orgID, MemberID: memberID, Type: ntype, Title: title, Body: b,
	}); err != nil {
		s.log.Error("写站内通知失败", "member_id", memberID, "type", ntype, "err", err)
	}
}

// ListNotifications 列本人通知(成员自助;只能看本人,US-13)。
func (s *Service) ListNotifications(ctx context.Context, c session.Claims, limit, offset int) ([]*model.Notification, int, int, error) {
	return s.store.ListNotifications(ctx, c.OrgID, c.MemberID, limit, offset)
}

// MarkNotificationRead 标已读(id=0 全部)。只能标本人。
func (s *Service) MarkNotificationRead(ctx context.Context, c session.Claims, id int64) error {
	if err := s.store.MarkNotificationRead(ctx, c.OrgID, c.MemberID, id); err != nil {
		return apperr.Internal("").WithCause(err)
	}
	return nil
}
