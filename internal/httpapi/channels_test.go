package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/swahome/legion-api/internal/channels"
)

type chanFixture struct {
	*payFixture
	eng     *channels.Engine
	ussdKey string
	waKey   string
}

const twilioToken = "twilio-token"

func newChanFixture(t *testing.T) *chanFixture {
	f := &chanFixture{payFixture: newPayFixture(t)}
	f.eng = &channels.Engine{Pool: f.pool, Payments: f.pay, DefaultCC: "254", Brand: "Legion", WebURL: "https://legion.test",
		Later: func(_ time.Duration, fn func()) { fn() }}
	key := []byte("hook-key")
	f.ussdKey, f.waKey = ChannelHookKey(key, "ussd"), ChannelHookKey(key, "whatsapp")
	f.h = f.srv.WithChannels(f.eng, ChannelConfig{HookKey: key, TwilioToken: twilioToken, PublicURL: "https://legion.test"}).Routes()
	return f
}

func (f *chanFixture) form(path string, form url.Values, mods ...func(*http.Request)) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "203.0.113.7:5555"
	for _, m := range mods {
		m(req)
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

// dial sends one USSD step as Africa's Talking does: the whole answer
// string so far.
func (f *chanFixture) dial(session, phone, text string) string {
	f.t.Helper()
	rec := f.form("/v1/hooks/ussd?key="+f.ussdKey, url.Values{"sessionId": {session}, "serviceCode": {"*384*123#"},
		"phoneNumber": {phone}, "networkCode": {"63902"}, "text": {text}})
	if rec.Code != 200 {
		f.t.Fatalf("ussd %q: %d %s", text, rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	if !strings.HasPrefix(out, "CON ") && !strings.HasPrefix(out, "END ") {
		f.t.Fatalf("ussd %q: no CON/END: %q", text, out)
	}
	if n := len([]rune(out)); n > 182 {
		f.t.Fatalf("ussd %q: %d characters: %q", text, n, out)
	}
	return out
}

func mustContain(t *testing.T, got string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Fatalf("missing %q in:\n%s", w, got)
		}
	}
}

func TestUSSDBalancesAndPayment(t *testing.T) {
	f := newChanFixture(t)

	if rec := f.form("/v1/hooks/ussd?key=wrong", url.Values{"sessionId": {"s"}, "phoneNumber": {"+254700000002"}}); rec.Code != 403 {
		t.Fatalf("wrong key: %d", rec.Code)
	}
	mustContain(t, f.dial("s0", "+254799999999", ""), "END This number is not registered on Legion", "https://legion.test")

	// Otieno is in two groups, so he chooses one first.
	out := f.dial("s1", "+254700000002", "")
	mustContain(t, out, "CON Welcome", "1. Imani", "2. Jirani", "0 Exit")
	out = f.dial("s1", "+254700000002", "2")
	mustContain(t, out, "Jirani. Choose:", "1. My account", "2. Make a payment", "3. Loans", "4. Switch group", "0 Back 00 Home")
	if strings.Contains(out, "withdrawal") {
		t.Fatalf("a member sees withdrawals: %s", out)
	}
	// A retry of the same step gets the same screen and does not move on.
	if again := f.dial("s1", "+254700000002", "2"); again != out {
		t.Fatalf("retry: %q", again)
	}
	out = f.dial("s1", "+254700000002", "2*1*1")
	mustContain(t, out, "Contributions paid: KES 2,200.50", "You owe: KES 1,099.50", "1. Pay a contribution")
	mustContain(t, f.dial("s1", "+254700000002", "2*1*1*0"), "My account:")
	mustContain(t, f.dial("s1", "+254700000002", "2*1*1*0*7"), "Please reply with one of the numbers shown.", "My account:")
	mustContain(t, f.dial("s1", "+254700000002", "2*1*1*0*7*00"), "Jirani. Choose:")

	// Paying: contribution, amount, confirm. The prompt goes to the caller.
	s := "2*1*1*0*7*00"
	mustContain(t, f.dial("s1", "+254700000002", s+"*2"), "What are you paying for?")
	mustContain(t, f.dial("s1", "+254700000002", s+"*2*1"), "Choose the contribution:", "1. Monthly savings")
	mustContain(t, f.dial("s1", "+254700000002", s+"*2*1*1"), "You owe KES 799.50 for Monthly savings")
	mustContain(t, f.dial("s1", "+254700000002", s+"*2*1*1*abc"), "Enter an amount in numbers")
	mustContain(t, f.dial("s1", "+254700000002", s+"*2*1*1*abc*500"), "Pay KES 500.00 for Monthly savings from MPesa 0700000002?")
	final := s + "*2*1*1*abc*500*1"
	out = f.dial("s1", "+254700000002", final)
	mustContain(t, out, "END You will get an MPesa prompt on 0700000002")
	if again := f.dial("s1", "+254700000002", final); again != out {
		t.Fatalf("retry after the end: %q", again)
	}
	if len(f.gw.orders) != 1 || f.gw.orders[0].Amount != "500.00" || f.gw.orders[0].Phone != "+254700000002" {
		t.Fatalf("orders: %+v", f.gw.orders)
	}
	var channel, lines string
	if err := f.pool.QueryRow(context.Background(), `SELECT channel, lines::text FROM payment_requests`).Scan(&channel, &lines); err != nil {
		t.Fatal(err)
	}
	if channel != "ussd" || !strings.Contains(lines, fmt.Sprintf(`"contribution_id": %d`, f.contribution)) {
		t.Fatalf("payment request: %s %s", channel, lines)
	}
	mustContain(t, f.dial("s1", "+254700000002", final+"*1"), "END This session has ended")

	// The transcript is there for support.
	r := f.admin("GET", f.adminTok, "/admin/channel-sessions?channel=ussd&phone=0000002", nil)
	expect(t, r, 200)
	items := r.list("items")
	if len(items) != 1 || items[0]["group"].(map[string]any)["name"] != "Jirani" || items[0]["ended_at"] == nil {
		t.Fatalf("sessions: %s", r.raw)
	}
	r = f.admin("GET", f.adminTok, fmt.Sprintf("/admin/channel-sessions/%d", int64(items[0]["id"].(float64))), nil)
	expect(t, r, 200)
	if msgs := r.list("messages"); len(msgs) < 10 || msgs[0]["input"] != "" {
		t.Fatalf("messages: %s", r.raw)
	}
	expect(t, f.admin("GET", f.memberTok, "/admin/channel-sessions", nil), 403)
}

func TestUSSDWithdrawalRequestAndApproval(t *testing.T) {
	f := newChanFixture(t)
	f.exec(`UPDATE group_accounts SET initial_balance = 5000 WHERE id = $1`, f.account)
	secretary := f.scalar(`INSERT INTO group_roles (group_id, name) VALUES ($1, 'Secretary') RETURNING id`, f.group)
	f.exec(`UPDATE memberships SET group_role_id = $2 WHERE id = $1`, f.member, secretary)
	f.exec(`INSERT INTO expense_categories (group_id, name) VALUES ($1, 'Venue hire')`, f.group)

	// Jane has one group, so she lands on its menu.
	out := f.dial("w1", "+254700000001", "")
	mustContain(t, out, "Jirani. Choose:", "3. Request a withdrawal", "4. Approve withdrawals", "5. Loans", "0 Exit")
	mustContain(t, f.dial("w1", "+254700000001", "3"), "What is the withdrawal for?")
	mustContain(t, f.dial("w1", "+254700000001", "3*1"), "1. Venue hire")
	// One open account: straight to the amount.
	mustContain(t, f.dial("w1", "+254700000001", "3*1*1"), "Equity main has KES 5,000.00")
	mustContain(t, f.dial("w1", "+254700000001", "3*1*1*9000"), "That is more than the KES 5,000.00")
	mustContain(t, f.dial("w1", "+254700000001", "3*1*1*9000*1500"), "Send the money to which MPesa number?")
	mustContain(t, f.dial("w1", "+254700000001", "3*1*1*9000*1500*07112"), "That is not a phone number")
	mustContain(t, f.dial("w1", "+254700000001", "3*1*1*9000*1500*07112*0711222333"),
		"Request KES 1,500.00 from Equity main for Venue hire, paid to 0711222333?")
	mustContain(t, f.dial("w1", "+254700000001", "3*1*1*9000*1500*07112*0711222333*1"), "END Your request was sent to the signatories", "Reference")

	var status, rcp string
	if err := f.pool.QueryRow(context.Background(), `SELECT status, recipient::text FROM withdrawal_requests`).Scan(&status, &rcp); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || !strings.Contains(rcp, "+254711222333") {
		t.Fatalf("request: %s %s", status, rcp)
	}

	// Otieno (a signatory now) approves it from his phone.
	f.dial("w2", "+254700000002", "")
	out = f.dial("w2", "+254700000002", "2*4")
	mustContain(t, out, "Requests waiting for you:", "1. 1,500.00 Venue hire")
	mustContain(t, f.dial("w2", "+254700000002", "2*4*1"), "KES 1,500.00 for Venue hire to 0711222333. Asked by")
	mustContain(t, f.dial("w2", "+254700000002", "2*4*1*1"), "END You approved the request")
	_ = f.pool.QueryRow(context.Background(), `SELECT status FROM withdrawal_requests`).Scan(&status)
	if status != "approved" {
		t.Fatalf("status after both signatories: %s", status)
	}
	// Nothing left to answer.
	f.dial("w3", "+254700000002", "")
	mustContain(t, f.dial("w3", "+254700000002", "2*4"), "No withdrawal requests are waiting for you.")
}

func TestUSSDLoanApplication(t *testing.T) {
	f := newChanFixture(t)
	r := f.post("/loan-types", map[string]any{
		"name": "Emergency", "interest_type": "fixed_balance", "interest_rate": 10, "rate_per": "month",
		"min_term": 1, "max_term": 12, "min_amount": "1000", "max_amount": "100000",
	})
	expect(t, r, 201)

	f.dial("l1", "+254700000002", "")
	mustContain(t, f.dial("l1", "+254700000002", "2*3"), "Loans:", "1. Apply for a loan")
	mustContain(t, f.dial("l1", "+254700000002", "2*3*1"), "1. Emergency")
	mustContain(t, f.dial("l1", "+254700000002", "2*3*1*1"), "Between KES 1,000.00 and KES 100,000.00")
	mustContain(t, f.dial("l1", "+254700000002", "2*3*1*1*500"), "1,000")
	mustContain(t, f.dial("l1", "+254700000002", "2*3*1*1*500*6000"), "over how many months? 1 to 12.")
	mustContain(t, f.dial("l1", "+254700000002", "2*3*1*1*500*6000*24"), "1 to 12")
	out := f.dial("l1", "+254700000002", "2*3*1*1*500*6000*24*6")
	mustContain(t, out, "Apply for KES 6,000.00 Emergency, repaid in 6 installments")
	mustContain(t, f.dial("l1", "+254700000002", "2*3*1*1*500*6000*24*6*1"), "END Your loan application was sent")

	var amount, status string
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT amount::text, installments, status FROM loan_applications`).Scan(&amount, &n, &status); err != nil {
		t.Fatal(err)
	}
	if amount != "6000.00" || n != 6 || status != "pending_approval" {
		t.Fatalf("application: %s %d %s", amount, n, status)
	}
	f.dial("l2", "+254700000002", "")
	mustContain(t, f.dial("l2", "+254700000002", "2*3*2"), "Emergency 6,000.00: waiting for approval")

	// Jane (the signatory) approves it.
	f.dial("l3", "+254700000001", "")
	mustContain(t, f.dial("l3", "+254700000001", "5"), "4. Applications waiting for me (1)")
	mustContain(t, f.dial("l3", "+254700000001", "5*4*1"), "applied for KES 6,000.00 Emergency over 6 installments")
	mustContain(t, f.dial("l3", "+254700000001", "5*4*1*2"), "Why are you declining?")
	mustContain(t, f.dial("l3", "+254700000001", "5*4*1*2*0*1"), "END You approved")
	_ = f.pool.QueryRow(context.Background(), `SELECT status FROM loan_applications`).Scan(&status)
	if status != "approved" {
		t.Fatalf("after approval: %s", status)
	}
}

func (f *chanFixture) whatsapp(sid, from, body string, sign bool) *httptest.ResponseRecorder {
	f.t.Helper()
	form := url.Values{"AccountSid": {"AC1"}, "MessageSid": {sid}, "From": {"whatsapp:" + from}, "WaId": {strings.TrimPrefix(from, "+")},
		"Body": {body}, "ProfileName": {"Jane"}}
	path := "/v1/hooks/whatsapp?key=" + f.waKey
	return f.form(path, form, func(r *http.Request) {
		sig := "bad"
		if sign {
			sig = channels.TwilioSignature(twilioToken, "https://legion.test"+path, form)
		}
		r.Header.Set("X-Twilio-Signature", sig)
	})
}

func twimlText(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Type"), "xml") {
		t.Fatalf("whatsapp: %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	start, end := strings.Index(body, "<Message>"), strings.Index(body, "</Message>")
	if start < 0 || end < 0 {
		t.Fatalf("no message: %s", body)
	}
	return strings.NewReplacer("&#39;", "'", "&#34;", `"`, "&amp;", "&", "&lt;", "<", "&gt;", ">", "&#xA;", "\n").Replace(body[start+9 : end])
}

func TestWhatsAppMenus(t *testing.T) {
	f := newChanFixture(t)
	if rec := f.whatsapp("SM0", "+254700000001", "Hi", false); rec.Code != 403 {
		t.Fatalf("unsigned: %d", rec.Code)
	}
	out := twimlText(t, f.whatsapp("SM1", "+254700000001", "Hi", true))
	mustContain(t, out, "Jirani. Choose:", "1. My account", "Reply 0 to exit.")
	out = twimlText(t, f.whatsapp("SM2", "+254700000001", "1", true))
	mustContain(t, out, "My account:", "Reply 0 to go back")
	// Twilio re-delivers a message: same answer, no second step.
	if again := twimlText(t, f.whatsapp("SM2", "+254700000001", "1", true)); again != out {
		t.Fatalf("redelivery: %s", again)
	}
	mustContain(t, twimlText(t, f.whatsapp("SM3", "+254700000001", "4", true)), "Cash at bank: KES 0.00", "Total: KES 0.00")
	mustContain(t, twimlText(t, f.whatsapp("SM4", "+254700000001", "menu", true)), "Jirani. Choose:")
	mustContain(t, twimlText(t, f.whatsapp("SM5", "+254700000001", "stop", true)), "Goodbye")
	// The next message starts a new conversation.
	mustContain(t, twimlText(t, f.whatsapp("SM6", "+254700000001", "5", true)), "Jirani. Choose:")
	var sessions int
	_ = f.pool.QueryRow(context.Background(), `SELECT count(*) FROM channel_sessions WHERE channel = 'whatsapp'`).Scan(&sessions)
	if sessions != 2 {
		t.Fatalf("sessions: %d", sessions)
	}
}

func TestChannelSwitchAndTestPhone(t *testing.T) {
	f := newChanFixture(t)
	expect(t, f.admin("PATCH", f.adminTok, "/admin/settings", map[string]any{"channels.ussd.enabled": false}), 200)
	mustContain(t, f.dial("x1", "+254700000002", ""), "END This service is not available right now")
	r := f.admin("GET", f.adminTok, "/admin/channels", nil)
	expect(t, r, 200)
	if r.body["ussd_enabled"] != false || r.body["whatsapp_enabled"] != true || r.body["whatsapp_signed"] != true {
		t.Fatalf("channels: %s", r.raw)
	}

	// The admin's test phone runs the menus as the admin, who has no group.
	r = f.admin("POST", f.adminTok, "/admin/channels/try", map[string]any{"format": "ussd"})
	expect(t, r, 200)
	if r.body["end"] != true || !strings.Contains(r.body["text"].(string), "not an active member of any group") {
		t.Fatalf("try: %s", r.raw)
	}
	// Jane is an admin too in this test: her test phone works like USSD.
	f.exec(`INSERT INTO user_platform_roles (user_id, role) SELECT user_id, 'admin' FROM memberships WHERE id = $1`, f.official)
	r = f.admin("POST", f.officialTok, "/admin/channels/try", map[string]any{"format": "ussd"})
	expect(t, r, 200)
	session := r.body["session"].(string)
	if !strings.HasPrefix(r.body["text"].(string), "CON Jirani. Choose:") {
		t.Fatalf("try: %s", r.raw)
	}
	r = f.admin("POST", f.officialTok, "/admin/channels/try", map[string]any{"format": "ussd", "session": session, "input": "1"})
	if !strings.Contains(r.body["text"].(string), "My account:") {
		t.Fatalf("try step: %s", r.raw)
	}
	var b []byte
	_ = f.pool.QueryRow(context.Background(), `SELECT state FROM channel_sessions WHERE session_key = $1`, session).Scan(&b)
	var st channels.State
	_ = json.Unmarshal(b, &st)
	if st.Screen != "account" {
		t.Fatalf("state: %s", b)
	}
}
