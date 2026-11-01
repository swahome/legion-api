package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/swahome/legion-api/internal/payments"
)

// recordingGateway accepts every order and remembers it.
type recordingGateway struct {
	mu     sync.Mutex
	orders []payments.Order
	refuse bool
}

func (g *recordingGateway) MakePayment(_ context.Context, o payments.Order) (payments.Answer, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.orders = append(g.orders, o)
	if g.refuse {
		return payments.Answer{Code: "1", Description: "Invalid phone"}, nil
	}
	return payments.Answer{Accepted: true, Code: "0", Description: "Success. Request accepted for processing"}, nil
}

type payFixture struct {
	*moneyFixture
	pay      *payments.Service
	gw       *recordingGateway
	account  int64 // Jirani's Equity account 0110012345
	otherAcc int64 // Imani's account at another bank
	equity   int64 // the "equity-bank" bank
}

func newPayFixture(t *testing.T) *payFixture {
	f := &payFixture{moneyFixture: newMoneyFixture(t), gw: &recordingGateway{}}
	f.pay = &payments.Service{Pool: f.pool, Gateway: f.gw, PublicURL: "https://legion.test",
		HookKey: []byte("hook-key"), DefaultCC: "254"}
	f.equity = f.scalar(`INSERT INTO banks (name, slug) VALUES ('Equity Bank Kenya', 'equity-bank') RETURNING id`)
	f.scalar(`INSERT INTO banks (name, slug) VALUES ('Co-operative Bank', 'co-operative-bank-of-kenya') RETURNING id`)
	f.account = f.scalar(`INSERT INTO group_accounts (group_id, kind, name, account_number, bank_id, is_default)
		VALUES ($1, 'bank', 'Equity main', '0110012345', $2, true) RETURNING id`, f.group, f.equity)
	f.otherAcc = f.scalar(`INSERT INTO group_accounts (group_id, kind, name, account_number, bank_id)
		VALUES ($1, 'bank', 'Imani bank', '0220099999', $2) RETURNING id`, f.other, f.bank)
	// Rebuild the handler with payments switched on.
	f.h = f.srv.WithPayments(f.pay, HookConfig{
		Equity:          HookAuth{Username: "equity", Password: "s3cret"},
		Coop:            HookAuth{IPs: []string{"10.1.1.1"}},
		ChamasoftDirect: HookAuth{IPs: []string{"10.2.2.2"}},
	}).Routes()
	return f
}

func (f *payFixture) hook(path string, body any, ip string) *httpResult {
	f.t.Helper()
	rec := f.do("POST", "/v1/hooks"+path, body, func(r *http.Request) {
		if ip != "" {
			r.RemoteAddr = ip + ":443"
		}
	})
	res := &httpResult{code: rec.Code, raw: rec.Body.String()}
	_ = json.Unmarshal(rec.Body.Bytes(), &res.body)
	return res
}

func (f *payFixture) equityAlert(tranid, drcr, amount, particulars string) *httpResult {
	return f.hook("/equity", map[string]any{"username": "equity", "password": "s3cret", "tranid": tranid,
		"tranCurrency": "KES", "tranDate": "2026-10-20 10:15:00", "tranAmount": amount, "trandrcr": drcr,
		"accid": "0110012345", "refNo": "REF" + tranid, "tranType": "CASH", "tranParticular": particulars,
		"tranRemarks": "0"}, "")
}

func (f *payFixture) alertID(provider string) int64 {
	return f.scalar(`SELECT id FROM transaction_alerts WHERE provider_ref = $1`, provider)
}

func (f *payFixture) memberBalance() string {
	var b string
	if err := f.pool.QueryRow(context.Background(), `SELECT coalesce(sum(r.sign * e.amount), 0)::text FROM ledger_entries e
		JOIN ledger_entry_rules r USING (entry_type) WHERE e.membership_id = $1 AND e.voided_at IS NULL
		AND r.bucket = 'contribution'`, f.member).Scan(&b); err != nil {
		f.t.Fatal(err)
	}
	return b
}

func TestEquityFeedAndManualReconciliation(t *testing.T) {
	f := newPayFixture(t)

	// Wrong credentials, as the legacy endpoint answered.
	r := f.hook("/equity", map[string]any{"username": "equity", "password": "nope", "tranid": "X1"}, "")
	if r.body["message"] != "Access Denied" || r.body["responseCode"] != float64(2) {
		t.Fatalf("bad credentials: %s", r.raw)
	}
	r = f.equityAlert("EQ1", "C", "1,000.00", "CASH DEP BY WANJIRU 0700000002")
	if r.body["message"] != "Request OK" || r.body["ACK"] != "OK" || r.body["responseCode"] != float64(0) {
		t.Fatalf("equity: %s", r.raw)
	}
	if r = f.equityAlert("EQ1", "C", "1000", ""); r.body["message"] != "Duplicate Request" {
		t.Fatalf("duplicate: %s", r.raw)
	}
	f.equityAlert("EQ2", "D", "250", "ATM WITHDRAWAL")
	var pw string
	_ = f.pool.QueryRow(context.Background(), `SELECT body FROM webhook_events WHERE source = 'equity' ORDER BY id LIMIT 1`).Scan(&pw)
	if strings.Contains(pw, "nope") || strings.Contains(pw, "s3cret") {
		t.Fatalf("webhook log keeps the feed password: %s", pw)
	}

	// Members cannot see the feed; officials can.
	expect(t, f.get(f.memberTok, "/transaction-alerts"), 403)
	r = f.get(f.officialTok, "/transaction-alerts?status=unreconciled")
	expect(t, r, 200)
	if items := r.list("items"); len(items) != 2 {
		t.Fatalf("alerts: %s", r.raw)
	}
	r = f.get(f.officialTok, "/transaction-alerts/summary")
	if r.body["in"].(map[string]any)["amount"] != "1000.00" || r.body["out"].(map[string]any)["count"] != float64(1) {
		t.Fatalf("summary: %s", r.raw)
	}

	// Auto-reconcile is off for bank feeds by default, but the detail
	// suggests the member found by the phone in the narration.
	eq1 := f.alertID("EQ1")
	r = f.get(f.officialTok, fmt.Sprintf("/transaction-alerts/%d", eq1))
	expect(t, r, 200)
	sug, _ := r.body["suggestion"].(map[string]any)
	if r.body["status"] != "unreconciled" || sug == nil || sug["member_id"] != float64(f.member) {
		t.Fatalf("detail: %s", r.raw)
	}

	path := fmt.Sprintf("/transaction-alerts/%d", eq1)
	// Lines must add up.
	r = f.post(path+"/reconcile", map[string]any{"lines": []map[string]any{
		{"kind": "contribution", "member_id": f.member, "contribution_id": f.contribution, "amount": "900"}}})
	if r.code != 400 || r.body["error"].(map[string]any)["code"] != "amount_mismatch" {
		t.Fatalf("mismatch: %s", r.raw)
	}
	before := f.memberBalance()
	r = f.post(path+"/reconcile", map[string]any{"lines": []map[string]any{
		{"kind": "contribution", "member_id": f.member, "contribution_id": f.contribution, "amount": "800"},
		{"kind": "fine", "member_id": f.member, "fine_category_id": f.fineCat, "amount": "200"}}})
	expect(t, r, 200)
	if r.body["status"] != "reconciled" || len(r.list("deposits")) != 2 {
		t.Fatalf("reconcile: %s", r.raw)
	}
	if after := f.memberBalance(); before == after {
		t.Fatalf("member balance did not move: %s", after)
	}
	// Twice is a conflict.
	expect(t, f.post(path+"/reconcile", map[string]any{"lines": []map[string]any{
		{"kind": "misc", "member_id": f.member, "amount": "1000"}}}), 409)

	// Undo voids the deposits it wrote and reopens the alert.
	r = f.post(path+"/undo", map[string]any{})
	expect(t, r, 200)
	if r.body["status"] != "unreconciled" || f.memberBalance() != before {
		t.Fatalf("undo: %s balance %s", r.raw, f.memberBalance())
	}

	// Match to a deposit recorded by hand on the same account.
	dep := f.post("/deposits", map[string]any{"kind": "misc", "account_id": f.account, "amount": "1000",
		"date": "2026-10-20", "member_id": f.member, "method": "cash"})
	expect(t, dep, 201)
	depID := dep.body["id"]
	r = f.post(path+"/match", map[string]any{"deposit_ids": []any{depID}})
	expect(t, r, 200)
	if d := r.list("deposits"); len(d) != 1 || d[0]["from_alert"] != false {
		t.Fatalf("match: %s", r.raw)
	}
	r = f.post(path+"/undo", map[string]any{})
	expect(t, r, 200)
	if v := f.scalar(`SELECT count(*) FROM deposits WHERE id = $1 AND voided_at IS NULL AND transaction_alert_id IS NULL`, depID); v != 1 {
		t.Fatalf("undoing a match must only unlink the deposit")
	}

	// Money out: record the expense it was.
	eq2 := f.alertID("EQ2")
	exp := f.scalar(`INSERT INTO expense_categories (group_id, name) VALUES ($1, 'Bank charges') RETURNING id`, f.group)
	r = f.post(fmt.Sprintf("/transaction-alerts/%d/reconcile", eq2), map[string]any{"lines": []map[string]any{
		{"kind": "expense", "expense_category_id": exp, "amount": "250"}}})
	expect(t, r, 200)
	if len(r.list("withdrawals")) != 1 {
		t.Fatalf("withdrawal: %s", r.raw)
	}

	// Dismiss needs a note.
	expect(t, f.post(path+"/dismiss", map[string]any{"status": "ignored"}), 400)
	r = f.post(path+"/dismiss", map[string]any{"status": "ignored", "note": "Bank reversed it"})
	expect(t, r, 200)
	if r.body["status"] != "ignored" || r.body["note"] != "Bank reversed it" {
		t.Fatalf("dismiss: %s", r.raw)
	}
}

func TestCoopFeedAutoReconciles(t *testing.T) {
	f := newPayFixture(t)
	f.exec(`UPDATE group_accounts SET bank_id = (SELECT id FROM banks WHERE slug = 'co-operative-bank-of-kenya') WHERE id = $1`, f.account)
	coop := func(id, amount, narration string) *httpResult {
		return f.hook("/coop", map[string]any{"TransactionId": id, "AcctNo": "0110012345", "Amount": amount,
			"Currency": "KES", "EventType": "CREDIT", "PaymentRef": "P" + id, "Narration": narration,
			"PostingDate": "2026-10-21", "TransactionDate": "2026-10-21 09:00:00"}, "10.1.1.1")
	}
	if r := coop("C0", "10", ""); r.code != 200 {
		t.Fatalf("coop: %s", r.raw)
	}
	if r := f.hook("/coop", map[string]any{"TransactionId": "C9"}, "10.9.9.9"); r.body["message"] != "Access Denied" {
		t.Fatalf("coop from an unknown IP: %s", r.raw)
	}

	// Off by default for bank feeds.
	coop("C1", "1500", "MPESA DEP 254700000002")
	if v := f.scalar(`SELECT count(*) FROM transaction_alerts WHERE provider_ref = 'C1' AND status = 'unreconciled'`); v != 1 {
		t.Fatal("bank feed reconciled without the group asking")
	}
	expect(t, f.call("PATCH", f.officialTok, "/reconciliation-settings", map[string]any{
		"auto_reconcile_payments": true, "auto_reconcile_bank_alerts": true}), 200)
	r := coop("C2", "1500", "MPESA DEP 254700000002")
	if r.body["MessageCode"] != float64(200) {
		t.Fatalf("coop ack: %s", r.raw)
	}
	// Otieno owes 799.50 on savings (Jan) and 300 on the officials fund
	// (Mar); the rest goes to savings.
	rows, err := f.pool.Query(context.Background(), `SELECT d.contribution_id, d.amount::text FROM deposits d
		JOIN transaction_alerts t ON t.id = d.transaction_alert_id WHERE t.provider_ref = 'C2' ORDER BY d.id`)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]string{}
	for rows.Next() {
		var c int64
		var a string
		_ = rows.Scan(&c, &a)
		got[c] = a
	}
	if got[f.contribution] != "1200.00" || got[f.hiddenContribution] != "300.00" || len(got) != 2 {
		t.Fatalf("allocation: %v", got)
	}
	if v := f.scalar(`SELECT count(*) FROM transaction_alerts WHERE provider_ref = 'C2' AND status = 'reconciled' AND auto_reconciled`); v != 1 {
		t.Fatal("C2 not auto-reconciled")
	}
}

func TestMemberPaymentAndCallback(t *testing.T) {
	f := newPayFixture(t)
	r := f.get(f.memberTok, "/pay/options")
	expect(t, r, 200)
	if r.body["enabled"] != true || r.body["phone"] != "+254700000002" || len(r.list("owed")) == 0 {
		t.Fatalf("options: %s", r.raw)
	}
	// Bad lines are refused before anything is sent.
	r = f.call("POST", f.memberTok, "/payments", map[string]any{"lines": []map[string]any{
		{"kind": "fine", "amount": "100"}}})
	expect(t, r, 400)
	r = f.call("POST", f.memberTok, "/payments", map[string]any{"phone": "0711 222333", "lines": []map[string]any{
		{"kind": "contribution", "contribution_id": f.contribution, "amount": "500"},
		{"kind": "fine", "fine_category_id": f.fineCat, "amount": "150.5"}}})
	expect(t, r, 201)
	if r.body["status"] != "sent" || r.body["amount"] != "650.50" || r.body["phone"] != "+254711222333" {
		t.Fatalf("start: %s", r.raw)
	}
	pid := int64(r.body["id"].(float64))
	if len(f.gw.orders) != 1 {
		t.Fatal("no order sent")
	}
	o := f.gw.orders[0]
	if o.AccountNumber != "0110012345" || o.Amount != "650.50" || !strings.Contains(o.CallbackURL, "/v1/hooks/chamasoft/payments/"+o.Reference+"?key=") {
		t.Fatalf("order: %+v", o)
	}
	// Officials see it; another member cannot open it.
	if items := f.get(f.officialTok, "/payments").list("items"); len(items) != 1 {
		t.Fatal("official cannot see the payment")
	}

	cb, _ := url.Parse(o.CallbackURL)
	body := map[string]any{"code": "200", "description": "Success", "data": map[string]any{
		"reference_number": o.Reference, "transaction_id": "QJK1ABC23", "transaction_date": "2026-10-22 08:00:00",
		"phone_number": "254711222333", "account_number": "0110012345", "payment_description": "MPesa payment"}}
	if r := f.hook(strings.TrimPrefix(cb.Path, "/v1/hooks")+"?key=wrong", body, ""); r.body["code"] != "1" {
		t.Fatalf("forged callback: %s", r.raw)
	}
	before := f.memberBalance()
	r = f.hook(strings.TrimPrefix(cb.RequestURI(), "/v1/hooks"), body, "")
	if r.body["code"] != "0" {
		t.Fatalf("callback: %s", r.raw)
	}
	if r := f.hook(strings.TrimPrefix(cb.RequestURI(), "/v1/hooks"), body, ""); r.body["response"] != "Duplicate deposit" {
		t.Fatalf("second callback: %s", r.raw)
	}
	r = f.get(f.memberTok, fmt.Sprintf("/payments/%d", pid))
	if r.body["status"] != "paid" || r.body["receipt"] != "QJK1ABC23" {
		t.Fatalf("payment: %s", r.raw)
	}
	if v := f.scalar(`SELECT count(*) FROM deposits d JOIN transaction_alerts t ON t.id = d.transaction_alert_id
		WHERE t.provider_ref = 'QJK1ABC23' AND t.status = 'reconciled' AND d.method = 'mpesa'`); v != 2 {
		t.Fatalf("deposits from the payment: %d", v)
	}
	if f.memberBalance() == before {
		t.Fatal("balance unchanged")
	}
	if v := f.scalar(`SELECT count(*) FROM sms_messages WHERE kind = 'payment_received' AND to_phone = '+254711222333'`); v != 1 {
		t.Fatal("no receipt text")
	}

	// A cancelled prompt.
	r = f.call("POST", f.memberTok, "/payments", map[string]any{"amount": "300"})
	expect(t, r, 201)
	o = f.gw.orders[1]
	cb, _ = url.Parse(o.CallbackURL)
	f.hook(strings.TrimPrefix(cb.RequestURI(), "/v1/hooks"), map[string]any{"code": "1032", "description": "Request cancelled by user",
		"data": map[string]any{"reference_number": o.Reference}}, "")
	r = f.get(f.memberTok, fmt.Sprintf("/payments/%d", int64(r.body["id"].(float64))))
	if r.body["status"] != "failed" || r.body["message"] != "Request cancelled by user" {
		t.Fatalf("cancelled: %s", r.raw)
	}

	// The gateway refusing the order.
	f.gw.refuse = true
	r = f.call("POST", f.memberTok, "/payments", map[string]any{"amount": "100"})
	expect(t, r, 201)
	if r.body["status"] != "failed" || r.body["message"] != "Invalid phone" {
		t.Fatalf("refused: %s", r.raw)
	}
}

func TestDirectAndMpesaC2B(t *testing.T) {
	f := newPayFixture(t)
	direct := map[string]any{"code": "200", "description": "Payment to account 0110012345", "data": map[string]any{
		"reference_number": "998877", "phone_number": "254700000002", "account_number": "0110012345", "amount": "500",
		"transaction_date": "1761030000", "transaction_id": "QJK9DIRECT", "customer_name": "OTIENO"}}
	if r := f.hook("/chamasoft/direct", direct, "10.9.9.9"); r.body["response"] != "Unknown host" {
		t.Fatalf("direct from unknown host: %s", r.raw)
	}
	if r := f.hook("/chamasoft/direct", direct, "10.2.2.2"); r.body["code"] != "0" {
		t.Fatalf("direct: %s", r.raw)
	}
	if v := f.scalar(`SELECT count(*) FROM transaction_alerts WHERE provider_ref = 'QJK9DIRECT' AND status = 'reconciled'`); v != 1 {
		t.Fatal("direct payment not reconciled to the member")
	}

	key := f.pay.HookSecret("mpesa")
	val := map[string]any{"TransactionType": "Pay Bill", "TransID": "RKT1C2B", "TransTime": "20261023101500",
		"TransAmount": "200.00", "BusinessShortCode": "546448", "BillRefNumber": "9999999", "MSISDN": "254799000000",
		"FirstName": "John", "LastName": "Doe"}
	if r := f.hook("/mpesa/validation?key="+key, val, ""); r.body["ResultCode"] != "C2B00012" {
		t.Fatalf("validation of an unknown account: %s", r.raw)
	}
	val["BillRefNumber"] = "0110012345"
	if r := f.hook("/mpesa/validation?key="+key, val, ""); r.body["ResultCode"] != float64(0) {
		t.Fatalf("validation: %s", r.raw)
	}
	if r := f.hook("/mpesa/confirmation?key=nope", val, ""); r.body["ResultCode"] != float64(1) {
		t.Fatalf("confirmation with a bad key: %s", r.raw)
	}
	if r := f.hook("/mpesa/confirmation?key="+key, val, ""); r.body["ResultCode"] != float64(0) {
		t.Fatalf("confirmation: %s", r.raw)
	}
	// An unknown payer stays for the officials.
	r := f.get(f.officialTok, "/transaction-alerts?status=unreconciled&source=mpesa")
	if items := r.list("items"); len(items) != 1 || items[0]["payer_name"] != "John Doe" || items[0]["amount"] != "200.00" {
		t.Fatalf("mpesa alert: %s", r.raw)
	}
}

func TestUnassignedAlertsAndBankPanel(t *testing.T) {
	f := newPayFixture(t)
	// An account number nobody has: the admins assign it.
	f.hook("/equity", map[string]any{"username": "equity", "password": "s3cret", "tranid": "LOST1", "tranAmount": "75",
		"trandrcr": "C", "accid": "000", "tranDate": "2026-10-20"}, "")
	r := f.admin("GET", f.adminTok, "/admin/transaction-alerts?unassigned=true", nil)
	expect(t, r, 200)
	if items := r.list("items"); len(items) != 1 || items[0]["group"] != nil {
		t.Fatalf("unassigned: %s", r.raw)
	}
	lost := f.alertID("LOST1")
	r = f.admin("POST", f.adminTok, fmt.Sprintf("/admin/transaction-alerts/%d/assign", lost), map[string]any{"account_id": f.account})
	expect(t, r, 200)
	if r.body["group"].(map[string]any)["id"] != float64(f.group) {
		t.Fatalf("assign: %s", r.raw)
	}
	expect(t, f.admin("POST", f.adminTok, fmt.Sprintf("/admin/transaction-alerts/%d/assign", lost), map[string]any{"account_id": f.account}), 400)
	expect(t, f.admin("GET", f.adminTok, "/admin/webhooks", nil), 200)

	// Bank staff.
	f.equityAlert("EQ7", "C", "300", "DEPOSIT")
	bankPath := fmt.Sprintf("/v1/bank/%d", f.equity)
	bank := func(tok, method, path string, body any) *httpResult {
		rec := f.do(method, bankPath+path, body, withBearer(tok))
		res := &httpResult{code: rec.Code, raw: rec.Body.String()}
		_ = json.Unmarshal(rec.Body.Bytes(), &res.body)
		return res
	}
	if r := bank(f.officialTok, "GET", "/groups", nil); r.code != 404 {
		t.Fatalf("non-staff opened the bank panel: %s", r.raw)
	}
	expect(t, bank(f.adminTok, "POST", "/users", map[string]any{"phone": "0700000001", "role": "staff"}), 200)
	r = bank(f.officialTok, "GET", "/groups", nil)
	expect(t, r, 200)
	if items := r.list("items"); len(items) != 1 || items[0]["name"] != "Jirani" {
		t.Fatalf("bank groups: %s", r.raw)
	}
	r = bank(f.officialTok, "GET", fmt.Sprintf("/groups/%d", f.group), nil)
	if r.code != 200 || len(r.list("accounts")) != 1 {
		t.Fatalf("bank group: %s", r.raw)
	}
	expect(t, bank(f.officialTok, "GET", fmt.Sprintf("/groups/%d", f.other), nil), 404)
	r = bank(f.officialTok, "GET", "/alerts", nil)
	if items := r.list("items"); len(items) != 2 {
		t.Fatalf("bank alerts: %s", r.raw)
	}
	r = bank(f.officialTok, "GET", "/dashboard?from=2026-10-01&to=2026-10-31", nil)
	expect(t, r, 200)
	if r.body["totals"].(map[string]any)["money_in"] != "375.00" {
		t.Fatalf("dashboard: %s", r.raw)
	}
	// Staff cannot manage users.
	expect(t, bank(f.officialTok, "POST", "/users", map[string]any{"phone": "0700000002"}), 403)
	rec := f.do("GET", "/v1/me", nil, withBearer(f.officialTok))
	if !strings.Contains(rec.Body.String(), `"banks":[{"id":`) {
		t.Fatalf("me: %s", rec.Body)
	}
}
