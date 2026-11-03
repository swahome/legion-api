// Package account holds what web and mobile sign-up share: phone codes,
// password resets, member invites and creating a group with its defaults.
// Every function takes the caller's transaction, and the SMS it queues go
// out only if that transaction commits.
package account

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/swahome/legion-api/internal/sms"
	"github.com/swahome/legion-api/internal/store"
)

// Service carries the settings the messages need.
type Service struct {
	// WebURL is where the web app lives, e.g. https://legion.com, for the
	// links in invites. Empty leaves the link out.
	WebURL string
	Now    func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

const (
	codeTTL        = 15 * time.Minute
	maxCodeTries   = 5
	codesPerHour   = 5
	inviteTTL      = 30 * 24 * time.Hour
	MinPasswordLen = 8
)

var (
	ErrCodeInvalid  = errors.New("code is wrong")
	ErrCodeExpired  = errors.New("code expired or used up")
	ErrTooManyCodes = errors.New("too many codes requested")
	ErrNoPhone      = errors.New("user has no phone number")
	ErrWeakPassword = fmt.Errorf("password must be at least %d characters", MinPasswordLen)
)

// AppName is the product name in messages (platform setting
// application_name, which the legacy import carries over).
func AppName(ctx context.Context, q *store.Queries) string {
	if v, err := q.GetPlatformSetting(ctx, "application_name"); err == nil {
		var name string
		if json.Unmarshal(v, &name) == nil && strings.TrimSpace(name) != "" {
			return strings.TrimSpace(name)
		}
	}
	return "Legion"
}

// ---- Codes ----

// Purpose says what a code proves.
type Purpose = store.UserCodePurpose

const (
	VerifyPhone   = store.UserCodePurposeVerifyPhone
	ResetPassword = store.UserCodePurposeResetPassword
	ChangePhone   = store.UserCodePurposeChangePhone
)

// SendCode makes a new code for the user, replacing any earlier one, and
// queues it by SMS. digits is 6 on the web; the mobile app's screens take 4.
func (s *Service) SendCode(ctx context.Context, q *store.Queries, u store.User, purpose Purpose, digits int) error {
	if u.Phone == nil {
		return ErrNoPhone
	}
	return s.SendCodeTo(ctx, q, u, purpose, digits, *u.Phone)
}

// SendCodeTo is SendCode to another number: a phone the user is moving to.
func (s *Service) SendCodeTo(ctx context.Context, q *store.Queries, u store.User, purpose Purpose, digits int, phone string) error {
	kind := string(purpose)
	n, err := q.CountRecentSMS(ctx, store.CountRecentSMSParams{UserID: &u.ID, Kind: kind, Since: s.now().Add(-time.Hour)})
	if err != nil {
		return err
	}
	if n >= codesPerHour {
		return ErrTooManyCodes
	}
	code := randomDigits(digits)
	if err := q.PutUserCode(ctx, store.PutUserCodeParams{
		UserID: u.ID, Purpose: purpose, CodeHash: codeHash(u.ID, purpose, code), ExpiresAt: s.now().Add(codeTTL),
	}); err != nil {
		return err
	}
	app := AppName(ctx, q)
	var body string
	switch purpose {
	case VerifyPhone:
		body = fmt.Sprintf("Dear %s, kindly use %s as the activation code. %s. DO NOT share this code", greet(u), code, app)
	case ChangePhone:
		body = fmt.Sprintf("Dear %s, use %s to confirm this as your new %s phone number. DO NOT share this code", greet(u), code, app)
	default:
		body = fmt.Sprintf("Dear %s, your password recovery code is %s - %s. Thank you. DO NOT share this code", greet(u), code, app)
	}
	_, err = sms.Queue(ctx, q, sms.Message{To: phone, Body: body, Kind: kind, UserID: &u.ID})
	return err
}

// CheckCode checks a code and uses it up. Wrong guesses count against it.
func (s *Service) CheckCode(ctx context.Context, q *store.Queries, userID int64, purpose Purpose, code string) error {
	if err := s.peekCode(ctx, q, userID, purpose, code); err != nil {
		return err
	}
	return q.DeleteUserCode(ctx, store.DeleteUserCodeParams{UserID: userID, Purpose: purpose})
}

func (s *Service) peekCode(ctx context.Context, q *store.Queries, userID int64, purpose Purpose, code string) error {
	c, err := q.GetUserCode(ctx, store.GetUserCodeParams{UserID: userID, Purpose: purpose})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrCodeExpired
	}
	if err != nil {
		return err
	}
	if c.ExpiresAt.Before(s.now()) || c.Attempts >= maxCodeTries {
		return ErrCodeExpired
	}
	if !hmac.Equal(c.CodeHash, codeHash(userID, purpose, strings.TrimSpace(code))) {
		if err := q.FailUserCode(ctx, store.FailUserCodeParams{UserID: userID, Purpose: purpose}); err != nil {
			return err
		}
		return ErrCodeInvalid
	}
	return nil
}

// ExchangeResetCode checks a reset code and swaps it for a one-use token
// (the mobile app checks the code on one screen and sets the password on
// the next).
func (s *Service) ExchangeResetCode(ctx context.Context, q *store.Queries, userID int64, code string) (string, error) {
	if err := s.peekCode(ctx, q, userID, ResetPassword, code); err != nil {
		return "", err
	}
	tok := RandomToken()
	return tok, q.SetUserCodeToken(ctx, store.SetUserCodeTokenParams{UserID: userID, Purpose: ResetPassword, TokenHash: TokenHash(tok)})
}

// UseResetToken returns the user a reset token belongs to and uses it up.
func (s *Service) UseResetToken(ctx context.Context, q *store.Queries, token string) (int64, error) {
	c, err := q.GetUserCodeByToken(ctx, TokenHash(token))
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrCodeExpired
	}
	if err != nil {
		return 0, err
	}
	if c.ExpiresAt.Before(s.now()) {
		return 0, ErrCodeExpired
	}
	return c.UserID, q.DeleteUserCode(ctx, store.DeleteUserCodeParams{UserID: c.UserID, Purpose: ResetPassword})
}

// ---- Passwords ----

// HashPassword checks the length and hashes a new password.
func HashPassword(pw string) (string, error) {
	if len(pw) < MinPasswordLen {
		return "", ErrWeakPassword
	}
	b, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	return string(b), err
}

// SetPassword sets a user's password after a reset or an invite, signs out
// their other web sessions and app tokens, and tells them by SMS when
// `notify` is set.
func (s *Service) SetPassword(ctx context.Context, q *store.Queries, u store.User, pw string, notify bool) error {
	hash, err := HashPassword(pw)
	if err != nil {
		return err
	}
	if err := q.ResetUserPassword(ctx, store.ResetUserPasswordParams{ID: u.ID, PasswordHash: &hash}); err != nil {
		return err
	}
	if err := q.RevokeUserRefreshTokens(ctx, u.ID); err != nil {
		return err
	}
	if err := q.DeleteUserMobileTokens(ctx, u.ID); err != nil {
		return err
	}
	if !notify || u.Phone == nil {
		return nil
	}
	body := fmt.Sprintf("Dear %s, you have successfully reset your %s password.", greet(u), AppName(ctx, q))
	_, err = sms.Queue(ctx, q, sms.Message{To: *u.Phone, Body: body, Kind: "password_changed", UserID: &u.ID})
	return err
}

// ---- Helpers ----

func greet(u store.User) string {
	if n := strings.TrimSpace(u.FirstName); n != "" {
		return titleCase(n)
	}
	return "member"
}

func titleCase(s string) string {
	words := strings.Fields(strings.ToLower(s))
	for i, w := range words {
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ")
}

func randomDigits(n int) string {
	max := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
	min := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n-1)), nil)
	v, err := rand.Int(rand.Reader, new(big.Int).Sub(max, min))
	if err != nil {
		panic(err)
	}
	return v.Add(v, min).String()
}

// codeHash binds a code to its user and purpose. A short code can't be
// protected by hashing alone; the expiry and attempt limit do that.
func codeHash(userID int64, purpose Purpose, code string) []byte {
	m := hmac.New(sha256.New, []byte(fmt.Sprintf("%d:%s", userID, purpose)))
	m.Write([]byte(code))
	return m.Sum(nil)
}

// RandomToken is an unguessable URL-safe token.
func RandomToken() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// TokenHash is what is stored for a token.
func TokenHash(tok string) []byte {
	h := sha256.Sum256([]byte(tok))
	return h[:]
}
