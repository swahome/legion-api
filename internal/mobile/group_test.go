package mobile

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// groupFixture adds a second member, reference data, a scheduled
// contribution and a few ledger entries to the base fixture.
type groupFixture struct {
	*fixture
	token          string
	group, self    int64
	other, contrib int64
}

func newGroupFixture(t *testing.T) *groupFixture {
	f := newFixture(t)
	g := &groupFixture{fixture: f, token: f.login()}
	pool := f.srv.pool
	ctx := context.Background()
	scan := func(dst *int64, sql string, args ...any) {
		t.Helper()
		if err := pool.QueryRow(ctx, sql, args...).Scan(dst); err != nil {
			t.Fatal(err)
		}
	}
	scan(&g.group, `SELECT id FROM groups WHERE slug = 'umoja'`)
	scan(&g.self, `SELECT id FROM memberships WHERE group_id = $1`, g.group)
	var uid int64
	scan(&uid, `INSERT INTO users (first_name, last_name, phone, email) VALUES ('Baraka', 'Mwangi', '+254700000002', 'b@example.com') RETURNING id`)
	scan(&g.other, `INSERT INTO memberships (group_id, user_id) VALUES ($1, $2) RETURNING id`, g.group, uid)
	scan(&g.contrib, `INSERT INTO contributions (group_id, name, kind, category, amount) VALUES ($1, 'Monthly savings', 'regular', 'savings', 1000) RETURNING id`, g.group)
	f.exec(`INSERT INTO contributions (group_id, name, kind, amount, is_hidden) VALUES ($1, 'Hidden', 'non_scheduled', 0, true)`, g.group)
	next := time.Now().AddDate(0, 0, 10).Format("2006-01-02")
	f.exec(`INSERT INTO contribution_schedules (contribution_id, frequency, rule, invoice_date, contribution_date, invoicing_active)
		VALUES ($1, 'monthly', '{"month_day_monthly": "5", "week_day_monthly": "0"}', $2::date - 3, $2::date, true)`, g.contrib, next)
	f.exec(`INSERT INTO contribution_fine_rules (contribution_id, kind, fixed_amount, chargeable_on) VALUES ($1, 'fixed', 50, 'first_day_of_the_month')`, g.contrib)
	old, recent := time.Now().AddDate(-1, 0, 0), time.Now().AddDate(0, -1, 0)
	for _, e := range []struct {
		member int64
		typ    string
		amount float64
		on     time.Time
	}{
		{g.self, "contribution_invoice", 1000, old},
		{g.self, "contribution_payment", 600, old},
		{g.self, "contribution_invoice", 1000, recent},
		{g.self, "contribution_payment", 1000, recent},
		{g.self, "fine_invoice", 200, recent},
		{g.self, "fine_payment", 50, recent},
		{g.other, "contribution_payment", 300, recent},
	} {
		var cid any = g.contrib
		if strings.HasPrefix(e.typ, "fine") {
			cid = nil
		}
		f.exec(`INSERT INTO ledger_entries (group_id, membership_id, contribution_id, entry_type, amount, entry_date)
			VALUES ($1, $2, $3, $4, $5, $6)`, g.group, e.member, cid, e.typ, e.amount, e.on)
	}
	f.exec(`INSERT INTO countries (name, code, currency_code, currency_name, calling_code) VALUES ('Kenya', 'KE', 'KES', 'Kenyan Shilling', '254')`)
	var bank int64
	scan(&bank, `INSERT INTO banks (name, slug, logo) VALUES ('Equity Bank', 'equity', 'equity.png') RETURNING id`)
	f.exec(`INSERT INTO banks (name, slug, settings) VALUES ('Some Wallet', 'wallet', '{"wallet": "1"}')`)
	f.exec(`INSERT INTO bank_branches (bank_id, name) VALUES ($1, 'Westlands')`, bank)
	return g
}

func (g *groupFixture) call(path string, extra map[string]any) map[string]any {
	g.t.Helper()
	body := map[string]any{"user_id": g.user, "group_id": g.group}
	for k, v := range extra {
		body[k] = v
	}
	r := g.post(path, g.token, body)
	if r.code != 200 {
		g.t.Fatalf("%s: HTTP %d %s", path, r.code, r.raw)
	}
	return r.body
}

func list(t *testing.T, resp map[string]any, key string) []map[string]any {
	t.Helper()
	raw, ok := resp[key].([]any)
	if !ok {
		t.Fatalf("%s missing: %v", key, resp)
	}
	out := make([]map[string]any, len(raw))
	for i, v := range raw {
		out[i] = v.(map[string]any)
	}
	return out
}

func TestGroupChecks(t *testing.T) {
	g := newGroupFixture(t)
	if r := g.call("contributions/get_group_contributions", map[string]any{"group_id": 999999}); r["status"] != float64(5) {
		t.Fatalf("unknown group: %v", r)
	}
	g.exec(`UPDATE groups SET status = 'suspended' WHERE id = $1`, g.group)
	if r := g.call("members/get_group_member_options", nil); r["status"] != float64(5) {
		t.Fatalf("suspended group: %v", r)
	}
	g.exec(`UPDATE groups SET status = 'active' WHERE id = $1`, g.group)
	g.exec(`UPDATE memberships SET status = 'removed' WHERE id = $1`, g.self)
	if r := g.call("members/get_group_member_options", nil); r["status"] != float64(6) {
		t.Fatalf("removed member: %v", r)
	}
	if r := g.call("get_group_data", nil); r["status"] != float64(0) || r["message"] != "Could not find member details" {
		t.Fatalf("get_group_data uses status 0: %v", r)
	}
	g.exec(`UPDATE memberships SET status = 'suspended' WHERE id = $1`, g.self)
	if r := g.call("get_member_dashboard_data", nil); r["message"] != "Could not proceed. Account is suspended" {
		t.Fatalf("suspended member: %v", r)
	}
	if r := g.post("contributions/no_such_action", g.token, map[string]any{"user_id": g.user}); r.code != 404 || r.body["error"] == nil {
		t.Fatalf("unknown module action: %d %v", r.code, r.body)
	}
}

func TestReferenceOptions(t *testing.T) {
	g := newGroupFixture(t)
	c := list(t, g.call("countries/get_country_options", nil), "countries")
	if len(c) != 1 || c[0]["name"] != "Kenya" {
		t.Fatalf("countries: %v", c)
	}
	cur := list(t, g.call("countries/get_currency_options", nil), "currencies")
	if cur[0]["name"] != "Kenyan Shilling" {
		t.Fatalf("currencies: %v", cur)
	}
	banks := list(t, g.call("banks/get_bank_options", nil), "banks")
	if len(banks) != 1 || banks[0]["name"] != "Equity Bank" || banks[0]["logo"] != "equity.png" {
		t.Fatalf("banks (no wallets): %v", banks)
	}
	if r := g.call("bank_branches/get_bank_branches_options", map[string]any{"bank_id": "x"}); r["message"] != "Request submitted is invalid" {
		t.Fatalf("bad bank id: %v", r)
	}
	br := list(t, g.call("bank_branches/get_bank_branches_options", map[string]any{"bank_id": banks[0]["id"]}), "bank_branches")
	if len(br) != 1 || br[0]["name"] != "Westlands" {
		t.Fatalf("branches: %v", br)
	}
	roles := list(t, g.call("group_roles/group_roles_list", nil), "group_roles")
	if len(roles) != 2 || roles[0]["name"] != "Chairperson" || roles[0]["in_use"] != float64(0) || roles[1]["in_use"] != float64(1) {
		t.Fatalf("roles: %v", roles)
	}
}

func TestGroupDataIsOneGroup(t *testing.T) {
	g := newGroupFixture(t)
	r := g.call("get_group_data", nil)
	groups := list(t, r, "user_groups")
	if r["status"] != float64(1) || r["message"] != "Group data" || len(groups) != 1 || groups[0]["id"] != fmt.Sprint(g.group) {
		t.Fatalf("group data: %v", r)
	}
}

func TestMembersAndDetails(t *testing.T) {
	g := newGroupFixture(t)
	g.exec(`UPDATE groups SET settings = settings - 'disable_arrears' WHERE id = $1`, g.group)
	r := g.call("members/get_members_and_details", nil)
	members := list(t, r, "members")
	if len(members) != 2 {
		t.Fatalf("members: %v", r)
	}
	me, other := members[0], members[1]
	if me["name"] != "Akinyi Otieno" || me["role"] != "Treasurer" || me["has_role"] != float64(1) || me["selected"] != true ||
		me["is_editable"] != float64(0) || me["last_seen"] != "Online" || me["phone"] != "254712345678" {
		t.Fatalf("me: %v", me)
	}
	if me["total_contributions"] != float64(1600) || me["contribution_arrears"] != float64(400) ||
		me["total_fines"] != float64(50) || me["total_fine_arrears"] != float64(150) {
		t.Fatalf("my totals: %v", me)
	}
	if other["role"] != "Member" || other["is_editable"] != float64(1) || other["last_seen"] != "Never" || other["total_contributions"] != float64(300) {
		t.Fatalf("other: %v", other)
	}
	d := r["group_details"].(map[string]any)
	if d["total_contributions"] != float64(1900) || d["total_fines"] != float64(50) {
		t.Fatalf("group details: %v", d)
	}

	// With member privacy on, an ordinary member sees only themselves.
	g.exec(`UPDATE groups SET settings = settings || '{"enable_member_information_privacy": "1"}' WHERE id = $1`, g.group)
	g.exec(`UPDATE memberships SET is_official = false, group_role_id = NULL WHERE id = $1`, g.self)
	if r := g.call("members/get_members_and_details", map[string]any{"view_profile_member_id": g.other}); r["status"] != float64(0) {
		t.Fatalf("privacy: %v", r)
	}
	if m := list(t, g.call("members/get_members_and_details", nil), "members"); len(m) != 1 || m[0]["view_member_profile"] != float64(0) {
		t.Fatalf("privacy list: %v", m)
	}
	if m := list(t, g.call("members/get_group_member_options", nil), "members"); len(m) != 1 {
		t.Fatalf("privacy options: %v", m)
	}
}

func TestMemberOptions(t *testing.T) {
	g := newGroupFixture(t)
	g.exec(`UPDATE memberships SET status = 'suspended' WHERE id = $1`, g.other)
	m := list(t, g.call("members/get_group_member_options", nil), "members")
	if len(m) != 1 || m[0]["identity"] != "254712345678" || m[0]["suspended"] != float64(0) {
		t.Fatalf("options: %v", m)
	}
	r := g.call("members/get_members_and_details", map[string]any{"show_full_details": 0})
	if s := list(t, r, "suspended_members"); len(s) != 1 || s[0]["total_contributions"] != nil {
		t.Fatalf("suspended list: %v", r)
	}
}

func TestDashboards(t *testing.T) {
	g := newGroupFixture(t)
	r := g.call("get_member_dashboard_data", nil)
	d := r["member_details"].(map[string]any)
	// The fixture group disables arrears.
	if r["role"] != "Treasurer" || d["total_contributions"] != float64(1600) || d["contribution_arrears"] != float64(0) ||
		d["total_arrears"] != float64(0) || !strings.HasSuffix(d["contribution_date_days_left"].(string), " days") {
		t.Fatalf("member dashboard: %v", r)
	}
	summary := d["member_contribution_summary"].([]any)
	if len(summary) != 1 || summary[0].(map[string]any)["paid"] != float64(1600) {
		t.Fatalf("summary: %v", summary)
	}
	g.exec(`UPDATE groups SET settings = settings - 'disable_arrears' WHERE id = $1`, g.group)
	d = g.call("get_member_dashboard_data", nil)["member_details"].(map[string]any)
	if d["contribution_arrears"] != float64(400) || d["fine_arrears"] != float64(150) || d["total_arrears"] != float64(550) {
		t.Fatalf("arrears: %v", d)
	}
	gd := g.call("get_group_dashboard_data", nil)["group_details"].(map[string]any)
	if gd["total_contributions"] != float64(1900) || gd["total_loan_balances"] != float64(0) {
		t.Fatalf("group dashboard: %v", gd)
	}
	cs := g.call("get_group_member_contribution_summary", nil)
	if cs["total_group_contribution"] != float64(1900) || cs["member_contribution_summary"].(map[string]any)["contribution_arrears"] != float64(400) {
		t.Fatalf("contribution summary: %v", cs)
	}
}

func TestContributions(t *testing.T) {
	g := newGroupFixture(t)
	cs := list(t, g.call("contributions/get_group_contributions", nil), "contributions")
	if len(cs) != 2 {
		t.Fatalf("contributions: %v", cs)
	}
	var monthly map[string]any
	for _, c := range cs {
		if c["name"] == "Monthly savings" {
			monthly = c
		}
	}
	if monthly["type"] != "Regular Contribution" || monthly["contribution_type"] != "1" ||
		monthly["frequency"] != "Once a Month (Monthly), Every 5th Day of the Month" || !strings.Contains(monthly["contribution_date"].(string), ", ") {
		t.Fatalf("monthly: %v", monthly)
	}

	if r := g.call("contributions/get_group_contribution", nil); r["message"] != "Bad request: Missing contribution id" {
		t.Fatalf("missing id: %v", r)
	}
	r := g.call("contributions/get_group_contribution", map[string]any{"id": g.contrib})
	s := r["contribution_settings"].(map[string]any)
	if s["type"] != "1" || s["contribution_frequency"] != "1" || s["month_day_monthly"] != "5" || s["category"] != "2" || s["regular_invoicing_active"] != "1" {
		t.Fatalf("settings: %v", s)
	}
	fines := r["contribution_fine_settings"].([]any)
	if len(fines) != 1 || fines[0].(map[string]any)["fixed_amount"] != "50" {
		t.Fatalf("fines: %v", fines)
	}
	if m := r["selected_group_members"].([]any); len(m) != 2 {
		t.Fatalf("members: %v", m)
	}

	opts := list(t, g.call("contributions/get_group_contribution_options", nil), "contributions")
	if len(opts) != 1 || opts[0]["name"] != "Monthly savings" {
		t.Fatalf("options skip hidden: %v", opts)
	}
	mo := list(t, g.call("contributions/get_member_contribution_options", nil), "contributions")
	if mo[0]["balance"] != float64(400) {
		t.Fatalf("member options: %v", mo)
	}
}

func TestContributionStatement(t *testing.T) {
	g := newGroupFixture(t)
	r := g.call("statements/contribution_statement", nil)
	h := r["statement_header"].(map[string]any)
	if h["payable"] != float64(1000) || h["paid"] != float64(600) || h["balance"] != float64(400) {
		t.Fatalf("header: %v", h)
	}
	body := list(t, r, "statement_body")
	if len(body) != 2 || body[0]["description"] != "Contribution invoice Monthly savings" || body[1]["balance"] != float64(400) {
		t.Fatalf("body: %v", body)
	}
	f := r["statement_footer"].(map[string]any)
	if f["payable"] != float64(2000) || f["paid"] != float64(1600) || f["balance"] != float64(400) {
		t.Fatalf("footer: %v", f)
	}
	if m := r["member_datails"].(map[string]any); m["role"] != "Admin" || m["group"] != "Umoja" {
		t.Fatalf("member: %v", m)
	}
	// Another member's statement, then one from outside the group.
	r = g.call("statements/contribution_statement", map[string]any{"member_id": g.other})
	if b := list(t, r, "statement_body"); len(b) != 1 || b[0]["paid"] != float64(300) {
		t.Fatalf("other: %v", r)
	}
	if r := g.call("statements/contribution_statement", map[string]any{"member_id": 999999}); r["status"] != float64(0) {
		t.Fatalf("outsider: %v", r)
	}
}
