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

	// New rows get ids after the imported ones.
	var id int64
	if err := pool.QueryRow(ctx, `INSERT INTO groups (name, slug) VALUES ('New', 'new') RETURNING id`).Scan(&id); err != nil || id <= 16 {
		t.Fatalf("new group id = %d, %v", id, err)
	}
}
