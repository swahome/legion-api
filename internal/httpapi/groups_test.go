package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

// fixture: group "jirani" with an official (Jane), a member (Otieno), a
// suspended member and an outsider; a second group "imani"; one regular
// contribution with ledger entries for Otieno.
type groupFixture struct {
	*harness
	group, other                     int64
	official, member, suspended      int64 // membership ids
	officialTok, memberTok, outTok   string
	adminTok                         string
	contribution, hiddenContribution int64
}

func (h *harness) token(phone string) string {
	h.t.Helper()
	rec := h.login(phone, "pw")
	if rec.Code != http.StatusOK {
		h.t.Fatalf("login %s: %d %s", phone, rec.Code, rec.Body)
	}
	var s sessionResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &s)
	return s.AccessToken
}

func (h *harness) scalar(sql string, args ...any) int64 {
	h.t.Helper()
	var id int64
	if err := h.pool.QueryRow(context.Background(), sql, args...).Scan(&id); err != nil {
		h.t.Fatalf("%s: %v", sql, err)
	}
	return id
}

func newGroupFixture(t *testing.T) *groupFixture {
	h := newHarness(t)
	f := &groupFixture{harness: h}
	jane := h.createUser("+254700000001", "jane@example.com", "pw")
	otieno := h.createUser("+254700000002", "", "pw")
	sus := h.createUser("+254700000003", "", "pw")
	h.createUser("+254700000004", "", "pw") // outsider
	admin := h.createUser("+254700000005", "", "pw")
	h.exec(`INSERT INTO user_platform_roles (user_id, role) VALUES ($1, 'admin')`, admin)

	f.group = h.scalar(`INSERT INTO groups (name, slug, status) VALUES ('Jirani', 'jirani', 'active') RETURNING id`)
	f.other = h.scalar(`INSERT INTO groups (name, slug, status) VALUES ('Imani', 'imani', 'active') RETURNING id`)
	treasurer := h.scalar(`INSERT INTO group_roles (group_id, name) VALUES ($1, 'Treasurer') RETURNING id`, f.group)
	f.official = h.scalar(`INSERT INTO memberships (group_id, user_id, is_official, group_role_id) VALUES ($1, $2, true, $3) RETURNING id`, f.group, jane, treasurer)
	f.member = h.scalar(`INSERT INTO memberships (group_id, user_id, membership_number) VALUES ($1, $2, 'J-002') RETURNING id`, f.group, otieno)
	f.suspended = h.scalar(`INSERT INTO memberships (group_id, user_id, status) VALUES ($1, $2, 'suspended') RETURNING id`, f.group, sus)
	h.exec(`INSERT INTO memberships (group_id, user_id) VALUES ($1, $2)`, f.other, otieno)

	f.contribution = h.scalar(`INSERT INTO contributions (group_id, name, kind, amount) VALUES ($1, 'Monthly savings', 'regular', 1000) RETURNING id`, f.group)
	h.exec(`INSERT INTO contribution_schedules (contribution_id, frequency, contribution_date) VALUES ($1, 'monthly', '2026-01-05')`, f.contribution)
	f.hiddenContribution = h.scalar(`INSERT INTO contributions (group_id, name, kind, amount, is_hidden, hidden_in_statement)
		VALUES ($1, 'Officials fund', 'non_scheduled', 0, true, true) RETURNING id`, f.group)

	// Otieno: invoiced 1000 in Jan, Feb and Mar; paid 1500 and 700; one
	// voided payment; one entry in the hidden contribution.
	for _, e := range []struct {
		typ, date, amt string
		voided         bool
		contrib        int64
	}{
		{"contribution_invoice", "2026-01-05", "1000", false, f.contribution},
		{"contribution_payment", "2026-01-20", "1500", false, f.contribution},
		{"contribution_invoice", "2026-02-05", "1000", false, f.contribution},
		{"contribution_payment", "2026-02-10", "999", true, f.contribution},
		{"contribution_invoice", "2026-03-05", "1000", false, f.contribution},
		{"contribution_payment", "2026-03-15", "700.50", false, f.contribution},
		{"contribution_invoice", "2026-03-20", "300", false, f.hiddenContribution},
	} {
		h.exec(`INSERT INTO ledger_entries (group_id, membership_id, contribution_id, entry_type, amount, entry_date, voided_at)
			VALUES ($1, $2, $3, $4, $5, $6, CASE WHEN $7 THEN now() END)`,
			f.group, f.member, e.contrib, e.typ, e.amt, e.date, e.voided)
	}

	f.officialTok = h.token("0700000001")
	f.memberTok = h.token("0700000002")
	f.outTok = h.token("0700000004")
	f.adminTok = h.token("0700000005")
	return f
}

func (f *groupFixture) get(tok, path string) *httpResult {
	return f.call("GET", tok, path, nil)
}

type httpResult struct {
	code int
	body map[string]any
	raw  string
}

func (f *groupFixture) call(method, tok, path string, body any) *httpResult {
	f.t.Helper()
	rec := f.do(method, fmt.Sprintf("/v1/groups/%d%s", f.group, path), body, withBearer(tok))
	res := &httpResult{code: rec.Code, raw: rec.Body.String()}
	_ = json.Unmarshal(rec.Body.Bytes(), &res.body)
	return res
}

func (r *httpResult) list(key string) []map[string]any {
	var out []map[string]any
	for _, v := range r.body[key].([]any) {
		out = append(out, v.(map[string]any))
	}
	return out
}

func expect(t *testing.T, r *httpResult, code int) {
	t.Helper()
	if r.code != code {
		t.Fatalf("status %d, want %d: %s", r.code, code, r.raw)
	}
}

func TestGroupAccess(t *testing.T) {
	f := newGroupFixture(t)

	r := f.get(f.memberTok, "/")
	expect(t, r, 200)
	if r.body["is_official"] != false || r.body["email"] != nil {
		t.Errorf("member view: %s", r.raw)
	}
	if s := r.body["summary"].(map[string]any); s["active_members"] != float64(2) || s["total_contributions_paid"] != "2200.50" {
		t.Errorf("summary: %v", s)
	}

	expect(t, f.get(f.officialTok, "/"), 200)
	expect(t, f.get(f.outTok, "/"), 404)

	r = f.get(f.adminTok, "/")
	expect(t, r, 200)
	if r.body["platform_admin"] != true || r.body["is_official"] != true || r.body["my_membership_id"] != nil {
		t.Errorf("admin view: %s", r.raw)
	}

	sus := f.token("0700000003")
	expect(t, f.get(sus, "/"), 404)

	rec := f.do("GET", "/v1/groups/999999/", nil, withBearer(f.officialTok))
	if rec.Code != 404 {
		t.Errorf("missing group: %d", rec.Code)
	}
	if rec := f.do("GET", fmt.Sprintf("/v1/groups/%d/", f.group), nil); rec.Code != 401 {
		t.Errorf("no token: %d", rec.Code)
	}
}

func TestMemberDirectory(t *testing.T) {
	f := newGroupFixture(t)

	r := f.get(f.memberTok, "/members")
	expect(t, r, 200)
	members := r.list("members")
	if len(members) != 2 {
		t.Fatalf("members = %d: %s", len(members), r.raw)
	}
	// Officials first.
	if members[0]["is_official"] != true || members[0]["role_name"] != "Treasurer" {
		t.Errorf("first = %v", members[0])
	}
	if _, ok := members[0]["phone"]; ok {
		t.Error("member can see an official's phone")
	}
	if members[1]["phone"] != "+254700000002" {
		t.Error("member cannot see own phone")
	}

	r = f.get(f.officialTok, "/members?status=all")
	expect(t, r, 200)
	if n := len(r.list("members")); n != 3 {
		t.Errorf("official all = %d", n)
	}
	if f.get(f.officialTok, "/members?q=J-002").list("members")[0]["id"] != float64(f.member) {
		t.Error("search by membership number")
	}
	expect(t, f.get(f.memberTok, "/members?status=suspended"), 403)
	expect(t, f.get(f.memberTok, "/members?status=bogus"), 400)

	f.exec(`UPDATE groups SET settings = '{"disable_member_directory":"1"}' WHERE id = $1`, f.group)
	expect(t, f.get(f.memberTok, "/members"), 403)
	expect(t, f.get(f.officialTok, "/members"), 200)

	// Member detail: self or official only.
	expect(t, f.get(f.memberTok, fmt.Sprintf("/members/%d", f.member)), 200)
	expect(t, f.get(f.memberTok, fmt.Sprintf("/members/%d", f.official)), 404)
	expect(t, f.get(f.officialTok, fmt.Sprintf("/members/%d", f.member)), 200)
}

func TestAddAndUpdateMembers(t *testing.T) {
	f := newGroupFixture(t)

	expect(t, f.call("POST", f.memberTok, "/members", map[string]any{"first_name": "X", "phone": "0711111111"}), 403)
	expect(t, f.call("POST", f.officialTok, "/members", map[string]any{"first_name": "X"}), 400)
	expect(t, f.call("POST", f.officialTok, "/members", map[string]any{"first_name": "X", "phone": "abc"}), 400)

	// A brand new person: user created, must reset password before login.
	r := f.call("POST", f.officialTok, "/members", map[string]any{
		"first_name": "Akinyi", "last_name": "Odhiambo", "phone": "0711 111 111", "membership_number": "J-010",
	})
	expect(t, r, 201)
	if r.body["phone"] != "+254711111111" || r.body["membership_number"] != "J-010" || r.body["status"] != "active" {
		t.Errorf("created: %s", r.raw)
	}
	if rec := f.login("0711111111", "anything"); rec.Code != 403 || errorCode(t, rec) != "password_reset_required" {
		t.Errorf("new user login: %d %s", rec.Code, rec.Body)
	}
	expect(t, f.call("POST", f.officialTok, "/members", map[string]any{"first_name": "Akinyi", "phone": "+254711111111"}), 409)

	// The outsider already has an account: it is reused, not duplicated.
	r = f.call("POST", f.officialTok, "/members", map[string]any{"first_name": "Ignored", "phone": "0700000004"})
	expect(t, r, 201)
	if r.body["first_name"] != "Jane" {
		t.Errorf("existing user not reused: %s", r.raw)
	}
	expect(t, f.get(f.outTok, "/"), 200)

	// Someone else's email with a new phone is refused.
	expect(t, f.call("POST", f.officialTok, "/members", map[string]any{"first_name": "Y", "phone": "0722222222", "email": "JANE@example.com"}), 409)

	// Re-adding a suspended member reactivates them.
	r = f.call("POST", f.officialTok, "/members", map[string]any{"first_name": "S", "phone": "0700000003"})
	expect(t, r, 201)
	if r.body["id"] != float64(f.suspended) {
		t.Errorf("reactivation created a new membership: %s", r.raw)
	}

	// A role from another group is rejected.
	otherRole := f.scalar(`INSERT INTO group_roles (group_id, name) VALUES ($1, 'Chair') RETURNING id`, f.other)
	expect(t, f.call("PATCH", f.officialTok, fmt.Sprintf("/members/%d", f.member), map[string]any{"role_id": otherRole}), 400)

	// The last official cannot step down...
	expect(t, f.call("PATCH", f.officialTok, fmt.Sprintf("/members/%d", f.official), map[string]any{"is_official": false}), 409)
	// ...until someone else is promoted.
	expect(t, f.call("PATCH", f.officialTok, fmt.Sprintf("/members/%d", f.member), map[string]any{"is_official": true}), 200)
	r = f.call("PATCH", f.officialTok, fmt.Sprintf("/members/%d", f.official), map[string]any{"is_official": false, "role_id": nil})
	expect(t, r, 200)
	if r.body["is_official"] != false || r.body["role_id"] != nil {
		t.Errorf("demote and clear role: %s", r.raw)
	}
	// Jane is no longer an official.
	expect(t, f.call("PATCH", f.officialTok, fmt.Sprintf("/members/%d", f.member), map[string]any{"status": "removed"}), 403)
	expect(t, f.call("PATCH", f.memberTok, fmt.Sprintf("/members/%d", f.official), map[string]any{"status": "bogus"}), 400)
	expect(t, f.call("PATCH", f.memberTok, "/members/999999", map[string]any{"status": "removed"}), 404)
}

func TestContributions(t *testing.T) {
	f := newGroupFixture(t)

	if n := len(f.get(f.memberTok, "/contributions").list("contributions")); n != 1 {
		t.Errorf("member sees %d contributions, want 1 (hidden excluded)", n)
	}
	if n := len(f.get(f.officialTok, "/contributions").list("contributions")); n != 2 {
		t.Errorf("official sees %d contributions", n)
	}
	expect(t, f.get(f.memberTok, fmt.Sprintf("/contributions/%d", f.hiddenContribution)), 404)

	expect(t, f.call("POST", f.memberTok, "/contributions", map[string]any{}), 403)
	expect(t, f.call("POST", f.officialTok, "/contributions", map[string]any{"name": "X", "kind": "regular", "amount": 100}), 400)
	expect(t, f.call("POST", f.officialTok, "/contributions", map[string]any{"name": "X", "kind": "weird", "amount": 100}), 400)
	expect(t, f.call("POST", f.officialTok, "/contributions", map[string]any{"name": "X", "kind": "one_time", "amount": "-5"}), 400)
	expect(t, f.call("POST", f.officialTok, "/contributions", map[string]any{"name": "X", "kind": "one_time", "amount": 1.005}), 400)

	otherMember := f.scalar(`SELECT id FROM memberships WHERE group_id = $1`, f.other)
	r := f.call("POST", f.officialTok, "/contributions", map[string]any{
		"name": " Welfare ", "kind": "regular", "category": "welfare", "amount": "250.5",
		"applies_to_all_members": false, "member_ids": []int64{f.member, otherMember},
		"schedule": map[string]any{"frequency": "weekly", "contribution_date": "2026-11-06", "invoice_date": "2026-11-03", "fines_enabled": true},
	})
	expect(t, r, 201)
	if r.body["name"] != "Welfare" || r.body["amount"] != "250.50" || r.body["category"] != "welfare" {
		t.Errorf("created: %s", r.raw)
	}
	ids := r.body["member_ids"].([]any)
	if len(ids) != 1 || ids[0] != float64(f.member) {
		t.Errorf("member_ids should drop other-group ids: %v", ids)
	}
	sch := r.body["schedule"].(map[string]any)
	if sch["frequency"] != "weekly" || sch["contribution_date"] != "2026-11-06" || sch["fines_enabled"] != true {
		t.Errorf("schedule: %v", sch)
	}
	id := int64(r.body["id"].(float64))

	expect(t, f.call("PATCH", f.officialTok, fmt.Sprintf("/contributions/%d", id), map[string]any{"kind": "one_time"}), 400)
	expect(t, f.call("PATCH", f.officialTok, fmt.Sprintf("/contributions/%d", id), map[string]any{"name": "  "}), 400)
	r = f.call("PATCH", f.officialTok, fmt.Sprintf("/contributions/%d", id), map[string]any{
		"amount": 300, "is_active": false, "member_ids": []int64{},
		"schedule": map[string]any{"frequency": "monthly", "contribution_date": "2026-12-01"},
	})
	expect(t, r, 200)
	if r.body["amount"] != "300.00" || r.body["is_active"] != false || r.body["name"] != "Welfare" {
		t.Errorf("updated: %s", r.raw)
	}
	if r.body["schedule"].(map[string]any)["frequency"] != "monthly" {
		t.Errorf("schedule not replaced: %s", r.raw)
	}
	if _, ok := r.body["member_ids"]; ok {
		t.Errorf("member list should be empty: %s", r.raw)
	}
	expect(t, f.call("PATCH", f.officialTok, "/contributions/999999", map[string]any{"amount": 1}), 404)
}

func TestMemberStatement(t *testing.T) {
	f := newGroupFixture(t)
	path := fmt.Sprintf("/members/%d/statement", f.member)

	r := f.get(f.memberTok, path)
	expect(t, r, 200)
	entries := r.list("entries")
	// 3 invoices and 2 payments. The voided payment and the
	// hidden-in-statement contribution are left out for members.
	if len(entries) != 5 {
		t.Fatalf("entries = %d: %s", len(entries), r.raw)
	}
	wantBal := []string{"1000.00", "-500.00", "500.00", "1500.00", "799.50"}
	for i, e := range entries {
		if e["balance"] != wantBal[i] {
			t.Errorf("entry %d balance %v, want %s", i, e["balance"], wantBal[i])
		}
	}
	if entries[1]["credit"] != "1500.00" || entries[1]["debit"] != nil || entries[0]["label"] != "Contribution invoice" {
		t.Errorf("entry shape: %v / %v", entries[0], entries[1])
	}
	if r.body["closing_balance"] != "799.50" || r.body["opening_balance"] != "0.00" {
		t.Errorf("balances: %s", r.raw)
	}

	// Date window: opening balance carries everything before it.
	r = f.get(f.memberTok, path+"?from=2026-02-01&to=2026-02-28")
	expect(t, r, 200)
	if r.body["opening_balance"] != "-500.00" || r.body["closing_balance"] != "500.00" || len(r.list("entries")) != 1 {
		t.Errorf("windowed: %s", r.raw)
	}
	expect(t, f.get(f.memberTok, path+"?from=2026-03-01&to=2026-02-01"), 400)
	expect(t, f.get(f.memberTok, path+"?from=yesterday"), 400)

	// Officials also see the hidden contribution.
	r = f.get(f.officialTok, path)
	expect(t, r, 200)
	if len(r.list("entries")) != 6 || r.body["closing_balance"] != "1099.50" {
		t.Errorf("official view: %s", r.raw)
	}

	// Members cannot read each other's statements.
	expect(t, f.get(f.memberTok, fmt.Sprintf("/members/%d/statement", f.official)), 404)

	// Balances report, officials only, most in arrears first.
	expect(t, f.get(f.memberTok, "/contribution-balances"), 403)
	r = f.get(f.officialTok, "/contribution-balances")
	expect(t, r, 200)
	bal := r.list("balances")
	if len(bal) != 2 || bal[0]["membership_id"] != float64(f.member) || bal[0]["balance"] != "1099.50" || bal[0]["paid"] != "2200.50" {
		t.Errorf("balances: %s", r.raw)
	}
	if bal[1]["balance"] != "0.00" {
		t.Errorf("official balance: %v", bal[1])
	}
}
