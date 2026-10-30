package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/swahome/legion-api/internal/store"
)

func (f *groupFixture) admin(method, tok, path string, body any) *httpResult {
	f.t.Helper()
	rec := f.do(method, "/v1"+path, body, withBearer(tok))
	res := &httpResult{code: rec.Code, raw: rec.Body.String()}
	_ = json.Unmarshal(rec.Body.Bytes(), &res.body)
	return res
}

func TestAdminRoutesNeedPlatformAdmin(t *testing.T) {
	f := newGroupFixture(t)
	for _, path := range []string{"/admin/stats", "/admin/groups", "/admin/users", "/admin/settings", "/admin/audit"} {
		expect(t, f.admin("GET", f.officialTok, path, nil), http.StatusForbidden)
		expect(t, f.admin("GET", f.adminTok, path, nil), http.StatusOK)
	}
	// Removing the role takes effect at once, without a new token.
	f.exec(`DELETE FROM user_platform_roles`)
	expect(t, f.admin("GET", f.adminTok, "/admin/stats", nil), http.StatusForbidden)
}

func TestAdminGroupsAndSuspension(t *testing.T) {
	f := newGroupFixture(t)
	r := f.admin("GET", f.adminTok, "/admin/groups?q=jir", nil)
	expect(t, r, http.StatusOK)
	if r.body["total"].(float64) != 1 || r.list("items")[0]["active_members"].(float64) != 2 {
		t.Fatalf("groups search: %s", r.raw)
	}
	expect(t, f.admin("PATCH", f.adminTok, fmt.Sprintf("/admin/groups/%d", f.group), map[string]string{"status": "bogus"}), http.StatusBadRequest)
	expect(t, f.admin("PATCH", f.adminTok, fmt.Sprintf("/admin/groups/%d", f.group), map[string]string{"status": "suspended"}), http.StatusOK)

	// Members of a suspended group are locked out; the other group still works.
	r = f.get(f.memberTok, "/members")
	expect(t, r, http.StatusForbidden)
	if r.body["error"].(map[string]any)["code"] != "group_suspended" {
		t.Fatalf("want group_suspended: %s", r.raw)
	}
	rec := f.do("GET", fmt.Sprintf("/v1/groups/%d/members", f.other), nil, withBearer(f.memberTok))
	if rec.Code != http.StatusOK {
		t.Fatalf("other group: %d", rec.Code)
	}
	// Platform admins can still look in.
	expect(t, f.get(f.adminTok, "/members"), http.StatusOK)

	rec = f.do("GET", "/v1/admin/audit", nil, withBearer(f.adminTok))
	var audit []map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &audit)
	if len(audit) != 1 || audit[0]["action"] != "group.status" || audit[0]["target_id"].(float64) != float64(f.group) {
		t.Fatalf("audit: %s", rec.Body)
	}
}

func TestAdminUsers(t *testing.T) {
	f := newGroupFixture(t)
	r := f.admin("POST", f.adminTok, "/admin/users", map[string]any{
		"first_name": "Amina", "last_name": "Hassan", "phone": "0711000111", "password": "longenough",
	})
	expect(t, r, http.StatusCreated)
	id := int64(r.body["id"].(float64))
	if r.body["phone"] != "+254711000111" {
		t.Fatalf("phone not normalised: %s", r.raw)
	}
	if rec := f.login("0711000111", "longenough"); rec.Code != http.StatusOK {
		t.Fatalf("new user cannot log in: %d %s", rec.Code, rec.Body)
	}

	// Same phone again is a conflict.
	r = f.admin("POST", f.adminTok, "/admin/users", map[string]any{
		"first_name": "Dup", "last_name": "User", "phone": "+254711000111", "password": "longenough",
	})
	expect(t, r, http.StatusConflict)

	r = f.admin("GET", f.adminTok, "/admin/users?q=amina", nil)
	if r.body["total"].(float64) != 1 {
		t.Fatalf("user search: %s", r.raw)
	}

	// A short password is refused; a good one revokes sessions.
	expect(t, f.admin("POST", f.adminTok, fmt.Sprintf("/admin/users/%d/password", id), map[string]any{"password": "short"}), http.StatusBadRequest)
	expect(t, f.admin("POST", f.adminTok, fmt.Sprintf("/admin/users/%d/password", id), map[string]any{"password": "anotherlong"}), http.StatusNoContent)
	if f.login("0711000111", "anotherlong").Code != http.StatusOK {
		t.Fatal("login with the new password failed")
	}

	// Deactivating blocks login.
	expect(t, f.admin("PATCH", f.adminTok, fmt.Sprintf("/admin/users/%d", id), map[string]any{"is_active": false}), http.StatusOK)
	if f.login("0711000111", "anotherlong").Code == http.StatusOK {
		t.Fatal("inactive user could log in")
	}

	r = f.admin("GET", f.adminTok, fmt.Sprintf("/admin/users/%d", f.scalar(`SELECT user_id FROM memberships WHERE id=$1`, f.member)), nil)
	expect(t, r, http.StatusOK)
	if len(r.body["memberships"].([]any)) != 2 {
		t.Fatalf("memberships: %s", r.raw)
	}
}

func TestAdminCannotRemoveLastAdmin(t *testing.T) {
	f := newGroupFixture(t)
	admin := f.scalar(`SELECT user_id FROM user_platform_roles WHERE role='admin'`)
	path := fmt.Sprintf("/admin/users/%d", admin)
	r := f.admin("PATCH", f.adminTok, path, map[string]any{"platform_roles": []string{}})
	expect(t, r, http.StatusConflict)
	expect(t, f.admin("PATCH", f.adminTok, path, map[string]any{"is_active": false}), http.StatusConflict)

	// Admins can't demote themselves, but a second admin can.
	other := f.scalar(`SELECT user_id FROM memberships WHERE id=$1`, f.official)
	expect(t, f.admin("PATCH", f.adminTok, fmt.Sprintf("/admin/users/%d", other), map[string]any{"platform_roles": []string{"admin"}}), http.StatusOK)
	expect(t, f.admin("PATCH", f.adminTok, path, map[string]any{"platform_roles": []string{}}), http.StatusConflict)
	expect(t, f.admin("PATCH", f.officialTok, path, map[string]any{"platform_roles": []string{}}), http.StatusOK)
	expect(t, f.admin("GET", f.adminTok, "/admin/stats", nil), http.StatusForbidden)
}

func TestAdminSettings(t *testing.T) {
	f := newGroupFixture(t)
	r := f.admin("PATCH", f.adminTok, "/admin/settings", map[string]any{"application_name": "Legion", "sms.sender_id": "LEGION", "allow_self_onboarding": 1})
	expect(t, r, http.StatusOK)
	if r.body["application_name"] != "Legion" || r.body["allow_self_onboarding"].(float64) != 1 {
		t.Fatalf("settings: %s", r.raw)
	}
	r = f.admin("PATCH", f.adminTok, "/admin/settings", map[string]any{"sms.sender_id": nil})
	if _, ok := r.body["sms.sender_id"]; ok || len(r.body) != 2 {
		t.Fatalf("null should delete: %s", r.raw)
	}
	expect(t, f.admin("PATCH", f.adminTok, "/admin/settings", map[string]any{"Bad Key": 1}), http.StatusBadRequest)
	expect(t, f.admin("PATCH", f.adminTok, "/admin/settings", map[string]any{}), http.StatusBadRequest)
}

func TestReferenceData(t *testing.T) {
	f := newGroupFixture(t)
	r := f.admin("POST", f.adminTok, "/admin/countries", map[string]any{
		"name": "Kenya", "code": "ke", "currency_code": "kes", "currency_name": "Kenya Shilling", "calling_code": "+254", "is_default": true,
	})
	expect(t, r, http.StatusCreated)
	if r.body["code"] != "KE" || r.body["calling_code"] != "254" {
		t.Fatalf("country not normalised: %s", r.raw)
	}
	ke := int64(r.body["id"].(float64))
	expect(t, f.admin("POST", f.adminTok, "/admin/countries", map[string]any{
		"name": "Kenya again", "code": "KE", "currency_code": "KES", "calling_code": "254",
	}), http.StatusConflict)
	expect(t, f.admin("POST", f.adminTok, "/admin/countries", map[string]any{
		"name": "Bad", "code": "KEN", "currency_code": "KES", "calling_code": "254",
	}), http.StatusBadRequest)

	// A second default country takes over the flag.
	r = f.admin("POST", f.adminTok, "/admin/countries", map[string]any{
		"name": "Uganda", "code": "UG", "currency_code": "UGX", "calling_code": "256", "is_default": true,
	})
	expect(t, r, http.StatusCreated)
	if n := f.scalar(`SELECT count(*) FROM countries WHERE is_default`); n != 1 {
		t.Fatalf("%d default countries", n)
	}

	expect(t, f.admin("POST", f.adminTok, "/admin/counties", map[string]any{"country_id": ke, "name": "Nairobi", "code": 47}), http.StatusCreated)

	r = f.admin("POST", f.adminTok, "/admin/banks", map[string]any{"name": "Equity Bank", "code": 68, "is_partner": true})
	expect(t, r, http.StatusCreated)
	if r.body["slug"] != "equity-bank" {
		t.Fatalf("slug: %s", r.raw)
	}
	bank := int64(r.body["id"].(float64))
	expect(t, f.admin("POST", f.adminTok, fmt.Sprintf("/admin/banks/%d/branches", bank), map[string]any{"name": "Kenyatta Avenue", "code": 12}), http.StatusCreated)
	r = f.admin("POST", f.adminTok, fmt.Sprintf("/admin/banks/%d/branches", bank), map[string]any{"name": "Old branch"})
	expect(t, r, http.StatusCreated)
	expect(t, f.admin("PATCH", f.adminTok, fmt.Sprintf("/admin/banks/%d/branches/%d", bank, int64(r.body["id"].(float64))), map[string]any{"is_active": false}), http.StatusOK)

	// Officials read the active lists; only admins write.
	expect(t, f.admin("POST", f.officialTok, "/admin/banks", map[string]any{"name": "Nope"}), http.StatusForbidden)
	rec := f.do("GET", fmt.Sprintf("/v1/reference/banks/%d/branches", bank), nil, withBearer(f.memberTok))
	var branches []map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &branches)
	if rec.Code != http.StatusOK || len(branches) != 1 {
		t.Fatalf("reference branches: %d %s", rec.Code, rec.Body)
	}
	rec = f.do("GET", "/v1/reference/counties?country_id="+fmt.Sprint(ke), nil, withBearer(f.memberTok))
	if rec.Code != http.StatusOK {
		t.Fatalf("reference counties: %d %s", rec.Code, rec.Body)
	}
}

func TestEveryEntryTypeHasALabel(t *testing.T) {
	for _, typ := range store.AllLedgerEntryTypeValues() {
		if entryLabels[typ] == "" {
			t.Errorf("no label for %s", typ)
		}
	}
}
