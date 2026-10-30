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
	{"ledger_entries", LedgerCols, func(p *Plan) [][]any { return p.Ledger }},
}

// Tables with identity ids whose sequences must move past the imported ids.
var identityTables = []string{"users", "groups", "group_roles", "memberships", "contributions", "contribution_fine_rules", "ledger_entries"}

var enumTypes = []string{
	"platform_role", "group_status", "membership_status", "contribution_kind",
	"contribution_category", "contribution_frequency", "fine_kind", "ledger_entry_type",
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
			_, err := tx.Exec(ctx, `TRUNCATE ledger_entries, contribution_fine_rules, contribution_members,
				contribution_schedules, contributions, memberships, group_roles, groups,
				refresh_tokens, login_attempts, user_platform_roles, users RESTART IDENTITY`)
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
			if t.name == "contributions" || t.name == "ledger_entries" {
				if err := numericColumn(rows, t.cols, "amount"); err != nil {
					return fmt.Errorf("%s: %w", t.name, err)
				}
			}
			if t.name == "contribution_fine_rules" {
				for _, c := range []string{"fixed_amount", "percentage_rate"} {
					if err := numericColumn(rows, t.cols, c); err != nil {
						return fmt.Errorf("%s: %w", t.name, err)
					}
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
	return mismatches, nil
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
