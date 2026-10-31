package legacy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Tables written by the import, in foreign key order.
var targets = []struct {
	name string
	cols []string
	rows func(*Plan) [][]any
}{
	{"users", UserCols, func(p *Plan) [][]any { return p.Users }},
	{"user_platform_roles", PlatformRoleCols, func(p *Plan) [][]any { return p.PlatformRoles }},
	{"groups", GroupCols, func(p *Plan) [][]any { return p.Groups }},
	{"group_roles", GroupRoleCols, func(p *Plan) [][]any { return p.GroupRoles }},
	{"memberships", MembershipCols, func(p *Plan) [][]any { return p.Memberships }},
	{"contributions", ContributionCols, func(p *Plan) [][]any { return p.Contributions }},
	{"contribution_schedules", ScheduleCols, func(p *Plan) [][]any { return p.Schedules }},
	{"contribution_members", ContributionMemberCols, func(p *Plan) [][]any { return p.ContributionMembers }},
	{"contribution_fine_rules", FineRuleCols, func(p *Plan) [][]any { return p.FineRules }},
	{"countries", CountryCols, func(p *Plan) [][]any { return p.Countries }},
	{"counties", CountyCols, func(p *Plan) [][]any { return p.Counties }},
	{"banks", BankCols, func(p *Plan) [][]any { return p.Banks }},
	{"bank_branches", BankBranchCols, func(p *Plan) [][]any { return p.BankBranches }},
	{"saccos", SaccoCols, func(p *Plan) [][]any { return p.Saccos }},
	{"sacco_branches", SaccoBranchCols, func(p *Plan) [][]any { return p.SaccoBranches }},
	{"mobile_money_providers", ProviderCols, func(p *Plan) [][]any { return p.Providers }},
	{"group_accounts", AccountCols, func(p *Plan) [][]any { return p.Accounts }},
	{"income_categories", IncomeCategoryCols, func(p *Plan) [][]any { return p.IncomeCategories }},
	{"expense_categories", ExpenseCategoryCols, func(p *Plan) [][]any { return p.ExpenseCategories }},
	{"fine_categories", FineCategoryCols, func(p *Plan) [][]any { return p.FineCategories }},
	{"depositors", DepositorCols, func(p *Plan) [][]any { return p.Depositors }},
	{"loan_types", LoanTypeCols, func(p *Plan) [][]any { return p.LoanTypes }},
	{"loans", LoanCols, func(p *Plan) [][]any { return p.Loans }},
	{"loan_charges", LoanChargeCols, func(p *Plan) [][]any { return p.LoanCharges }},
	{"loan_guarantors", LoanGuarantorCols, func(p *Plan) [][]any { return p.LoanGuarantors }},
	{"deposits", DepositCols, func(p *Plan) [][]any { return p.Deposits }},
	{"loan_repayments", LoanRepaymentCols, func(p *Plan) [][]any { return p.LoanRepayments }},
	{"withdrawals", WithdrawalCols, func(p *Plan) [][]any { return p.Withdrawals }},
	{"account_transfers", AccountTransferCols, func(p *Plan) [][]any { return p.AccountTransfers }},
	{"account_entries", AccountEntryCols, func(p *Plan) [][]any { return p.AccountEntries }},
	{"withdrawal_requests", WithdrawalRequestCols, func(p *Plan) [][]any { return p.WithdrawalRequests }},
	{"withdrawal_approvals", WithdrawalApprovalCols, func(p *Plan) [][]any { return p.WithdrawalApprovals }},
	{"ledger_entries", LedgerCols, func(p *Plan) [][]any { return p.Ledger }},
	{"platform_settings", SettingCols, func(p *Plan) [][]any { return p.Settings }},
	{"mobile_access_tokens", MobileTokenCols, func(p *Plan) [][]any { return p.MobileTokens }},
}

// Tables with identity ids whose sequences must move past the imported ids.
var identityTables = []string{"users", "groups", "group_roles", "memberships", "contributions",
	"contribution_fine_rules", "ledger_entries", "countries", "counties", "banks", "bank_branches",
	"saccos", "sacco_branches", "mobile_money_providers", "group_accounts", "income_categories",
	"expense_categories", "fine_categories", "depositors", "deposits", "withdrawals", "account_transfers",
	"account_entries", "withdrawal_requests", "withdrawal_approvals",
	"loan_types", "loans", "loan_charges", "loan_repayments", "loan_guarantors"}

var enumTypes = []string{
	"platform_role", "group_status", "membership_status", "contribution_kind",
	"contribution_category", "contribution_frequency", "fine_kind", "ledger_entry_type",
	"account_kind", "deposit_kind", "withdrawal_kind", "payment_method",
	"withdrawal_request_status", "approval_decision",
	"interest_type", "interest_rate_per", "repayment_frequency", "loan_charge_kind",
}

// Columns that hold decimal strings to send as numeric.
var numericColumns = map[string][]string{
	"contributions": {"amount"}, "ledger_entries": {"amount"},
	"contribution_fine_rules": {"fixed_amount", "percentage_rate"},
	"group_accounts":          {"initial_balance"}, "fine_categories": {"amount"},
	"deposits": {"amount"}, "withdrawals": {"amount"}, "account_transfers": {"amount"},
	"account_entries": {"amount"}, "withdrawal_requests": {"amount"},
	"loan_types":   {"interest_rate", "fixed_interest", "fixed_interest_rate", "min_amount", "max_amount", "savings_multiple"},
	"loans":        {"amount", "interest_rate", "fixed_interest", "fixed_interest_rate"},
	"loan_charges": {"principal", "interest", "amount"}, "loan_repayments": {"amount"}, "loan_guarantors": {"amount"},
}

var ErrNotEmpty = errors.New("target database already has data; pass -replace to wipe and reload it")

// Load writes the plan in one transaction. With replace it first empties
// every slice table (including refresh tokens and login attempts);
// otherwise it refuses to run against a database that has users.
func Load(ctx context.Context, pool *pgxpool.Pool, p *Plan, replace bool) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	types, err := conn.Conn().LoadTypes(ctx, enumTypes)
	if err != nil {
		return fmt.Errorf("load enum types: %w", err)
	}
	conn.Conn().TypeMap().RegisterTypes(types)

	return pgx.BeginFunc(ctx, conn.Conn(), func(tx pgx.Tx) error {
		if replace {
			_, err := tx.Exec(ctx, `TRUNCATE job_runs, loan_guarantors, loan_repayments, loan_charges, loans, loan_types, withdrawal_approvals, withdrawal_requests, ledger_entries, account_entries, account_transfers, withdrawals,
				deposits, depositors, fine_categories, expense_categories, income_categories, group_accounts,
				mobile_money_providers, sacco_branches, saccos, contribution_fine_rules, contribution_members,
				contribution_schedules, contributions, memberships, group_roles, groups,
				refresh_tokens, login_attempts, user_platform_roles, admin_audit_log, platform_settings,
				mobile_access_tokens, mobile_pins,
				bank_branches, banks, counties, countries, users RESTART IDENTITY`)
			if err != nil {
				return fmt.Errorf("truncate: %w", err)
			}
		} else {
			var n int64
			if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM users) + (SELECT count(*) FROM groups)`).Scan(&n); err != nil {
				return err
			}
			if n > 0 {
				return ErrNotEmpty
			}
		}
		for _, t := range targets {
			rows := t.rows(p)
			for _, c := range numericColumns[t.name] {
				if err := numericColumn(rows, t.cols, c); err != nil {
					return fmt.Errorf("%s: %w", t.name, err)
				}
			}
			n, err := tx.CopyFrom(ctx, pgx.Identifier{t.name}, t.cols, pgx.CopyFromRows(rows))
			if err != nil {
				return fmt.Errorf("copy %s: %w", t.name, err)
			}
			if int(n) != len(rows) {
				return fmt.Errorf("copy %s: wrote %d of %d rows", t.name, n, len(rows))
			}
		}
		for _, t := range identityTables {
			_, err := tx.Exec(ctx, fmt.Sprintf(
				`SELECT setval(pg_get_serial_sequence('%[1]s', 'id'), GREATEST((SELECT max(id) FROM %[1]s), 1))`, t))
			if err != nil {
				return fmt.Errorf("reset %s id sequence: %w", t, err)
			}
		}
		return nil
	})
}

// numericColumn converts string amounts to pgtype.Numeric in place.
func numericColumn(rows [][]any, cols []string, col string) error {
	idx := -1
	for i, c := range cols {
		if c == col {
			idx = i
		}
	}
	if idx < 0 {
		return fmt.Errorf("no column %s", col)
	}
	for _, r := range rows {
		var s string
		switch v := r[idx].(type) {
		case nil, pgtype.Numeric: // NULL, or already converted by an earlier Load
			continue
		case string:
			s = v
		case *string:
			if v == nil {
				r[idx] = nil
				continue
			}
			s = *v
		default:
			return fmt.Errorf("%s: unexpected %T", col, v)
		}
		var n pgtype.Numeric
		if err := n.Scan(s); err != nil {
			return fmt.Errorf("%s %q: %w", col, s, err)
		}
		r[idx] = n
	}
	return nil
}

// Reconcile compares contribution balances computed in Postgres through
// ledger_entry_rules with the legacy algorithm (Statements_m::
// get_member_contribution_balance) run over the raw statements, per member.
// It also compares against the legacy cache table
// member_contributions_fines_balances, which is informational only: it is
// an integer cache that the old app refreshes on its own schedule.
func Reconcile(ctx context.Context, pool *pgxpool.Pool, t Tables, w io.Writer) (mismatches int, err error) {
	payable := map[int64]bool{1: true, 21: true, 22: true, 23: true, 24: true, 25: true, 27: true, 30: true}
	paid := map[int64]bool{9: true, 10: true, 11: true, 15: true, 26: true}
	memberGroup := map[int64]int64{}
	for _, m := range t["members"] {
		memberGroup[m.Int("id")] = m.Int("group_id")
	}
	legacy := map[int64]float64{}
	for _, r := range t["statements"] {
		// The legacy query filters on both member_id and the member's group_id.
		if !r.Flag("active") || memberGroup[r.Int("member_id")] != r.Int("group_id") {
			continue
		}
		typ := r.Int("transaction_type")
		amt, _ := strconv.ParseFloat(strings.TrimSpace(r.Str("amount")), 64)
		switch {
		case payable[typ]:
			legacy[r.Int("member_id")] += amt
		case paid[typ]:
			legacy[r.Int("member_id")] -= amt
		}
	}

	rows, err := pool.Query(ctx, `
		SELECT e.membership_id, sum(r.sign * e.amount)::float8
		FROM ledger_entries e JOIN ledger_entry_rules r USING (entry_type)
		WHERE e.voided_at IS NULL AND r.bucket = 'contribution' AND e.membership_id IS NOT NULL
		GROUP BY e.membership_id`)
	if err != nil {
		return 0, err
	}
	ours := map[int64]float64{}
	for rows.Next() {
		var id int64
		var bal float64
		if err := rows.Scan(&id, &bal); err != nil {
			return 0, err
		}
		ours[id] = bal
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	var memberIDs []int64
	for id := range ours {
		memberIDs = append(memberIDs, id)
	}
	sort.Slice(memberIDs, func(i, j int) bool { return memberIDs[i] < memberIDs[j] })
	for _, id := range memberIDs {
		if math.Abs(ours[id]-legacy[id]) > 0.005 {
			mismatches++
			if mismatches <= 20 {
				fmt.Fprintf(w, "  member %d: legion %.2f, legacy %.2f\n", id, ours[id], legacy[id])
			}
		}
	}
	fmt.Fprintf(w, "contribution balances: %d members checked against the legacy algorithm, %d mismatches\n", len(memberIDs), mismatches)

	cacheDiff, cacheChecked := 0, 0
	for _, r := range t["member_contributions_fines_balances"] {
		id := r.Int("member_id")
		bal, ok := ours[id]
		if !ok || r["contributions_balance"] == nil {
			continue
		}
		cacheChecked++
		// The cache stores whole shillings.
		if math.Abs(bal-float64(r.Int("contributions_balance"))) >= 1 {
			cacheDiff++
		}
	}
	fmt.Fprintf(w, "legacy balance cache (informational): %d members compared, %d differ by 1 KES or more\n", cacheChecked, cacheDiff)
	if err := reconcileAccounts(ctx, pool, t, w); err != nil {
		return mismatches, err
	}
	return mismatches, reconcileLoans(ctx, pool, t, w)
}

// reconcileAccounts compares each account's balance with the legacy
// running total (current_balance, which the old app adds to the initial
// balance). Informational: the legacy total is an integer kept up to date
// by the old app's own jobs, and an anonymized dump cannot attribute most
// entries to an account.
func reconcileAccounts(ctx context.Context, pool *pgxpool.Pool, t Tables, w io.Writer) error {
	legacy := map[accountKey]float64{}
	for table, kind := range map[string]string{"bank_accounts": "bank", "sacco_accounts": "sacco",
		"mobile_money_accounts": "mobile_money", "petty_cash_accounts": "petty_cash"} {
		for _, r := range t[table] {
			if r["current_balance"] != nil {
				legacy[accountKey{kind, r.Int("id")}] = float64(r.Int("current_balance"))
			}
		}
	}
	rows, err := pool.Query(ctx, `
		SELECT a.kind::text, a.legacy_id, coalesce(sum(e.direction * e.amount), 0)::float8,
		       count(*) FILTER (WHERE e.id IS NOT NULL)
		FROM group_accounts a
		LEFT JOIN account_entries e ON e.account_id = a.id AND e.voided_at IS NULL
		WHERE a.legacy_id IS NOT NULL
		GROUP BY a.id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	checked, differ := 0, 0
	for rows.Next() {
		var kind string
		var lid int64
		var ours float64
		var n int64
		if err := rows.Scan(&kind, &lid, &ours, &n); err != nil {
			return err
		}
		want, ok := legacy[accountKey{kind, lid}]
		if !ok || n == 0 {
			continue
		}
		checked++
		if math.Abs(ours-want) >= 1 {
			differ++
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	var unknown int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM account_entries WHERE account_id IS NULL`).Scan(&unknown); err != nil {
		return err
	}
	fmt.Fprintf(w, "account balances (informational): %d accounts compared with legacy current_balance, %d differ by 1 KES or more; %d cash book entries have no known account\n",
		checked, differ, unknown)
	return nil
}

// WriteReport prints the import counts and issues.
func (r Report) Write(w io.Writer) {
	fmt.Fprintln(w, "imported rows:")
	for _, t := range targets {
		fmt.Fprintf(w, "  %-26s %7d\n", t.name, r.Imported[t.name])
	}
	if len(r.Issues) == 0 {
		return
	}
	fmt.Fprintln(w, "data issues (rows):")
	keys := make([]string, 0, len(r.Issues))
	for k := range r.Issues {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(w, "  %7d  %s\n", r.Issues[k], k)
	}
}

// reconcileLoans compares each imported loan's balance with the legacy one:
// live loan_invoices less live loan_repayments (Loans_m::get_total_loan_balances).
func reconcileLoans(ctx context.Context, pool *pgxpool.Pool, t Tables, w io.Writer) error {
	legacy := map[int64]float64{}
	for _, r := range t["loan_invoices"] {
		if r.Flag("active") {
			f, _ := strconv.ParseFloat(clean(r.Str("amount_payable")), 64)
			legacy[r.Int("loan_id")] += f
		}
	}
	for _, r := range t["loan_repayments"] {
		if r.Flag("active") {
			f, _ := strconv.ParseFloat(clean(r.Str("amount")), 64)
			legacy[r.Int("loan_id")] -= f
		}
	}
	rows, err := pool.Query(ctx, `
		SELECT l.id,
		       coalesce((SELECT sum(amount) FROM loan_charges c WHERE c.loan_id = l.id AND c.voided_at IS NULL), 0)::float8
		     - coalesce((SELECT sum(amount) FROM loan_repayments r WHERE r.loan_id = l.id AND r.voided_at IS NULL), 0)::float8
		FROM loans l WHERE l.voided_at IS NULL`)
	if err != nil {
		return err
	}
	defer rows.Close()
	checked, differ := 0, 0
	var owed float64
	for rows.Next() {
		var id int64
		var ours float64
		if err := rows.Scan(&id, &ours); err != nil {
			return err
		}
		checked++
		if math.Abs(ours-legacy[id]) >= 0.01 {
			differ++
		}
		if ours > 0 {
			owed += ours
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	fmt.Fprintf(w, "loan balances: %d live loans compared with legacy invoices less repayments, %d differ; %.2f owed in all\n",
		checked, differ, owed)
	return nil
}
