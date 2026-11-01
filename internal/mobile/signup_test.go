package mobile

import (
	"context"
	"fmt"
	"regexp"
	"testing"
	"time"
)

// appUser registers a new app user through generate_pin, verify_pin and
// register_user, and returns its token and id.
func appUser(t *testing.T, f *fixture, phone string) (string, int64) {
	t.Helper()
	if r := f.post("generate_pin", "", map[string]any{"identity": phone}); r.body["status"] != float64(1) {
		t.Fatalf("generate: %v", r.body)
	}
	pin := f.otp.last["+254"+phone[1:]]
	r := f.post("verify_pin", "", map[string]any{"identity": phone, "pin": pin})
	code, _ := r.body["unique_code"].(string)
	if r.body["user_exists"] != float64(0) || code == "" {
		t.Fatalf("verify: %v", r.body)
	}
	body := map[string]any{"first_name": "wanjiru", "last_name": "kamau", "identity": phone, "password": "short"}
	if r := f.post("register_user", "", body); r.body["status"] == float64(1) {
		t.Fatalf("short password accepted: %v", r.body)
	}
	body["password"] = "a-long-password"
	if r := f.post("register_user", "", body); r.body["status"] != float64(9) {
		t.Fatalf("no unique_code: %v", r.body)
	}
	body["unique_code"] = code
	r = f.post("register_user", "", body)
	if r.body["status"] != float64(1) || r.body["message"] != "Registration request successful" {
		t.Fatalf("register: %v", r.body)
	}
	var id int64
	if err := f.srv.pool.QueryRow(context.Background(), `SELECT id FROM users WHERE phone = $1 AND is_validated`, "+254"+phone[1:]).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return r.body["access_token"].(string), id
}

func TestAppSignupAndGroupSetup(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	token, uid := appUser(t, f, "0722000111")
	call := func(path string, gid int64, extra map[string]any) map[string]any {
		t.Helper()
		body := map[string]any{"user_id": uid, "group_id": gid}
		for k, v := range extra {
			body[k] = v
		}
		r := f.post(path, token, body)
		if r.code != 200 {
			t.Fatalf("%s: HTTP %d %s", path, r.code, r.raw)
		}
		return r.body
	}

	r := call("create_group", 0, map[string]any{"group_name": "Tujenge Pamoja"})
	wantStatus(t, "create_group", r, float64(1))
	var gid int64
	var st string
	if err := f.srv.pool.QueryRow(ctx, `SELECT id, status FROM groups WHERE slug = 'tujenge-pamoja'`).Scan(&gid, &st); err != nil || st != "setup" {
		t.Fatalf("group: %v %s", err, st)
	}
	var treasurer int64
	_ = f.srv.pool.QueryRow(ctx, `SELECT id FROM group_roles WHERE group_id = $1 AND name = 'Treasurer'`, gid).Scan(&treasurer)

	r = call("setup_tasks/add_group_members", gid, map[string]any{"members": []any{
		map[string]any{"first_name": "Juma", "last_name": "Ali", "phone": "0733000222", "group_role_id": treasurer},
		map[string]any{"first_name": "Neema", "last_name": "Achieng", "phone": "0733000333"},
	}})
	wantStatus(t, "add members", r, float64(1))

	r = call("setup_tasks/create_group_contribution_setting", gid, map[string]any{"name": "Monthly", "amount": "1,000",
		"type": "1", "regular_invoicing_active": "1", "contribution_frequency": "1",
		"month_day_monthly": "5", "week_day_monthly": "0", "sms_notifications_enabled": "1",
		"request_id": fmt.Sprintf("%d_1", time.Now().Unix())})
	wantStatus(t, "create contribution", r, float64(1))
	cid := int64(r["contribution_id"].(float64))
	if roles, _ := r["group_roles"].(map[string]any); len(roles) != 4 {
		t.Fatalf("roles: %v", r["group_roles"])
	}
	var due string
	if err := f.srv.pool.QueryRow(ctx, `SELECT to_char(contribution_date, 'DD') FROM contribution_schedules
		WHERE contribution_id = $1 AND invoicing_active AND invoice_days_before = 2`, cid).Scan(&due); err != nil || due != "05" {
		t.Fatalf("schedule: %v %q", err, due)
	}
	r = call("setup_tasks/edit_contribution_setting", gid, map[string]any{"id": cid, "name": "Monthly savings", "amount": "1500",
		"type": "1", "contribution_frequency": "1", "month_day_monthly": "10", "week_day_monthly": "0"})
	wantStatus(t, "edit contribution", r, float64(1))
	if r := call("setup_tasks/edit_contribution_setting", gid, map[string]any{"id": cid, "name": "x", "amount": "1", "type": "3"}); r["status"] != float64(0) {
		t.Fatalf("type change: %v", r)
	}

	var self int64
	_ = f.srv.pool.QueryRow(ctx, `SELECT id FROM memberships WHERE group_id = $1 AND user_id = $2`, gid, uid).Scan(&self)
	if r := call("setup_tasks/contributing_members", gid, map[string]any{"contribution_id": cid}); r["status"] == float64(1) {
		t.Fatalf("no members: %v", r)
	}
	r = call("setup_tasks/contributing_members", gid, map[string]any{"contribution_id": cid,
		"contributing_members": []any{map[string]any{"member_id": self}}})
	if r["message"] != "successfully added members" {
		t.Fatalf("members: %v", r)
	}
	var n int
	_ = f.srv.pool.QueryRow(ctx, `SELECT count(*) FROM contribution_members WHERE contribution_id = $1`, cid).Scan(&n)
	if n != 1 {
		t.Fatalf("contribution members %d", n)
	}

	r = call("setup_tasks/contribution_fine_settings", gid, map[string]any{"contribution_id": cid, "enable_fines": "1",
		"fines": []any{map[string]any{"fine_type": "1", "fixed_amount": "100", "fixed_fine_mode": "1",
			"fixed_fine_chargeable_on": "first_day_of_the_month", "fixed_fine_frequency": "0", "fine_limit": "1"}}})
	wantStatus(t, "fines", r, float64(1))
	_ = f.srv.pool.QueryRow(ctx, `SELECT count(*) FROM contribution_fine_rules WHERE contribution_id = $1 AND next_fine_date IS NOT NULL`, cid).Scan(&n)
	if n != 1 {
		t.Fatalf("fine rules %d", n)
	}

	// Nobody is texted until setup is complete.
	_ = f.srv.pool.QueryRow(ctx, `SELECT count(*) FROM sms_messages WHERE kind = 'invite'`).Scan(&n)
	if n != 0 {
		t.Fatalf("invites before complete: %d", n)
	}
	wantStatus(t, "complete", call("complete_group_setup", gid, nil), float64(1))
	_ = f.srv.pool.QueryRow(ctx, `SELECT status FROM groups WHERE id = $1`, gid).Scan(&st)
	_ = f.srv.pool.QueryRow(ctx, `SELECT count(*) FROM sms_messages WHERE kind = 'invite' AND group_id = $1`, gid).Scan(&n)
	if st != "active" || n != 2 {
		t.Fatalf("after complete: %s, %d invites", st, n)
	}
	// Members of a live group can be texted again.
	var juma int64
	_ = f.srv.pool.QueryRow(ctx, `SELECT m.id FROM memberships m JOIN users u ON u.id = m.user_id WHERE m.group_id = $1 AND u.first_name = 'Juma'`, gid).Scan(&juma)
	wantStatus(t, "resend", call("setup_tasks/resend_group_invitation", gid, map[string]any{"member_id": juma}), float64(1))
}

func TestAppPasswordReset(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if r := f.post("forgot_password", "", map[string]any{"phone": "0799999999"}); r.body["status"] != float64(4) {
		t.Fatalf("unknown: %v", r.body)
	}
	if r := f.post("forgot_password", "", map[string]any{"phone": "0712345678"}); r.body["status"] != float64(1) {
		t.Fatalf("forgot: %v", r.body)
	}
	var body string
	if err := f.srv.pool.QueryRow(ctx, `SELECT body FROM sms_messages WHERE kind = 'reset_password'`).Scan(&body); err != nil {
		t.Fatal(err)
	}
	code := regexp.MustCompile(`code is (\d{4}) `).FindStringSubmatch(body)
	if code == nil {
		t.Fatalf("sms: %q", body)
	}
	if r := f.post("validate_forgot_password_code", "", map[string]any{"phone": "0712345678", "pin": "0000"}); r.body["status"] == float64(1) && code[1] != "0000" {
		t.Fatalf("wrong code accepted: %v", r.body)
	}
	r := f.post("validate_forgot_password_code", "", map[string]any{"phone": "0712345678", "pin": code[1]})
	key, _ := r.body["secret_key"].(string)
	if r.body["status"] != float64(1) || key == "" {
		t.Fatalf("validate: %v", r.body)
	}
	r = f.post("reset_password", "", map[string]any{"phone": "0712345678", "secret_key": key, "password": "brand-new-pw", "confirm_password": "brand-new-pw"})
	if r.body["status"] != float64(1) || r.body["message"] != "Password successfully changed" {
		t.Fatalf("reset: %v", r.body)
	}
	if r := f.post("reset_password", "", map[string]any{"phone": "0712345678", "secret_key": key, "password": "brand-new-pw", "confirm_password": "brand-new-pw"}); r.body["status"] == float64(1) {
		t.Fatalf("key reused: %v", r.body)
	}
	if r := f.post("login", "", map[string]any{"phone": "0712345678", "password": "brand-new-pw", "user_id": 0, "group_id": 0}); r.body["status"] != float64(1) {
		t.Fatalf("login: %v", r.body)
	}
}
