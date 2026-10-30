package mobile

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/swahome/legion-api/internal/auth"
	"github.com/swahome/legion-api/internal/store"
)

const (
	pinTTL         = 10 * time.Minute
	maxPinAttempts = 5
)

// The PIN sign-in: generate_pin sends a 4-digit PIN, verify_pin trades it
// for an access token. The legacy PINs never expired and could be guessed
// without limit; these expire after 10 minutes and lock after 5 wrong tries.

func (s *Server) generatePin(ctx context.Context, c *call) (any, error) {
	if c.req.Str("identity") == "" {
		return status(0, "Phone field empty"), nil
	}
	return s.sendPin(ctx, c, false)
}

// resendPin sends a new PIN, but only to someone who asked for one before.
func (s *Server) resendPin(ctx context.Context, c *call) (any, error) {
	return s.sendPin(ctx, c, true)
}

func (s *Server) sendPin(ctx context.Context, c *call, resend bool) (any, error) {
	id, err := auth.ParseIdentity(c.req.Str("identity"), s.auth.DefaultCallingCode())
	if err != nil {
		return status(0, "Invalid phone number or email address"), nil
	}
	if resend {
		if _, err := s.q.GetMobilePin(ctx, id.Key()); errors.Is(err, pgx.ErrNoRows) {
			return status(0, "Generate new pin"), nil
		} else if err != nil {
			return nil, err
		}
	}
	pin := randomPin()
	err = s.q.UpsertMobilePin(ctx, store.UpsertMobilePinParams{
		Identity: id.Key(), PinHash: pinHash(id.Key(), pin), UniqueCode: string(randomAlnum(16)), ExpiresAt: s.now().Add(pinTTL),
	})
	if err != nil {
		return nil, err
	}
	if err := s.otp.SendOTP(ctx, id.Phone, id.Email, pin, c.req.Str("appSignature")); err != nil {
		slog.WarnContext(ctx, "mobile: OTP not sent", "err", err)
		return status(0, "Error occured sending pin. Try again later"), nil
	}
	return status(1, "OTP sent"), nil
}

func (s *Server) verifyPin(ctx context.Context, c *call) (any, error) {
	rawID, pin := c.req.Str("identity"), c.req.Str("pin")
	if rawID == "" || pin == "" {
		return status(0, "User identity and pin is invalid"), nil
	}
	id, err := auth.ParseIdentity(rawID, s.auth.DefaultCallingCode())
	if err != nil {
		return status(0, "Invalid user identity. Kindly use a valid phone number or email address"), nil
	}
	p, err := s.q.GetMobilePin(ctx, id.Key())
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (p.ExpiresAt.Before(s.now()) || p.Attempts >= maxPinAttempts)) {
		return status(9, "Authentication does not exists. Try generating a new pin"), nil
	}
	if err != nil {
		return nil, err
	}
	if !hmac.Equal(p.PinHash, pinHash(id.Key(), pin)) {
		if err := s.q.FailMobilePin(ctx, id.Key()); err != nil {
			return nil, err
		}
		return status(0, "User verification failed. The pin entered does not match pin sent to you."), nil
	}

	user, err := s.auth.LookupIdentity(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		// No account yet: the app goes on to sign-up with unique_code.
		if err := s.q.MarkMobilePinVerified(ctx, id.Key()); err != nil {
			return nil, err
		}
		return map[string]any{"status": 1, "message": "Pin successfully verified", "user": nil,
			"unique_code": p.UniqueCode, "user_exists": 0, "user_is_validated": 0}, nil
	}
	if err != nil {
		return nil, err
	}
	if !user.IsActive {
		return status(0, "Could not complete the process. This account has been deactivated. Kindly contact support"), nil
	}
	if err := s.q.DeleteMobilePin(ctx, id.Key()); err != nil {
		return nil, err
	}
	resp, err := s.signIn(ctx, user)
	if err != nil {
		return nil, err
	}
	resp["user_exists"] = 1
	resp["unique_code"] = p.UniqueCode
	resp["user"].(map[string]any)["mobile_token"] = ""
	return resp, nil
}

// login is the password sign-in. It shares the web login's throttle.
func (s *Server) login(ctx context.Context, c *call) (any, error) {
	identity := c.req.Str("phone")
	if identity == "" {
		identity = c.req.Str("email")
	}
	password := c.req.Str("password")
	if identity == "" || password == "" {
		return timed(status(0, "User information is invalid. Try again"), s.now()), nil
	}
	user, err := s.auth.Authenticate(ctx, identity, password, clientInfo(c.r))
	var locked *auth.LockedOutError
	switch {
	case errors.As(err, &locked):
		return timed(status(0, fmt.Sprintf("Temporarily locked out. Try again in %d minutes.", int(locked.RetryAfter.Minutes())+1)), s.now()), nil
	case errors.Is(err, auth.ErrInvalidCredentials):
		return timed(status(0, "Invalid password username combination. Use the correct password"), s.now()), nil
	case errors.Is(err, auth.ErrPasswordResetRequired):
		return timed(status(0, "You need to reset your password. Use forgot password to set a new one."), s.now()), nil
	case errors.Is(err, auth.ErrAccountInactive):
		return timed(status(0, "account is inactive"), s.now()), nil
	case err != nil:
		return nil, err
	}
	return s.signIn(ctx, user)
}

// signIn issues an access token and returns the legacy login response
// with the user's check-in data.
func (s *Server) signIn(ctx context.Context, user store.User) (map[string]any, error) {
	token := string(randomAlnum(32))
	if err := s.q.CreateMobileToken(ctx, store.CreateMobileTokenParams{
		TokenHash: hashToken(token), UserID: user.ID, ExpiresAt: s.now().Add(tokenTTL),
	}); err != nil {
		return nil, err
	}
	checkin, err := s.checkinData(ctx, user.ID)
	if err != nil {
		return nil, err
	}
	resp := map[string]any{
		"status":            1,
		"message":           "Successfully logged in",
		"time":              s.now().Unix(),
		"access_token":      token,
		"user":              userPayload(user),
		"user_is_validated": boolInt(user.IsValidated),
	}
	for k, v := range checkin {
		resp[k] = v
	}
	return resp, nil
}

func (s *Server) getUserCheckinData(ctx context.Context, c *call) (any, error) {
	user, err := s.q.GetUserByID(ctx, c.userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return timed(status(4, "Could not find user details"), s.now()), nil
	}
	if err != nil {
		return nil, err
	}
	if err := s.q.TouchLastLogin(ctx, user.ID); err != nil {
		return nil, err
	}
	u := userPayload(user)
	u["username"] = strings.ToUpper(legacyPhone(user.Phone))
	resp := map[string]any{"status": 1, "message": "Checkin data", "time": s.now().Unix(), "user": u}
	checkin, err := s.checkinData(ctx, user.ID)
	if err != nil {
		return nil, err
	}
	for k, v := range checkin {
		resp[k] = v
	}
	return resp, nil
}

// userPayload is the legacy user object: names upper-cased, the phone
// without "+", and ids as strings, as MySQL returned them.
func userPayload(u store.User) map[string]any {
	var avatar any
	if u.Avatar != "" {
		avatar = u.Avatar
	}
	return map[string]any{
		"id":         fmt.Sprint(u.ID),
		"first_name": strings.ToUpper(u.FirstName),
		"last_name":  strings.ToUpper(u.LastName),
		"phone":      legacyPhone(u.Phone),
		"email":      deref(u.Email),
		"avatar":     avatar,
	}
}

// legacyPhone drops the "+": the old database stored 254712345678.
func legacyPhone(p *string) string {
	return strings.TrimPrefix(deref(p), "+")
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func timed(m map[string]any, now time.Time) map[string]any {
	m["time"] = now.Unix()
	return m
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func randomPin() string {
	n, err := rand.Int(rand.Reader, big.NewInt(9000))
	if err != nil {
		panic(err)
	}
	return fmt.Sprint(1000 + n.Int64())
}

// pinHash binds the PIN to its identity. A 4-digit PIN can't be protected
// by hashing alone; the expiry and attempt limit do that.
func pinHash(identity, pin string) []byte {
	m := hmac.New(sha256.New, []byte(identity))
	m.Write([]byte(pin))
	return m.Sum(nil)
}
