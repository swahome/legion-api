package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const issuer = "legion-api"

var ErrInvalidToken = errors.New("invalid or expired token")

// Claims are the access token claims. Group access is not encoded in the
// token: it is checked per request against memberships, so removing a member
// takes effect immediately.
type Claims struct {
	UserID int64
	Admin  bool // platform admin
}

type TokenIssuer struct {
	secret []byte
	ttl    time.Duration
	now    func() time.Time
}

func NewTokenIssuer(secret []byte, ttl time.Duration) *TokenIssuer {
	return &TokenIssuer{secret: secret, ttl: ttl, now: time.Now}
}

type jwtClaims struct {
	Admin bool `json:"adm,omitempty"`
	jwt.RegisteredClaims
}

// Issue returns a signed access token and its expiry.
func (t *TokenIssuer) Issue(c Claims) (string, time.Time, error) {
	now := t.now()
	exp := now.Add(t.ttl)
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwtClaims{
		Admin: c.Admin,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    issuer,
			Subject:   strconv.FormatInt(c.UserID, 10),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
		},
	})
	s, err := tok.SignedString(t.secret)
	return s, exp, err
}

func (t *TokenIssuer) Parse(s string) (Claims, error) {
	var jc jwtClaims
	_, err := jwt.ParseWithClaims(s, &jc, func(*jwt.Token) (any, error) { return t.secret, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(issuer),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(t.now),
	)
	if err != nil {
		return Claims{}, ErrInvalidToken
	}
	id, err := strconv.ParseInt(jc.Subject, 10, 64)
	if err != nil {
		return Claims{}, ErrInvalidToken
	}
	return Claims{UserID: id, Admin: jc.Admin}, nil
}

// newRefreshToken returns an opaque token for the client and the hash we store.
func newRefreshToken() (token string, hash []byte, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", nil, fmt.Errorf("generate refresh token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, hashRefreshToken(token), nil
}

func hashRefreshToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}
