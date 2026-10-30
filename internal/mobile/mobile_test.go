package mobile

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/swahome/legion-api/internal/auth"
	"github.com/swahome/legion-api/internal/testdb"
)

// app plays the mobile app: it holds the server's public key and its own
// private key, the reverse of the server.
type app struct {
	t         *testing.T
	serverPub *rsa.PublicKey
	appKey    *rsa.PrivateKey
	url       string
	version   string
}

var (
	keysOnce            sync.Once
	serverKey, appKey   *rsa.PrivateKey
	serverKeysUnderTest *Keys
)

func testKeys(t *testing.T) (*Keys, *rsa.PrivateKey, *rsa.PrivateKey) {
	keysOnce.Do(func() {
		serverKey, _ = rsa.GenerateKey(rand.Reader, 2048)
		appKey, _ = rsa.GenerateKey(rand.Reader, 2048)
		keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(serverKey)})
		pub, _ := x509.MarshalPKIXPublicKey(&appKey.PublicKey)
		certPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pub})
		var err error
		if serverKeysUnderTest, err = ParseKeys(keyPEM, certPEM); err != nil {
			panic(err)
		}
	})
	return serverKeysUnderTest, serverKey, appKey
}

// seal encrypts a request the way the app does.
func (a *app) seal(body any) (secret string, payload []byte) {
	key := []byte("k3y") // short on purpose: the server pads it with "0"
	wrapped, err := rsa.EncryptPKCS1v15(rand.Reader, a.serverPub, key)
	if err != nil {
		a.t.Fatal(err)
	}
	plain, _ := json.Marshal(body)
	ct, err := sealAES(key, plain)
	if err != nil {
		a.t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(wrapped), []byte(ct)
}

// open decrypts a response the way the app does.
func (a *app) open(raw []byte) map[string]any {
	a.t.Helper()
	var env struct{ Secret, Body string }
	if err := json.Unmarshal(raw, &env); err != nil || env.Secret == "" {
		a.t.Fatalf("not an encrypted response: %s", raw)
	}
	wrapped, _ := base64.StdEncoding.DecodeString(env.Secret)
	key, err := rsa.DecryptPKCS1v15(nil, a.appKey, wrapped)
	if err != nil || len(key) != 16 {
		a.t.Fatalf("response key: %v", err)
	}
	ctB64, ivB64, _ := strings.Cut(env.Body, ":")
	ct, _ := base64.StdEncoding.DecodeString(ctB64)
	iv, _ := base64.StdEncoding.DecodeString(ivB64)
	block, _ := aes.NewCipher(key)
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(ct, ct)
	ct = ct[:len(ct)-int(ct[len(ct)-1])]
	var out map[string]any
	if err := json.Unmarshal(ct, &out); err != nil {
		a.t.Fatalf("response JSON: %v: %s", err, ct)
	}
	return out
}

type reply struct {
	code int
	body map[string]any
	raw  []byte
}

func (a *app) post(path, token string, body any) reply {
	a.t.Helper()
	secret, payload := a.seal(body)
	return a.send(path, token, secret, payload)
}

func (a *app) send(path, token, secret string, payload []byte) reply {
	a.t.Helper()
	req, _ := http.NewRequest("POST", a.url+"/mobile/"+path, bytes.NewReader(payload))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if secret != "" {
		req.Header.Set("Secret", secret)
	}
	req.Header.Set("Versioncode", a.version)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer res.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(res.Body)
	r := reply{code: res.StatusCode, raw: buf.Bytes()}
	if strings.Contains(buf.String(), `"secret"`) {
		r.body = a.open(buf.Bytes())
	} else {
		_ = json.Unmarshal(buf.Bytes(), &r.body)
	}
	return r
}

type fakeOTP struct {
	mu   sync.Mutex
	last map[string]string
	fail bool
}

func (f *fakeOTP) SendOTP(_ context.Context, phone, email, pin, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return errNoOTPSender
	}
	f.last[phone+email] = pin
	return nil
}

type fixture struct {
	*app
	srv  *Server
	otp  *fakeOTP
	exec func(sql string, args ...any)
	user int64
}

func newFixture(t *testing.T) *fixture {
	pool := testdb.New(t)
	keys, server, appPriv := testKeys(t)
	svc := auth.NewService(pool, auth.NewTokenIssuer([]byte("0123456789abcdef0123456789abcdef"), time.Minute), time.Hour, "254")
	otp := &fakeOTP{last: map[string]string{}}
	srv := New(pool, keys, svc, otp)
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	hash, _ := bcrypt.GenerateFromPassword([]byte("secret-pw"), 8)
	var uid int64
	if err := pool.QueryRow(context.Background(), `INSERT INTO users (first_name, last_name, phone, password_hash, avatar)
		VALUES ('Akinyi', 'Otieno', '+254712345678', $1, 'a.jpg') RETURNING id`, string(hash)).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	var gid, role int64
	_ = pool.QueryRow(context.Background(), `INSERT INTO groups (name, slug, status, settings) VALUES ('Umoja', 'umoja', 'active',
		'{"size": "12", "theme": "blue", "disable_arrears": "1"}') RETURNING id`).Scan(&gid)
	_ = pool.QueryRow(context.Background(), `INSERT INTO group_roles (group_id, name) VALUES ($1, 'Treasurer') RETURNING id`, gid).Scan(&role)
	exec(`INSERT INTO group_roles (group_id, name) VALUES ($1, 'Chairperson')`, gid)
	exec(`INSERT INTO memberships (group_id, user_id, is_official, group_role_id) VALUES ($1, $2, true, $3)`, gid, uid, role)

	a := &app{t: t, serverPub: &server.PublicKey, appKey: appPriv, url: ts.URL, version: "80"}
	return &fixture{app: a, srv: srv, otp: otp, exec: exec, user: uid}
}

func (f *fixture) login() string {
	f.t.Helper()
	r := f.post("login", "", map[string]any{"phone": "0712 345 678", "password": "secret-pw", "user_id": 0, "group_id": 0})
	if r.body["status"] != float64(1) {
		f.t.Fatalf("login: %s", r.raw)
	}
	return r.body["access_token"].(string)
}

func TestPasswordLoginReturnsTokenAndCheckinData(t *testing.T) {
	f := newFixture(t)
	r := f.post("login", "", map[string]any{"phone": "0712345678", "password": "secret-pw"})
	if r.code != 200 || r.body["status"] != float64(1) {
		t.Fatalf("login: %d %v", r.code, r.body)
	}
	if tok := r.body["access_token"].(string); len(tok) != 32 {
		t.Fatalf("token %q", tok)
	}
	u := r.body["user"].(map[string]any)
	if u["first_name"] != "AKINYI" || u["phone"] != "254712345678" || u["avatar"] != "a.jpg" || u["id"] == nil {
		t.Fatalf("user: %v", u)
	}
	groups := r.body["user_groups"].([]any)
	if len(groups) != 1 {
		t.Fatalf("groups: %v", r.body)
	}
	g := groups[0].(map[string]any)
	if g["name"] != "Umoja" || g["size"] != "12" || g["theme"] != "blue" || g["role"] != "Treasurer" || g["is_admin"] != float64(1) ||
		g["activation_status"] != "Active" || g["group_currency"] != "KES" || g["disable_arrears"] != "1" {
		t.Fatalf("group: %v", g)
	}
	// Roles keep PHP's order: by name.
	if !strings.Contains(string(mustJSON(t, f, r)), `"group_roles":{"`) {
		t.Fatal("group_roles should be an object")
	}
	if inv := r.body["invited_user_groups"].([]any); len(inv) != 0 {
		t.Fatalf("invited: %v", inv)
	}
}

// mustJSON re-reads the decrypted response as raw JSON, to check key order.
func mustJSON(t *testing.T, f *fixture, r reply) []byte {
	var env struct{ Secret, Body string }
	_ = json.Unmarshal(r.raw, &env)
	wrapped, _ := base64.StdEncoding.DecodeString(env.Secret)
	key, _ := rsa.DecryptPKCS1v15(nil, f.appKey, wrapped)
	ctB64, ivB64, _ := strings.Cut(env.Body, ":")
	ct, _ := base64.StdEncoding.DecodeString(ctB64)
	iv, _ := base64.StdEncoding.DecodeString(ivB64)
	block, _ := aes.NewCipher(key)
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(ct, ct)
	out := ct[:len(ct)-int(ct[len(ct)-1])]
	if i, j := bytes.Index(out, []byte("Chairperson")), bytes.Index(out, []byte("Treasurer")); i < 0 || j < 0 || i > j {
		t.Fatalf("roles not ordered by name: %s", out)
	}
	return out
}

func TestLoginFailures(t *testing.T) {
	f := newFixture(t)
	r := f.post("login", "", map[string]any{"phone": "0712345678", "password": "wrong"})
	if r.body["status"] != float64(0) || !strings.Contains(r.body["message"].(string), "Invalid password") {
		t.Fatalf("wrong password: %v", r.body)
	}
	r = f.post("login", "", map[string]any{"phone": "", "password": ""})
	if r.body["status"] != float64(0) {
		t.Fatalf("empty: %v", r.body)
	}
}

func TestOlderBuildsKeepTheResponseWrapper(t *testing.T) {
	f := newFixture(t)
	f.version = "76"
	r := f.post("login", "", map[string]any{"phone": "0712345678", "password": "secret-pw"})
	resp, ok := r.body["response"].(map[string]any)
	if !ok || resp["status"] != float64(1) {
		t.Fatalf("want wrapped response: %v", r.body)
	}
	f.version = "73"
	r = f.post("login", "", map[string]any{"phone": "0712345678", "password": "secret-pw"})
	if r.body["status"] != float64(0) || !strings.Contains(r.body["message"].(string), "outdated") {
		t.Fatalf("old build: %s", r.raw)
	}
}

func TestTokenChecks(t *testing.T) {
	f := newFixture(t)
	body := map[string]any{"user_id": f.user, "group_id": 0}

	if r := f.post("get_user_checkin_data", "", body); r.body["status"] != "401" {
		t.Fatalf("no token: %v", r.body)
	}
	if r := f.post("get_user_checkin_data", "not-a-token", body); r.body["status"] != float64(400) {
		t.Fatalf("bad token: %v", r.body)
	}
	tok := f.login()
	if r := f.post("get_user_checkin_data", tok, map[string]any{"user_id": f.user + 1}); r.body["status"] != float64(400) ||
		r.body["message"] != "user Authentication token is invalid." {
		t.Fatalf("someone else's user_id: %v", r.body)
	}
	// user_id may come as a string.
	r := f.post("get_user_checkin_data", tok, map[string]any{"user_id": fmt.Sprint(f.user)})
	if r.body["status"] != float64(1) || len(r.body["user_groups"].([]any)) != 1 {
		t.Fatalf("checkin: %v", r.body)
	}
	if r := f.post("no_such_action", tok, body); r.code != 404 || !strings.Contains(r.body["error"].(string), "mobile/no_such_action") {
		t.Fatalf("unknown action: %d %v", r.code, r.body)
	}

	// A deactivated user's token stops working at once.
	f.exec(`UPDATE users SET is_active = false WHERE id = $1`, f.user)
	if r := f.post("get_user_checkin_data", tok, body); r.body["status"] != float64(400) {
		t.Fatalf("inactive user: %v", r.body)
	}
}

func TestBadPayloads(t *testing.T) {
	f := newFixture(t)
	secret, _ := f.seal(map[string]any{})
	if r := f.send("login", "", secret, nil); r.body["status"] != float64(2) {
		t.Fatalf("empty body: %v", r.body)
	}
	if r := f.send("login", "", secret, []byte("garbage")); r.body["status"] != float64(3) {
		t.Fatalf("garbage: %v", r.body)
	}
	_, payload := f.seal(map[string]any{"phone": "0712345678"})
	if r := f.send("login", "", "", payload); r.body["status"] != "401" {
		t.Fatalf("no Secret: %v", r.body)
	}
}

func TestPinSignIn(t *testing.T) {
	f := newFixture(t)
	if r := f.post("resend_pin", "", map[string]any{"identity": "0712345678"}); r.body["message"] != "Generate new pin" {
		t.Fatalf("resend before generate: %v", r.body)
	}
	if r := f.post("generate_pin", "", map[string]any{"identity": "0712345678"}); r.body["status"] != float64(1) {
		t.Fatalf("generate: %v", r.body)
	}
	pin := f.otp.last["+254712345678"]
	if len(pin) != 4 {
		t.Fatalf("pin %q", pin)
	}
	wrong := "0000"
	if r := f.post("verify_pin", "", map[string]any{"identity": "254712345678", "pin": wrong}); r.body["status"] != float64(0) {
		t.Fatalf("wrong pin: %v", r.body)
	}
	r := f.post("verify_pin", "", map[string]any{"identity": "+254712345678", "pin": pin})
	if r.body["status"] != float64(1) || r.body["user_exists"] != float64(1) || len(r.body["access_token"].(string)) != 32 {
		t.Fatalf("verify: %v", r.body)
	}
	// A PIN works once.
	if r := f.post("verify_pin", "", map[string]any{"identity": "0712345678", "pin": pin}); r.body["status"] != float64(9) {
		t.Fatalf("reuse: %v", r.body)
	}
}

func TestPinLocksAfterWrongTries(t *testing.T) {
	f := newFixture(t)
	f.post("generate_pin", "", map[string]any{"identity": "0712345678"})
	pin := f.otp.last["+254712345678"]
	for range maxPinAttempts {
		f.post("verify_pin", "", map[string]any{"identity": "0712345678", "pin": "9999x"})
	}
	if r := f.post("verify_pin", "", map[string]any{"identity": "0712345678", "pin": pin}); r.body["status"] != float64(9) {
		t.Fatalf("locked pin accepted: %v", r.body)
	}
}

func TestPinForNewUserAndExpiry(t *testing.T) {
	f := newFixture(t)
	f.post("generate_pin", "", map[string]any{"identity": "new@example.com"})
	pin := f.otp.last["new@example.com"]
	r := f.post("verify_pin", "", map[string]any{"identity": "New@Example.com", "pin": pin})
	if r.body["status"] != float64(1) || r.body["user_exists"] != float64(0) || r.body["unique_code"] == "" {
		t.Fatalf("new user: %v", r.body)
	}

	f.post("generate_pin", "", map[string]any{"identity": "0712345678"})
	f.exec(`UPDATE mobile_pins SET expires_at = now() - interval '1 second'`)
	if r := f.post("verify_pin", "", map[string]any{"identity": "0712345678", "pin": f.otp.last["+254712345678"]}); r.body["status"] != float64(9) {
		t.Fatalf("expired pin: %v", r.body)
	}

	f.otp.fail = true
	if r := f.post("generate_pin", "", map[string]any{"identity": "0712345678"}); r.body["status"] != float64(0) {
		t.Fatalf("unsent pin: %v", r.body)
	}
}

func TestAESKeyPadding(t *testing.T) {
	if got := string(aesKey([]byte("abc"))); got != "abc0000000000000" {
		t.Fatal(got)
	}
	if got := string(aesKey([]byte("0123456789abcdefXYZ"))); got != "0123456789abcdef" {
		t.Fatal(got)
	}
}
