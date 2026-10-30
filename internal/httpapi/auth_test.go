package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"github.com/swahome/legion-api/internal/auth"
	"github.com/swahome/legion-api/internal/testdb"
)

type harness struct {
	t    *testing.T
	pool *pgxpool.Pool
	h    http.Handler
}

func newHarness(t *testing.T) *harness {
	pool := testdb.New(t)
	svc := auth.NewService(pool, auth.NewTokenIssuer([]byte("0123456789abcdef0123456789abcdef"), 15*time.Minute), time.Hour, "254")
	return &harness{t: t, pool: pool, h: New(pool, svc, false).Routes()}
}

// createUser inserts a user with a legacy-style ($2y$) bcrypt hash, the
// format PHP's password_hash and Ion Auth wrote.
func (h *harness) createUser(phone, email, password string) int64 {
	h.t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), 8)
	if err != nil {
		h.t.Fatal(err)
	}
	legacy := "$2y$" + strings.TrimPrefix(string(hash), "$2a$")
	var id int64
	err = h.pool.QueryRow(context.Background(),
		`INSERT INTO users (first_name, last_name, phone, email, password_hash)
		 VALUES ('Jane', 'Wanjiru', NULLIF($1, ''), NULLIF($2, ''), $3) RETURNING id`,
		phone, email, legacy).Scan(&id)
	if err != nil {
		h.t.Fatal(err)
	}
	return id
}

func (h *harness) exec(sql string, args ...any) {
	h.t.Helper()
	if _, err := h.pool.Exec(context.Background(), sql, args...); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) do(method, path string, body any, mods ...func(*http.Request)) *httptest.ResponseRecorder {
	h.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.RemoteAddr = "203.0.113.7:5555"
	for _, m := range mods {
		m(req)
	}
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)
	return rec
}

func (h *harness) login(identity, password string) *httptest.ResponseRecorder {
	return h.do("POST", "/v1/auth/login", loginRequest{Identity: identity, Password: password})
}

func withCookie(c *http.Cookie) func(*http.Request) {
	return func(r *http.Request) { r.AddCookie(c) }
}

func withBearer(tok string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }
}

func refreshCookieOf(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == refreshCookie {
			return c
		}
	}
	t.Fatalf("no refresh cookie in response (status %d)", rec.Code)
	return nil
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var b errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatalf("decode error body %q: %v", rec.Body.String(), err)
	}
	return b.Error.Code
}

func TestLoginWithLegacyHashByPhoneAndEmail(t *testing.T) {
	h := newHarness(t)
	h.createUser("+254712345678", "jane@example.com", "s3cret!")

	for _, id := range []string{"0712345678", "+254 712 345 678", "254712345678", "JANE@example.com"} {
		rec := h.login(id, "s3cret!")
		if rec.Code != http.StatusOK {
			t.Fatalf("login %q: status %d body %s", id, rec.Code, rec.Body)
		}
		var resp sessionResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.AccessToken == "" || resp.TokenType != "Bearer" || resp.ExpiresIn <= 0 || resp.User.FirstName != "Jane" {
			t.Fatalf("login %q: unexpected response %+v", id, resp)
		}
		c := refreshCookieOf(t, rec)
		if !c.HttpOnly || c.Path != refreshCookiePath || c.SameSite != http.SameSiteStrictMode {
			t.Fatalf("refresh cookie attributes wrong: %+v", c)
		}
	}
}

func TestLoginFailuresAndLockout(t *testing.T) {
	h := newHarness(t)
	h.createUser("+254712345678", "", "s3cret!")

	for i := 0; i < auth.MaxLoginAttempts; i++ {
		rec := h.login("0712345678", "wrong")
		if rec.Code != http.StatusUnauthorized || errorCode(t, rec) != "invalid_credentials" {
			t.Fatalf("attempt %d: status %d body %s", i+1, rec.Code, rec.Body)
		}
	}
	// Now locked out, even with the right password.
	rec := h.login("0712345678", "s3cret!")
	if rec.Code != http.StatusTooManyRequests || errorCode(t, rec) != "too_many_attempts" {
		t.Fatalf("expected lockout, got %d %s", rec.Code, rec.Body)
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" || ra == "0" {
		t.Fatalf("Retry-After = %q", ra)
	}
	// The lockout is per identity and IP: another IP can still log in.
	rec = h.do("POST", "/v1/auth/login", loginRequest{Identity: "0712345678", Password: "s3cret!"},
		func(r *http.Request) { r.RemoteAddr = "198.51.100.9:1234" })
	if rec.Code != http.StatusOK {
		t.Fatalf("other IP: status %d %s", rec.Code, rec.Body)
	}
}

func TestLoginUnknownUserLooksLikeWrongPassword(t *testing.T) {
	h := newHarness(t)
	rec := h.login("0799999999", "whatever")
	if rec.Code != http.StatusUnauthorized || errorCode(t, rec) != "invalid_credentials" {
		t.Fatalf("status %d body %s", rec.Code, rec.Body)
	}
}

func TestLoginBlockedStates(t *testing.T) {
	h := newHarness(t)
	reset := h.createUser("+254700000001", "", "pw")
	inactive := h.createUser("+254700000002", "", "pw")
	h.exec(`UPDATE users SET password_reset_required = true WHERE id = $1`, reset)
	h.exec(`UPDATE users SET is_active = false WHERE id = $1`, inactive)

	if rec := h.login("0700000001", "pw"); rec.Code != http.StatusForbidden || errorCode(t, rec) != "password_reset_required" {
		t.Fatalf("reset: %d %s", rec.Code, rec.Body)
	}
	if rec := h.login("0700000002", "pw"); rec.Code != http.StatusForbidden || errorCode(t, rec) != "account_inactive" {
		t.Fatalf("inactive: %d %s", rec.Code, rec.Body)
	}
	// An inactive account with the wrong password must not reveal its state.
	if rec := h.login("0700000002", "nope"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("inactive wrong pw: %d %s", rec.Code, rec.Body)
	}
}

func TestLoginRejectsBadBodies(t *testing.T) {
	h := newHarness(t)
	if rec := h.do("POST", "/v1/auth/login", map[string]string{"identity": "0712345678"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing password: %d", rec.Code)
	}
	if rec := h.do("POST", "/v1/auth/login", map[string]string{"identity": "x", "password": "y", "extra": "z"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown field: %d", rec.Code)
	}
}

func TestRefreshRotationAndLogout(t *testing.T) {
	h := newHarness(t)
	h.createUser("+254712345678", "", "pw")
	first := refreshCookieOf(t, h.login("0712345678", "pw"))

	rec := h.do("POST", "/v1/auth/refresh", nil, withCookie(first))
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh: %d %s", rec.Code, rec.Body)
	}
	second := refreshCookieOf(t, rec)
	if second.Value == first.Value {
		t.Fatal("refresh token was not rotated")
	}

	// Replaying the old token fails (within the grace window the family survives).
	if rec := h.do("POST", "/v1/auth/refresh", nil, withCookie(first)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("replay: %d", rec.Code)
	}
	rec = h.do("POST", "/v1/auth/refresh", nil, withCookie(second))
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh after replay in grace window: %d %s", rec.Code, rec.Body)
	}
	third := refreshCookieOf(t, rec)

	if rec := h.do("POST", "/v1/auth/logout", nil, withCookie(third)); rec.Code != http.StatusNoContent {
		t.Fatalf("logout: %d", rec.Code)
	}
	if rec := h.do("POST", "/v1/auth/refresh", nil, withCookie(third)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("refresh after logout: %d", rec.Code)
	}
	if rec := h.do("POST", "/v1/auth/refresh", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("refresh without cookie: %d", rec.Code)
	}
}

func TestMeReturnsMembershipsAndRoles(t *testing.T) {
	h := newHarness(t)
	uid := h.createUser("+254712345678", "", "pw")
	h.exec(`INSERT INTO user_platform_roles (user_id, role) VALUES ($1, 'admin')`, uid)
	h.exec(`INSERT INTO groups (name, slug, status) VALUES ('Jirani Welfare Group', 'jirani', 'active'), ('Imani Community Group', 'imani', 'active')`)
	h.exec(`INSERT INTO group_roles (group_id, name) SELECT id, 'Treasurer' FROM groups WHERE slug = 'jirani'`)
	h.exec(`INSERT INTO memberships (group_id, user_id, is_official, group_role_id)
	        SELECT g.id, $1, true, r.id FROM groups g JOIN group_roles r ON r.group_id = g.id WHERE g.slug = 'jirani'`, uid)
	h.exec(`INSERT INTO memberships (group_id, user_id) SELECT id, $1 FROM groups WHERE slug = 'imani'`, uid)

	if rec := h.do("GET", "/v1/me", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", rec.Code)
	}
	if rec := h.do("GET", "/v1/me", nil, withBearer("garbage")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad token: %d", rec.Code)
	}

	var sess sessionResponse
	_ = json.Unmarshal(h.login("0712345678", "pw").Body.Bytes(), &sess)
	rec := h.do("GET", "/v1/me", nil, withBearer(sess.AccessToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("me: %d %s", rec.Code, rec.Body)
	}
	var me meResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	if len(me.PlatformRoles) != 1 || me.PlatformRoles[0] != "admin" {
		t.Fatalf("roles = %v", me.PlatformRoles)
	}
	if len(me.Memberships) != 2 {
		t.Fatalf("memberships = %+v", me.Memberships)
	}
	// Ordered by group name.
	if me.Memberships[0].GroupSlug != "imani" || me.Memberships[0].IsOfficial {
		t.Fatalf("first membership = %+v", me.Memberships[0])
	}
	j := me.Memberships[1]
	if j.GroupSlug != "jirani" || !j.IsOfficial || j.RoleName == nil || *j.RoleName != "Treasurer" || j.Currency != "KES" {
		t.Fatalf("second membership = %+v", j)
	}
}

func TestHealth(t *testing.T) {
	h := newHarness(t)
	if rec := h.do("GET", "/healthz", nil); rec.Code != http.StatusOK {
		t.Fatalf("healthz: %d", rec.Code)
	}
}
