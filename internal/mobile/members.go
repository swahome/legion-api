package mobile

import (
	"context"
	"fmt"
	"strings"

	"github.com/swahome/legion-api/internal/store"
)

// memberTotals are a member's dashboard figures, the legacy
// _get_member_group_dashboard_data. Loans arrive with milestone 4.
type memberTotals struct {
	contributions, contributionArrears, fines, fineArrears float64
}

func (s *Server) memberTotals(ctx context.Context, g *member, membershipID *int64) (map[int64]memberTotals, error) {
	rows, err := s.q.MobileMemberTotals(ctx, store.MobileMemberTotalsParams{GroupID: g.group.ID, MembershipID: membershipID})
	if err != nil {
		return nil, err
	}
	noArrears := g.flag("disable_arrears")
	out := make(map[int64]memberTotals, len(rows))
	for _, r := range rows {
		t := memberTotals{contributions: amount(r.ContributionsPaid), fines: amount(r.FinesPaid)}
		if !noArrears {
			t.contributionArrears, t.fineArrears = amount(r.ContributionArrears), amount(r.FineArrears)
		}
		out[r.MembershipID] = t
	}
	return out, nil
}

// groupTotals is the legacy _get_group_dashboard_data. Loans, expenses and
// account balances stay 0 until milestones 3 and 4 bring them.
func (s *Server) groupTotals(ctx context.Context, g *member) (contributions, fines float64, err error) {
	t, err := s.q.MobileGroupTotals(ctx, g.group.ID)
	if err != nil {
		return 0, 0, err
	}
	return amount(t.ContributionsPaid), amount(t.FinesPaid), nil
}

func roleName(m store.MobileGroupMembersRow, admin string) string {
	if m.RoleName != nil {
		return *m.RoleName
	}
	if m.IsOfficial {
		return admin
	}
	return "Member"
}

func (s *Server) getMembersAndDetails(ctx context.Context, c *call, g *member) (any, error) {
	limit, offset := page(c)
	full := c.req.Str("show_full_details") != "0" && c.req.Str("show_full_details") != "false" // defaults to TRUE
	viewID := c.req.Int("view_profile_member_id")
	viewProfile := 1
	if !g.official() && g.flag("enable_member_information_privacy") {
		if viewID != 0 && viewID != g.m.ID {
			return status(0, "Settings > Enable member informaton privacy is enabled within the group and thus you can not view other member profiles"), nil
		}
		viewID, viewProfile = g.m.ID, 0
	}
	var only *int64
	if viewID != 0 {
		only = &viewID
	}
	rows, err := s.q.MobileGroupMembers(ctx, store.MobileGroupMembersParams{GroupID: g.group.ID, MembershipID: only, Lim: limit, Off: offset})
	if err != nil {
		return nil, err
	}
	var totals map[int64]memberTotals
	var lb *loanBook
	if full {
		if totals, err = s.memberTotals(ctx, g, only); err != nil {
			return nil, err
		}
		if lb, err = s.loanBook(ctx, g, nil); err != nil {
			return nil, err
		}
	}
	var filter any = []any{false}
	if only != nil {
		filter = []any{fmt.Sprint(viewID)}
	}
	now := s.now()
	members, suspended := []any{}, []any{}
	for _, m := range rows {
		lastSeen := "Never"
		if m.LastLoginAt != nil {
			if now.Sub(*m.LastLoginAt).Minutes() < 2 {
				lastSeen = "Online"
			} else {
				lastSeen = "Last seen " + timeAgo(*m.LastLoginAt, now)
			}
		}
		var phone any = 0
		if m.Phone != nil {
			phone = legacyPhone(m.Phone)
		}
		row := map[string]any{
			"name":                m.FirstName + " " + m.LastName,
			"first_name":          m.FirstName,
			"last_name":           m.LastName,
			"role":                roleName(m, "Group Admin"),
			"has_role":            boolInt(m.RoleName != nil),
			"is_editable":         boolInt(m.UserID != g.user.ID && m.LastLoginAt == nil),
			"view_member_profile": viewProfile,
			"phone":               phone,
			"email":               deref(m.Email),
			"last_seen":           lastSeen,
			"avatar":              nullable(m.Avatar),
			"member_id":           fmt.Sprint(m.ID),
			"user_id":             fmt.Sprint(m.UserID),
			"selected":            m.UserID == g.user.ID || m.ID == viewID,
			"filter_params":       map[string]any{"member_id": filter},
		}
		if full {
			t := totals[m.ID]
			row["total_contributions"] = t.contributions
			row["total_fines"] = t.fines
			row["total_loan_balances"] = float64(lb.byMember[m.ID]) / 100
			row["contribution_arrears"] = t.contributionArrears
			row["total_fine_arrears"] = t.fineArrears
			row["lump_sum_balance"] = 0
		}
		if m.Status == store.MembershipStatusActive {
			members = append(members, row)
		} else {
			suspended = append(suspended, row)
		}
	}
	var details any = []any{}
	if full {
		contributions, fines, err := s.groupTotals(ctx, g)
		if err != nil {
			return nil, err
		}
		details = map[string]any{
			"total_contributions":    contributions,
			"total_fines":            fines,
			"total_loan_balances":    float64(lb.balance) / 100,
			"total_expense_payments": 0,
		}
	}
	return map[string]any{
		"status": 1, "time": now.Unix(), "message": "Member Details",
		"members": members, "suspended_members": suspended, "group_details": details,
	}, nil
}

// getGroupMemberOptions lists active members for the app's pickers. With
// member privacy on, ordinary members see only themselves.
func (s *Server) getGroupMemberOptions(ctx context.Context, c *call, g *member) (any, error) {
	var only *int64
	if !g.official() && g.flag("enable_member_information_privacy") {
		only = &g.m.ID
	}
	rows, err := s.q.MobileGroupMembers(ctx, store.MobileGroupMembersParams{
		GroupID: g.group.ID, ActiveOnly: true, MembershipID: only, Lim: 100000,
	})
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(rows))
	for _, m := range rows {
		identity := legacyPhone(m.Phone)
		if m.Email != nil && strings.Contains(*m.Email, "@") {
			identity = *m.Email
		}
		var phone any = 0
		if m.Phone != nil {
			phone = legacyPhone(m.Phone)
		}
		out = append(out, map[string]any{
			"id":        fmt.Sprint(m.ID),
			"name":      m.FirstName + " " + m.LastName,
			"avatar":    nullable(m.Avatar),
			"user_id":   fmt.Sprint(m.UserID),
			"identity":  identity,
			"phone":     phone,
			"suspended": 0,
		})
	}
	return timed(map[string]any{"status": 1, "message": "Members", "members": out}, s.now()), nil
}

// nullable is a text column that MySQL held as NULL when empty.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
