package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// lastSMS is the newest text queued to a phone.
func (h *harness) lastSMS(phone string) (kind, body string) {
	h.t.Helper()
	err := h.pool.QueryRow(context.Background(),
		`SELECT kind, body FROM sms_messages WHERE to_phone = $1 ORDER BY id DESC LIMIT 1`, phone).Scan(&kind, &body)
	if err != nil {
		h.t.Fatalf("no sms to %s: %v", phone, err)
	}
	return kind, body
}

func (h *harness) smsCount(phone string) int64 {
	return h.scalar(`SELECT count(*) FROM sms_messages WHERE to_phone = $1`, phone)
}

var reCode = regexp.MustCompile(`\b(\d{4,6})\b`)

func codeIn(t *testing.T, body string) string {
	t.Helper()
	m := reCode.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no code in %q", body)
	}
	return m[1]
}

func (h *harness) json(method, path, tok string, body any) *httpResult {
	h.t.Helper()
	var mods []func(*http.Request)
	if tok != "" {
		mods = append(mods, withBearer(tok))
	}
	rec := h.do(method, path, body, mods...)
	res := &httpResult{code: rec.Code, raw: rec.Body.String()}
	_ = json.Unmarshal(rec.Body.Bytes(), &res.body)
	return res
}

func TestSignupVerifyAndCreateGroup(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_ = ctx

	bad := h.json("POST", "/v1/auth/signup", "", map[string]any{"first_name": "Amina", "phone": "0711000001", "password": "short"})
	expect(t, bad, http.StatusBadRequest)

	res := h.json("POST", "/v1/auth/signup", "", map[string]any{
		"first_name": " Amina ", "last_name": "Odhiambo", "phone": "0711000001", "email": "Amina@Example.com", "password": "correct horse",
	})
	expect(t, res, http.StatusCreated)
	tok := res.body["access_token"].(string)
	user := res.body["user"].(map[string]any)
	if user["phone_verified"] != false || user["phone"] != "+254711000001" || user["email"] != "amina@example.com" {
		t.Fatalf("user = %v", user)
	}
	kind, body := h.lastSMS("+254711000001")
	if kind != "verify_phone" || !strings.Contains(body, "Dear Amina") || len(codeIn(t, body)) != 6 {
		t.Fatalf("sms %s %q", kind, body)
	}
	code := codeIn(t, body)

	// The same phone or email can't sign up twice.
	dup := h.json("POST", "/v1/auth/signup", "", map[string]any{"first_name": "X", "phone": "+254711000001", "password": "password1"})
	expect(t, dup, http.StatusConflict)
	if dup.body["error"].(map[string]any)["code"] != "phone_taken" {
		t.Fatal(dup.raw)
	}
	dup = h.json("POST", "/v1/auth/signup", "", map[string]any{"first_name": "X", "phone": "0711000009", "email": "amina@example.com", "password": "password1"})
	expect(t, dup, http.StatusConflict)

	// No group before the phone is verified.
	g := h.json("POST", "/v1/groups", tok, map[string]any{"name": "Umoja Chama"})
	expect(t, g, http.StatusForbidden)

	wrong := "000000"
	if code == wrong {
		wrong = "111111"
	}
	expect(t, h.json("POST", "/v1/auth/verify-phone", tok, map[string]any{"code": wrong}), http.StatusBadRequest)
	expect(t, h.json("POST", "/v1/auth/verify-phone", tok, map[string]any{"code": code}), http.StatusOK)
	// A code works once.
	expect(t, h.json("POST", "/v1/auth/verify-phone", tok, map[string]any{"code": code}), http.StatusBadRequest)
	expect(t, h.json("POST", "/v1/auth/verify-phone/resend", tok, nil), http.StatusConflict)

	me := h.json("GET", "/v1/me", tok, nil)
	if me.body["user"].(map[string]any)["phone_verified"] != true {
		t.Fatal(me.raw)
	}

	g = h.json("POST", "/v1/groups", tok, map[string]any{"name": "  Umoja   Chama ", "size": 12, "type": "2"})
	expect(t, g, http.StatusCreated)
	grp := g.body["group"].(map[string]any)
	if grp["name"] != "Umoja Chama" || grp["status"] != "setup" || grp["slug"] != "umoja-chama" || grp["currency"] != "KES" ||
		grp["type_name"] != "Chama" || grp["size"] != float64(12) {
		t.Fatalf("group = %v", grp)
	}
	gid := int64(grp["id"].(float64))
	if n := h.scalar(`SELECT count(*) FROM group_roles WHERE group_id = $1 AND NOT is_editable`, gid); n != 4 {
		t.Fatalf("roles = %d", n)
	}
	if n := h.scalar(`SELECT count(*) FROM group_accounts WHERE group_id = $1 AND kind = 'petty_cash' AND name = 'Cash at Hand'`, gid); n != 1 {
		t.Fatalf("petty cash = %d", n)
	}
	if n := h.scalar(`SELECT (SELECT count(*) FROM expense_categories WHERE group_id = $1) + (SELECT count(*) FROM income_categories WHERE group_id = $1)
		+ (SELECT count(*) FROM fine_categories WHERE group_id = $1)`, gid); n != 23+6+7 {
		t.Fatalf("categories = %d", n)
	}
	chair := h.scalar(`SELECT count(*) FROM memberships m JOIN group_roles r ON r.id = m.group_role_id
		WHERE m.group_id = $1 AND m.is_official AND r.name = 'Chair Person'`, gid)
	if chair != 1 {
		t.Fatal("creator is not the Chair Person")
	}
	// Same name, next slug.
	g2 := h.json("POST", "/v1/groups", tok, map[string]any{"name": "Umoja Chama"})
	expect(t, g2, http.StatusCreated)
	if g2.body["group"].(map[string]any)["slug"] != "umoja-chama-2" {
		t.Fatal(g2.raw)
	}
	expect(t, h.json("POST", "/v1/groups", tok, map[string]any{"name": "X", "type": "99"}), http.StatusBadRequest)
}

func TestSetupWizardAndInvites(t *testing.T) {
	h := newHarness(t)
	owner := h.createUser("+254722000001", "", "pw")
	h.exec(`UPDATE users SET is_validated = true, first_name = 'Grace', last_name = 'Muthoni' WHERE id = $1`, owner)
	tok := h.token("+254722000001")
	g := h.json("POST", "/v1/groups", tok, map[string]any{"name": "Tujenge"})
	expect(t, g, http.StatusCreated)
	gid := int64(g.body["group"].(map[string]any)["id"].(float64))
	base := fmt.Sprintf("/v1/groups/%d", gid)

	// Details step.
	p := h.json("PATCH", base, tok, map[string]any{"email": "tujenge@example.com", "phone": "0722000099", "size": 20, "currency": "UGX"})
	expect(t, p, http.StatusOK)
	if p.body["currency"] != "UGX" || p.body["phone"] != "+254722000099" {
		t.Fatal(p.raw)
	}

	// Members added during setup are not texted yet.
	m := h.json("POST", base+"/members", tok, map[string]any{"first_name": "peter", "last_name": "Kamau", "phone": "0722000002"})
	expect(t, m, http.StatusCreated)
	peter := int64(m.body["id"].(float64))
	expect(t, h.json("POST", base+"/members", tok, map[string]any{"first_name": "Mary", "phone": "0722000003"}), http.StatusCreated)
	if n := h.smsCount("+254722000002"); n != 0 {
		t.Fatalf("texted during setup: %d", n)
	}
	expect(t, h.json("POST", fmt.Sprintf("%s/members/%d/invite", base, peter), tok, nil), http.StatusConflict)

	c := h.json("POST", base+"/contributions", tok, map[string]any{
		"name": "Monthly", "kind": "regular", "amount": "500",
		"schedule":   map[string]any{"frequency": "monthly", "contribution_date": "2026-12-05"},
		"fine_rules": []map[string]any{{"kind": "fixed", "fixed_amount": "50", "chargeable_on": "1"}},
	})
	expect(t, c, http.StatusCreated)

	s := h.json("GET", base+"/setup", tok, nil)
	expect(t, s, http.StatusOK)
	steps := map[string]map[string]any{}
	for _, st := range s.list("steps") {
		steps[st["key"].(string)] = st
	}
	if steps["members"]["count"] != float64(3) || steps["members"]["done"] != true || steps["contributions"]["count"] != float64(1) ||
		steps["fines"]["count"] != float64(1) || steps["accounts"]["done"] != false || s.body["members_to_invite"] != float64(2) {
		t.Fatalf("setup = %s", s.raw)
	}

	// Setup groups are not billed.
	if n := h.scalar(`SELECT count(*) FROM groups WHERE id = $1 AND status = 'setup'`, gid); n != 1 {
		t.Fatal("not in setup")
	}

	done := h.json("POST", base+"/setup/complete", tok, nil)
	expect(t, done, http.StatusOK)
	if done.body["invites_sent"] != float64(2) || done.body["group"].(map[string]any)["status"] != "active" {
		t.Fatal(done.raw)
	}
	expect(t, h.json("POST", base+"/setup/complete", tok, nil), http.StatusConflict)
	// Country and currency are fixed once live.
	expect(t, h.json("PATCH", base, tok, map[string]any{"currency": "KES"}), http.StatusConflict)

	kind, body := h.lastSMS("+254722000002")
	if kind != "invite" || !strings.Contains(body, "Dear Peter, you have been invited to Legion by Grace Muthoni.") ||
		!strings.Contains(body, "Tujenge group") {
		t.Fatalf("invite %q", body)
	}
	link := regexp.MustCompile(`https://legion\.test/join/([A-Za-z0-9_-]+)`).FindStringSubmatch(body)
	if link == nil {
		t.Fatalf("no link in %q", body)
	}
	// The owner gets no invite.
	if n := h.smsCount("+254722000001"); n != 0 {
		t.Fatalf("owner texted %d", n)
	}

	inv := h.json("GET", "/v1/auth/invites/"+link[1], "", nil)
	expect(t, inv, http.StatusOK)
	if inv.body["first_name"] != "peter" || inv.body["group_name"] != "Tujenge" || inv.body["needs_password"] != true ||
		inv.body["invited_by"] != "Grace Muthoni" {
		t.Fatal(inv.raw)
	}
	expect(t, h.json("GET", "/v1/auth/invites/nope", "", nil), http.StatusNotFound)
	expect(t, h.json("POST", "/v1/auth/invites/"+link[1], "", map[string]any{"password": "short"}), http.StatusBadRequest)
	acc := h.json("POST", "/v1/auth/invites/"+link[1], "", map[string]any{"password": "peters password"})
	expect(t, acc, http.StatusOK)
	if acc.body["user"].(map[string]any)["phone_verified"] != true {
		t.Fatal(acc.raw)
	}
	expect(t, h.json("POST", "/v1/auth/invites/"+link[1], "", map[string]any{"password": "again again"}), http.StatusGone)
	if rec := h.login("0722000002", "peters password"); rec.Code != http.StatusOK {
		t.Fatalf("login after invite: %d %s", rec.Code, rec.Body)
	}

	// Resending gives Peter (who now has a password) a plain login message.
	expect(t, h.json("POST", fmt.Sprintf("%s/members/%d/invite", base, peter), tok, nil), http.StatusAccepted)
	_, body = h.lastSMS("+254722000002")
	if !strings.Contains(body, "Log in at https://legion.test .") || strings.Contains(body, "/join/") {
		t.Fatalf("resend %q", body)
	}

	// Adding a member to a live group texts them straight away, unless asked not to.
	expect(t, h.json("POST", base+"/members", tok, map[string]any{"first_name": "Joy", "phone": "0722000004"}), http.StatusCreated)
	if kind, _ := h.lastSMS("+254722000004"); kind != "invite" {
		t.Fatal(kind)
	}
	expect(t, h.json("POST", base+"/members", tok, map[string]any{"first_name": "Quiet", "phone": "0722000005", "send_invite": false}), http.StatusCreated)
	if n := h.smsCount("+254722000005"); n != 0 {
		t.Fatal("texted although send_invite was false")
	}

	// The platform admin's outbox lists them.
	admin := h.createUser("+254722000010", "", "pw")
	h.exec(`INSERT INTO user_platform_roles (user_id, role) VALUES ($1, 'admin')`, admin)
	out := h.json("GET", fmt.Sprintf("/v1/admin/sms?group_id=%d", gid), h.token("+254722000010"), nil)
	expect(t, out, http.StatusOK)
	if n := len(out.list("messages")); n != 4 {
		t.Fatalf("outbox %d: %s", n, out.raw)
	}
	expect(t, h.json("GET", "/v1/admin/sms", tok, nil), http.StatusForbidden)
}

func TestForgotAndResetPassword(t *testing.T) {
	h := newHarness(t)
	uid := h.createUser("+254733000001", "wanjiru@example.com", "old password")
	if rec := h.login("+254733000001", "old password"); rec.Code != http.StatusOK {
		t.Fatal(rec.Body)
	}

	// Unknown accounts get the same answer.
	expect(t, h.json("POST", "/v1/auth/forgot-password", "", map[string]any{"identity": "0733999999"}), http.StatusAccepted)
	expect(t, h.json("POST", "/v1/auth/forgot-password", "", map[string]any{"identity": "not a phone"}), http.StatusBadRequest)

	// By email: the code still goes to the phone.
	expect(t, h.json("POST", "/v1/auth/forgot-password", "", map[string]any{"identity": "Wanjiru@example.com"}), http.StatusAccepted)
	kind, body := h.lastSMS("+254733000001")
	if kind != "reset_password" || !strings.Contains(body, "password recovery code") {
		t.Fatal(body)
	}
	code := codeIn(t, body)
	wrong := "123456"
	if code == wrong {
		wrong = "654321"
	}
	expect(t, h.json("POST", "/v1/auth/reset-password", "", map[string]any{"identity": "0733000001", "code": wrong, "password": "new password"}), http.StatusBadRequest)
	expect(t, h.json("POST", "/v1/auth/reset-password", "", map[string]any{"identity": "0733999999", "code": code, "password": "new password"}), http.StatusBadRequest)
	res := h.json("POST", "/v1/auth/reset-password", "", map[string]any{"identity": "0733000001", "code": code, "password": "new password"})
	expect(t, res, http.StatusOK)
	if res.body["access_token"] == "" {
		t.Fatal(res.raw)
	}
	if kind, _ := h.lastSMS("+254733000001"); kind != "password_changed" {
		t.Fatal(kind)
	}
	if rec := h.login("0733000001", "old password"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("old password still works: %d", rec.Code)
	}
	if rec := h.login("0733000001", "new password"); rec.Code != http.StatusOK {
		t.Fatalf("new password: %d", rec.Code)
	}
	if n := h.scalar(`SELECT count(*) FROM refresh_tokens WHERE user_id = $1 AND revoked_at IS NULL`, uid); n != 2 {
		// the reset's own session and the login after it
		t.Fatalf("live refresh tokens = %d", n)
	}
	expect(t, h.json("POST", "/v1/auth/reset-password", "", map[string]any{"identity": "0733000001", "code": code, "password": "another one"}), http.StatusBadRequest)

	// Five wrong guesses use the code up; five codes an hour at most.
	expect(t, h.json("POST", "/v1/auth/forgot-password", "", map[string]any{"identity": "0733000001"}), http.StatusAccepted)
	_, body = h.lastSMS("+254733000001")
	code = codeIn(t, body)
	for i := 0; i < 5; i++ {
		expect(t, h.json("POST", "/v1/auth/reset-password", "", map[string]any{"identity": "0733000001", "code": wrong, "password": "new password"}), http.StatusBadRequest)
	}
	r := h.json("POST", "/v1/auth/reset-password", "", map[string]any{"identity": "0733000001", "code": code, "password": "new password"})
	if r.body["error"].(map[string]any)["code"] != "code_expired" {
		t.Fatal(r.raw)
	}
	for i := 0; i < 3; i++ {
		expect(t, h.json("POST", "/v1/auth/forgot-password", "", map[string]any{"identity": "0733000001"}), http.StatusAccepted)
	}
	expect(t, h.json("POST", "/v1/auth/forgot-password", "", map[string]any{"identity": "0733000001"}), http.StatusTooManyRequests)
}
