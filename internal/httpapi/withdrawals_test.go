package httpapi

import (
	"fmt"
	"testing"
)

func (f *moneyFixture) id(r *httpResult) int64 {
	f.t.Helper()
	return int64(r.body["id"].(float64))
}

func (f *moneyFixture) balances() map[string]string {
	f.t.Helper()
	r := f.get(f.officialTok, "/accounts")
	expect(f.t, r, 200)
	return accountBalances(r)
}

func TestWithdrawals(t *testing.T) {
	f := newMoneyFixture(t)
	bank := f.account(map[string]any{"kind": "bank", "name": "Equity", "institution_id": f.bank, "initial_balance": "5000"})
	expense := f.scalar(`INSERT INTO expense_categories (group_id, name) VALUES ($1, 'Rent') RETURNING id`, f.group)
	otherExpense := f.scalar(`INSERT INTO expense_categories (group_id, name) VALUES ($1, 'Rent') RETURNING id`, f.other)

	for name, bad := range map[string]map[string]any{
		"loans come later":         {"kind": "loan_disbursement", "account_id": bank, "amount": "10", "date": "2026-04-01"},
		"expense needs a category": {"kind": "expense", "account_id": bank, "amount": "10", "date": "2026-04-01"},
		"category elsewhere":       {"kind": "expense", "account_id": bank, "amount": "10", "date": "2026-04-01", "expense_category_id": otherExpense},
		"expense has no member":    {"kind": "expense", "account_id": bank, "amount": "10", "date": "2026-04-01", "expense_category_id": expense, "member_id": f.member},
		"refund needs a contrib":   {"kind": "contribution_refund", "account_id": bank, "amount": "10", "date": "2026-04-01", "member_id": f.member},
		"refund more than paid":    {"kind": "contribution_refund", "account_id": bank, "amount": "2200.51", "date": "2026-04-01", "member_id": f.member, "contribution_id": f.contribution},
		"zero":                     {"kind": "expense", "account_id": bank, "amount": "0", "date": "2026-04-01", "expense_category_id": expense},
	} {
		if r := f.post("/withdrawals", bad); r.code != 400 {
			t.Errorf("%s: status %d, want 400: %s", name, r.code, r.raw)
		}
	}
	expect(t, f.call("POST", f.memberTok, "/withdrawals", map[string]any{}), 403)

	r := f.post("/withdrawals", map[string]any{"kind": "expense", "account_id": bank, "amount": "1200", "method": "cheque",
		"date": "2026-04-03", "expense_category_id": expense, "description": "April rent"})
	expect(t, r, 201)
	if r.body["expense_category"].(map[string]any)["name"] != "Rent" || r.body["amount"] != "1200.00" {
		t.Errorf("expense: %s", r.raw)
	}
	// Otieno paid 2200.50 into Monthly savings in the fixture.
	r = f.post("/withdrawals", map[string]any{"kind": "contribution_refund", "account_id": bank, "amount": "200.50",
		"date": "2026-04-04", "member_id": f.member, "contribution_id": f.contribution})
	expect(t, r, 201)
	refund := f.id(r)
	if got := f.scalar(`SELECT count(*) FROM ledger_entries WHERE withdrawal_id = $1 AND entry_type = 'contribution_refund' AND voided_at IS NULL`, refund); got != 1 {
		t.Errorf("refund ledger rows = %d", got)
	}
	if b := f.balances()["Equity"]; b != "3599.50" {
		t.Errorf("balance after withdrawals = %s", b)
	}

	r = f.get(f.officialTok, "/withdrawals?kind=contribution_refund")
	expect(t, r, 200)
	page := r.body["withdrawals"].(map[string]any)
	if page["total"] != float64(1) || r.body["totals_by_kind"].(map[string]any)["expense"] != "1200.00" {
		t.Errorf("list: %s", r.raw)
	}
	r = f.get(f.officialTok, fmt.Sprintf("/cash-book?account_id=%d&from=2026-04-01&to=2026-04-30", bank))
	expect(t, r, 200)
	if e := r.list("entries"); len(e) != 2 || e[0]["type"] != "withdrawal.expense" || e[0]["money_out"] != "1200.00" {
		t.Errorf("cash book: %s", r.raw)
	}

	expect(t, f.post(fmt.Sprintf("/withdrawals/%d/void", refund), nil), 200)
	expect(t, f.post(fmt.Sprintf("/withdrawals/%d/void", refund), nil), 409)
	expect(t, f.post("/withdrawals/999999/void", nil), 404)
	if got := f.scalar(`SELECT count(*) FROM ledger_entries WHERE withdrawal_id = $1 AND voided_at IS NULL`, refund); got != 0 {
		t.Errorf("voided refund still has %d ledger rows", got)
	}
	if b := f.balances()["Equity"]; b != "3800.00" {
		t.Errorf("balance after void = %s", b)
	}
}

func TestTransfers(t *testing.T) {
	f := newMoneyFixture(t)
	bank := f.account(map[string]any{"kind": "bank", "name": "Equity", "institution_id": f.bank, "initial_balance": "5000"})
	box := f.account(map[string]any{"kind": "petty_cash", "name": "Cash box"})

	expect(t, f.post("/transfers", map[string]any{"from_account_id": bank, "to_account_id": bank, "amount": "10", "date": "2026-04-01"}), 400)
	expect(t, f.post("/transfers", map[string]any{"from_account_id": bank, "to_account_id": 999999, "amount": "10", "date": "2026-04-01"}), 400)
	expect(t, f.call("POST", f.memberTok, "/transfers", map[string]any{}), 403)

	r := f.post("/transfers", map[string]any{"from_account_id": bank, "to_account_id": box, "amount": "750", "date": "2026-04-01", "description": "Float"})
	expect(t, r, 201)
	tr := f.id(r)
	if r.body["from_account"].(map[string]any)["name"] != "Equity" || r.body["to_account"].(map[string]any)["name"] != "Cash box" {
		t.Errorf("transfer: %s", r.raw)
	}
	if b := f.balances(); b["Equity"] != "4250.00" || b["Cash box"] != "750.00" {
		t.Errorf("balances: %v", b)
	}
	r = f.get(f.officialTok, fmt.Sprintf("/transfers?account_id=%d", box))
	expect(t, r, 200)
	if r.body["transfers"].(map[string]any)["total"] != float64(1) {
		t.Errorf("list: %s", r.raw)
	}
	r = f.get(f.officialTok, "/cash-book?from=2026-04-01&to=2026-04-01")
	expect(t, r, 200)
	if e := r.list("entries"); len(e) != 2 || e[0]["type"] != "transfer.out" || e[1]["type"] != "transfer.in" {
		t.Errorf("cash book: %s", r.raw)
	}

	expect(t, f.post(fmt.Sprintf("/transfers/%d/void", tr), nil), 200)
	expect(t, f.post(fmt.Sprintf("/transfers/%d/void", tr), nil), 409)
	expect(t, f.post("/transfers/999999/void", nil), 404)
	if b := f.balances(); b["Equity"] != "5000.00" || b["Cash box"] != "0.00" {
		t.Errorf("balances after void: %v", b)
	}
}

func TestWithdrawalRequests(t *testing.T) {
	f := newMoneyFixture(t)
	bank := f.account(map[string]any{"kind": "bank", "name": "Equity", "institution_id": f.bank, "initial_balance": "5000"})
	box := f.account(map[string]any{"kind": "petty_cash", "name": "Cash box"})
	expense := f.scalar(`INSERT INTO expense_categories (group_id, name) VALUES ($1, 'Rent') RETURNING id`, f.group)
	mobile := map[string]any{"type": "mobile", "phone": "0722000111"}
	rent := map[string]any{"kind": "expense", "account_id": bank, "amount": "1000", "expense_category_id": expense,
		"description": "Hall rent", "recipient": mobile}

	// Jane is the only role holder so far: one signatory is not enough.
	r := f.post("/withdrawal-requests", rent)
	expect(t, r, 409)
	if r.body["error"].(map[string]any)["code"] != "not_enough_signatories" {
		t.Errorf("one signatory: %s", r.raw)
	}
	expect(t, f.get(f.memberTok, "/withdrawal-requests"), 403)
	secretary := f.scalar(`INSERT INTO group_roles (group_id, name) VALUES ($1, 'Secretary') RETURNING id`, f.group)
	f.exec(`UPDATE memberships SET group_role_id = $1 WHERE id = $2`, secretary, f.member)

	for name, bad := range map[string]map[string]any{
		"no recipient":   {"kind": "expense", "account_id": bank, "amount": "10", "expense_category_id": expense},
		"bad recipient":  {"kind": "expense", "account_id": bank, "amount": "10", "expense_category_id": expense, "recipient": map[string]any{"type": "bank"}},
		"too much":       {"kind": "expense", "account_id": bank, "amount": "5000.01", "expense_category_id": expense, "recipient": mobile},
		"loan":           {"kind": "loan_disbursement", "account_id": bank, "amount": "10", "recipient": mobile},
		"transfer to":    {"kind": "funds_transfer", "account_id": bank, "amount": "10"},
		"transfer extra": {"kind": "funds_transfer", "account_id": bank, "to_account_id": box, "amount": "10", "recipient": mobile},
	} {
		if r := f.post("/withdrawal-requests", bad); r.code != 400 {
			t.Errorf("%s: status %d, want 400: %s", name, r.code, r.raw)
		}
	}
	expect(t, f.call("POST", f.memberTok, "/withdrawal-requests", rent), 403)

	// Jane asks; her own approval counts, so Otieno's answer decides it.
	r = f.post("/withdrawal-requests", rent)
	expect(t, r, 201)
	req := f.id(r)
	if r.body["status"] != "pending" || r.body["approvals"] != float64(1) || r.body["signatories"] != float64(2) ||
		len(r.body["reference_number"].(string)) != 10 || r.body["recipient"].(map[string]any)["phone"] != "0722000111" {
		t.Errorf("created: %s", r.raw)
	}
	path := fmt.Sprintf("/withdrawal-requests/%d", req)

	r = f.get(f.memberTok, "/withdrawal-requests?awaiting=me")
	expect(t, r, 200)
	if r.body["requests"].(map[string]any)["total"] != float64(1) {
		t.Errorf("awaiting me: %s", r.raw)
	}
	r = f.get(f.officialTok, "/withdrawal-requests?awaiting=me")
	expect(t, r, 200)
	if r.body["requests"].(map[string]any)["total"] != float64(0) {
		t.Errorf("jane has already answered: %s", r.raw)
	}
	expect(t, f.get(f.outTok, "/withdrawal-requests"), 404) // not in the group

	expect(t, f.call("POST", f.officialTok, path+"/approve", map[string]any{"password": "pw"}), 409) // already answered
	expect(t, f.call("POST", f.memberTok, path+"/approve", map[string]any{}), 400)
	expect(t, f.call("POST", f.memberTok, path+"/approve", map[string]any{"password": "nope"}), 401)
	expect(t, f.post(path+"/pay", map[string]any{"date": "2026-04-05"}), 409) // not approved yet

	r = f.call("POST", f.memberTok, path+"/approve", map[string]any{"password": "pw"})
	expect(t, r, 200)
	if r.body["status"] != "approved" || r.body["my_decision"] != "approved" || len(r.list("approval_list")) != 2 {
		t.Errorf("approved: %s", r.raw)
	}
	expect(t, f.call("POST", f.memberTok, path+"/approve", map[string]any{"password": "pw"}), 409)

	expect(t, f.call("POST", f.memberTok, path+"/pay", map[string]any{"date": "2026-04-05"}), 403)
	expect(t, f.post(path+"/pay", map[string]any{}), 400)
	r = f.post(path+"/pay", map[string]any{"date": "2026-04-05", "method": "mpesa"})
	expect(t, r, 200)
	if r.body["status"] != "paid" || r.body["withdrawal_id"] == nil {
		t.Errorf("paid: %s", r.raw)
	}
	expect(t, f.post(path+"/pay", map[string]any{"date": "2026-04-05"}), 409)
	expect(t, f.post(path+"/cancel", nil), 409)
	if b := f.balances()["Equity"]; b != "4000.00" {
		t.Errorf("balance after paying = %s", b)
	}

	// A decline needs a reason and closes the request.
	r = f.post("/withdrawal-requests", rent)
	expect(t, r, 201)
	declined := fmt.Sprintf("/withdrawal-requests/%d", f.id(r))
	expect(t, f.call("POST", f.memberTok, declined+"/decline", map[string]any{"password": "pw"}), 400)
	r = f.call("POST", f.memberTok, declined+"/decline", map[string]any{"password": "pw", "reason": "Rent is paid"})
	expect(t, r, 200)
	if r.body["status"] != "declined" || r.body["decline_reason"] != "Rent is paid" {
		t.Errorf("declined: %s", r.raw)
	}

	// An approved transfer request moves money between accounts when paid;
	// a cancelled one cannot be paid.
	r = f.post("/withdrawal-requests", map[string]any{"kind": "funds_transfer", "account_id": bank, "to_account_id": box, "amount": "300"})
	expect(t, r, 201)
	transfer := fmt.Sprintf("/withdrawal-requests/%d", f.id(r))
	expect(t, f.call("POST", f.memberTok, transfer+"/approve", map[string]any{"password": "pw"}), 200)
	r = f.post(transfer+"/pay", map[string]any{"date": "2026-04-06"})
	expect(t, r, 200)
	if r.body["transfer_id"] == nil {
		t.Errorf("transfer paid: %s", r.raw)
	}
	if b := f.balances(); b["Equity"] != "3700.00" || b["Cash box"] != "300.00" {
		t.Errorf("balances after transfer: %v", b)
	}

	r = f.post("/withdrawal-requests", rent)
	expect(t, r, 201)
	cancelled := fmt.Sprintf("/withdrawal-requests/%d", f.id(r))
	r = f.post(cancelled+"/cancel", map[string]any{"reason": "Typo"})
	expect(t, r, 200)
	if r.body["status"] != "cancelled" {
		t.Errorf("cancelled: %s", r.raw)
	}
	expect(t, f.call("POST", f.memberTok, cancelled+"/approve", map[string]any{"password": "pw"}), 409)

	r = f.get(f.officialTok, "/withdrawal-requests?status=paid")
	expect(t, r, 200)
	if r.body["requests"].(map[string]any)["total"] != float64(2) {
		t.Errorf("paid list: %s", r.raw)
	}
	expect(t, f.get(f.officialTok, "/withdrawal-requests/999999"), 404)
}
