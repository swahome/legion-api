package mobile

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func (f *moneyFixture) count(sql string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.srv.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *moneyFixture) reqID() string {
	return fmt.Sprintf("%d_%d", time.Now().Unix(), time.Now().UnixNano())
}

func wantStatus(t *testing.T, what string, r map[string]any, status any) {
	t.Helper()
	if r["status"] != status {
		t.Fatalf("%s: status %v, want %v: %v", what, r["status"], status, r)
	}
}

func TestRequestIDs(t *testing.T) {
	f := newMoneyFixture(t)
	payment := []any{map[string]any{
		"member_id": f.self, "amount": "1,500", "account_id": fmt.Sprintf("bank-%d", f.bank),
		"deposit_date": time.Now().Format("02-01-2006"), "deposit_method": "1", "contribution_id": f.contrib,
	}}
	path := "deposits/record_contribution_payments"
	wantStatus(t, "no id", f.call(path, map[string]any{"contribution_payments": payment}), float64(11))
	wantStatus(t, "old id", f.call(path, map[string]any{"contribution_payments": payment, "request_id": "1600000000_abc"}), float64(13))

	id := f.reqID()
	wantStatus(t, "first", f.call(path, map[string]any{"contribution_payments": payment, "request_id": id}), float64(1))
	wantStatus(t, "repeat", f.call(path, map[string]any{"contribution_payments": payment, "request_id": id}), float64(12))
	if n := f.count(`SELECT count(*) FROM deposits WHERE group_id = $1 AND amount = 1500`, f.group); n != 1 {
		t.Fatalf("deposits: %d", n)
	}
	if n := f.count(`SELECT count(*) FROM ledger_entries WHERE deposit_id IS NOT NULL AND amount = 1500 AND entry_type = 'contribution_payment'`); n != 1 {
		t.Fatalf("ledger: %d", n)
	}

	// A failed request releases its id so the app can retry it.
	bad := f.reqID()
	r := f.call(path, map[string]any{"request_id": bad})
	wantStatus(t, "empty", r, float64(0))
	if r["message"] != "Error contribution payments array empty." {
		t.Fatalf("empty: %v", r)
	}
	wantStatus(t, "retry", f.call(path, map[string]any{"contribution_payments": payment, "request_id": bad}), float64(1))
}

func TestRecordDeposits(t *testing.T) {
	f := newMoneyFixture(t)
	today := time.Now().Format("2006-01-02")
	acct := fmt.Sprintf("bank-%d", f.bank)

	// Old style: all or nothing.
	r := f.call("deposits/record_fine_payments", map[string]any{"request_id": f.reqID(), "fine_payments": []any{
		map[string]any{"member_id": f.self, "amount": 100, "account_id": acct, "deposit_date": today, "deposit_method": 2,
			"fine_category_id": fmt.Sprintf("fine_category-%d", f.fineCat)},
		map[string]any{"member_id": 999999, "amount": 100, "account_id": acct, "deposit_date": today, "deposit_method": 2,
			"fine_category_id": f.fineCat},
	}})
	wantStatus(t, "bad member", r, float64(0))
	if n := f.count(`SELECT count(*) FROM deposits WHERE kind = 'fine'`); n != 0 {
		t.Fatalf("partial write: %d", n)
	}
	r = f.call("deposits/record_fine_payments", map[string]any{"request_id": f.reqID(), "fine_payments": []any{
		map[string]any{"member_id": f.self, "amount": 100, "account_id": acct, "deposit_date": today},
	}})
	if r["message"] != "Error data validation errors, some values are missing." {
		t.Fatalf("missing method: %v", r)
	}

	// New style, every active member at the same amount.
	r = f.call("deposits/new_record_contribution_payments", map[string]any{
		"deposit_date": time.Now().Unix(), "deposit_method": "1", "contribution_id": f.contrib, "account_id": acct,
		"amount": "250", "member_type_id": "2",
	})
	wantStatus(t, "everyone", r, float64(1))
	if n := f.count(`SELECT count(*) FROM deposits WHERE kind = 'contribution' AND amount = 250 AND method = 'mpesa'`); n != 2 {
		t.Fatalf("everyone: %d deposits", n)
	}
	// Individual amounts.
	r = f.call("deposits/new_record_contribution_payments", map[string]any{
		"deposit_date": today, "deposit_method": "2", "contribution_id": f.contrib, "account_id": acct,
		"member_type_id": "1", "individual_payments": map[string]any{fmt.Sprint(f.other): "75"},
	})
	wantStatus(t, "individual", r, float64(1))
	r = f.call("deposits/new_record_contribution_payments", map[string]any{
		"deposit_date": today, "deposit_method": "2", "contribution_id": f.contrib, "account_id": acct, "member_type_id": "1",
	})
	if v := r["validation_errors"].(map[string]any); v["member_type_id"] != "Select atleast 1 member to record individual payment" {
		t.Fatalf("no members: %v", r)
	}
	// A contribution's late fine, named "contribution-<id>".
	r = f.call("deposits/new_record_fine_payments", map[string]any{
		"deposit_date": today, "deposit_method": "1", "fine_category_id": fmt.Sprintf("contribution-%d", f.contrib),
		"account_id": acct, "member_type_id": "1", "individual_payments": map[string]any{fmt.Sprint(f.self): 30},
	})
	wantStatus(t, "contribution fine", r, float64(1))
	if n := f.count(`SELECT count(*) FROM deposits WHERE kind = 'fine' AND contribution_id = $1 AND fine_category_id IS NULL`, f.contrib); n != 1 {
		t.Fatalf("contribution fine: %d", n)
	}
	r = f.call("deposits/new_record_miscellaneous_payments", map[string]any{
		"deposit_date": today, "deposit_method": "2", "member_id": f.other, "account_id": f.petty, "amount": "40",
	})
	if r["message"] != "Miscellaneous payment successfully recorded" {
		t.Fatalf("misc: %v", r)
	}
	r = f.call("deposits/new_record_income_payment", map[string]any{
		"deposit_date": today, "deposit_method": "2", "depositor_id": 1, "income_category_id": 999999,
		"account_id": f.petty, "amount": "40",
	})
	if v, _ := r["validation_errors"].(map[string]any); v["income_category_id"] == nil && v["depositor_id"] == nil {
		t.Fatalf("bad income category: %v", r)
	}
	r = f.call("deposits/new_record_miscellaneous_payments", map[string]any{
		"deposit_date": time.Now().AddDate(0, 0, 3).Format("2006-01-02"), "deposit_method": "2", "member_id": f.other,
		"account_id": f.petty, "amount": "40",
	})
	if v, _ := r["validation_errors"].(map[string]any); v["deposit_date"] == nil {
		t.Fatalf("future date: %v", r)
	}

	// Voiding takes the payment off the member's ledger too.
	var dep int64
	_ = f.srv.pool.QueryRow(context.Background(), `SELECT id FROM deposits WHERE kind = 'misc'`).Scan(&dep)
	wantStatus(t, "void", f.call("deposits/void", map[string]any{"id": dep}), float64(1))
	if n := f.count(`SELECT count(*) FROM ledger_entries WHERE deposit_id = $1 AND voided_at IS NULL`, dep); n != 0 {
		t.Fatalf("ledger not voided")
	}
	if r := f.call("deposits/void", map[string]any{"id": dep}); r["message"] != "This deposit is already voided." {
		t.Fatalf("void twice: %v", r)
	}
	if r := f.call("deposits/void", map[string]any{"id": 999999}); r["message"] != "Error occured while voiding" {
		t.Fatalf("void missing: %v", r)
	}
}

func TestRecordWithdrawals(t *testing.T) {
	f := newMoneyFixture(t)
	today := time.Now().Format("2006-01-02")
	var expCat int64
	_ = f.srv.pool.QueryRow(context.Background(), `SELECT id FROM expense_categories WHERE name = 'Rent'`).Scan(&expCat)

	r := f.call("withdrawals/record_expenses", map[string]any{"request_id": f.reqID(), "expenses": []any{
		map[string]any{"expense_date": today, "expense_category_id": expCat, "withdrawal_method": 1, "account_id": f.bank, "amount": 100},
		map[string]any{"expense_date": today, "expense_category_id": expCat, "withdrawal_method": 3, "account_id": f.bank, "amount": 50},
	}})
	if r["status"] != float64(1) || r["success"] != "Successfully added transactions" {
		t.Fatalf("expenses: %v", r)
	}
	wantStatus(t, "new expense", f.call("withdrawals/new_record_expenses", map[string]any{
		"expense_date": today, "expense_category_id": expCat, "withdrawal_method": "2", "account_id": f.petty, "amount": "20",
	}), float64(1))
	if n := f.count(`SELECT count(*) FROM withdrawals WHERE kind = 'expense' AND created_by IS NOT NULL`); n != 3 {
		t.Fatalf("expenses recorded: %d", n)
	}

	// The member paid 1600 into the contribution.
	r = f.call("withdrawals/record_contribution_refund", map[string]any{"request_id": f.reqID(),
		"member_id": f.self, "refund_date": today, "contribution_id": f.contrib, "account_id": f.bank,
		"refund_method": "1", "amount": "5000"})
	if v, _ := r["validation_errors"].(map[string]any); v["amount"] == nil {
		t.Fatalf("refund too large: %v", r)
	}
	r = f.call("withdrawals/record_contribution_refund", map[string]any{"request_id": f.reqID(),
		"member_id": f.self, "refund_date": today, "contribution_id": f.contrib, "account_id": f.bank,
		"refund_method": "1", "amount": "100"})
	if r["success"] != "Successfully recorded a transaction" {
		t.Fatalf("refund: %v", r)
	}

	// The bank holds 1000 + 2500 - 700 - 150 - 100 = 2550.
	r = f.call("withdrawals/record_funds_transfer", map[string]any{"request_id": f.reqID(),
		"transfer_date": today, "from_account_id": f.bank, "to_account_id": f.petty, "amount": "3000"})
	if v, _ := r["validation_errors"].(map[string]any); v["amount"] == nil {
		t.Fatalf("overdraw: %v", r)
	}
	r = f.call("withdrawals/record_funds_transfer", map[string]any{"request_id": f.reqID(),
		"transfer_date": today, "from_account_id": f.bank, "to_account_id": f.petty, "amount": "500"})
	wantStatus(t, "transfer", r, float64(1))
	if n := f.count(`SELECT count(*) FROM account_entries WHERE account_transfer_id IS NOT NULL`); n != 2 {
		t.Fatalf("transfer entries: %d", n)
	}

	wantStatus(t, "void", f.call("withdrawals/void", map[string]any{"withdrawal_id": f.expense}), float64(1))
	if r := f.call("withdrawals/void", map[string]any{"withdrawal_id": 999999}); r["status"] != float64(0) {
		t.Fatalf("void missing: %v", r)
	}
}

func TestFinesAndInvoices(t *testing.T) {
	f := newMoneyFixture(t)
	today := time.Now().Format("2006-01-02")
	r := f.call("fines/fine_members", map[string]any{
		"fine_date": today, "fine_category_id": fmt.Sprintf("fine_category-%d", f.fineCat), "amount": "200",
		"members": []any{map[string]any{"member_id": f.self, "member_name": "x"}, map[string]any{"member_id": f.other}},
	})
	if r["status"] != "1" || r["message"] != "2 members fined" {
		t.Fatalf("fine members: %v", r)
	}
	r = f.call("fines/new_fine_members", map[string]any{
		"fine_date": today, "fine_category_id": fmt.Sprintf("contribution-%d", f.contrib), "amount": "50", "member_type_id": "2",
	})
	if r["message"] != "Successfully fined 2 members" {
		t.Fatalf("new fine members: %v", r)
	}
	if n := f.count(`SELECT count(*) FROM ledger_entries WHERE entry_type = 'contribution_fine_invoice' AND contribution_id = $1`, f.contrib); n != 2 {
		t.Fatalf("contribution fines: %d", n)
	}

	r = f.call("invoices/create", map[string]any{"request_id": f.reqID(),
		"type": "4", "send_to": "2", "member_ids": []any{f.other}, "amount_payable": "300",
		"invoice_date": today, "due_date": time.Now().AddDate(0, 0, 7).Format("2006-01-02"), "description": "Party"})
	wantStatus(t, "misc invoice", r, float64(1))
	r = f.call("invoices/create", map[string]any{"request_id": f.reqID(),
		"type": "1", "send_to": "1", "amount_payable": "1000", "invoice_date": today, "due_date": today})
	if v, _ := r["validation_errors"].(map[string]any); v["contribution_id"] == nil {
		t.Fatalf("contribution invoice without contribution: %v", r)
	}
	var inv int64
	_ = f.srv.pool.QueryRow(context.Background(), `SELECT id FROM ledger_entries WHERE entry_type = 'misc_invoice'`).Scan(&inv)
	invoices := list(t, f.call("invoices/get_group_invoices", nil), "invoices")
	found := false
	for _, i := range invoices {
		found = found || i["id"] == fmt.Sprint(inv)
	}
	if !found {
		t.Fatalf("new invoice not listed: %v", invoices)
	}
	wantStatus(t, "void invoice", f.call("invoices/void", map[string]any{"id": inv}), float64(1))
	if r := f.call("invoices/void", map[string]any{"id": inv}); r["message"] != "Invoice not available" {
		t.Fatalf("void twice: %v", r)
	}
}

func TestCategoryWrites(t *testing.T) {
	f := newMoneyFixture(t)
	r := f.call("income_categories/create", map[string]any{"name": "Dividends", "description": "From shares"})
	if r["status"] != float64(1) || r["success"] != "Dividends  successfully added" {
		t.Fatalf("create: %v", r)
	}
	if r := f.call("income_categories/create", map[string]any{"name": "dividends"}); r["status"] != float64(0) {
		t.Fatalf("duplicate: %v", r)
	}
	var id int64
	_ = f.srv.pool.QueryRow(context.Background(), `SELECT id FROM income_categories WHERE name = 'Dividends'`).Scan(&id)
	wantStatus(t, "edit", f.call("income_categories/edit", map[string]any{"id": id, "name": "Dividend income"}), float64(1))
	wantStatus(t, "hide", f.call("income_categories/hide", map[string]any{"id": id}), float64(1))
	if r := f.call("income_categories/hide", map[string]any{"id": id}); r["message"] != "Income category already hidden" {
		t.Fatalf("hide twice: %v", r)
	}
	wantStatus(t, "unhide", f.call("income_categories/unhide", map[string]any{"id": id}), float64(1))
	wantStatus(t, "delete", f.call("income_categories/delete", map[string]any{"id": id}), float64(1))
	if n := f.count(`SELECT count(*) FROM income_categories WHERE id = $1 AND deleted_at IS NULL`, id); n != 0 {
		t.Fatal("not deleted")
	}
	// The fixture's fine category has no charges, so it can go; Rent has an expense, so it stays.
	if r := f.call("fine_categories/delete", map[string]any{"id": f.fineCat}); r["status"] != float64(1) {
		t.Fatalf("delete unused fine category: %v", r)
	}
	var rent int64
	_ = f.srv.pool.QueryRow(context.Background(), `SELECT id FROM expense_categories WHERE name = 'Rent'`).Scan(&rent)
	if r := f.call("expense_categories/delete", map[string]any{"id": rent}); r["message"] != "Expense category has existing transactions and can not be deleted" {
		t.Fatalf("delete used: %v", r)
	}
	r = f.call("fine_categories/create", map[string]any{"name": "Absence", "amount": "150"})
	if r["success"] != "Absence as a fine category was successfully created" {
		t.Fatalf("fine category: %v", r)
	}
	if n := f.count(`SELECT count(*) FROM fine_categories WHERE name = 'Absence' AND amount = 150`); n != 1 {
		t.Fatal("fine amount not saved")
	}
	r = f.call("depositors/create", map[string]any{"name": "Chama Bank", "phone": "0711000111", "email": "x@example.com"})
	if r["success"] != "Depositor successfully created" {
		t.Fatalf("depositor: %v", r)
	}
	if n := f.count(`SELECT count(*) FROM depositors WHERE name = 'Chama Bank' AND phone = '+254711000111'`); n != 1 {
		t.Fatal("depositor phone not normalised")
	}

	// Members who aren't officials can't change the books.
	f.exec(`UPDATE memberships SET is_official = false, group_role_id = NULL WHERE id = $1`, f.self)
	for _, path := range []string{"income_categories/create", "deposits/void", "fines/new_fine_members", "depositors/create"} {
		if r := f.call(path, map[string]any{"name": "Nope", "id": 1}); r["message"] != "You are not allowed to perform this request. For group admins only" {
			t.Fatalf("%s as a member: %v", path, r)
		}
	}
}

func TestAppDate(t *testing.T) {
	loc := nairobi
	for in, want := range map[string]string{
		"2026-10-05": "2026-10-05", "05-10-2026": "2026-10-05", "10/05/2026": "2026-10-05",
		"5 Oct 2026": "2026-10-05", "1791158400": "2026-10-05", "1791158400000": "2026-10-05",
	} {
		got := appDate(in, loc)
		if got == nil || got.Format("2006-01-02") != want {
			t.Errorf("appDate(%q) = %v, want %s", in, got, want)
		}
	}
	if appDate("yesterday-ish", loc) != nil || appDate("", loc) != nil {
		t.Error("garbage should not parse")
	}
	if appAmount("1,500.5") != "1500.5" || appAmount("12.345") != "12.35" || appAmount("abc") != "" {
		t.Error("appAmount")
	}
}
