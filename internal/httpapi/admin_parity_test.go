package httpapi

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestAdminGroupDetailEditAndViews(t *testing.T) {
	f := newGroupFixture(t)
	path := fmt.Sprintf("/admin/groups/%d", f.group)
	r := f.admin("GET", f.adminTok, path, nil)
	expect(t, r, http.StatusOK)
	if r.body["active_members"].(float64) != 2 || r.body["notifications_off"] != false {
		t.Fatalf("detail: %s", r.raw)
	}
	expect(t, f.admin("GET", f.officialTok, path, nil), http.StatusForbidden)
	expect(t, f.admin("GET", f.adminTok, "/admin/groups/999999", nil), http.StatusNotFound)

	r = f.admin("PATCH", f.adminTok, path, map[string]any{"name": "Jirani Renamed", "phone": "0722000333", "notifications_off": true})
	expect(t, r, http.StatusOK)
	if r.body["name"] != "Jirani Renamed" || r.body["phone"] != "+254722000333" || r.body["notifications_off"] != true {
		t.Fatalf("edit: %s", r.raw)
	}
	expect(t, f.admin("PATCH", f.adminTok, path, map[string]any{"name": "  "}), http.StatusBadRequest)
	// Clearing the phone with null.
	r = f.admin("PATCH", f.adminTok, path, map[string]any{"phone": nil})
	expect(t, r, http.StatusOK)
	if r.body["phone"] != nil || r.body["name"] != "Jirani Renamed" {
		t.Fatalf("clear phone: %s", r.raw)
	}

	// A group nobody has signed in to for 90 days is a candidate for archiving.
	r = f.admin("GET", f.adminTok, "/admin/groups?view=inactive", nil)
	if r.body["total"].(float64) != 0 {
		t.Fatalf("everyone signed in, yet: %s", r.raw)
	}
	f.exec(`UPDATE users SET last_login_at = now() - interval '100 days'`)
	r = f.admin("GET", f.adminTok, "/admin/groups?view=inactive&sort=newest", nil)
	expect(t, r, http.StatusOK)
	if r.body["total"].(float64) < 1 {
		t.Fatalf("inactive view: %s", r.raw)
	}
	expect(t, f.admin("GET", f.adminTok, "/admin/groups?view=bogus", nil), http.StatusBadRequest)
}

func TestAdminStatsUsersRolesExport(t *testing.T) {
	f := newGroupFixture(t)
	r := f.admin("GET", f.adminTok, "/admin/stats", nil)
	expect(t, r, http.StatusOK)
	for _, k := range []string{"groups_today", "users_today", "sms_sent_today", "accounts_connected", "invoices_today", "payments_paid_amount"} {
		if _, ok := r.body[k]; !ok {
			t.Fatalf("stats missing %s: %s", k, r.raw)
		}
	}

	r = f.admin("GET", f.adminTok, "/admin/users?q=", nil)
	expect(t, r, http.StatusOK)
	found := false
	for _, u := range r.list("items") {
		if gl, ok := u["group_list"].([]any); ok && len(gl) > 0 {
			found = true
		}
	}
	if !found {
		t.Fatalf("no user lists their groups: %s", r.raw)
	}

	rec := f.do("GET", "/v1/admin/roles", nil, withBearer(f.adminTok))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"role":"admin","name":"Admin"`) {
		t.Fatalf("roles: %d %s", rec.Code, rec.Body)
	}

	rec = f.do("GET", "/v1/admin/groups/export?q=jir", nil, withBearer(f.adminTok))
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/csv") {
		t.Fatalf("groups export: %d %s", rec.Code, rec.Header())
	}
	lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[1], "Jirani") {
		t.Fatalf("groups export rows: %q", lines)
	}
	rec = f.do("GET", "/v1/admin/users/export", nil, withBearer(f.adminTok))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Last login") {
		t.Fatalf("users export: %d", rec.Code)
	}
	rec = f.do("GET", "/v1/admin/users/export", nil, withBearer(f.officialTok))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("export needs admin: %d", rec.Code)
	}
}

func TestLoginOptionsArePublic(t *testing.T) {
	f := newGroupFixture(t)
	f.exec(`INSERT INTO countries (name, code, currency_code, currency_name, calling_code, is_default, is_active)
	        VALUES ('Kenya', 'KE', 'KES', 'Kenyan shilling', '254', true, true) ON CONFLICT DO NOTHING`)
	f.exec(`INSERT INTO platform_settings (key, value) VALUES ('links.terms', '"https://example.com/terms"'), ('links.demo', '"javascript:alert(1)"')`)
	rec := f.do("GET", "/v1/auth/options", nil)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, `"calling_code":"254"`) ||
		!strings.Contains(body, `"terms":"https://example.com/terms"`) || strings.Contains(body, "javascript") {
		t.Fatalf("options: %d %s", rec.Code, body)
	}
}
