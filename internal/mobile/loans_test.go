package mobile

import (
	"context"
	"fmt"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// asOther signs in the fixture's second member (Baraka) and returns a
// caller for their requests.
func (f *moneyFixture) asOther() func(string, map[string]any) map[string]any {
	f.t.Helper()
	hash, _ := bcrypt.GenerateFromPassword([]byte("other-pw"), 8)
	var uid int64
	if err := f.srv.pool.QueryRow(context.Background(), `UPDATE users SET password_hash = $1
		WHERE id = (SELECT user_id FROM memberships WHERE id = $2) RETURNING id`, string(hash), f.other).Scan(&uid); err != nil {
		f.t.Fatal(err)
	}
	r := f.post("login", "", map[string]any{"phone": "0700000002", "password": "other-pw", "user_id": 0, "group_id": 0})
	if r.body["status"] != float64(1) {
		f.t.Fatalf("login: %s", r.raw)
	}
	tok := r.body["access_token"].(string)
	return func(path string, extra map[string]any) map[string]any {
		f.t.Helper()
		body := map[string]any{"user_id": uid, "group_id": f.group}
		for k, v := range extra {
			body[k] = v
		}
		res := f.post(path, tok, body)
		if res.code != 200 {
			f.t.Fatalf("%s: HTTP %d %s", path, res.code, res.raw)
		}
		return res.body
	}
}

func (f *moneyFixture) loanType(extra map[string]any) string {
	f.t.Helper()
	form := map[string]any{"name": "Emergency", "interest_type": "1", "interest_rate": "10", "interest_rate_per": "5",
		"repayment_period_type": "1", "fixed_repayment_period": "4", "loan_amount_type": "1",
		"minimum_loan_amount": "1000", "maximum_loan_amount": "50,000", "grace_period": "1"}
	for k, v := range extra {
		form[k] = v
	}
	r := f.call("loan_types/create", form)
	wantStatus(t(f), "create loan type", r, float64(1))
	return r["id"].(string)
}

func t(f *moneyFixture) *testing.T { return f.t }

func TestMobileLoanTypes(t *testing.T) {
	f := newMoneyFixture(t)
	r := f.call("loan_types/create", map[string]any{"name": "x"})
	wantStatus(t, "invalid", r, float64(0))
	if r["validation_errors"].(map[string]any)["interest_type"] == nil {
		t.Fatalf("validation: %v", r)
	}
	id := f.loanType(nil)
	if r := f.call("loan_types/create", map[string]any{"name": "emergency", "interest_type": "1", "repayment_period_type": "1",
		"fixed_repayment_period": "4", "minimum_loan_amount": "1", "maximum_loan_amount": "2"}); r["status"] != float64(0) {
		t.Fatalf("duplicate name: %v", r)
	}
	r = f.call("loan_types/update_loan_type_fines", map[string]any{"id": id, "enable_loan_fines": "1", "loan_fine_type": "1",
		"fixed_fine_amount": "100", "fixed_amount_fine_frequency": "2", "fixed_amount_fine_frequency_on": "1"})
	wantStatus(t, "fines", r, float64(1))
	r = f.call("loan_types/update_general_details", map[string]any{"id": id, "enable_loan_processing_fee": "1",
		"loan_processing_fee_type": "1", "loan_processing_fee_fixed_amount": "200", "enable_loan_guarantors": "1",
		"loan_guarantors_type": "1", "minimum_guarantors": "1"})
	wantStatus(t, "general details", r, float64(1))

	one := f.call("loan_types/get_group_loan_type", map[string]any{"id": id})
	wantStatus(t, "get", one, float64(1))
	data := one["data"].(map[string]any)
	lt, fine, gen := data["loan_type"].(map[string]any), data["fine"].(map[string]any), data["general_details"].(map[string]any)
	if lt["name"] != "EMERGENCY" || fmt.Sprint(gen["enable_loan_guarantors"]) != "1" || fmt.Sprint(fine["fixed_fine_amount"]) != "100.00" ||
		fmt.Sprint(gen["loan_processing_fee_fixed_amount"]) != "200.00" {
		t.Fatalf("loan type: %v", data)
	}
	if l := list(t, f.call("loan_types/get_group_loan_types_list", nil), "loan_types"); len(l) != 1 {
		t.Fatalf("list: %v", l)
	}
	wantStatus(t, "hide", f.call("loan_types/hide", map[string]any{"id": id}), float64(1))
	if r := f.call("loan_types/get_group_loan_type_application_options", nil); len(r["loan_types"].([]any)) != 0 {
		t.Fatalf("hidden type offered: %v", r)
	}
	wantStatus(t, "unhide", f.call("loan_types/unhide", map[string]any{"id": id}), float64(1))
	wantStatus(t, "delete", f.call("loan_types/delete", map[string]any{"id": id}), float64(1))
	if n := f.count(`SELECT count(*) FROM loan_types WHERE group_id = $1 AND deleted_at IS NULL`, f.group); n != 0 {
		t.Fatalf("not deleted: %d", n)
	}
}

func TestMobileLoans(t *testing.T) {
	f := newMoneyFixture(t)
	typeID := f.loanType(nil)
	acct := fmt.Sprintf("bank-%d", f.bank)
	day := time.Now().AddDate(0, -1, 0).Format("02-01-2006")

	// A custom loan without a type.
	r := f.call("loans/record_member_loan", map[string]any{"member_id": f.other, "loan_amount": "1,000", "disbursement_date": day,
		"account_id": acct, "repayment_period": "2", "interest_type": "1", "interest_rate": "10", "loan_interest_rate_per": "5",
		"loan_repayment_frequency": "3", "grace_period": "1"})
	wantStatus(t, "record custom", r, float64(1))
	r = f.call("loans/record_member_loan", map[string]any{"member_id": f.self, "loan_amount": "4000", "disbursement_date": day,
		"account_id": acct, "repayment_period": "4", "loan_type_id": typeID})
	wantStatus(t, "record typed", r, float64(1))
	r = f.call("loans/record_member_loan", map[string]any{"member_id": f.self, "loan_amount": "90000", "disbursement_date": day,
		"account_id": acct, "repayment_period": "4", "loan_type_id": typeID})
	if r["status"] != float64(0) || r["validation_errors"].(map[string]any)["loan_amount"] == nil {
		t.Fatalf("over limit: %v", r)
	}

	loans := list(t, f.call("loans/get_group_loans_list", nil), "loans")
	if len(loans) != 2 {
		t.Fatalf("loans: %v", loans)
	}
	var mine, theirs string
	for _, l := range loans {
		if l["member_id"] == fmt.Sprint(f.self) {
			mine = l["id"].(string)
			if l["loan_type"] != "EMERGENCY" || l["amount"] != "4000.00" || l["is_fully_paid"] != float64(0) {
				t.Fatalf("my loan: %v", l)
			}
		} else {
			theirs = l["id"].(string)
		}
	}
	g := f.call("loans/get_group_loan", map[string]any{"id": theirs})["loan"].(map[string]any)
	if g["loan_amount"] != "1000.00" || g["account_id"] != acct || g["loan_type_name"] != "Normal Loan" || g["repayment_period"] != "2" {
		t.Fatalf("group loan: %v", g)
	}
	opts := list(t, f.call("loans/get_member_loan_options", nil), "loans")
	// 4000 + 10% flat for the term = 4400.
	if len(opts) != 1 || opts[0]["balance"] != float64(4400) {
		t.Fatalf("options: %v", opts)
	}
	if all := list(t, f.call("loans/get_members_loans_options", nil), "loans"); len(all) != 2 {
		t.Fatalf("everyone's options: %v", all)
	}

	// Repayments: all or nothing.
	today := time.Now().Format("02-01-2006")
	rep := func(loan string, member int64, amt string) map[string]any {
		return map[string]any{"loan_id": loan, "member_id": member, "account_id": acct, "amount": amt, "deposit_date": today, "deposit_method": "1"}
	}
	r = f.call("deposits/record_loan_repayments", map[string]any{"request_id": f.reqID(),
		"loan_repayments": []any{rep(mine, f.self, "1000"), rep(theirs, f.self, "500")}})
	wantStatus(t, "wrong member", r, float64(0))
	if n := f.count(`SELECT count(*) FROM loan_repayments`); n != 0 {
		t.Fatalf("partial repayments: %d", n)
	}
	r = f.call("deposits/record_loan_repayments", map[string]any{"request_id": f.reqID(),
		"loan_repayments": []any{rep(mine, f.self, "1000"), rep(theirs, f.other, "1100")}})
	wantStatus(t, "repayments", r, float64(1))

	st := f.call("loans/statement", map[string]any{"id": mine})["data"].(map[string]any)
	body := st["statement_body"].([]any)
	foot := st["statement_footer"].(map[string]any)
	if len(body) != 2 || body[1].(map[string]any)["balance"] != float64(3400) || foot["paid"] != float64(1000) || foot["balance"] != float64(3400) {
		t.Fatalf("statement: %v", st)
	}
	if l := f.call("loans/get_group_loans_list", map[string]any{"is_member_loans": "1", "member_id": f.other}); l["loans"].([]any)[0].(map[string]any)["is_fully_paid"] != float64(1) {
		t.Fatalf("their loan should be paid: %v", l)
	}

	// Dashboards and reports carry the loan figures.
	d := f.call("get_group_dashboard_data", nil)["group_details"].(map[string]any)
	if d["total_loaned_amount"] != float64(5000) || d["total_loan_repaid"] != float64(2100) || d["total_loan_balances"] != float64(3400) {
		t.Fatalf("group dashboard: %v", d)
	}
	md := f.call("get_member_dashboard_data", nil)["member_details"].(map[string]any)
	if md["total_loan_balances"] != float64(3400) {
		t.Fatalf("member dashboard: %v", md)
	}
	sum := f.call("reports/loans_summary", nil)["data"].(map[string]any)["statement_footer"].(map[string]any)
	if sum["total_loan"] != float64(5000) || sum["total_paid"] != float64(2100) || sum["total_balance"] != float64(3400) {
		t.Fatalf("loans summary: %v", sum)
	}
	ms := f.call("get_group_member_loans_summary", nil)["member_loans_summary"].(map[string]any)
	if ms["total_loan_amount"] != float64(4000) || ms["next_instalment_amount"] == float64(0) {
		t.Fatalf("member loans summary: %v", ms)
	}

	calc := f.call("loans/get_member_loan_calculator", map[string]any{"loan_type_id": typeID, "loan_amount": "4000"})
	if calc["amortization_totals"].(map[string]any)["total_payable"] != float64(4400) || len(calc["breakdown"].([]any)) != 4 {
		t.Fatalf("calculator: %v", calc)
	}
	calc = f.call("loan_applications/get_group_loan_calculator", map[string]any{"loan_type_id": typeID, "loan_amount": "4000"})
	if calc["breakdown"].([]any)[0].(map[string]any)["amount_payable"] != "1,100.00" {
		t.Fatalf("application calculator: %v", calc)
	}

	// Edit is refused once repaid; void works.
	r = f.call("loans/edit_member_loan", map[string]any{"id": mine, "member_id": f.self, "loan_amount": "3000", "disbursement_date": day,
		"account_id": acct, "repayment_period": "4", "loan_type_id": typeID})
	wantStatus(t, "edit repaid", r, float64(0))
	wantStatus(t, "void", f.call("loans/void", map[string]any{"id": mine}), float64(1))
	if l := list(t, f.call("loans/get_group_loans_list", nil), "loans"); len(l) != 1 {
		t.Fatalf("after void: %v", l)
	}
}

func TestMobileLoanApplications(t *testing.T) {
	f := newMoneyFixture(t)
	typeID := f.loanType(nil)
	f.call("loan_types/update_general_details", map[string]any{"id": typeID, "enable_loan_guarantors": "1",
		"loan_guarantors_type": "1", "minimum_guarantors": "1"})
	other := f.asOther()

	r := f.call("loan_applications/create_group_loan_application", map[string]any{"loan_product_id": typeID, "loan_amount": "5000"})
	wantStatus(t, "no guarantors", r, float64(0))
	r = f.call("loan_applications/create_group_loan_application", map[string]any{"loan_product_id": typeID, "loan_amount": "5000",
		"guarantors": []any{fmt.Sprint(f.other)}, "amounts": []any{"5000"}, "comments": []any{"Neighbour"}})
	wantStatus(t, "apply", r, float64(1))
	if r["message"] != "Loan Application Processed. Approval sent to all guarantors." {
		t.Fatalf("apply: %v", r)
	}
	appID := fmt.Sprint(r["loan_application_id"])

	mine := list(t, f.call("loan_applications/get_member_loan_applications", nil), "data")
	if len(mine) != 1 || mine[0]["status"] != "pending_guarantors" || mine[0]["loan_amount"] != "5000.00" {
		t.Fatalf("my applications: %v", mine)
	}
	asks := list(t, other("loan_applications/get_member_action_guarantorship_requests", nil), "data")
	if len(asks) != 1 || asks[0]["loan_application_id"] != appID || asks[0]["amount"] != "5000.00" {
		t.Fatalf("guarantor requests: %v", asks)
	}
	wantStatus(t, "not mine to guarantee", f.call("loan_applications/approve_loan_guarantorship_request",
		map[string]any{"loan_application_id": appID}), float64(0))
	wantStatus(t, "decline needs reason", other("loan_applications/decline_loan_guarantorship_request",
		map[string]any{"loan_application_id": appID}), float64(0))
	wantStatus(t, "guarantee", other("loan_applications/approve_loan_guarantorship_request",
		map[string]any{"loan_application_id": appID}), float64(1))

	// The fixture's only signatory is the applicant, so it is approved.
	g := list(t, f.call("loan_applications/get_loan_application_guarantorship_requests", map[string]any{"loan_application_id": appID}), "data")
	if len(g) != 1 || g[0]["is_approved"] != float64(1) {
		t.Fatalf("guarantors: %v", g)
	}
	all := list(t, f.call("loan_applications/get_group_loan_applications", nil), "data")
	if len(all) != 1 || all[0]["status"] != "approved" {
		t.Fatalf("applications: %v", all)
	}
	wantStatus(t, "void approved", f.call("loan_applications/void_group_loan_application", map[string]any{"id": appID}), float64(1))
	wantStatus(t, "void again", f.call("loan_applications/void_group_loan_application", map[string]any{"id": appID}), float64(0))

	// With a second signatory, the other member approves.
	f.exec(`UPDATE memberships SET group_role_id = (SELECT id FROM group_roles WHERE name = 'Chairperson') WHERE id = $1`, f.other)
	r = f.call("loan_applications/create_group_loan_application", map[string]any{"loan_product_id": typeID, "loan_amount": "2000",
		"guarantors": []any{fmt.Sprint(f.other)}, "amounts": []any{""}, "comments": []any{""}})
	wantStatus(t, "apply again", r, float64(1))
	appID = fmt.Sprint(r["loan_application_id"])
	other("loan_applications/approve_loan_guarantorship_request", map[string]any{"loan_application_id": appID})
	if a := list(t, other("loan_applications/get_member_action_approval_requests", nil), "data"); len(a) != 1 {
		t.Fatalf("approval requests: %v", a)
	}
	wantStatus(t, "bad action", other("loan_applications/approve_loan_application", map[string]any{"loan_application_id": appID, "action": "9"}), float64(0))
	wantStatus(t, "wrong password", other("loan_applications/approve_loan_application",
		map[string]any{"loan_application_id": appID, "action": "1", "password": "nope"}), float64(0))
	wantStatus(t, "approve", other("loan_applications/approve_loan_application",
		map[string]any{"loan_application_id": appID, "action": "1", "password": "other-pw"}), float64(1))
	ap := list(t, f.call("loan_applications/get_loan_application_approval_requests", map[string]any{"loan_application_id": appID}), "data")
	if len(ap) != 2 {
		t.Fatalf("approvals: %v", ap)
	}
	if n := f.count(`SELECT count(*) FROM loan_applications WHERE id = $1 AND status = 'approved'`, appID); n != 1 {
		t.Fatal("not approved")
	}
	wantStatus(t, "documents", f.call("loan_applications/upload_document", nil), float64(0))
}
