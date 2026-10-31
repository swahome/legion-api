package httpapi

import (
	"fmt"
	"testing"
	"time"
)

func TestBankLoans(t *testing.T) {
	f := newMoneyFixture(t)
	bank := f.account(map[string]any{"kind": "bank", "name": "Equity", "institution_id": f.bank, "initial_balance": "1000"})
	day := time.Now().AddDate(0, -2, 0).Format(dateLayout)
	ends := time.Now().AddDate(1, 0, 0).Format(dateLayout)
	base := map[string]any{"description": "KCB business loan", "amount": "50000", "total_payable": "60000",
		"starts_on": day, "ends_on": ends, "account_id": bank, "method": "funds_transfer"}
	with := func(k string, v any) map[string]any {
		m := map[string]any{}
		for a, b := range base {
			m[a] = b
		}
		m[k] = v
		return m
	}
	for _, bad := range []map[string]any{
		with("description", " "), with("amount", "0"), with("total_payable", "40000"), with("balance", "70000"),
		with("ends_on", time.Now().AddDate(0, -3, 0).Format(dateLayout)), with("account_id", 999999),
	} {
		if r := f.post("/bank-loans", bad); r.code != 400 {
			t.Errorf("%v: %d %s", bad, r.code, r.raw)
		}
	}
	expect(t, f.call("POST", f.memberTok, "/bank-loans", base), 403)
	r := f.post("/bank-loans", base)
	expect(t, r, 201)
	loan := r.body["bank_loan"].(map[string]any)
	id := int64(loan["id"].(float64))
	if loan["balance"] != "60000.00" || loan["status"] != "active" || loan["account"].(map[string]any)["name"] != "Equity" {
		t.Fatalf("created: %s", r.raw)
	}
	if b := accountBalances(f.get(f.officialTok, "/accounts"))["Equity"]; b != "51000.00" {
		t.Errorf("bank after loan came in: %s", b)
	}
	// The money in can only be voided with the loan.
	dep := pagedItems(f.get(f.officialTok, "/deposits?kind=bank_loan"), "deposits")
	if len(dep) != 1 {
		t.Fatalf("bank loan deposits: %v", dep)
	}
	expect(t, f.post(fmt.Sprintf("/deposits/%d/void", int64(dep[0]["id"].(float64))), nil), 409)

	path := fmt.Sprintf("/bank-loans/%d", id)
	rep := map[string]any{"account_id": bank, "amount": "20000", "paid_on": time.Now().Format(dateLayout), "method": "cheque"}
	expect(t, f.post(path+"/repayments", map[string]any{"account_id": bank, "amount": "10",
		"paid_on": time.Now().AddDate(0, -3, 0).Format(dateLayout)}), 400)
	r = f.post(path+"/repayments", rep)
	expect(t, r, 201)
	if l := r.body["bank_loan"].(map[string]any); l["repaid"] != "20000.00" || l["balance"] != "40000.00" {
		t.Errorf("after repayment: %s", r.raw)
	}
	repID := int64(r.list("repayments")[0]["id"].(float64))
	if b := accountBalances(f.get(f.officialTok, "/accounts"))["Equity"]; b != "31000.00" {
		t.Errorf("bank after repayment: %s", b)
	}
	// Members can see the group's bank loans.
	r = f.get(f.memberTok, "/bank-loans")
	expect(t, r, 200)
	if len(r.list("bank_loans")) != 1 || r.body["totals"].(map[string]any)["balance"] != "40000.00" {
		t.Errorf("list: %s", r.raw)
	}
	r = f.post(path+"/repayments", map[string]any{"account_id": bank, "amount": "40000", "paid_on": time.Now().Format(dateLayout)})
	expect(t, r, 201)
	if l := r.body["bank_loan"].(map[string]any); l["status"] != "paid" {
		t.Errorf("paid off: %s", r.raw)
	}
	if n := len(f.get(f.officialTok, "/bank-loans?status=open").list("bank_loans")); n != 0 {
		t.Errorf("open loans: %d", n)
	}
	expect(t, f.post(fmt.Sprintf("%s/repayments/%d/void", path, repID), nil), 200)
	expect(t, f.post(fmt.Sprintf("%s/repayments/%d/void", path, repID), nil), 409)
	expect(t, f.post(fmt.Sprintf("%s/repayments/%d/void", path, 999999), nil), 404)

	r = f.call("PATCH", f.officialTok, path, map[string]any{"description": "KCB loan", "total_payable": "65000",
		"balance": "65000", "ends_on": ends})
	expect(t, r, 200)
	if l := r.body["bank_loan"].(map[string]any); l["balance"] != "25000.00" || l["description"] != "KCB loan" {
		t.Errorf("edited: %s", r.raw)
	}

	// An older loan entered part-paid starts at its balance.
	r = f.post("/bank-loans", with("balance", "15000"))
	expect(t, r, 201)
	if l := r.body["bank_loan"].(map[string]any); l["balance"] != "15000.00" {
		t.Errorf("part-paid: %s", r.raw)
	}

	// Voiding takes the money in and the repayments with it.
	expect(t, f.post(path+"/void", nil), 200)
	expect(t, f.post(path+"/void", nil), 409)
	expect(t, f.post(path+"/repayments", rep), 409)
	if b := accountBalances(f.get(f.officialTok, "/accounts"))["Equity"]; b != "51000.00" {
		t.Errorf("bank after void: %s", b)
	}
}

func TestDebtorsAndDebtorLoans(t *testing.T) {
	f := newMoneyFixture(t)
	bank := f.account(map[string]any{"kind": "bank", "name": "Equity", "institution_id": f.bank, "initial_balance": "100000"})
	expect(t, f.post("/debtors", map[string]any{"name": " "}), 400)
	expect(t, f.post("/debtors", map[string]any{"name": "Jo", "email": "not-an-email"}), 400)
	expect(t, f.call("POST", f.memberTok, "/debtors", map[string]any{"name": "Jo"}), 403)
	expect(t, f.get(f.memberTok, "/debtors"), 403)
	r := f.post("/debtors", map[string]any{"name": "Jo Trader", "phone": "+254700111222", "id_number": "1234"})
	expect(t, r, 201)
	debtor := int64(r.body["id"].(float64))
	r = f.call("PATCH", f.officialTok, fmt.Sprintf("/debtors/%d", debtor), map[string]any{"name": "Jo Traders Ltd", "phone": "+254700111222"})
	expect(t, r, 200)
	if r.body["name"] != "Jo Traders Ltd" || r.body["id_number"] != "" {
		t.Errorf("edited: %s", r.raw)
	}

	// Loan types are offered to members or to debtors.
	r = f.post("/loan-types", map[string]any{"name": "Trade credit", "interest_type": "fixed_balance", "interest_rate": 5,
		"for_debtors": true, "fixed_term": 2, "processing_fee": map[string]any{"kind": "fixed", "amount": "100"}})
	expect(t, r, 201)
	debtorType := int64(r.body["id"].(float64))
	expect(t, f.post("/loan-types", map[string]any{"name": "Savings x3", "interest_type": "fixed_balance",
		"for_debtors": true, "amount_basis": "savings", "savings_multiple": "3"}), 400)
	r = f.post("/loan-types", map[string]any{"name": "Member loan", "interest_type": "fixed_balance", "interest_rate": 10})
	expect(t, r, 201)
	memberType := int64(r.body["id"].(float64))
	if l := f.get(f.officialTok, "/loan-types").list("loan_types"); len(l) != 1 || l[0]["name"] != "Member loan" {
		t.Errorf("member types: %v", l)
	}
	if l := f.get(f.officialTok, "/loan-types?for=debtors").list("loan_types"); len(l) != 1 || l[0]["for_debtors"] != true {
		t.Errorf("debtor types: %v", l)
	}
	r = f.call("PATCH", f.officialTok, fmt.Sprintf("/loan-types/%d", debtorType), map[string]any{"name": "Trade credit",
		"interest_type": "fixed_balance", "interest_rate": 5, "fixed_term": 2, "for_debtors": false,
		"processing_fee": map[string]any{"kind": "fixed", "amount": "100"}})
	expect(t, r, 200)
	if r.body["for_debtors"] != true {
		t.Errorf("a type moved from debtors to members: %s", r.raw)
	}
	// Members cannot apply with a debtor loan type.
	expect(t, f.call("POST", f.memberTok, "/loan-applications", map[string]any{"loan_type_id": debtorType, "amount": "1000", "installments": 2}), 400)

	day := time.Now().AddDate(0, -1, 0).Format(dateLayout)
	base := map[string]any{"debtor_id": debtor, "loan_type_id": debtorType, "account_id": bank, "amount": "10000",
		"disbursed_on": day, "method": "cash"}
	expect(t, f.post("/loans", map[string]any{"debtor_id": debtor, "member_id": f.member, "loan_type_id": debtorType,
		"account_id": bank, "amount": "10000", "disbursed_on": day}), 400)
	expect(t, f.post("/loans", map[string]any{"debtor_id": debtor, "loan_type_id": memberType, "account_id": bank,
		"amount": "10000", "disbursed_on": day, "installments": 2}), 400)
	expect(t, f.post("/loans", map[string]any{"member_id": f.member, "loan_type_id": debtorType, "account_id": bank,
		"amount": "10000", "disbursed_on": day}), 400)
	r = f.call("POST", f.officialTok, "/loans/preview", map[string]any{"for_debtors": true, "loan_type_id": debtorType, "amount": "10000"})
	expect(t, r, 200)
	if r.body["total"] != "11000.00" {
		t.Errorf("preview: %s", r.raw)
	}
	r = f.post("/loans", base)
	expect(t, r, 201)
	loanID := int64(r.body["id"].(float64))
	if r.body["member"] != nil || r.body["debtor"].(map[string]any)["name"] != "Jo Traders Ltd" ||
		r.body["totals"].(map[string]any)["payable"] != "11000.00" {
		t.Fatalf("debtor loan: %s", r.raw)
	}
	// 100000 - 10000 paid out + 100 fee.
	if b := accountBalances(f.get(f.officialTok, "/accounts"))["Equity"]; b != "90100.00" {
		t.Errorf("bank after debtor loan: %s", b)
	}
	if wd := pagedItems(f.get(f.officialTok, "/withdrawals?kind=external_loan_disbursement"), "withdrawals"); len(wd) != 1 {
		t.Errorf("disbursement: %v", wd)
	}
	r = f.post(fmt.Sprintf("/loans/%d/repayments", loanID), map[string]any{"account_id": bank, "amount": "5500",
		"paid_on": time.Now().Format(dateLayout)})
	expect(t, r, 201)
	if r.body["totals"].(map[string]any)["balance"] != "5500.00" {
		t.Errorf("after repayment: %s", r.raw)
	}
	if d := pagedItems(f.get(f.officialTok, "/deposits?kind=external_lending_repayment"), "deposits"); len(d) != 1 {
		t.Errorf("repayment deposit: %v", d)
	}

	// Debtor loans are kept apart from member loans.
	if n := len(pagedItems(f.get(f.officialTok, "/loans"), "loans")); n != 0 {
		t.Errorf("member loan list shows debtor loans: %d", n)
	}
	r = f.get(f.officialTok, fmt.Sprintf("/loans?debtor_id=%d", debtor))
	if items := pagedItems(r, "loans"); len(items) != 1 || r.body["totals"].(map[string]any)["balance"] != "5500.00" {
		t.Errorf("debtor loans: %s", r.raw)
	}
	expect(t, f.get(f.memberTok, fmt.Sprintf("/loans/%d", loanID)), 404)
	r = f.get(f.officialTok, fmt.Sprintf("/debtors/%d", debtor))
	if r.body["loans"] != float64(1) || r.body["balance"] != "5500.00" {
		t.Errorf("debtor: %s", r.raw)
	}

	// A suspended debtor keeps their loans but cannot borrow.
	expect(t, f.post(fmt.Sprintf("/debtors/%d/suspend", debtor), nil), 200)
	expect(t, f.post("/loans", base), 400)
	if n := len(f.get(f.officialTok, "/debtors").list("debtors")); n != 0 {
		t.Errorf("suspended debtor listed: %d", n)
	}
	expect(t, f.post(fmt.Sprintf("/debtors/%d/activate", debtor), nil), 200)
	expect(t, f.call("DELETE", f.officialTok, fmt.Sprintf("/debtors/%d", debtor), nil), 409)
	expect(t, f.post(fmt.Sprintf("/loans/%d/void", loanID), nil), 200)
	expect(t, f.call("DELETE", f.officialTok, fmt.Sprintf("/debtors/%d", debtor), nil), 204)
	expect(t, f.get(f.officialTok, fmt.Sprintf("/debtors/%d", debtor)), 404)
}
