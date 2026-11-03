package mobile

import (
	"context"
	"time"

	"github.com/swahome/legion-api/internal/store"
)

// The older and smaller dashboards the app still calls: the combined
// get_member_dashboard, get_group_more_dashboard_data, the bank and fines
// summaries, the twelve-month charts and the account balances report.

// totalArrears adds only what is owed, unless one side is a credit and the
// other is clear, as the legacy dashboards did.
func totalArrears(ca, fa float64) float64 {
	switch {
	case fa > 0 && ca > 0:
		return ca + fa
	case fa > 0:
		return fa
	case ca > 0:
		return ca
	case ca == 0 && fa < 0:
		return fa
	case fa == 0 && ca < 0:
		return ca
	}
	return 0
}

// memberRole is the caller's role as the dashboards name it.
func (s *Server) memberRole(ctx context.Context, g *member) (string, error) {
	role := "Member"
	if g.m.IsOfficial {
		role = "Admin"
	}
	name, err := s.roleOf(ctx, g)
	if err != nil {
		return "", err
	}
	if name != "" {
		role = name
	}
	return role, nil
}

func (s *Server) getMemberDashboard(ctx context.Context, c *call, g *member) (any, error) {
	contributions, fines, err := s.groupTotals(ctx, g)
	if err != nil {
		return nil, err
	}
	expenses, err := s.q.MobileGroupExpenses(ctx, g.group.ID)
	if err != nil {
		return nil, err
	}
	bank, cash, accounts, err := s.cashBalances(ctx, g)
	if err != nil {
		return nil, err
	}
	lb, err := s.loanBook(ctx, g, nil)
	if err != nil {
		return nil, err
	}
	// With member privacy on, ordinary members see only their own
	// transactions in the group's list too.
	var groupScope *int64
	if g.flag("enable_member_information_privacy") && !g.official() {
		groupScope = &g.m.ID
	}
	groupRecent, err := s.recentDeposits(ctx, g, groupScope, 10)
	if err != nil {
		return nil, err
	}
	totals, err := s.memberTotals(ctx, g, &g.m.ID)
	if err != nil {
		return nil, err
	}
	t := totals[g.m.ID]
	mine, err := s.loanBook(ctx, g, &g.m.ID)
	if err != nil {
		return nil, err
	}
	recent, err := s.recentDeposits(ctx, g, &g.m.ID, 5)
	if err != nil {
		return nil, err
	}
	next, err := s.nextContribution(ctx, g)
	if err != nil {
		return nil, err
	}
	role, err := s.memberRole(ctx, g)
	if err != nil {
		return nil, err
	}
	unread, err := s.unreadCount(ctx, g)
	if err != nil {
		return nil, err
	}
	partner, deps, wds, err := s.alertSummary(ctx, g)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"status": 1,
		"time":   s.now().Unix(),
		"group_details": map[string]any{
			"total_contributions":    contributions,
			"total_fines":            fines,
			"total_loan_balances":    float64(lb.balance) / 100,
			"total_expense_payments": amount(expenses),
			"bank_balances":          bank,
			"cash_balances":          cash,
			"account_balances":       accounts,
			"recent_transactions":    groupRecent,
		},
		"member_details": map[string]any{
			"total_contributions":         t.contributions,
			"total_fines":                 t.fines,
			"total_loan_balances":         float64(mine.balance) / 100,
			"contribution_arrears":        t.contributionArrears,
			"fine_arrears":                t.fineArrears,
			"total_arrears":               totalArrears(t.contributionArrears, t.fineArrears),
			"next_contribution_date":      mobileShortTime(next),
			"contribution_date_days_left": elapsed(next, s.now()),
			"recent_transactions":         recent,
			"role":                        role,
			"member_id":                   g.m.ID,
		},
		"notification_count":             unread,
		"has_partner_bank":               partner,
		"unreconciled_deposits_count":    deps,
		"unreconciled_withdrawals_count": wds,
	}, nil
}

func (s *Server) getGroupMoreDashboardData(ctx context.Context, c *call, g *member) (any, error) {
	contributions, fines, err := s.groupTotals(ctx, g)
	if err != nil {
		return nil, err
	}
	expenses, err := s.q.MobileGroupExpenses(ctx, g.group.ID)
	if err != nil {
		return nil, err
	}
	lb, err := s.loanBook(ctx, g, nil)
	if err != nil {
		return nil, err
	}
	totals, err := s.memberTotals(ctx, g, &g.m.ID)
	if err != nil {
		return nil, err
	}
	t := totals[g.m.ID]
	mine, err := s.loanBook(ctx, g, &g.m.ID)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"status": 1,
		"time":   s.now().Unix(),
		"group_details": map[string]any{
			"total_contributions":    contributions,
			"total_fines":            fines,
			"total_loan_balances":    float64(lb.balance) / 100,
			"total_expense_payments": amount(expenses),
		},
		"member_details": map[string]any{
			"total_contributions":  t.contributions,
			"total_fines":          t.fines,
			"total_loan_balances":  float64(mine.balance) / 100,
			"contribution_arrears": t.contributionArrears,
			"fine_arrears":         t.fineArrears,
			"total_arrears":        totalArrears(t.contributionArrears, t.fineArrears),
			"member_id":            g.m.ID,
		},
	}, nil
}

func (s *Server) getGroupBankBalances(ctx context.Context, c *call, g *member) (any, error) {
	bank, cash, _, err := s.cashBalances(ctx, g)
	if err != nil {
		return nil, err
	}
	return map[string]any{"status": 1, "total_bank_balance": bank + cash,
		"account_balances": map[string]any{"bank_account_balance": bank, "cash_account_balance": cash}}, nil
}

func (s *Server) getGroupMemberFinesSummary(ctx context.Context, c *call, g *member) (any, error) {
	_, fines, err := s.groupTotals(ctx, g)
	if err != nil {
		return nil, err
	}
	totals, err := s.memberTotals(ctx, g, &g.m.ID)
	if err != nil {
		return nil, err
	}
	t := totals[g.m.ID]
	return map[string]any{"status": 1, "total_group_fines_paid": fines,
		"member_fines_summary": map[string]any{"fine_paid": t.fines, "fine_arrears": t.fineArrears}}, nil
}

// chart is the last twelve months, oldest first, keyed "Jan", "Feb" and so
// on, as the legacy _get_group_chart_data built it.
type chart struct {
	months                                []string // "Jan"
	mine, deposits, withdrawals           []float64
	memberRecords, depositWithdrawRecords int32
	expenses                              []any
}

func (s *Server) chartData(ctx context.Context, g *member) (chart, error) {
	now := s.now().In(g.loc)
	since := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -11, 0)
	rows, err := s.q.MonthlyCashFlow(ctx, store.MonthlyCashFlowParams{MembershipID: &g.m.ID, GroupID: g.group.ID, Since: since})
	if err != nil {
		return chart{}, err
	}
	byMonth := make(map[string]store.MonthlyCashFlowRow, len(rows))
	for _, r := range rows {
		byMonth[r.Month] = r
	}
	var ch chart
	for i := 0; i < 12; i++ {
		m := since.AddDate(0, i, 0)
		r := byMonth[m.Format("2006-01")]
		ch.months = append(ch.months, m.Format("Jan"))
		ch.mine = append(ch.mine, amount(r.Mine))
		ch.deposits = append(ch.deposits, amount(r.MoneyIn))
		ch.withdrawals = append(ch.withdrawals, amount(r.MoneyOut))
		ch.memberRecords += r.MineCount
		ch.depositWithdrawRecords += r.Rows
	}
	cats, err := s.q.MobileExpenseCategoryTotals(ctx, g.group.ID)
	if err != nil {
		return chart{}, err
	}
	ch.expenses = []any{}
	for _, c := range cats {
		if amount(c.Total) != 0 {
			ch.expenses = append(ch.expenses, map[string]any{"expense_name": c.Name, "amount": amount(c.Total)})
		}
	}
	return ch, nil
}

func (ch chart) byMonth(vals []float64) object {
	o := make(object, 0, len(vals))
	for i, v := range vals {
		o = append(o, field{ch.months[i], v})
	}
	return o
}

func (ch chart) monthList(vals []float64) []any {
	out := make([]any, 0, len(vals))
	for i, v := range vals {
		out = append(out, map[string]any{"month": ch.months[i], "amount": v})
	}
	return out
}

// memberAndGroupChart is get_member_and_group_chart_data, and with list
// get_ios_member_and_group_chart_data, which sends the months as lists.
func (s *Server) memberAndGroupChart(list bool) groupFn {
	return func(ctx context.Context, c *call, g *member) (any, error) {
		ch, err := s.chartData(ctx, g)
		if err != nil {
			return nil, err
		}
		shape := func(v []float64) any { return ch.byMonth(v) }
		if list {
			shape = func(v []float64) any { return ch.monthList(v) }
		}
		return map[string]any{
			"status":                     1,
			"time":                       s.now().Unix(),
			"member_records":             ch.memberRecords,
			"deposit_withdrawal_records": ch.depositWithdrawRecords,
			"expenses_records":           len(ch.expenses),
			"group_transactions": map[string]any{"deposits": shape(ch.deposits), "withdrawals": shape(ch.withdrawals),
				"expenses": ch.expenses},
			"member_transactions": shape(ch.mine),
		}, nil
	}
}

var accountCategories = []struct {
	kind store.AccountKind
	name string
}{{"bank", "Bank Accounts"}, {"sacco", "Sacco Accounts"}, {"mobile_money", "Mobile Money Accounts"}, {"petty_cash", "Petty Cash Accounts"}}

// accountBalances is reports/account_balances: open accounts by category.
func (s *Server) accountBalances(ctx context.Context, c *call, g *member) (any, error) {
	rows, _, err := s.groupAccounts(ctx, g)
	if err != nil {
		return nil, err
	}
	var grand float64
	out := []any{}
	for _, cat := range accountCategories {
		var total float64
		list := []any{}
		for _, a := range rows {
			if a.Kind != cat.kind || a.IsClosed {
				continue
			}
			bal := amount(a.Balance)
			total += bal
			var number any = 0
			if a.AccountNumber != "" {
				number = a.AccountNumber
			}
			list = append(list, map[string]any{"position": len(list) + 1, "account_name": accountLabel(a),
				"account_number": number, "account_balance": bal})
		}
		grand += total
		if len(list) > 0 {
			out = append(out, map[string]any{"category_name": cat.name, "total_balance": total, "account_balances": list})
		}
	}
	return map[string]any{"status": 1, "message": "successful", "time": s.now().Unix(),
		"data": map[string]any{"balances": out, "grand_total_balance": grand}}, nil
}
