package mobile

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swahome/legion-api/internal/mail"
)

func today() string { return time.Now().Format("02-01-2006") }

func TestStocksAndMoneyMarket(t *testing.T) {
	f := newMoneyFixture(t)
	bank := fmt.Sprintf("bank-%d", f.bank)
	r := f.call("stocks/record_stock_purchase", map[string]any{"request_id": f.reqID(), "purchase_date": today(), "name": "Safaricom",
		"number_of_shares": "100", "account_id": bank, "price_per_share": "12.50"})
	wantStatus(t, "buy", r, float64(1))
	stocks := list(t, f.call("stocks/get_group_stocks_list", nil), "group_stocks")
	if len(stocks) != 1 {
		t.Fatalf("stocks: %v", stocks)
	}
	id := stocks[0]["id"]
	wantStatus(t, "price", f.call("stocks/update_stock_current_price", map[string]any{"id": id, "current_price": "15"}), float64(1))
	r = f.call("stocks/sell_stocks", map[string]any{"id": id, "account_id": bank, "sale_date": today(),
		"number_of_shares_to_be_sold": "500", "sale_price_per_share": "15"})
	wantStatus(t, "oversell", r, float64(0))
	r = f.call("stocks/sell_stocks", map[string]any{"id": id, "account_id": bank, "sale_date": today(),
		"number_of_shares_to_be_sold": "40", "sale_price_per_share": "15"})
	wantStatus(t, "sell", r, float64(1))
	if n := len(list(t, f.call("stocks/get_group_stock_sales_list", nil), "stock_sales")); n != 1 {
		t.Fatalf("sales: %d", n)
	}
	// 1000 opening + 2500 deposit - 700 rent - 1250 shares + 600 sale.
	if bal := f.count(`SELECT (initial_balance + coalesce((SELECT sum(direction * amount) FROM account_entries
		WHERE account_id = $1 AND voided_at IS NULL), 0))::int FROM group_accounts WHERE id = $1`, f.bank); bal != 2150 {
		t.Fatalf("balance after stocks: %d", bal)
	}
	wantStatus(t, "void stock", f.call("stocks/void", map[string]any{"id": id}), float64(1))
	if n := f.count(`SELECT count(*) FROM account_entries WHERE account_id = $1 AND voided_at IS NULL`, f.bank); n != 2 {
		t.Fatalf("voiding the stock should void its purchase and sale, %d entries left", n)
	}

	r = f.call("money_market_investments/create", map[string]any{"investment_institution_name": "CIC",
		"investment_date": today(), "investment_amount": "500", "withdrawal_account_id": bank})
	wantStatus(t, "invest", r, float64(1))
	inv := list(t, f.call("money_market_investments/get_money_market_investment_list", nil), "investments")
	if len(inv) != 1 {
		t.Fatalf("investments: %v", inv)
	}
	mid := inv[0]["id"]
	wantStatus(t, "top up", f.call("money_market_investments/topup", map[string]any{"id": mid, "top_up_date": today(),
		"top_up_amount": "100", "top_up_withdrawal_account_id": bank}), float64(1))
	wantStatus(t, "cash in", f.call("money_market_investments/cashin", map[string]any{"id": mid, "cash_in_date": today(),
		"cash_in_amount": "650", "cash_in_deposit_account_id": bank}), float64(1))
	if n := len(list(t, f.call("money_market_investments/get_money_market_investment_cashins", nil), "cashins")); n != 1 {
		t.Fatalf("cashins: %d", n)
	}
	wantStatus(t, "top up closed", f.call("money_market_investments/topup", map[string]any{"id": mid, "top_up_date": today(),
		"top_up_amount": "100", "top_up_withdrawal_account_id": bank}), float64(0))
}

func TestAssets(t *testing.T) {
	f := newMoneyFixture(t)
	wantStatus(t, "category", f.call("asset_categories/create", map[string]any{"name": "Land"}), float64(1))
	wantStatus(t, "duplicate category", f.call("asset_categories/create", map[string]any{"name": "land"}), float64(0))
	cats := list(t, f.call("asset_categories/get_group_asset_categories", nil), "asset_categories")
	if len(cats) != 1 {
		t.Fatalf("categories: %v", cats)
	}
	r := f.call("assets/create_asset", map[string]any{"name": "Plot 7", "cost": "900", "asset_category_id": cats[0]["id"]})
	wantStatus(t, "asset", r, float64(1))
	assets := list(t, f.call("assets/get_group_assets", nil), "asset")
	if len(assets) != 1 {
		t.Fatalf("assets: %v", assets)
	}
	r = f.call("assets/record_asset_purchase_payments", map[string]any{"request_id": f.reqID(), "payment_date": today(),
		"asset_id": assets[0]["id"], "account_id": fmt.Sprintf("bank-%d", f.bank), "payment_method": "1", "amount": "400"})
	wantStatus(t, "pay", r, float64(1))
	if n := len(list(t, f.call("assets/asset_purchase_payments", map[string]any{"id": assets[0]["id"]}), "payments")); n != 1 {
		t.Fatalf("payments: %d", n)
	}
}

func TestContributionTransferToFine(t *testing.T) {
	f := newMoneyFixture(t)
	r := f.call("deposits/record_contribution_transfer", map[string]any{"request_id": f.reqID(), "transfer_date": today(),
		"contribution_from_id": fmt.Sprint(f.contrib), "member_id": fmt.Sprint(f.self), "amount": "100", "transfer_to": "2",
		"fine_category_to_id": fmt.Sprintf("fine_category-%d", f.fineCat)})
	wantStatus(t, "transfer", r, float64(1))
	r = f.call("deposits/record_contribution_transfer", map[string]any{"request_id": f.reqID(), "transfer_date": today(),
		"contribution_from_id": fmt.Sprint(f.contrib), "member_id": fmt.Sprint(f.self), "amount": "999999", "transfer_to": "2",
		"fine_category_to_id": fmt.Sprintf("fine_category-%d", f.fineCat)})
	wantStatus(t, "transfer more than paid", r, float64(0))
	rows := list(t, f.call("deposits/get_group_contribution_transfers", nil), "deposits")
	if len(rows) != 1 || rows[0]["description"] != "Contribution transfer to Fine payment" {
		t.Fatalf("transfers: %v", rows)
	}
	// The fine statement shows it as paid.
	fs := f.call("statements/fine_statement", nil)
	body := fs["statement_body"].([]any)
	last := body[len(body)-1].(map[string]any)
	if !strings.HasPrefix(last["description"].(string), "Contribution transfer to fine") || last["paid"] != float64(100) {
		t.Fatalf("fine statement: %v", body)
	}
	wantStatus(t, "void", f.call("deposits/void_contribution_transfer", map[string]any{"id": rows[0]["id"]}), float64(1))
	if n := f.count(`SELECT count(*) FROM ledger_entries WHERE contribution_transfer_id IS NOT NULL AND voided_at IS NULL`); n != 0 {
		t.Fatalf("%d transfer entries left after void", n)
	}
}

func TestMeetingSyncsAndNotifies(t *testing.T) {
	f := newMoneyFixture(t)
	acct := map[string]any{"account_id": fmt.Sprintf("bank-%d", f.bank)}
	r := f.call("meetings/create_group_meeting", map[string]any{"title": "November", "venue": "Hall", "date": today() + " 14:00",
		"members": map[string]any{"present": []any{map[string]any{"id": f.self}}, "late": []any{map[string]any{"id": f.other}}},
		"agenda":  []any{"Savings"},
		"collections": map[string]any{
			"contributions": []any{
				map[string]any{"amount": "300", "account": acct, "member": map[string]any{"id": fmt.Sprint(f.other)},
					"contribution": map[string]any{"id": fmt.Sprint(f.contrib)}},
				map[string]any{"amount": "300", "account": acct, "member": map[string]any{"id": "999999"},
					"contribution": map[string]any{"id": fmt.Sprint(f.contrib)}},
			},
		}})
	wantStatus(t, "meeting", r, float64(1))
	if r["skipped_entries"] != float64(1) {
		t.Fatalf("want the unknown member skipped: %v", r)
	}
	m := list(t, f.call("meetings/get_group_meetings", nil), "meetings")
	if len(m) != 1 {
		t.Fatalf("meetings: %v", m)
	}
	n := f.call("notifications/get_all_group_member_notifications", nil)
	if n["notification_count"] != float64(1) || len(n["notifications"].([]any)) != 1 {
		t.Fatalf("notifications: %v", n)
	}
	wantStatus(t, "mark", f.call("notifications/mark_all_as_read", map[string]any{"mark_all_notifications_as_read": "1"}), float64(1))
	if c := f.call("notifications/get_group_member_notification_count", nil); c["notification_count"] != float64(0) {
		t.Fatalf("count after marking: %v", c)
	}
}

func TestProfileAndRoles(t *testing.T) {
	f := newMoneyFixture(t)
	r := f.call("users/update_name", map[string]any{"name": "Akinyi Wanjiru Otieno"})
	wantStatus(t, "name", r, float64(1))
	if n := f.count(`SELECT count(*) FROM users WHERE id = $1 AND first_name = 'Akinyi Wanjiru' AND last_name = 'Otieno'`, f.user); n != 1 {
		t.Fatal("name not saved")
	}
	wantStatus(t, "role", f.call("group_roles/create", map[string]any{"name": "Secretary"}), float64(1))
	wantStatus(t, "duplicate role", f.call("group_roles/create", map[string]any{"name": "secretary"}), float64(0))
	wantStatus(t, "suspend", f.call("members/suspend_member", map[string]any{"member_id": f.other, "id": f.other}), float64(1))
	wantStatus(t, "group name", f.call("groups/update_name", map[string]any{"name": "Umoja Women"}), float64(1))
}

func TestDashboardsAndCharts(t *testing.T) {
	f := newMoneyFixture(t)
	f.exec(`UPDATE account_entries SET membership_id = $1 WHERE deposit_id = $2`, f.self, f.deposit)
	d := f.call("get_member_dashboard", nil)
	gd := d["group_details"].(map[string]any)
	if gd["bank_balances"] != float64(2800) || len(gd["recent_transactions"].([]any)) == 0 {
		t.Fatalf("member dashboard: %v", gd)
	}
	if md := d["member_details"].(map[string]any); md["role"] != "Treasurer" {
		t.Fatalf("member details: %v", md)
	}
	b := f.call("get_group_bank_balances", nil)
	if b["total_bank_balance"] != float64(3100) {
		t.Fatalf("bank balances: %v", b)
	}
	ch := f.call("get_member_and_group_chart_data", nil)
	deps := ch["group_transactions"].(map[string]any)["deposits"].(map[string]any)
	if len(deps) != 12 || deps[time.Now().AddDate(0, 0, -2).Format("Jan")] != float64(2800) || ch["member_records"] != float64(1) {
		t.Fatalf("chart: %v", ch)
	}
	ios := f.call("get_ios_member_and_group_chart_data", nil)["member_transactions"].([]any)
	if len(ios) != 12 || ios[11].(map[string]any)["month"] != time.Now().Format("Jan") {
		t.Fatalf("ios chart: %v", ios)
	}
	ab := f.call("reports/account_balances", nil)["data"].(map[string]any)
	if ab["grand_total_balance"] != float64(3100) || len(ab["balances"].([]any)) != 2 {
		t.Fatalf("account balances: %v", ab)
	}
	wantStatus(t, "fines summary", f.call("get_group_member_fines_summary", nil), float64(1))
	if s := f.call("statements/total_group_savings", nil); s["total_group_contributions"] != "1,900.00" {
		t.Fatalf("savings: %v", s)
	}
}

type sentMail struct{ got []mail.Message }

func (m *sentMail) Send(_ context.Context, msg mail.Message) error {
	m.got = append(m.got, msg)
	return nil
}

func TestStatementFiles(t *testing.T) {
	f := newMoneyFixture(t)
	dir := t.TempDir()
	mailer := &sentMail{}
	f.srv.WithUploads(dir).WithStatements("https://legion.test/v1/legacy/uploads", mailer, nil)

	r := f.call("statements/download_statement", map[string]any{"type": "2"})
	wantStatus(t, "download", r, float64(1))
	r = f.call("statements/send_email_statement", map[string]any{"email": "not-an-email"})
	wantStatus(t, "bad email", r, float64(0))
	r = f.call("statements/send_email_statement", map[string]any{"email": "akinyi@example.com"})
	if r["message"] != "Request received. Statement will be sent to akinyi@example.com." {
		t.Fatalf("email: %v", r)
	}
	if n := f.srv.ProcessStatements(context.Background()); n != 2 {
		t.Fatalf("processed %d", n)
	}
	if n := f.count(`SELECT count(*) FROM statement_requests WHERE status = 'done'`); n != 2 {
		t.Fatalf("%d done", n)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "statements", "*.pdf"))
	if len(files) != 2 {
		t.Fatalf("files: %v", files)
	}
	b, _ := os.ReadFile(files[0])
	if !strings.HasPrefix(string(b), "%PDF-") {
		t.Fatal("not a PDF")
	}
	if len(mailer.got) != 1 || mailer.got[0].To != "akinyi@example.com" || !strings.HasPrefix(string(mailer.got[0].File), "%PDF-") {
		t.Fatalf("mail: %+v", mailer.got)
	}
	n := f.call("notifications/get_all_group_member_notifications", nil)["notifications"].([]any)
	if len(n) != 1 {
		t.Fatalf("notifications: %v", n)
	}
	note := n[0].(map[string]any)
	if note["subject"] != "File Type Ready" || note["category"] != float64(16) || note["call_to_action"] != "View File" ||
		!strings.HasPrefix(note["file_path"].(string), "https://legion.test/v1/legacy/uploads/statements/") {
		t.Fatalf("notification: %v", note)
	}
	// The link is served.
	rel := strings.TrimPrefix(note["file_path"].(string), "https://legion.test/v1/legacy")
	res, err := httpGet(f.url + rel)
	if err != nil || res != 200 {
		t.Fatalf("GET %s: %d %v", rel, res, err)
	}

	// Without email set up, the request fails after its tries.
	f.srv.WithStatements("https://legion.test/v1/legacy/uploads", nil, nil)
	f.call("statements/send_email_statement", nil)
	for i := 0; i < 3; i++ {
		f.srv.ProcessStatements(context.Background())
	}
	if n := f.count(`SELECT count(*) FROM statement_requests WHERE status = 'failed' AND attempts = 3`); n != 1 {
		t.Fatalf("want one failed request, got %d", n)
	}
}

func TestBillingAndBanks(t *testing.T) {
	f := newMoneyFixture(t)
	f.exec(`INSERT INTO billing_invoices (group_id, number, due_date, amount, tax, prorated_amount, amount_paid, billing_cycle, is_paid)
		VALUES ($1, 'INV-1', now(), 1160, 160, 0, 1160, 1, true)`, f.group)
	inv := f.call("billing/get_group_billing_invoices", nil)
	rows := inv["invoices"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["subscription"] != float64(1000) || rows[0].(map[string]any)["status"] != "Paid" {
		t.Fatalf("invoices: %v", inv)
	}
	wantStatus(t, "pay subscription", f.call("billing/make_mpesa_payment", nil), float64(0))

	f.exec(`UPDATE group_accounts SET is_verified = false WHERE id = $1`, f.bank)
	bankID := f.count(`SELECT bank_id::int FROM group_accounts WHERE id = $1`, f.bank)
	req := map[string]any{"bank_id": bankID, "id": f.bank, "signatory_phone": "0712345678"}
	if r := f.call("bank_accounts/connect", req); r["message"] != bankDown {
		t.Fatalf("connect without a bank link: %v", r)
	}
	f.srv.WithBankLinker(FakeBankLinker{})
	wantStatus(t, "connect", f.call("bank_accounts/connect", req), float64(1))
	req["verification_code"] = "ab12"
	wantStatus(t, "verify", f.call("bank_accounts/verify", req), float64(1))
	if n := f.count(`SELECT count(*) FROM group_accounts WHERE id = $1 AND is_verified`, f.bank); n != 1 {
		t.Fatal("not verified")
	}
	wantStatus(t, "connect again", f.call("bank_accounts/connect", req), float64(0))
	wantStatus(t, "disconnect", f.call("bank_accounts/disconnect", map[string]any{"id": f.bank}), float64(1))

	if r := f.call("deposits/calculate_convenience_charge", map[string]any{"amount": "100"}); r["status"] != float64(0) {
		t.Fatalf("charge without payments: %v", r)
	}
}

func httpGet(url string) (int, error) {
	res, err := http.Get(url)
	if err != nil {
		return 0, err
	}
	res.Body.Close()
	return res.StatusCode, nil
}
