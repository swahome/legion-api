package httpapi

import (
	"fmt"
	"testing"
	"time"
)

// moneyFixture adds a bank with a branch, a sacco, a fine category and an
// income category to the group fixture.
type moneyFixture struct {
	*groupFixture
	bank, branch, otherBranch, sacco int64
	fineCat, incomeCat, otherFineCat int64
}

func newMoneyFixture(t *testing.T) *moneyFixture {
	f := &moneyFixture{groupFixture: newGroupFixture(t)}
	f.bank = f.scalar(`INSERT INTO banks (name, slug) VALUES ('Equity Bank', 'equity') RETURNING id`)
	f.branch = f.scalar(`INSERT INTO bank_branches (bank_id, name) VALUES ($1, 'Thika') RETURNING id`, f.bank)
	other := f.scalar(`INSERT INTO banks (name, slug) VALUES ('KCB', 'kcb') RETURNING id`)
	f.otherBranch = f.scalar(`INSERT INTO bank_branches (bank_id, name) VALUES ($1, 'Moi Avenue') RETURNING id`, other)
	f.sacco = f.scalar(`INSERT INTO saccos (name, slug) VALUES ('Stima Sacco', 'stima') RETURNING id`)
	f.fineCat = f.scalar(`INSERT INTO fine_categories (group_id, name, amount) VALUES ($1, 'Lateness', 200) RETURNING id`, f.group)
	f.otherFineCat = f.scalar(`INSERT INTO fine_categories (group_id, name) VALUES ($1, 'Elsewhere') RETURNING id`, f.other)
	f.incomeCat = f.scalar(`INSERT INTO income_categories (group_id, name) VALUES ($1, 'Interest') RETURNING id`, f.group)
	return f
}

func (f *moneyFixture) post(path string, body any) *httpResult {
	return f.call("POST", f.officialTok, path, body)
}

func (f *moneyFixture) account(body map[string]any) int64 {
	f.t.Helper()
	r := f.post("/accounts", body)
	expect(f.t, r, 201)
	return int64(r.body["id"].(float64))
}

func accountBalances(r *httpResult) map[string]string {
	out := map[string]string{}
	for _, a := range r.list("accounts") {
		out[a["name"].(string)] = a["balance"].(string)
	}
	return out
}

func TestAccounts(t *testing.T) {
	f := newMoneyFixture(t)

	expect(t, f.call("POST", f.memberTok, "/accounts", map[string]any{"kind": "petty_cash", "name": "Box"}), 403)
	expect(t, f.get(f.memberTok, "/accounts"), 403)

	for _, bad := range []map[string]any{
		{"kind": "vault", "name": "X"},
		{"kind": "bank", "name": " "},
		{"kind": "bank", "name": "No bank"},
		{"kind": "bank", "name": "Wrong branch", "institution_id": f.bank, "branch_id": f.otherBranch},
		{"kind": "sacco", "name": "Bank as sacco", "institution_id": 999999},
		{"kind": "petty_cash", "name": "Box", "institution_id": f.bank},
		{"kind": "bank", "name": "Neg", "institution_id": f.bank, "initial_balance": "-5"},
	} {
		if r := f.post("/accounts", bad); r.code != 400 {
			t.Errorf("%v: status %d, want 400: %s", bad, r.code, r.raw)
		}
	}

	equity := f.account(map[string]any{"kind": "bank", "name": "Equity main", "account_number": "0123456789",
		"institution_id": f.bank, "branch_id": f.branch, "initial_balance": "1000", "is_default": true})
	second := f.account(map[string]any{"kind": "bank", "name": "Equity two", "institution_id": f.bank, "is_default": true})
	f.account(map[string]any{"kind": "sacco", "name": "Stima", "institution_id": f.sacco})
	box := f.account(map[string]any{"kind": "petty_cash", "name": "Cash box"})

	r := f.get(f.officialTok, "/accounts")
	expect(t, r, 200)
	accts := r.list("accounts")
	if len(accts) != 4 || r.body["total_balance"] != "1000.00" {
		t.Fatalf("accounts: %s", r.raw)
	}
	byID := map[int64]map[string]any{}
	for _, a := range accts {
		byID[int64(a["id"].(float64))] = a
	}
	if a := byID[equity]; a["is_default"] != false || a["balance"] != "1000.00" ||
		a["institution"].(map[string]any)["name"] != "Equity Bank" || a["branch"].(map[string]any)["name"] != "Thika" {
		t.Errorf("equity (default should have moved to the second account): %v", a)
	}
	if byID[second]["is_default"] != true {
		t.Errorf("second account should be default: %v", byID[second])
	}

	// Edit, close and delete.
	r = f.call("PATCH", f.officialTok, fmt.Sprintf("/accounts/%d", equity), map[string]any{"name": "Equity", "branch_id": nil})
	expect(t, r, 200)
	if r.body["name"] != "Equity" || r.body["branch"] != nil {
		t.Errorf("patch: %s", r.raw)
	}
	expect(t, f.call("PATCH", f.officialTok, fmt.Sprintf("/accounts/%d", equity), map[string]any{"branch_id": f.otherBranch}), 400)
	expect(t, f.call("PATCH", f.officialTok, fmt.Sprintf("/accounts/%d", equity), map[string]any{"kind": "sacco"}), 400)
	r = f.call("PATCH", f.officialTok, fmt.Sprintf("/accounts/%d", second), map[string]any{"is_closed": true})
	expect(t, r, 200)
	if r.body["is_closed"] != true || r.body["is_default"] != false {
		t.Errorf("close: %s", r.raw)
	}
	expect(t, f.call("DELETE", f.officialTok, fmt.Sprintf("/accounts/%d", box), nil), 204)
	expect(t, f.call("PATCH", f.officialTok, fmt.Sprintf("/accounts/%d", box), map[string]any{"name": "Gone"}), 404)

	// Another group's account is invisible.
	otherAcct := f.scalar(`INSERT INTO group_accounts (group_id, kind, name) VALUES ($1, 'petty_cash', 'Theirs') RETURNING id`, f.other)
	expect(t, f.call("PATCH", f.officialTok, fmt.Sprintf("/accounts/%d", otherAcct), map[string]any{"name": "Mine"}), 404)
	expect(t, f.get(f.officialTok, fmt.Sprintf("/cash-book?account_id=%d", otherAcct)), 404)
}

func TestDeposits(t *testing.T) {
	f := newMoneyFixture(t)
	bank := f.account(map[string]any{"kind": "bank", "name": "Equity", "institution_id": f.bank, "initial_balance": "1000"})
	box := f.account(map[string]any{"kind": "petty_cash", "name": "Cash box"})
	closed := f.account(map[string]any{"kind": "petty_cash", "name": "Old box"})
	expect(t, f.call("PATCH", f.officialTok, fmt.Sprintf("/accounts/%d", closed), map[string]any{"is_closed": true}), 200)
	depositor := f.post("/depositors", map[string]any{"name": "Bank of Africa"})
	expect(t, depositor, 201)

	tomorrow := time.Now().AddDate(0, 0, 2).Format(dateLayout)
	otherMember := f.scalar(`SELECT id FROM memberships WHERE group_id = $1`, f.other)
	for name, bad := range map[string]map[string]any{
		"loan repayments come later": {"kind": "loan_repayment", "account_id": bank, "amount": "10", "date": "2026-04-01", "member_id": f.member},
		"zero amount":                {"kind": "misc", "account_id": bank, "amount": "0", "date": "2026-04-01", "member_id": f.member},
		"future date":                {"kind": "misc", "account_id": bank, "amount": "10", "date": tomorrow, "member_id": f.member},
		"no member":                  {"kind": "contribution", "account_id": bank, "amount": "10", "date": "2026-04-01", "contribution_id": f.contribution},
		"no contribution":            {"kind": "contribution", "account_id": bank, "amount": "10", "date": "2026-04-01", "member_id": f.member},
		"member of another group":    {"kind": "misc", "account_id": bank, "amount": "10", "date": "2026-04-01", "member_id": otherMember},
		"closed account":             {"kind": "misc", "account_id": closed, "amount": "10", "date": "2026-04-01", "member_id": f.member},
		"unknown account":            {"kind": "misc", "account_id": 999999, "amount": "10", "date": "2026-04-01", "member_id": f.member},
		"fine needs a category":      {"kind": "fine", "account_id": bank, "amount": "10", "date": "2026-04-01", "member_id": f.member},
		"fine category elsewhere":    {"kind": "fine", "account_id": bank, "amount": "10", "date": "2026-04-01", "member_id": f.member, "fine_category_id": f.otherFineCat},
		"income has no member":       {"kind": "income", "account_id": bank, "amount": "10", "date": "2026-04-01", "member_id": f.member, "income_category_id": f.incomeCat},
		"bad method":                 {"kind": "misc", "account_id": bank, "amount": "10", "date": "2026-04-01", "member_id": f.member, "method": "barter"},
	} {
		if r := f.post("/deposits", bad); r.code != 400 {
			t.Errorf("%s: status %d, want 400: %s", name, r.code, r.raw)
		}
	}
	expect(t, f.call("POST", f.memberTok, "/deposits", map[string]any{}), 403)

	// Otieno owes 1099.50 on the fixture's contribution; pay 500 into the bank.
	r := f.post("/deposits", map[string]any{"kind": "contribution", "account_id": bank, "method": "mpesa",
		"amount": "500", "date": "2026-04-02", "member_id": f.member, "contribution_id": f.contribution,
		"description": " April savings "})
	expect(t, r, 201)
	contribDeposit := int64(r.body["id"].(float64))
	if r.body["description"] != "April savings" || r.body["account"].(map[string]any)["name"] != "Equity" ||
		r.body["member"].(map[string]any)["id"] != float64(f.member) || len(r.list("ledger_entries")) != 1 {
		t.Errorf("deposit: %s", r.raw)
	}
	expect(t, f.post("/deposits", map[string]any{"kind": "fine", "account_id": box, "method": "cash",
		"amount": "200", "date": "2026-04-03", "member_id": f.member, "fine_category_id": f.fineCat}), 201)
	expect(t, f.post("/deposits", map[string]any{"kind": "income", "account_id": bank, "amount": "75.25",
		"date": "2026-04-04", "income_category_id": f.incomeCat, "depositor_id": depositor.body["id"]}), 201)

	balance := func() string {
		r := f.get(f.officialTok, "/contribution-balances")
		for _, b := range r.list("balances") {
			if b["membership_id"] == float64(f.member) {
				return b["balance"].(string)
			}
		}
		return ""
	}
	if got := balance(); got != "599.50" {
		t.Errorf("member contribution balance %s, want 599.50", got)
	}
	r = f.get(f.officialTok, "/accounts")
	if b := accountBalances(r); b["Equity"] != "1575.25" || b["Cash box"] != "200.00" || r.body["total_balance"] != "1775.25" {
		t.Errorf("balances: %s", r.raw)
	}

	// Listing and filters.
	r = f.get(f.officialTok, "/deposits")
	expect(t, r, 200)
	page := r.body["deposits"].(map[string]any)
	if page["total"] != float64(3) || r.body["totals_by_kind"].(map[string]any)["income"] != "75.25" {
		t.Errorf("list: %s", r.raw)
	}
	r = f.get(f.officialTok, fmt.Sprintf("/deposits?kind=fine&member_id=%d", f.member))
	if items := r.body["deposits"].(map[string]any)["items"].([]any); len(items) != 1 ||
		items[0].(map[string]any)["fine_category"].(map[string]any)["name"] != "Lateness" {
		t.Errorf("fine filter: %s", r.raw)
	}
	expect(t, f.get(f.officialTok, "/deposits?kind=gift"), 400)

	// Cash book: opening balance is the initial balance; running balance per row.
	r = f.get(f.officialTok, "/cash-book?from=2026-04-01&to=2026-04-30")
	expect(t, r, 200)
	entries := r.list("entries")
	if r.body["opening_balance"] != "1000.00" || r.body["closing_balance"] != "1775.25" || len(entries) != 3 ||
		entries[0]["balance"] != "1500.00" || entries[0]["type"] != "deposit.contribution" || entries[2]["money_in"] != "75.25" {
		t.Errorf("cash book: %s", r.raw)
	}
	r = f.get(f.officialTok, fmt.Sprintf("/cash-book?from=2026-04-03&to=2026-04-30&account_id=%d", bank))
	if r.body["opening_balance"] != "1500.00" || len(r.list("entries")) != 1 || r.body["closing_balance"] != "1575.25" {
		t.Errorf("account statement: %s", r.raw)
	}
	expect(t, f.get(f.officialTok, "/cash-book?from=2026-05-01&to=2026-04-01"), 400)

	// An account with transactions cannot be deleted.
	expect(t, f.call("DELETE", f.officialTok, fmt.Sprintf("/accounts/%d", bank), nil), 409)

	// Voiding reverses the account and the member's ledger.
	r = f.post(fmt.Sprintf("/deposits/%d/void", contribDeposit), nil)
	expect(t, r, 200)
	if r.body["voided_at"] == nil || r.list("ledger_entries")[0]["voided_at"] == nil {
		t.Errorf("void: %s", r.raw)
	}
	expect(t, f.post(fmt.Sprintf("/deposits/%d/void", contribDeposit), nil), 409)
	expect(t, f.post("/deposits/999999/void", nil), 404)
	if got := balance(); got != "1099.50" {
		t.Errorf("after void, member balance %s, want 1099.50", got)
	}
	if b := accountBalances(f.get(f.officialTok, "/accounts")); b["Equity"] != "1075.25" {
		t.Errorf("after void, bank %s", b["Equity"])
	}
	if r := f.get(f.officialTok, "/deposits"); r.body["deposits"].(map[string]any)["total"] != float64(2) {
		t.Errorf("voided deposits are hidden by default: %s", r.raw)
	}
	if r := f.get(f.officialTok, "/deposits?include_voided=true"); r.body["deposits"].(map[string]any)["total"] != float64(3) {
		t.Errorf("include_voided: %s", r.raw)
	}
}

func TestCategories(t *testing.T) {
	f := newMoneyFixture(t)
	r := f.post("/fine-categories", map[string]any{"name": " Absence ", "amount": "150"})
	expect(t, r, 201)
	id := int64(r.body["id"].(float64))
	if r.body["name"] != "Absence" || r.body["amount"] != "150.00" {
		t.Errorf("create: %s", r.raw)
	}
	expect(t, f.post("/income-categories", map[string]any{"name": "Rent", "amount": "1"}), 400)
	expect(t, f.post("/income-categories", map[string]any{"name": ""}), 400)
	expect(t, f.call("POST", f.memberTok, "/fine-categories", map[string]any{"name": "X"}), 403)

	r = f.call("PATCH", f.officialTok, fmt.Sprintf("/fine-categories/%d", id), map[string]any{"is_hidden": true})
	expect(t, r, 200)
	// Members see visible categories only; officials can ask for all.
	if r := f.get(f.memberTok, "/fine-categories"); len(r.list("categories")) != 1 {
		t.Errorf("member list: %s", r.raw)
	}
	if r := f.get(f.officialTok, "/fine-categories?all=true"); len(r.list("categories")) != 2 {
		t.Errorf("official list: %s", r.raw)
	}
	expect(t, f.call("PATCH", f.officialTok, fmt.Sprintf("/fine-categories/%d", f.otherFineCat), map[string]any{"name": "Mine"}), 404)

	r = f.post("/expense-categories", map[string]any{"name": "Stationery"})
	expect(t, r, 201)
	if r.body["is_administrative"] != false {
		t.Errorf("expense: %s", r.raw)
	}
	r = f.post("/depositors", map[string]any{"name": "Chama partner", "phone": " "})
	expect(t, r, 201)
	if r.body["phone"] != nil {
		t.Errorf("blank phone should be null: %s", r.raw)
	}
}
