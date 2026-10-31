package httpapi

import (
	"fmt"
	"testing"
	"time"
)

func TestLoanTypes(t *testing.T) {
	f := newMoneyFixture(t)
	for _, bad := range []map[string]any{
		{"name": " ", "interest_type": "fixed_balance"},
		{"name": "X", "interest_type": "magic"},
		{"name": "X", "interest_type": "fixed_balance", "rate_per": "fortnight"},
		{"name": "X", "interest_type": "fixed_balance", "fixed_term": 3, "min_term": 1},
		{"name": "X", "interest_type": "fixed_balance", "amount_basis": "savings"},
		{"name": "X", "interest_type": "fixed_balance", "guarantors": "always"},
		{"name": "X", "interest_type": "fixed_balance", "late_fine": map[string]any{"kind": "fixed", "frequency": "monthly"}},
		{"name": "X", "interest_type": "fixed_balance", "balance_fine": map[string]any{"kind": "percentage", "rate": 5, "rate_on": "installment_balance", "frequency": "monthly"}},
		{"name": "X", "interest_type": "fixed_amount"},
	} {
		if r := f.post("/loan-types", bad); r.code != 400 {
			t.Errorf("%v: %d %s", bad, r.code, r.raw)
		}
	}
	expect(t, f.call("POST", f.memberTok, "/loan-types", map[string]any{"name": "Mine"}), 403)
	r := f.post("/loan-types", map[string]any{
		"name": "Normal", "interest_type": "fixed_balance", "interest_rate": 10, "rate_per": "month",
		"min_term": 1, "max_term": 12, "min_amount": "1000", "max_amount": "100000",
		"late_fine":      map[string]any{"kind": "fixed", "amount": "100", "frequency": "monthly"},
		"processing_fee": map[string]any{"kind": "percentage", "rate": 2, "on": "amount"},
	})
	expect(t, r, 201)
	id := int64(r.body["id"].(float64))
	if r.body["late_fine"].(map[string]any)["scope"] != "installment" || r.body["needs_approval"] != true {
		t.Errorf("created: %s", r.raw)
	}
	expect(t, f.post("/loan-types", map[string]any{"name": "normal", "interest_type": "fixed_balance"}), 400)
	r = f.call("PATCH", f.officialTok, fmt.Sprintf("/loan-types/%d", id), map[string]any{
		"name": "Normal", "interest_type": "reducing_balance", "interest_rate": 12, "rate_per": "year", "fixed_term": 6, "is_hidden": true,
	})
	expect(t, r, 200)
	if r.body["interest_type"] != "reducing_balance" || r.body["fixed_term"] != float64(6) || r.body["late_fine"] != nil {
		t.Errorf("patched: %s", r.raw)
	}
	if n := len(f.get(f.memberTok, "/loan-types").list("loan_types")); n != 0 {
		t.Errorf("member sees hidden type")
	}
	if n := len(f.get(f.officialTok, "/loan-types?include_hidden=true").list("loan_types")); n != 1 {
		t.Errorf("official list: %d", n)
	}
	expect(t, f.call("DELETE", f.officialTok, fmt.Sprintf("/loan-types/%d", id), nil), 204)
	expect(t, f.call("DELETE", f.officialTok, fmt.Sprintf("/loan-types/%d", id), nil), 404)
}

func TestLoans(t *testing.T) {
	f := newMoneyFixture(t)
	bank := f.account(map[string]any{"kind": "bank", "name": "Equity", "institution_id": f.bank, "initial_balance": "100000"})
	r := f.post("/loan-types", map[string]any{
		"name": "Normal", "interest_type": "fixed_balance", "interest_rate": 10, "rate_per": "month",
		"min_term": 1, "max_term": 12, "max_amount": "50000", "guarantors": "over_max", "min_guarantors": 1,
		"processing_fee": map[string]any{"kind": "fixed", "amount": "200"},
	})
	expect(t, r, 201)
	typeID := int64(r.body["id"].(float64))
	day := time.Now().AddDate(0, -3, 0).Format(dateLayout)

	// Preview: 12000 over 3 months at 10% a month flat = 3600 interest.
	r = f.call("POST", f.memberTok, "/loans/preview", map[string]any{"loan_type_id": typeID, "amount": "12000", "installments": 3})
	expect(t, r, 200)
	if r.body["interest"] != "3600.00" || r.body["total"] != "15600.00" || r.body["processing_fee"] != "200.00" {
		t.Fatalf("preview: %s", r.raw)
	}

	base := map[string]any{"member_id": f.member, "loan_type_id": typeID, "account_id": bank, "amount": "12000",
		"disbursed_on": day, "installments": 3, "method": "cash"}
	with := func(k string, v any) map[string]any {
		m := map[string]any{}
		for a, b := range base {
			m[a] = b
		}
		m[k] = v
		return m
	}
	for _, bad := range []map[string]any{
		with("installments", 13), with("amount", "60000"), with("member_id", 999999),
		with("account_id", 999999), with("disbursed_on", time.Now().AddDate(0, 0, 2).Format(dateLayout)),
		with("guarantors", []map[string]any{{"member_id": f.member}}),
	} {
		if r := f.post("/loans", bad); r.code != 400 {
			t.Errorf("%v: %d %s", bad, r.code, r.raw)
		}
	}
	expect(t, f.call("POST", f.memberTok, "/loans", base), 403)

	r = f.post("/loans", base)
	expect(t, r, 201)
	loanID := int64(r.body["id"].(float64))
	tot := r.body["totals"].(map[string]any)
	charges := r.list("charges")
	if len(charges) != 3 || tot["payable"] != "15600.00" || tot["balance"] != "15600.00" || r.body["status"] != "arrears" {
		t.Fatalf("loan: %s", r.raw)
	}
	if charges[0]["amount"] != "5200.00" || charges[0]["status"] != "overdue" {
		t.Errorf("first installment: %v", charges[0])
	}
	// The bank paid out 12000 and took the 200 fee: 100000 - 12000 + 200.
	if b := accountBalances(f.get(f.officialTok, "/accounts"))["Equity"]; b != "88200.00" {
		t.Errorf("bank after loan: %s", b)
	}

	// A member sees their own loan, not someone else's.
	expect(t, f.get(f.memberTok, fmt.Sprintf("/loans/%d", loanID)), 200)
	officialLoan := f.post("/loans", with("member_id", f.official))
	expect(t, officialLoan, 201)
	expect(t, f.get(f.memberTok, fmt.Sprintf("/loans/%d", int64(officialLoan.body["id"].(float64)))), 404)
	if n := len(pagedItems(f.get(f.memberTok, "/loans"), "loans")); n != 1 {
		t.Errorf("member loan list: %d", n)
	}

	// Repay 6000: the first installment is cleared and 800 goes on the second.
	path := fmt.Sprintf("/loans/%d/repayments", loanID)
	expect(t, f.post(path, map[string]any{"account_id": bank, "amount": "0", "paid_on": day}), 400)
	r = f.post(path, map[string]any{"account_id": bank, "amount": "6000", "paid_on": day, "method": "mpesa"})
	expect(t, r, 201)
	charges = r.list("charges")
	if charges[0]["status"] != "paid" || charges[1]["paid"] != "800.00" || r.body["totals"].(map[string]any)["balance"] != "9600.00" {
		t.Fatalf("after repayment: %s", r.raw)
	}
	reps := r.list("repayments")
	repID := int64(reps[0]["id"].(float64))
	stmt := r.list("statement")
	if last := stmt[len(stmt)-1]; last["balance"] != "9600.00" {
		t.Errorf("statement: %v", stmt)
	}
	// The repayment is a deposit too; voiding it there voids the repayment.
	depID := int64(reps[0]["deposit_id"].(float64))
	expect(t, f.post(fmt.Sprintf("/deposits/%d/void", depID), nil), 200)
	r = f.get(f.officialTok, fmt.Sprintf("/loans/%d", loanID))
	if r.body["totals"].(map[string]any)["paid"] != "0.00" {
		t.Errorf("deposit void did not void the repayment: %s", r.raw)
	}
	expect(t, f.post(fmt.Sprintf("/loans/%d/repayments/%d/void", loanID, repID), nil), 409)

	// Pay it all off.
	r = f.post(path, map[string]any{"account_id": bank, "amount": "15600", "paid_on": day})
	expect(t, r, 201)
	if r.body["status"] != "paid" {
		t.Errorf("paid off: %s", r.raw)
	}
	r = f.get(f.officialTok, "/loans?status=paid")
	if n := len(pagedItems(r, "loans")); n != 1 {
		t.Errorf("paid filter: %s", r.raw)
	}

	// The disbursement cannot be voided on its own; voiding the loan undoes everything.
	wd := pagedItems(f.get(f.officialTok, "/withdrawals?kind=loan_disbursement"), "withdrawals")
	wid := int64(wd[0]["id"].(float64))
	expect(t, f.post(fmt.Sprintf("/withdrawals/%d/void", wid), nil), 409)
	expect(t, f.post(fmt.Sprintf("/loans/%d/void", loanID), nil), 200)
	expect(t, f.post(fmt.Sprintf("/loans/%d/void", loanID), nil), 409)
	expect(t, f.post(path, map[string]any{"account_id": bank, "amount": "10", "paid_on": day}), 409)
	// Only the official's loan is left: 100000 - 12000 + 200.
	if b := accountBalances(f.get(f.officialTok, "/accounts"))["Equity"]; b != "88200.00" {
		t.Errorf("bank after void: %s", b)
	}

	// Without a loan type, every term is given.
	r = f.post("/loans", map[string]any{"member_id": f.member, "account_id": bank, "amount": "10000",
		"disbursed_on": day, "installments": 4, "interest_type": "reducing_balance", "interest_rate": 12, "rate_per": "year",
		"late_fine": map[string]any{"kind": "percentage", "rate": 5, "rate_on": "installment_balance", "frequency": "monthly"}})
	expect(t, r, 201)
	charges = r.list("charges")
	var principal float64
	for _, c := range charges {
		var p float64
		fmt.Sscan(c["principal"].(string), &p)
		principal += p
	}
	if len(charges) != 4 || principal != 10000 {
		t.Errorf("reducing loan principal %v: %s", principal, r.raw)
	}
}

func pagedItems(r *httpResult, key string) []map[string]any {
	var out []map[string]any
	for _, x := range r.body[key].(map[string]any)["items"].([]any) {
		out = append(out, x.(map[string]any))
	}
	return out
}
