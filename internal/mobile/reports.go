package mobile

import (
	"context"
	"strings"
	"time"

	"github.com/swahome/legion-api/internal/books"
	"github.com/swahome/legion-api/internal/store"
)

// Reports the app shows: contribution and fine summaries per member, the
// expense summary and the group's transaction statement (the cash book).
// The loan summaries arrive with loans in milestone 4.

// reportMembers is the active members a summary covers: just the caller
// when the group hides members' details from each other.
func (s *Server) reportMembers(ctx context.Context, g *member) ([]store.MobileGroupMembersRow, error) {
	p := store.MobileGroupMembersParams{GroupID: g.group.ID, ActiveOnly: true, Lim: 100000}
	if !g.official() && g.flag("enable_member_information_privacy") {
		p.MembershipID = &g.m.ID
	}
	return s.q.MobileGroupMembers(ctx, p)
}

// memberSummary is contribution_summary (fines false) and fines_summary.
func (s *Server) memberSummary(fines bool) groupFn {
	return func(ctx context.Context, c *call, g *member) (any, error) {
		members, err := s.reportMembers(ctx, g)
		if err != nil {
			return nil, err
		}
		totals, err := s.q.MobileMemberTotals(ctx, store.MobileMemberTotalsParams{GroupID: g.group.ID})
		if err != nil {
			return nil, err
		}
		byMember := make(map[int64]store.MobileMemberTotalsRow, len(totals))
		for _, t := range totals {
			byMember[t.MembershipID] = t
		}
		noArrears := g.flag("disable_arrears")
		balances := make([]any, 0, len(members))
		for _, m := range members {
			t := byMember[m.ID]
			paid, arrears := t.ContributionsPaid, t.ContributionArrears
			if fines {
				paid, arrears = t.FinesPaid, t.FineArrears
			}
			owed := amount(arrears)
			if noArrears {
				owed = 0
			}
			balances = append(balances, map[string]any{
				"member_id": m.ID, "name": m.FirstName + " " + m.LastName, "paid": amount(paid), "arrears": owed,
			})
		}
		return timed(map[string]any{"status": 1, "message": "success", "balances": balances}, s.now()), nil
	}
}

func (s *Server) expensesSummary(ctx context.Context, c *call, g *member) (any, error) {
	rows, err := s.q.MobileExpenseCategoryTotals(ctx, g.group.ID)
	if err != nil {
		return nil, err
	}
	var total int64
	expenses := make([]any, 0, len(rows))
	for i, r := range rows {
		total += books.Cents(r.Total)
		expenses = append(expenses, map[string]any{"position": i + 1, "expense_name": r.Name, "amount": amount(r.Total)})
	}
	return timed(map[string]any{"status": 1, "message": "successful", "data": map[string]any{
		"expenses": expenses, "total_expenses": float64(total) / 100,
	}}, s.now()), nil
}

// reportTimeLong is timestamp_to_mobile_report_time: "5th Feb, 2024 ".
func reportTimeLong(t time.Time) string {
	return ordinal(t.Day()) + t.Format(" Jan, 2006 ")
}

// statementLine is how the legacy statement described a cash book entry.
func statementLine(e store.CashBookRow) string {
	acct := ""
	if e.AccountName != nil {
		acct = *e.AccountName
	}
	var out string
	switch {
	case e.DepositKind.Valid:
		out = strings.TrimSpace(depositNames[e.DepositKind.DepositKind])
		if e.MemberName != "" {
			out += " from " + e.MemberName
		}
		out += " to " + acct
	case e.WithdrawalKind.Valid:
		out = withdrawalNames[e.WithdrawalKind.WithdrawalKind]
		if e.MemberName != "" {
			out += " to " + e.MemberName
		}
		out += ", withdrawn from " + acct
	case e.AccountTransferID != nil && e.Direction > 0:
		out = "Funds Transfer to " + acct
	case e.AccountTransferID != nil:
		out = "Funds Transfer from " + acct
	default:
		out = "Transaction on " + acct
	}
	if e.Description != "" {
		out += " : " + e.Description
	}
	return out
}

// transactionStatement is the group's cash book across all accounts, from
// "from" (default four months ago) to "to" (default tomorrow). The app
// sent the dates in the URL query.
func (s *Server) transactionStatement(ctx context.Context, c *call, g *member) (any, error) {
	now := s.now().In(g.loc)
	read := func(key string) *time.Time {
		v := c.r.URL.Query().Get(key)
		if v == "" {
			v = c.req.Str(key)
		}
		return appDate(v, g.loc)
	}
	from, to := read("from"), read("to")
	if from == nil {
		d := time.Date(now.Year(), now.Month()-4, now.Day(), 0, 0, 0, 0, time.UTC)
		from = &d
	}
	if to == nil {
		d := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
		to = &d
	}
	opening, err := s.q.CashBookOpening(ctx, store.CashBookOpeningParams{GroupID: g.group.ID, Before: *from})
	if err != nil {
		return nil, err
	}
	initial, err := s.q.InitialBalances(ctx, store.InitialBalancesParams{GroupID: g.group.ID})
	if err != nil {
		return nil, err
	}
	rows, err := s.q.CashBook(ctx, store.CashBookParams{GroupID: g.group.ID, FromDate: *from, ToDate: *to})
	if err != nil {
		return nil, err
	}
	balance := books.Cents(opening) + books.Cents(initial)
	body := make([]any, 0, len(rows))
	for _, e := range rows {
		cents := books.Cents(e.Amount)
		var in, out float64
		if e.Direction > 0 {
			balance += cents
			in = float64(cents) / 100
		} else {
			balance -= cents
			out = float64(cents) / 100
		}
		body = append(body, map[string]any{
			"transaction_date": reportTimeLong(e.EntryDate), "description": statementLine(e),
			"withdrawn": out, "deposited": in, "balance": float64(balance) / 100,
		})
	}
	start := float64(books.Cents(opening)+books.Cents(initial)) / 100
	return map[string]any{"status": 1, "message": "successful", "data": map[string]any{
		"statement_details": map[string]any{
			"statement_as_at":       reportTimeLong(now),
			"statement_period_from": reportTimeLong(*from),
			"statement_period_to":   reportTimeLong(*to),
		},
		"statement_header": map[string]any{
			"description": "Balance B/F", "date": reportTimeLong(*from), "withdrawn": 0, "deposited": 0, "balance": start,
		},
		"statement_body":   body,
		"statement_footer": map[string]any{"description": "Totals", "balance": float64(balance) / 100},
	}}, nil
}
