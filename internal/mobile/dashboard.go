package mobile

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/swahome/legion-api/internal/store"
)

// The dashboards add up the ledger. Loans, expenses, account balances,
// notifications and bank alerts come with later milestones; until then
// they are 0 or empty, in the shapes the app expects.

func suspendedReply(now time.Time) map[string]any {
	return timed(status(0, "Could not proceed. Account is suspended"), now)
}

// nextContribution is the next contribution date, or the Unix epoch when
// there is none, as the legacy dashboard printed it.
func (s *Server) nextContribution(ctx context.Context, g *member) (time.Time, error) {
	now := s.now().In(g.loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	d, err := s.q.NextContributionDate(ctx, store.NextContributionDateParams{GroupID: g.group.ID, Today: today})
	if errors.Is(err, pgx.ErrNoRows) {
		return dateOrEpoch(nil, g.loc), nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return dateOrEpoch(d, g.loc), nil
}

func (s *Server) getMemberDashboardData(ctx context.Context, c *call, g *member) (any, error) {
	if g.m.Status != store.MembershipStatusActive {
		return suspendedReply(s.now()), nil
	}
	totals, err := s.memberTotals(ctx, g, &g.m.ID)
	if err != nil {
		return nil, err
	}
	t := totals[g.m.ID]
	summary, err := s.contributionSummary(ctx, g)
	if err != nil {
		return nil, err
	}
	next, err := s.nextContribution(ctx, g)
	if err != nil {
		return nil, err
	}
	// total_arrears adds only what is owed, unless one side is a credit and
	// the other is clear, as the legacy dashboard did.
	ca, fa := t.contributionArrears, t.fineArrears
	var total float64
	switch {
	case fa > 0 && ca > 0:
		total = ca + fa
	case fa > 0:
		total = fa
	case ca > 0:
		total = ca
	case ca == 0 && fa < 0:
		total = fa
	case fa == 0 && ca < 0:
		total = ca
	}
	role := "Member"
	if g.m.IsOfficial {
		role = "Admin"
	}
	if name, err := s.roleOf(ctx, g); err != nil {
		return nil, err
	} else if name != "" {
		role = name
	}
	return map[string]any{
		"status":                         1,
		"time":                           s.now().Unix(),
		"role":                           role,
		"member_id":                      fmt.Sprint(g.m.ID),
		"notification_count":             0,
		"unreconciled_deposits_count":    0,
		"unreconciled_withdrawals_count": 0,
		"partner":                        0,
		"member_details": map[string]any{
			"total_contributions":         t.contributions,
			"total_fines":                 t.fines,
			"total_loan_balances":         0,
			"contribution_arrears":        ca,
			"fine_arrears":                fa,
			"total_arrears":               total,
			"next_contribution_date":      mobileShortTime(next),
			"contribution_date_days_left": elapsed(next, s.now()),
			"recent_transactions":         []any{}, // deposits arrive with milestone 3
			"member_contribution_summary": summary,
		},
	}, nil
}

// roleOf is the caller's active group role name, or "".
func (s *Server) roleOf(ctx context.Context, g *member) (string, error) {
	if g.m.GroupRoleID == nil {
		return "", nil
	}
	roles, err := s.q.GroupRoleNames(ctx, []int64{g.group.ID})
	if err != nil {
		return "", err
	}
	for _, r := range roles {
		if r.ID == *g.m.GroupRoleID {
			return r.Name, nil
		}
	}
	return "", nil
}

// contributionSummary is paid and balance per open contribution.
func (s *Server) contributionSummary(ctx context.Context, g *member) ([]any, error) {
	rows, err := s.q.MobileMemberContributionSummary(ctx, store.MobileMemberContributionSummaryParams{GroupID: g.group.ID, MembershipID: &g.m.ID})
	if err != nil {
		return nil, err
	}
	noArrears := g.flag("disable_arrears")
	out := make([]any, 0, len(rows))
	for _, r := range rows {
		balance := amount(r.Balance)
		if noArrears {
			balance = 0
		}
		out = append(out, map[string]any{"name": r.Name, "paid": amount(r.Paid), "balance": balance})
	}
	return out, nil
}

func (s *Server) getGroupDashboardData(ctx context.Context, c *call, g *member) (any, error) {
	if g.m.Status != store.MembershipStatusActive {
		return suspendedReply(s.now()), nil
	}
	contributions, fines, err := s.groupTotals(ctx, g)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"status": 1,
		"time":   s.now().Unix(),
		"group_details": map[string]any{
			"total_contributions":    contributions,
			"total_fines":            fines,
			"total_loan_balances":    0,
			"total_loaned_amount":    0,
			"total_loan_repaid":      0,
			"total_expense_payments": 0,
			"bank_balances":          0,
			"cash_balances":          0,
			"account_balances":       []any{},
		},
		"notification_count":             0,
		"has_partner_bank":               0,
		"unreconciled_deposits_count":    0,
		"unreconciled_withdrawals_count": 0,
	}, nil
}

func (s *Server) getGroupMemberContributionSummary(ctx context.Context, c *call, g *member) (any, error) {
	if g.m.Status != store.MembershipStatusActive {
		return suspendedReply(s.now()), nil
	}
	contributions, _, err := s.groupTotals(ctx, g)
	if err != nil {
		return nil, err
	}
	totals, err := s.memberTotals(ctx, g, &g.m.ID)
	if err != nil {
		return nil, err
	}
	next, err := s.nextContribution(ctx, g)
	if err != nil {
		return nil, err
	}
	t := totals[g.m.ID]
	return map[string]any{
		"status":                   1,
		"total_group_contribution": contributions,
		"member_contribution_summary": map[string]any{
			"contribution_paid":         t.contributions,
			"contribution_arrears":      t.contributionArrears,
			"next_contribution_date":    next.Unix(),
			"days_to_next_contribution": elapsed(next, s.now()),
		},
	}, nil
}
