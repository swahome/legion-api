package legacy

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/swahome/legion-api/internal/testdb"
)

var fixedNow = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

func readFixture(t *testing.T) Tables {
	t.Helper()
	tables, err := ReadDump("testdata/mini.sql", DumpTables...)
	if err != nil {
		t.Fatal(err)
	}
	return tables
}

func TestReadDump(t *testing.T) {
	tables := readFixture(t)
	users := tables["users"]
	if len(users) != 3 {
		t.Fatalf("users = %d", len(users))
	}
	if got := users[0].Str("last_name"); got != "O'Neil" {
		t.Errorf("escaped quote: got %q", got)
	}
	if users[0]["middle_name"] != nil {
		t.Error("NULL should be nil")
	}
	if got := tables["investment_groups"][1].Str("name"); got != "Second, Group; Ltd" {
		t.Errorf("comma and semicolon inside string: got %q", got)
	}
	if got := tables["statements"][0].Str("description"); got != "Invoice; July" {
		t.Errorf("got %q", got)
	}
	if _, ok := tables["users_groups"]; !ok {
		t.Error("users_groups not read")
	}
}

func TestTransform(t *testing.T) {
	p, err := Transform(readFixture(t), readFixture(t)["users_groups"], Options{}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	r := p.Report
	want := map[string]int{
		"users": 2, "user_platform_roles": 1, "groups": 2, "group_roles": 2, "memberships": 2,
		"contributions": 2, "contribution_schedules": 2, "contribution_members": 1,
		"contribution_fine_rules": 2, "ledger_entries": 7,
	}
	for k, v := range want {
		if r.Imported[k] != v {
			t.Errorf("%s: imported %d, want %d", k, r.Imported[k], v)
		}
	}
	for _, issue := range []string{
		"users: no phone or email, skipped",
		"users: duplicate phone, cleared on later user",
		"users: duplicate email, cleared on later user",
		"users: non-bcrypt password, reset required",
		"user_platform_roles: unknown role id 99, skipped",
		"memberships: user missing, skipped",
		"memberships: duplicate user in group, kept one",
		"memberships: role missing or from another group, cleared",
		"groups: owner user missing, cleared",
		"groups: missing or duplicate slug, regenerated",
		"regular_contribution_settings: contribution missing, skipped",
		"contribution_members: member missing or in another group, skipped",
		"contribution_fine_rules: percentage fine without rate, skipped",
		"ledger_entries: unknown transaction_type 99, skipped",
		"ledger_entries: member missing or in another group, kept without member",
	} {
		if r.Issues[issue] == 0 {
			t.Errorf("missing issue %q; got %v", issue, r.Issues)
		}
	}

	jane := p.Users[0]
	if jane[6] == nil || *jane[6].(*string) != "+254712345678" {
		t.Errorf("phone = %v", jane[6])
	}
	// User 11 loses the duplicate phone but keeps its email. User 12 only
	// had a duplicate email, so it has no identity left and is skipped.
	if len(p.Users) != 2 || p.Users[1][0].(int64) != 11 {
		t.Fatalf("users = %v", p.Users)
	}
	if p.Users[1][9] != true || p.Users[1][8] != (*string)(nil) || p.Users[1][10] != false {
		t.Errorf("user 11 should need a reset and be inactive: %v", p.Users[1])
	}

	// The active duplicate membership wins over the deleted one.
	var kept []int64
	for _, m := range p.Memberships {
		kept = append(kept, m[0].(int64))
	}
	if len(kept) != 2 || kept[0] != 100 || kept[1] != 101 {
		t.Errorf("memberships kept = %v", kept)
	}
	// The contribution with a member list does not apply to everyone.
	if p.Contributions[0][14] != false || p.Contributions[1][14] != true {
		t.Errorf("applies_to_all_members = %v, %v", p.Contributions[0][14], p.Contributions[1][14])
	}
}

func TestTransformSyntheticPhones(t *testing.T) {
	p, err := Transform(readFixture(t), nil, Options{SyntheticPhones: true}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	// With synthetic phones every user has an identity, including user 12.
	if len(p.Users) != 3 {
		t.Fatalf("users = %d", len(p.Users))
	}
	if got := *p.Users[0][6].(*string); got != "+254700000010" {
		t.Errorf("phone = %s", got)
	}
}

func TestLoadAndReconcile(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	tables := readFixture(t)
	p, err := Transform(tables, tables["users_groups"], Options{}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := Load(ctx, pool, p, false); err != nil {
		t.Fatal(err)
	}
	if err := Load(ctx, pool, p, false); err != ErrNotEmpty {
		t.Fatalf("second load without replace: %v", err)
	}
	if err := Load(ctx, pool, p, true); err != nil {
		t.Fatalf("reload with replace: %v", err)
	}

	var buf bytes.Buffer
	n, err := Reconcile(ctx, pool, tables, &buf)
	if err != nil || n != 0 {
		t.Fatalf("reconcile: %d mismatches, %v\n%s", n, err, buf.String())
	}
	if !strings.Contains(buf.String(), "1 members checked") {
		t.Errorf("report: %s", buf.String())
	}

	// 4200 invoiced + 100 transferred out - 3000.50 paid - 50 transferred in.
	var bal string
	err = pool.QueryRow(ctx, `
		SELECT sum(r.sign * e.amount)::text FROM ledger_entries e
		JOIN ledger_entry_rules r USING (entry_type)
		WHERE e.membership_id = 100 AND e.voided_at IS NULL AND r.bucket = 'contribution'`).Scan(&bal)
	if err != nil || bal != "1249.50" {
		t.Fatalf("balance = %s, %v", bal, err)
	}

	// Account balances: initial balance plus live cash book entries.
	got := map[string]string{}
	rows, err := pool.Query(ctx, `
		SELECT coalesce(a.name, 'unknown ' || e.account_kind), (coalesce(a.initial_balance, 0) + sum(e.direction * e.amount))::text
		FROM account_entries e LEFT JOIN group_accounts a ON a.id = e.account_id
		WHERE e.voided_at IS NULL GROUP BY 1, a.initial_balance ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name, b string
		if err := rows.Scan(&name, &b); err != nil {
			t.Fatal(err)
		}
		got[name] = b
	}
	if got["Main"] != "600.00" || got["Projects"] != "3000.50" || got["Petty cash"] != "200.00" || got["unknown bank"] != "50.00" {
		t.Errorf("account balances: %v", got)
	}
	var ledgerDeposit, fineCat *int64
	if err := pool.QueryRow(ctx, `SELECT (SELECT deposit_id FROM ledger_entries WHERE id = 2), (SELECT fine_category_id FROM ledger_entries WHERE id = 6)`).Scan(&ledgerDeposit, &fineCat); err != nil ||
		ledgerDeposit == nil || *ledgerDeposit != 500 || fineCat == nil || *fineCat != 7 {
		t.Errorf("ledger links: %v %v %v", ledgerDeposit, fineCat, err)
	}
	if !strings.Contains(buf.String(), "account balances (informational)") {
		t.Errorf("report: %s", buf.String())
	}

	// New rows get ids after the imported ones.
	var id int64
	if err := pool.QueryRow(ctx, `INSERT INTO groups (name, slug) VALUES ('New', 'new') RETURNING id`).Scan(&id); err != nil || id <= 16 {
		t.Fatalf("new group id = %d, %v", id, err)
	}
}

func TestLegacyTypeCodes(t *testing.T) {
	for _, c := range []struct {
		typ           int64
		kind, account string
	}{
		{1, "contribution", "bank"}, {3, "contribution", "mobile_money"}, {7, "contribution", "petty_cash"},
		{4, "fine", "bank"}, {8, "fine", "petty_cash"}, {9, "misc", "bank"}, {16, "income", "petty_cash"},
		{17, "loan_repayment", "bank"}, {38, "funds_transfer", "sacco"},
		{44, "loan_processing_income", "petty_cash"}, {52, "external_lending_repayment", "petty_cash"},
	} {
		kind, acct, ok := DepositType(c.typ)
		if !ok || kind != c.kind || acct != c.account {
			t.Errorf("deposit type %d = %s/%s, want %s/%s", c.typ, kind, acct, c.kind, c.account)
		}
	}
	if _, _, ok := DepositType(53); ok {
		t.Error("deposit type 53 should be unknown")
	}
	for _, c := range []struct {
		typ           int64
		kind, account string
	}{
		{1, "expense", "bank"}, {12, "loan_disbursement", "petty_cash"}, {21, "contribution_refund", "bank"},
		{30, "funds_transfer", "sacco"}, {40, "dividend", "petty_cash"},
	} {
		kind, acct, ok := WithdrawalType(c.typ)
		if !ok || kind != c.kind || acct != c.account {
			t.Errorf("withdrawal type %d = %s/%s, want %s/%s", c.typ, kind, acct, c.kind, c.account)
		}
	}
	for _, c := range []struct {
		typ     int64
		dir     int16
		account string
	}{
		{1, 1, "bank"}, {7, 1, "petty_cash"}, {40, 1, "petty_cash"}, {41, -1, "bank"}, {64, -1, "petty_cash"},
		{73, 1, "bank"}, {77, -1, "bank"}, {82, 1, "sacco"}, {88, 1, "petty_cash"}, {89, -1, "bank"}, {92, -1, "petty_cash"},
	} {
		dir, acct, ok := EntryType(c.typ)
		if !ok || dir != c.dir || acct != c.account {
			t.Errorf("entry type %d = %d/%s, want %d/%s", c.typ, dir, acct, c.dir, c.account)
		}
	}
}

func TestTransformMoney(t *testing.T) {
	tables := readFixture(t)
	p, err := Transform(tables, tables["users_groups"], Options{}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	r := p.Report
	for k, v := range map[string]int{
		"group_accounts": 3, "fine_categories": 1, "deposits": 3, "withdrawals": 1, "account_entries": 5,
		"withdrawal_requests": 2, "withdrawal_approvals": 3,
	} {
		if r.Imported[k] != v {
			t.Errorf("%s: imported %d, want %d", k, r.Imported[k], v)
		}
	}
	for _, issue := range []string{
		"bank_accounts: group missing, skipped",
		"bank_accounts: bank_branch_id missing, cleared",
		"deposits: unknown type 99, skipped",
		"deposits: account taken as the group's only account of that kind",
		"deposits: account unknown (several accounts of that kind), kept without account",
		"withdrawal_requests: unknown withdrawal_for 9, skipped",
		"withdrawal_requests: group has no single bank account, kept without account",
		"withdrawal_approvals: duplicate for the same member, skipped",
		"withdrawal_approvals: request missing or in another group, skipped",
	} {
		if r.Issues[issue] == 0 {
			t.Errorf("missing issue %q in %v", issue, r.Issues)
		}
	}
	// Legacy account ids overlap across the four tables; new ids do not.
	if p.Accounts[0][0] != int64(1) || p.Accounts[2][0] != int64(3) || p.Accounts[2][3] != "petty_cash" || p.Accounts[2][1] != int64(1) {
		t.Errorf("account ids: %v", p.Accounts)
	}
	// "bank-2" resolves to the second bank account (new id 2).
	if id := p.Deposits[0][4].(*int64); id == nil || *id != 2 {
		t.Errorf("deposit 500 account = %v", p.Deposits[0][4])
	}
	if refs := string(p.Deposits[2][17].([]byte)); refs != `{"loan_id":31}` {
		t.Errorf("deposit refs = %s", refs)
	}
	// Request 800 was paid out by the payment API; 801 was declined by a
	// signatory and paid a member back from Monthly Savings.
	paid, declined := p.WithdrawalRequests[0], p.WithdrawalRequests[1]
	if paid[4] != "expense" || paid[5] != "paid" || string(paid[13].([]byte)) != `{"phone":"254712345678","type":"mobile"}` {
		t.Errorf("request 800 = %v", paid)
	}
	if declined[4] != "contribution_refund" || declined[5] != "declined" || *declined[11].(*int64) != 101 ||
		*declined[12].(*int64) != 44 || declined[15] != "Not now" {
		t.Errorf("request 801 = %v", declined)
	}
	if a := p.WithdrawalApprovals[2]; a[5] != "declined" || a[7] != "Not now" || *a[8].(*int64) != 11 {
		t.Errorf("approval 902 = %v", a)
	}
}

func TestAccountIDPrefixRepeated(t *testing.T) {
	tables := readFixture(t)
	p, err := Transform(tables, tables["users_groups"], Options{}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	// Withdrawal 600 is "bank-bank-1": the first bank account (new id 1).
	if id := p.Withdrawals[0][4].(*int64); id == nil || *id != 1 {
		t.Errorf("withdrawal 600 account = %v", p.Withdrawals[0][4])
	}
}
