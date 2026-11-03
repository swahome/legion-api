package mobile

import (
	"context"
	"encoding/json"
	"regexp"

	"github.com/swahome/legion-api/internal/store"
)

// In-app notifications: the list a member sees in the app, its unread
// count, and marking them read. The legacy server also pushed each one
// through Firebase's legacy FCM API, which Google has shut down; the
// app's mobile_token is stored (users/update_user_mobile_token) so push
// can be added with FCM HTTP v1 later.

// Legacy notification categories this server writes.
const (
	categoryMeeting   int16 = 7
	categoryStatement int16 = 16
)

// notify writes one member's notification.
func notify(ctx context.Context, q *store.Queries, groupID, membershipID int64, userID, fromUserID *int64,
	subject, message string, category int16, refs map[string]any, file *notificationFile) error {
	b, _ := json.Marshal(refs)
	if refs == nil {
		b = []byte("{}")
	}
	p := store.CreateNotificationParams{GroupID: groupID, MembershipID: membershipID, UserID: userID, FromUserID: fromUserID,
		Subject: subject, Message: message, Category: category, CallToAction: "View", Refs: b}
	if file != nil {
		p.FilePath, p.FileType, p.FileSize = file.path, file.kind, &file.size
		if file.cta != "" {
			p.CallToAction = file.cta
		}
	}
	_, err := q.CreateNotification(ctx, p)
	return err
}

type notificationFile struct {
	path, kind, cta string
	size            int64
}

// notifyGroup writes the same notification to every active member.
func notifyGroup(ctx context.Context, q *store.Queries, groupID int64, fromUserID *int64, subject, message string,
	category int16, refs map[string]any) error {
	members, err := q.GroupMembersToNotify(ctx, groupID)
	if err != nil {
		return err
	}
	for _, m := range members {
		uid := m.UserID
		if err := notify(ctx, q, groupID, m.ID, &uid, fromUserID, subject, message, category, refs, nil); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) unreadCount(ctx context.Context, g *member) (int32, error) {
	return s.q.CountUnreadNotifications(ctx, store.CountUnreadNotificationsParams{GroupID: g.group.ID, MembershipID: g.m.ID})
}

var tags = regexp.MustCompile(`<[^>]*>`)

func (s *Server) getAllGroupMemberNotifications(ctx context.Context, c *call, g *member) (any, error) {
	limit, offset := pageOf(c, 100)
	rows, err := s.q.ListMemberNotifications(ctx, store.ListMemberNotificationsParams{GroupID: g.group.ID,
		MembershipID: g.m.ID, Lim: limit, Off: offset})
	if err != nil {
		return nil, err
	}
	unread, err := s.unreadCount(ctx, g)
	if err != nil {
		return nil, err
	}
	out := []any{}
	for i, n := range rows {
		item := map[string]any{}
		var refs map[string]any
		_ = json.Unmarshal(n.Refs, &refs)
		for k, v := range refs {
			item[k] = v
		}
		var size any
		if n.FileSize != nil {
			size = *n.FileSize
		}
		for k, v := range map[string]any{
			"count": int(offset) + i + 1, "id": n.ID, "subject": n.Subject, "message": tags.ReplaceAllString(n.Message, ""),
			"category": n.Category, "time_ago": timeAgo(n.CreatedAt, s.now()), "is_read": boolInt(n.IsRead),
			"from_user_id": nullableID(n.FromUserID), "to_user_id": nullableID(n.UserID), "to_member_id": n.MembershipID,
			"group_id": n.GroupID, "created_on": n.CreatedAt.Unix(), "call_to_action": n.CallToAction,
			"call_to_action_link": n.Link, "file_path": n.FilePath, "file_type": n.FileType, "file_size": size, "active": 1,
		} {
			item[k] = v
		}
		out = append(out, item)
	}
	return map[string]any{"status": 1, "message": "Member notifications", "time": s.now().Unix(),
		"all_notification_counts": len(out), "notifications": out, "notification_count": unread}, nil
}

func nullableID(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

func (s *Server) getGroupMemberNotificationCount(ctx context.Context, c *call, g *member) (any, error) {
	unread, err := s.unreadCount(ctx, g)
	if err != nil {
		return nil, err
	}
	return map[string]any{"status": 1, "message": "Member notifications", "time": s.now().Unix(), "notification_count": unread}, nil
}

func (s *Server) markAllNotifications(read bool) groupFn {
	flag := "mark_all_notifications_as_read"
	if !read {
		flag = "mark_all_notifications_as_unread"
	}
	return func(ctx context.Context, c *call, g *member) (any, error) {
		if v := c.req.Str(flag); v == "" || v == "0" {
			return map[string]any{"status": 0, "time": s.now().Unix(), "error": "Kindly try again later. No operation performed."}, nil
		}
		if err := s.q.MarkMemberNotifications(ctx, store.MarkMemberNotificationsParams{IsRead: read, GroupID: g.group.ID,
			MembershipID: g.m.ID}); err != nil {
			return nil, err
		}
		return map[string]any{"status": 1, "time": s.now().Unix(), "success": "success."}, nil
	}
}

func (s *Server) markNotificationRead(ctx context.Context, c *call, g *member) (any, error) {
	n, err := s.q.MarkNotificationRead(ctx, store.MarkNotificationReadParams{GroupID: g.group.ID, ID: c.req.Int("notification_id")})
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return map[string]any{"status": 0, "time": s.now().Unix(), "error": "Notification not available"}, nil
	}
	return map[string]any{"status": 1, "time": s.now().Unix(), "success": "success"}, nil
}

// alertSummary is what the dashboards show beside notifications: whether
// the group banks with a partner bank, and its unreconciled alerts.
func (s *Server) alertSummary(ctx context.Context, g *member) (partner int, deposits, withdrawals int32, err error) {
	accounts, _, err := s.groupAccounts(ctx, g)
	if err != nil {
		return 0, 0, 0, err
	}
	for _, a := range accounts {
		if a.Kind == store.AccountKindBank && (a.IsVerified || a.IsDefault) {
			partner = 1
		}
	}
	counts, err := s.q.AlertCounts(ctx, &g.group.ID)
	if err != nil {
		return 0, 0, 0, err
	}
	for _, c := range counts {
		if c.Direction > 0 {
			deposits = c.Alerts
		} else {
			withdrawals = c.Alerts
		}
	}
	return partner, deposits, withdrawals, nil
}

func (s *Server) getGroupMemberNotificationsSummary(ctx context.Context, c *call, g *member) (any, error) {
	unread, err := s.unreadCount(ctx, g)
	if err != nil {
		return nil, err
	}
	partner, deps, wds, err := s.alertSummary(ctx, g)
	if err != nil {
		return nil, err
	}
	return map[string]any{"status": 1, "notification_summary": map[string]any{"notification_count": unread,
		"unreconciled_deposits_count": deps, "unreconciled_withdrawals_count": wds}, "has_partner_bank": partner}, nil
}
