package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/swahome/legion-api/internal/testdb"
)

func TestRefreshReuseAfterGraceRevokesFamily(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	hash, _ := bcrypt.GenerateFromPassword([]byte("pw"), 4)
	if _, err := pool.Exec(ctx, `INSERT INTO users (phone, password_hash) VALUES ('+254712345678', $1)`, string(hash)); err != nil {
		t.Fatal(err)
	}
	svc := NewService(pool, NewTokenIssuer([]byte("0123456789abcdef0123456789abcdef"), time.Minute), time.Hour, "254")
	clock := time.Now()
	svc.now = func() time.Time { return clock }

	s1, err := svc.Login(ctx, "0712345678", "pw", ClientInfo{})
	if err != nil {
		t.Fatal(err)
	}
	s2, err := svc.Refresh(ctx, s1.RefreshToken, ClientInfo{})
	if err != nil {
		t.Fatal(err)
	}

	// An attacker replays the stolen first token well after it was rotated.
	clock = clock.Add(time.Minute)
	if _, err := svc.Refresh(ctx, s1.RefreshToken, ClientInfo{}); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("replay: %v", err)
	}
	// The legitimate holder's current token is now dead too.
	if _, err := svc.Refresh(ctx, s2.RefreshToken, ClientInfo{}); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("family not revoked: %v", err)
	}
}

func TestRefreshTokenExpires(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	hash, _ := bcrypt.GenerateFromPassword([]byte("pw"), 4)
	if _, err := pool.Exec(ctx, `INSERT INTO users (email, password_hash) VALUES ('a@example.com', $1)`, string(hash)); err != nil {
		t.Fatal(err)
	}
	svc := NewService(pool, NewTokenIssuer([]byte("0123456789abcdef0123456789abcdef"), time.Minute), time.Hour, "254")
	clock := time.Now()
	svc.now = func() time.Time { return clock }

	s, err := svc.Login(ctx, "a@example.com", "pw", ClientInfo{})
	if err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(2 * time.Hour)
	if _, err := svc.Refresh(ctx, s.RefreshToken, ClientInfo{}); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expired refresh accepted: %v", err)
	}
}
