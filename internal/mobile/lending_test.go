package mobile

import (
	"fmt"
	"testing"
	"time"
)

func TestMobileBankLoans(t *testing.T) {
	f := newMoneyFixture(t)
	acct := fmt.Sprintf("bank-%d", f.bank)
	start := time.Now().AddDate(0, -1, 0).Format("02-01-2006")
	end := time.Now().AddDate(1, 0, 0).Format("02-01-2006")
	form := map[string]any{"description": "Equity group loan", "amount_loaned": "100,000",
		"total_loan_amount_payable": "120000", "loan_balance": "120000", "loan_start_date": start,
		"loan_end_date": end, "account_id": acct, "request_id": f.reqID()}
	r := f.call("deposits/record_bank_loan", map[string]any{"description": "x", "request_id": f.reqID()})
	wantStatus(t, "missing", r, float64(0))
	if r["validation_errors"].(map[string]any)["amount_loaned"] == nil {
		t.Fatalf("validation: %v", r)
	}
	bad := map[string]any{}
	for k, v := range form {
		bad[k] = v
	}
	bad["total_loan_amount_payable"], bad["request_id"] = "50000", f.reqID()
	r = f.call("deposits/record_bank_loan", bad)
	if r["status"] != float64(0) || r["validation_errors"].(map[string]any)["total_loan_amount_payable"] == nil {
		t.Fatalf("total under amount: %v", r)
	}
	wantStatus(t, "record", f.call("deposits/record_bank_loan", form), float64(1))

	r = f.call("bank_loans/get_group_bank_loan_options", nil)
	loans := r["loans"].([]any)
	if len(loans) != 1 || loans[0].(map[string]any)["balance"] != "120000.00" {
		t.Fatalf("options: %v", r)
	}
	id := loans[0].(map[string]any)["id"]

	pay := map[string]any{"bank_loan_id": id, "repayment_date": time.Now().Format("02-01-2006"), "account_id": acct,
		"repayment_method": "1", "description": "First installment", "amount": "20000", "request_id": f.reqID()}
	r = f.call("withdrawals/record_bank_loan_repayment", pay)
	wantStatus(t, "repay", r, float64(1))
	if r["success"] != "Successfully recorded a transaction" {
		t.Errorf("repay: %v", r)
	}
	pay["bank_loan_id"], pay["request_id"] = "999999", f.reqID()
	if r := f.call("withdrawals/record_bank_loan_repayment", pay); r["message"] != "Could not find bank loan" {
		t.Errorf("unknown loan: %v", r)
	}

	r = f.call("reports/bank_loans_summary", nil)
	wantStatus(t, "summary", r, float64(1))
	data := r["data"].(map[string]any)
	row := data["statement_body"].([]any)[0].(map[string]any)
	foot := data["statement_footer"].(map[string]any)
	if row["loan_balance"] != "100000.00" || row["amount_payble"] != "120000.00" || foot["total_repaid"] != "20000.00" ||
		row["account"] == "" {
		t.Errorf("summary: %v", r)
	}
	if n := f.count(`SELECT count(*) FROM deposits WHERE kind = 'bank_loan' AND voided_at IS NULL AND bank_loan_id IS NOT NULL`); n != 1 {
		t.Errorf("bank loan deposits: %d", n)
	}

	wantStatus(t, "void", f.call("deposits/void_bank_loan", map[string]any{"bank_loan_id": id}), float64(1))
	wantStatus(t, "void again", f.call("deposits/void_bank_loan", map[string]any{"bank_loan_id": id}), float64(0))
	if n := f.count(`SELECT count(*) FROM withdrawals WHERE kind = 'bank_loan_repayment' AND voided_at IS NULL`); n != 0 {
		t.Errorf("repayments left after void: %d", n)
	}
	if loans := f.call("bank_loans/get_group_bank_loan_options", nil)["loans"].([]any); len(loans) != 0 {
		t.Errorf("voided loan still offered: %v", loans)
	}
}

func TestMobileDebtors(t *testing.T) {
	f := newMoneyFixture(t)
	f.exec(`INSERT INTO debtors (group_id, name, phone, description) VALUES ($1, 'Jo Traders', '+254700111222', 'Shop next door')`, f.group)
	f.exec(`INSERT INTO debtors (group_id, name, is_active) VALUES ($1, 'Gone', false)`, f.group)
	r := f.call("debtors/get_debtors_list", nil)
	wantStatus(t, "debtors", r, float64(1))
	list := r["debtors"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["name"] != "Jo Traders" {
		t.Fatalf("debtors: %v", r)
	}

	// debtor_loan_types/* work on their own list of types.
	member := f.loanType(nil)
	r = f.call("debtor_loan_types/create", map[string]any{"name": "Trade credit", "interest_type": "1", "interest_rate": "5",
		"interest_rate_per": "5", "repayment_period_type": "1", "fixed_repayment_period": "2", "loan_amount_type": "1",
		"minimum_loan_amount": "1000", "maximum_loan_amount": "20000", "grace_period": "1"})
	wantStatus(t, "create debtor type", r, float64(1))
	debtorType := r["id"].(string)
	types := f.call("debtor_loan_types/get_group_loan_types_list", nil)["loan_types"].([]any)
	if len(types) != 1 || types[0].(map[string]any)["name"] != "TRADE CREDIT" {
		t.Errorf("debtor types: %v", types)
	}
	types = f.call("loan_types/get_group_loan_types_list", nil)["loan_types"].([]any)
	if len(types) != 1 || types[0].(map[string]any)["id"] != member {
		t.Errorf("member types: %v", types)
	}
	wantStatus(t, "member type through debtor actions", f.call("debtor_loan_types/get_group_loan_type", map[string]any{"id": member}), float64(0))
	wantStatus(t, "hide", f.call("debtor_loan_types/hide", map[string]any{"id": debtorType}), float64(1))
	wantStatus(t, "delete", f.call("debtor_loan_types/delete", map[string]any{"id": debtorType}), float64(1))
}
