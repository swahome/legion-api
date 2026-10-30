package auth

import (
	"testing"
	"time"
)

func TestTokenRoundTrip(t *testing.T) {
	ti := NewTokenIssuer([]byte("0123456789abcdef0123456789abcdef"), time.Minute)
	tok, _, err := ti.Issue(Claims{UserID: 42, Admin: true})
	if err != nil {
		t.Fatal(err)
	}
	c, err := ti.Parse(tok)
	if err != nil {
		t.Fatal(err)
	}
	if c.UserID != 42 || !c.Admin {
		t.Fatalf("got %+v", c)
	}
}

func TestTokenRejectsExpiredAndForeign(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	ti := NewTokenIssuer(secret, time.Minute)
	tok, _, _ := ti.Issue(Claims{UserID: 1})

	later := NewTokenIssuer(secret, time.Minute)
	later.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	if _, err := later.Parse(tok); err == nil {
		t.Error("expired token accepted")
	}

	other := NewTokenIssuer([]byte("ffffffffffffffffffffffffffffffff"), time.Minute)
	if _, err := other.Parse(tok); err == nil {
		t.Error("token signed with another secret accepted")
	}
	if _, err := ti.Parse("not.a.token"); err == nil {
		t.Error("garbage accepted")
	}
}
