package mobile

import (
	"context"
	"strings"
	"testing"
	"time"
)

// moneyFixture adds accounts, categories, a depositor, deposits and an
// expense to the group fixture.
type moneyFixture struct {
	*groupFixture
	bank, petty, fineCat, deposit, expense int64
}

func newMoneyFixture(t *testing.T) *moneyFixture {
	g := newGroupFixture(t)
	f := &moneyFixture{groupFixture: g}
	ctx := context.Background()
	scan := func(dst *int64, sql string, args ...any) {
		t.Helper()
		if err := g.srv.pool.QueryRow(ctx, sql, args...).Scan(dst); err != nil {
			t.Fatal(err)
		}
	}
	var bankID, branch, depositor, income, expCat int64
	scan(&bankID, `SELECT id FROM banks WHERE slug = 'equity'`)
	scan(&branch, `SELECT id FROM bank_branches WHERE bank_id = $1`, bankID)
	g.exec(`UPDATE banks SET is_partner = true WHERE id = $1`, bankID)
	scan(&f.bank, `INSERT INTO group_accounts (group_id, kind, name, account_number, bank_id, bank_branch_id, initial_balance, is_verified)
		VALUES ($1, 'bank', 'Umoja main', '0123', $2, $3, 1000, true) RETURNING id`, g.group, bankID, branch)
	scan(&f.petty, `INSERT INTO group_accounts (group_id, kind, name) VALUES ($1, 'petty_cash', 'Cash box') RETURNING id`, g.group)
	g.exec(`INSERT INTO group_accounts (group_id, kind, name, is_closed) VALUES ($1, 'petty_cash', 'Old box', true)`, g.group)
	scan(&f.fineCat, `INSERT INTO fine_categories (group_id, name, amount) VALUES ($1, 'Lateness', 200) RETURNING id`, g.group)
	scan(&income, `INSERT INTO income_categories (group_id, name) VALUES ($1, 'Interest') RETURNING id`, g.group)
	scan(&expCat, `INSERT INTO expense_categories (group_id, name) VALUES ($1, 'Rent') RETURNING id`, g.group)
	scan(&depositor, `INSERT INTO depositors (group_id, name, email) VALUES ($1, 'Bank of Africa', 'boa@example.com') RETURNING id`, g.group)
	day := time.Now().AddDate(0, 0, -2).Format("2006-01-02")
	scan(&f.deposit, `INSERT INTO deposits (group_id, account_id, account_kind, kind, method, membership_id, contribution_id, amount, deposit_date)
		VALUES ($1, $2, 'bank', 'contribution', 'mpesa', $3, $4, 2500, $5) RETURNING id`, g.group, f.bank, g.self, g.contrib, day)
	g.exec(`INSERT INTO account_entries (group_id, account_id, account_kind, direction, amount, entry_date, deposit_id)
		VALUES ($1, $2, 'bank', 1, 2500, $3, $4)`, g.group, f.bank, day, f.deposit)
	var inc int64
	scan(&inc, `INSERT INTO deposits (group_id, account_id, account_kind, kind, income_category_id, depositor_id, amount, deposit_date)
		VALUES ($1, $2, 'petty_cash', 'income', $3, $4, 300, $5) RETURNING id`, g.group, f.petty, income, depositor, day)
	g.exec(`INSERT INTO account_entries (group_id, account_id, account_kind, direction, amount, entry_date, deposit_id)
		VALUES ($1, $2, 'petty_cash', 1, 300, $3, $4)`, g.group, f.petty, day, inc)
	scan(&f.expense, `INSERT INTO withdrawals (group_id, account_id, account_kind, kind, method, expense_category_id, amount, withdrawal_date, description)
		VALUES ($1, $2, 'bank', 'expense', 'cheque', $3, 700, $4, 'October rent') RETURNING id`, g.group, f.bank, expCat, day)
	g.exec(`INSERT INTO account_entries (group_id, account_id, account_kind, direction, amount, entry_date, withdrawal_id)
		VALUES ($1, $2, 'bank', -1, 700, $3, $4)`, g.group, f.bank, day, f.expense)
	return f
}

func TestAccountActions(t *testing.T) {
	f := newMoneyFixture(t)
	r := f.call("accounts/get_group_active_account_options", nil)
	accts := r["accounts"].(map[string]any)
	banks := accts["bank_accounts"].([]any)
	petty := accts["petty_cash_accounts"].([]any)
	if len(banks) != 1 || banks[0].(map[string]any)["name"] != "Equity Bank(Westlands) - Umoja main(0123)" || len(petty) != 1 {
		t.Fatalf("active options: %v", accts)
	}
	if all := f.call("accounts/get_group_account_options", nil)["accounts"].(map[string]any); len(all["petty_cash_accounts"].([]any)) != 2 {
		t.Fatalf("all options: %v", all)
	}
	b := list(t, f.call("bank_accounts/get_group_bank_accounts_list", nil), "banks")
	// 1000 opening + 2500 deposit - 700 expense.
	if len(b) != 1 || b[0]["account_balance"] != float64(2800) || b[0]["is_partner"] != float64(1) || b[0]["is_verified"] != float64(1) ||
		b[0]["description"] != "Equity Bank (Westlands) - 0123" {
		t.Fatalf("banks: %v", b)
	}
	p := list(t, f.call("petty_cash_accounts/get_group_petty_cash_accounts_list", nil), "petty_accounts")
	if len(p) != 2 {
		t.Fatalf("petty: %v", p)
	}
	gd := f.call("get_group_dashboard_data", nil)["group_details"].(map[string]any)
	if gd["bank_balances"] != float64(2800) || gd["cash_balances"] != float64(300) || gd["total_expense_payments"] != float64(700) ||
		len(gd["account_balances"].([]any)) != 2 {
		t.Fatalf("group dashboard: %v", gd)
	}
}

func TestCategoryActions(t *testing.T) {
	f := newMoneyFixture(t)
	if c := list(t, f.call("fine_categories/group_fine_categories_list", nil), "fine_categories"); len(c) != 1 || c[0]["amount"] != float64(200) {
		t.Fatalf("fine categories: %v", c)
	}
	if c := list(t, f.call("expense_categories/get_group_expense_category_options", nil), "expense_category_options"); len(c) != 1 || c[0]["name"] != "Rent" {
		t.Fatalf("expense options: %v", c)
	}
	if c := list(t, f.call("income_categories/get_group_income_category_options", nil), "income_categories"); len(c) != 1 {
		t.Fatalf("income options: %v", c)
	}
	if d := list(t, f.call("depositors/get_group_depositor_options", nil), "depositors"); len(d) != 1 || d[0]["identity"] != "boa@example.com" {
		t.Fatalf("depositors: %v", d)
	}
}

func TestDepositAndWithdrawalActions(t *testing.T) {
	f := newMoneyFixture(t)
	d := list(t, f.call("deposits/get_deposits_list", nil), "deposits")
	if len(d) != 2 {
		t.Fatalf("deposits: %v", d)
	}
	var contribution map[string]any
	for _, x := range d {
		if strings.HasPrefix(x["type"].(string), "Contribution payment") {
			contribution = x
		}
	}
	if contribution == nil || contribution["type"] != "Contribution payment made by Akinyi Otieno" ||
		contribution["narative"] != `Two Thousand, Five Hundred KESs only -- Contribution payment for "Monthly savings" contribution via MPesa Deposited to Equity Bank (Westlands) - Umoja main (0123)` {
		t.Fatalf("contribution deposit: %v", contribution)
	}
	if only := list(t, f.call("deposits/get_deposits_list", map[string]any{"status": []any{"5"}}), "deposits"); len(only) != 1 || only[0]["type"] != "Income Bank of Africa" {
		t.Fatalf("income filter: %v", only)
	}
	rec := f.call("deposits/get_group_deposit", map[string]any{"deposit_id": f.deposit})["data"].(map[string]any)
	if rec["amount_paid"] != "Two Thousand, Five Hundred KESs only" || rec["member"] != "Akinyi Otieno" {
		t.Fatalf("deposit receipt: %v", rec)
	}
	if r := f.call("deposits/get_group_deposit", map[string]any{"deposit_id": 999999}); r["status"] != float64(0) {
		t.Fatalf("missing deposit: %v", r)
	}
	w := list(t, f.call("withdrawals/get_group_withdrawal_list", nil), "withdrawals")
	if len(w) != 1 || w[0]["type"] != "Expense --- Expense for Rent : October rent" || w[0]["amount"] != "700.00" {
		t.Fatalf("withdrawals: %v", w)
	}
	wr := f.call("withdrawals/get_group_withdrawal", map[string]any{"withdrawal_id": f.expense})["data"].(map[string]any)
	if wr["amount_paid"] != "Seven Hundred KESs only" {
		t.Fatalf("withdrawal receipt: %v", wr)
	}
	recent := f.call("get_member_dashboard_data", nil)["member_details"].(map[string]any)["recent_transactions"].([]any)
	if len(recent) != 1 || recent[0].(map[string]any)["description"] != "Monthly savings" {
		t.Fatalf("recent: %v", recent)
	}
	if g := f.call("get_group_recent_transactions", nil); len(g["recent_transactions"].([]any)) != 2 {
		t.Fatalf("group recent: %v", g)
	}
}

func TestInvoiceAndFineActions(t *testing.T) {
	f := newMoneyFixture(t)
	inv := list(t, f.call("invoices/get_group_invoices", nil), "invoices")
	// Two contribution invoices of 1000 and a fine of 200. Akinyi paid 1600
	// towards the contribution and 50 towards the fine: the older invoice is
	// paid in full, the newer one 600.
	if len(inv) != 3 {
		t.Fatalf("invoices: %v", inv)
	}
	paid := map[string]string{}
	for _, i := range inv {
		paid[i["type"].(string)+" "+i["invoice_date"].(string)] = i["amount_paid"].(string)
	}
	var got []string
	for _, i := range inv {
		got = append(got, i["amount_paid"].(string))
	}
	if strings.Join(got, ",") != "50.00,600.00,1000.00" && strings.Join(got, ",") != "600.00,50.00,1000.00" {
		t.Fatalf("paid: %v", inv)
	}
	fines := list(t, f.call("fines/get_group_fines_list", nil), "fines")
	if len(fines) != 1 || fines[0]["amount_payable"] != "200.00" || fines[0]["amount_paid"] != "50.00" {
		t.Fatalf("fines: %v", fines)
	}
	r := f.call("invoices/get_group_invoice", map[string]any{"invoice_id": inv[len(inv)-1]["id"]})["data"].(map[string]any)
	if r["type"] != "Contribution invoice" || r["type_description"] != "Contribution invoice for Monthly savings contribution ---- One Thousand KESs only" {
		t.Fatalf("invoice: %v", r)
	}
	opts := f.call("fines/get_group_fine_options", nil)
	cats := list(t, opts, "fine_category_options")
	if len(cats) != 1 || cats[0]["amount"] != float64(200) {
		t.Fatalf("fine options: %v", opts)
	}
}

func TestNumberToWords(t *testing.T) {
	for in, want := range map[string]string{
		"0": "zero", "45": "fourty-five", "120": "one hundred and twenty", "1005": "one thousand and five",
		"2500.00": "two thousand, five hundred", "1200.50": "one thousand, two hundred point five",
		"3000000": "three million",
	} {
		if got := numberToWords(in); got != want {
			t.Errorf("%s: %q, want %q", in, got, want)
		}
	}
}
