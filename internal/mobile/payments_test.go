package mobile

import (
	"context"
	"sync"
	"testing"

	"github.com/swahome/legion-api/internal/payments"
)

type stubGateway struct {
	mu     sync.Mutex
	orders []payments.Order
}

func (g *stubGateway) MakePayment(_ context.Context, o payments.Order) (payments.Answer, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.orders = append(g.orders, o)
	return payments.Answer{Accepted: true, Code: "0", Description: "Accepted"}, nil
}

func TestPaymentAndReconcileActions(t *testing.T) {
	f := newMoneyFixture(t)
	gw := &stubGateway{}
	f.srv.WithPayments(&payments.Service{Pool: f.srv.pool, Gateway: gw, PublicURL: "https://legion.test",
		HookKey: []byte("k"), DefaultCC: "254"})
	f.exec(`UPDATE group_accounts SET is_default = true WHERE id = $1`, f.bank)

	r := f.call("deposits/make_new_group_payment", map[string]any{"payment_for": "1", "contribution_id": f.contrib,
		"amount": "1,500", "phone_number": "0712345678"})
	if r["status"] != float64(1) || r["message"] != "Payment in progress. Please wait to enter pin" {
		t.Fatalf("make_new_group_payment: %v", r)
	}
	if len(gw.orders) != 1 || gw.orders[0].Amount != "1500.00" || gw.orders[0].Phone != "+254712345678" {
		t.Fatalf("order: %+v", gw.orders)
	}
	r = f.call("deposits/make_payment_to_group_bank_express", map[string]any{"payment_for": "2",
		"fine_category_id": f.fineCat, "amount": "200", "phone_number": "0712345678"})
	if r["status"] != float64(1) {
		t.Fatalf("express: %v", r)
	}
	if r = f.call("deposits/make_new_group_payment", map[string]any{"payment_for": "3", "amount": "200", "phone_number": "0712345678"}); r["status"] != float64(0) {
		t.Fatalf("loan payment with no loan: %v", r)
	}
	if r = f.call("deposits/make_group_arrears_payment", map[string]any{"amount": "700"}); r["status"] != float64(1) {
		t.Fatalf("arrears: %v", r)
	}
	if r = f.call("transaction_alerts/payment_request", map[string]any{"amount": "100", "contribution_id": f.contrib}); r["status"] != float64(1) {
		t.Fatalf("payment_request: %v", r)
	}
	var n int64
	_ = f.srv.pool.QueryRow(context.Background(), `SELECT count(*) FROM payment_requests WHERE channel = 'app' AND status = 'sent'`).Scan(&n)
	if n != 4 {
		t.Fatalf("payment requests: %d", n)
	}

	// A bank credit waiting to be reconciled.
	var alert int64
	if err := f.srv.pool.QueryRow(context.Background(), `INSERT INTO transaction_alerts (source, provider_ref, account_number,
		group_id, account_id, direction, amount, transaction_at, particulars)
		VALUES ('equity', 'EQ9', '0123', $1, $2, 1, 1200, now(), 'CASH DEPOSIT') RETURNING id`, f.group, f.bank).Scan(&alert); err != nil {
		t.Fatal(err)
	}
	r = f.call("transaction_alerts/get_group_unreconcilled_deposits", nil)
	deps := list(t, r, "unreconciled_deposits")
	if len(deps) != 1 || deps[0]["transactionAlertId"] != float64(alert) || deps[0]["amount"] != "1200.00" {
		t.Fatalf("unreconciled: %v", r)
	}
	if r = f.call("transaction_alerts/get_group_unreconcilled_withdrawals", nil); len(list(t, r, "unreconciled_withdrawals")) != 0 {
		t.Fatalf("withdrawals: %v", r)
	}
	r = f.call("transaction_alerts/reconcile_deposits", map[string]any{"transaction_alert_id": alert,
		"reconcile_deposits_break_down": []any{map[string]any{"deposit_for_type": 1, "member_id": f.other,
			"contribution_id": f.contrib, "amount": "1000"}}})
	if r["message"] != "The amount reconciled has to be equal to the amount deposited" {
		t.Fatalf("mismatch: %v", r)
	}
	r = f.call("transaction_alerts/reconcile_deposits", map[string]any{"transaction_alert_id": alert,
		"reconcile_deposits_break_down": []any{
			map[string]any{"deposit_for_type": 1, "member_id": f.other, "contribution_id": f.contrib, "amount": "1000"},
			map[string]any{"deposit_for_type": "2", "member_id": f.other, "fine_category_id": "fine_category-" + itoa(f.fineCat), "amount": "200"}}})
	if r["status"] != float64(1) || r["message"] != "Reconcillation successfully recorded" {
		t.Fatalf("reconcile: %v", r)
	}
	if r = f.call("transaction_alerts/reconcile_deposits", map[string]any{"transaction_alert_id": alert,
		"reconcile_deposits_break_down": []any{map[string]any{"deposit_for_type": 3, "member_id": f.other, "amount": "1200"}}}); r["message"] != "Transaction already reconciled" {
		t.Fatalf("again: %v", r)
	}
	_ = f.srv.pool.QueryRow(context.Background(), `SELECT count(*) FROM deposits WHERE transaction_alert_id = $1`, alert).Scan(&n)
	if n != 2 {
		t.Fatalf("deposits: %d", n)
	}
}

func itoa(n int64) string {
	b := []byte{}
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
