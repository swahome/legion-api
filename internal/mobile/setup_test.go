package mobile

import (
	"context"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestAccountSetup(t *testing.T) {
	f := newMoneyFixture(t)
	var bankID, branch int64
	_ = f.srv.pool.QueryRow(context.Background(), `SELECT id FROM banks WHERE slug = 'equity'`).Scan(&bankID)
	_ = f.srv.pool.QueryRow(context.Background(), `SELECT id FROM bank_branches WHERE bank_id = $1`, bankID).Scan(&branch)

	r := f.call("bank_accounts/create", map[string]any{"account_name": "Savings", "bank_id": bankID, "bank_branch_id": branch,
		"account_number": "12a45", "initial_balance": "100"})
	if v, _ := r["validation_errors"].(map[string]any); v["account_number"] == nil {
		t.Fatalf("bad account number: %v", r)
	}
	r = f.call("bank_accounts/create", map[string]any{"account_name": "Savings", "bank_id": bankID, "bank_branch_id": branch,
		"account_number": "1234567", "initial_balance": "1,000"})
	if r["status"] != float64(1) || r["message"] != "Successfully added" || r["is_partner"] != float64(1) {
		t.Fatalf("create bank: %v", r)
	}
	id := int64(r["id"].(float64))
	r = f.call("bank_accounts/edit", map[string]any{"id": id, "account_name": "Savings 2", "bank_id": bankID,
		"bank_branch_id": branch, "account_number": "7654321"})
	wantStatus(t, "edit", r, float64(1))
	if n := f.count(`SELECT count(*) FROM group_accounts WHERE id = $1 AND name = 'Savings 2' AND account_number = '7654321'`, id); n != 1 {
		t.Fatal("edit not saved")
	}
	if r := f.call("bank_accounts/close", map[string]any{"id": id}); r["message"] != "Bank Account successfully closed" {
		t.Fatalf("close: %v", r)
	}
	if r := f.call("bank_accounts/close", map[string]any{"id": id}); r["message"] != "Sorry, the bank account is already closed" {
		t.Fatalf("close twice: %v", r)
	}
	wantStatus(t, "activate", f.call("bank_accounts/activate", map[string]any{"id": id}), float64(1))
	if r := f.call("bank_accounts/delete", map[string]any{"id": id, "password": "nope"}); r["message"] != "You entered the wrong password." {
		t.Fatalf("wrong password: %v", r)
	}
	wantStatus(t, "delete", f.call("bank_accounts/delete", map[string]any{"id": id, "password": "secret-pw"}), float64(1))
	// The fixture's bank account has money through it.
	if r := f.call("bank_accounts/delete", map[string]any{"id": f.bank, "password": "secret-pw"}); r["status"] != float64(0) {
		t.Fatalf("delete used: %v", r)
	}
	// A petty cash id is not a bank account.
	if r := f.call("bank_accounts/close", map[string]any{"id": f.petty}); r["status"] != float64(0) {
		t.Fatalf("wrong kind: %v", r)
	}

	r = f.call("petty_cash_accounts/create", map[string]any{"account_name": "Float"})
	if r["status"] != float64(1) || r["name"] != "Float" {
		t.Fatalf("petty: %v", r)
	}
	var sacco, provider int64
	_ = f.srv.pool.QueryRow(context.Background(), `INSERT INTO saccos (name, slug) VALUES ('Stima', 'stima') RETURNING id`).Scan(&sacco)
	_ = f.srv.pool.QueryRow(context.Background(), `INSERT INTO mobile_money_providers (name, slug) VALUES ('M-Pesa', 'mpesa') RETURNING id`).Scan(&provider)
	f.exec(`INSERT INTO sacco_branches (sacco_id, name) VALUES ($1, 'Nairobi')`, sacco)
	if s := list(t, f.call("saccos/get_sacco_options", nil), "saccos"); len(s) != 1 || s[0]["name"] != "Stima" {
		t.Fatalf("saccos: %v", s)
	}
	branches := list(t, f.call("sacco_branches/get_sacco_branches_options", map[string]any{"sacco_id": sacco}), "sacco_branches")
	if len(branches) != 1 {
		t.Fatalf("sacco branches: %v", branches)
	}
	r = f.call("sacco_accounts/create", map[string]any{"account_name": "Stima savings", "sacco_id": sacco,
		"sacco_branch_id": branches[0]["id"], "account_number": "S-1"})
	wantStatus(t, "sacco", r, float64(1))
	if p := list(t, f.call("mobile_money_providers/get_mobile_money_provider_options", nil), "mobile_money_providers"); len(p) != 1 {
		t.Fatalf("providers: %v", p)
	}
	r = f.call("mobile_money_accounts/create", map[string]any{"account_name": "Till", "mobile_money_provider_id": provider, "account_number": "555"})
	wantStatus(t, "mobile money", r, float64(1))
}

func TestWithdrawalRequestsFromApp(t *testing.T) {
	f := newMoneyFixture(t)
	ctx := context.Background()
	var expCat, role, otherUser int64
	_ = f.srv.pool.QueryRow(ctx, `SELECT id FROM expense_categories WHERE name = 'Rent'`).Scan(&expCat)
	_ = f.srv.pool.QueryRow(ctx, `SELECT id FROM group_roles WHERE name = 'Chairperson'`).Scan(&role)
	_ = f.srv.pool.QueryRow(ctx, `SELECT user_id FROM memberships WHERE id = $1`, f.other).Scan(&otherUser)

	ask := map[string]any{"request_id": f.reqID(), "withdrawal_for": "1", "amount": "300", "recipient": "1",
		"phone": "0722000111", "expense_category_id": expCat, "description": "Hall hire", "transfer_from": f.bank}
	if r := f.call("withdrawals/request_funds_transfer", ask); r["message"] != "Group does not have enough signatories" {
		t.Fatalf("one signatory: %v", r)
	}
	// The second member becomes a signatory and can sign in.
	hash, _ := bcrypt.GenerateFromPassword([]byte("other-pw"), 8)
	f.exec(`UPDATE memberships SET group_role_id = $1 WHERE id = $2`, role, f.other)
	f.exec(`UPDATE users SET password_hash = $1 WHERE id = $2`, string(hash), otherUser)

	ask["request_id"] = f.reqID()
	r := f.call("withdrawals/request_funds_transfer", ask)
	if r["status"] != float64(1) {
		t.Fatalf("request: %v", r)
	}
	reqID := r["request_id"]

	posts := list(t, f.call("withdrawals/withdrawal_request_list", map[string]any{"status": []any{"1"}}), "posts")
	if len(posts) != 1 || posts[0]["approved"] != float64(1) || posts[0]["pending"] != float64(1) || posts[0]["is_owner"] != float64(1) ||
		posts[0]["recipient"] != "Mobile Wallet  -- Phone Number: 254722000111" || posts[0]["withdrawal_for"] != "Expense Payment" {
		t.Fatalf("list: %v", posts)
	}
	v := f.call("withdrawals/view_withdrawal_request", map[string]any{"id": reqID})
	if v["approval_status"] != "Pending Approval" || len(v["pending_approvals"].([]any)) != 1 ||
		v["description"] != "Expense Payment Rent of  KES 300.00  : Hall hire" {
		t.Fatalf("view: %v", v)
	}

	// The other signatory approves from their phone.
	lr := f.post("login", "", map[string]any{"phone": "0700000002", "password": "other-pw"})
	token := lr.body["access_token"].(string)
	answer := func(extra map[string]any) map[string]any {
		body := map[string]any{"user_id": otherUser, "group_id": f.group, "id": reqID}
		for k, v := range extra {
			body[k] = v
		}
		return f.post("withdrawals/respond_to_withdrawal_request", token, body).body
	}
	if r := answer(nil); r["message"] != "You must approve or decline withdrawal" {
		t.Fatalf("no answer: %v", r)
	}
	if r := answer(map[string]any{"approve": 1}); r["message"] != "You have successfully approved request" {
		t.Fatalf("approve: %v", r)
	}
	if n := f.count(`SELECT count(*) FROM withdrawal_requests WHERE status = 'approved'`); n != 1 {
		t.Fatal("not approved")
	}
	if r := answer(map[string]any{"approve": 1}); r["status"] != float64(0) {
		t.Fatalf("approve twice: %v", r)
	}
	if r := f.call("withdrawals/cancel_withdrawal_request", map[string]any{"id": reqID, "reason": "x"}); r["message"] != "Request has already been approved therefore can not be canceled" {
		t.Fatalf("cancel approved: %v", r)
	}

	// A second request, cancelled by whoever asked.
	ask["request_id"], ask["amount"] = f.reqID(), "50"
	id2 := f.call("withdrawals/request_funds_transfer", ask)["request_id"]
	if r := f.call("withdrawals/cancel_withdrawal_request", map[string]any{"id": id2}); r["message"] != "Kindly give a reason why you want to cancel this request" {
		t.Fatalf("cancel without reason: %v", r)
	}
	wantStatus(t, "cancel", f.call("withdrawals/cancel_withdrawal_request", map[string]any{"id": id2, "reason": "Not needed"}), float64(1))
	declined := list(t, f.call("withdrawals/withdrawal_request_list", map[string]any{"status": []any{"3"}}), "posts")
	if len(declined) != 1 || declined[0]["status"] != "Not needed" || declined[0]["status_code"] != float64(3) {
		t.Fatalf("declined list: %v", declined)
	}
}

func TestReports(t *testing.T) {
	f := newMoneyFixture(t)
	f.exec(`UPDATE groups SET settings = settings - 'disable_arrears' WHERE id = $1`, f.group)
	b := list(t, f.call("reports/contribution_summary", nil), "balances")
	got := map[float64]map[string]any{}
	for _, x := range b {
		got[x["member_id"].(float64)] = x
	}
	// The fixture's ledger: the caller paid 1600 of 2000, the other member 300.
	if me := got[float64(f.self)]; me["paid"] != float64(1600) || me["arrears"] != float64(400) {
		t.Fatalf("contribution summary: %v", b)
	}
	fines := list(t, f.call("reports/fines_summary", nil), "balances")
	for _, x := range fines {
		if x["member_id"] == float64(f.self) && (x["paid"] != float64(50) || x["arrears"] != float64(150)) {
			t.Fatalf("fines summary: %v", x)
		}
	}
	e := f.call("reports/expenses_summary", nil)["data"].(map[string]any)
	if e["total_expenses"] != float64(700) || len(e["expenses"].([]any)) != 1 {
		t.Fatalf("expenses: %v", e)
	}
	s := f.call("reports/transaction_statement", nil)["data"].(map[string]any)
	body := s["statement_body"].([]any)
	// Opening 1000 (bank) + 2500 in + 300 in - 700 out.
	if len(body) != 3 || s["statement_footer"].(map[string]any)["balance"] != float64(3100) ||
		s["statement_header"].(map[string]any)["balance"] != float64(1000) {
		t.Fatalf("statement: %v", s)
	}
	first := body[0].(map[string]any)
	if first["description"] == "" || first["transaction_date"] == "" {
		t.Fatalf("statement line: %v", first)
	}
}
