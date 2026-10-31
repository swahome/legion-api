// Package auth handles login, access tokens and refresh token rotation.
package auth

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"github.com/swahome/legion-api/internal/store"
)

// Throttling matches the legacy Ion Auth config: maximum_login_attempts = 3,
// lockout_time = 600, tracked per identity and IP address.
const (
	MaxLoginAttempts = 3
	LockoutWindow    = 10 * time.Minute

	// reuseGrace is how long a just-rotated refresh token can be presented
	// again before it counts as reuse.
	reuseGrace = 10 * time.Second
)

var (
	ErrInvalidCredentials    = errors.New("invalid credentials")
	ErrPasswordResetRequired = errors.New("password reset required")
	ErrAccountInactive       = errors.New("account inactive")
)

// LockedOutError is returned when too many failed attempts were made.
type LockedOutError struct{ RetryAfter time.Duration }

func (e *LockedOutError) Error() string {
	return fmt.Sprintf("too many failed login attempts, retry in %s", e.RetryAfter.Round(time.Second))
}

// dummyHash is compared against when the user does not exist, so a missing
// account takes as long to reject as a wrong password.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("legion-dummy-password"), 8)

type Service struct {
	pool       *pgxpool.Pool
	q          *store.Queries
	tokens     *TokenIssuer
	refreshTTL time.Duration
	defaultCC  string
	now        func() time.Time
}

func NewService(pool *pgxpool.Pool, tokens *TokenIssuer, refreshTTL time.Duration, defaultCC string) *Service {
	return &Service{
		pool:       pool,
		q:          store.New(pool),
		tokens:     tokens,
		refreshTTL: refreshTTL,
		defaultCC:  defaultCC,
		now:        time.Now,
	}
}

func (s *Service) Tokens() *TokenIssuer { return s.tokens }

// DefaultCallingCode is the calling code used to normalise local phone numbers.
func (s *Service) DefaultCallingCode() string { return s.defaultCC }

// ClientInfo describes the caller, for throttling and the refresh token audit trail.
type ClientInfo struct {
	IP        *netip.Addr
	UserAgent string
}

type Session struct {
	User             store.User
	AccessToken      string
	AccessExpiresAt  time.Time
	RefreshToken     string
	RefreshExpiresAt time.Time
}

func (s *Service) Login(ctx context.Context, rawIdentity, password string, c ClientInfo) (Session, error) {
	user, err := s.Authenticate(ctx, rawIdentity, password, c)
	if err != nil {
		return Session{}, err
	}
	return s.startSession(ctx, s.q, user, uuid.New(), c)
}

// Authenticate checks a password with the login throttle, without starting
// a web session. The mobile app API issues its own tokens.
func (s *Service) Authenticate(ctx context.Context, rawIdentity, password string, c ClientInfo) (store.User, error) {
	id, err := ParseIdentity(rawIdentity, s.defaultCC)
	if err != nil {
		return store.User{}, ErrInvalidCredentials
	}
	if err := s.checkThrottle(ctx, id.Key(), c.IP); err != nil {
		return store.User{}, err
	}

	user, err := s.lookup(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		return store.User{}, s.failAttempt(ctx, id.Key(), c.IP)
	}
	if err != nil {
		return store.User{}, err
	}
	if user.PasswordResetRequired || user.PasswordHash == nil {
		return store.User{}, ErrPasswordResetRequired
	}
	if bcrypt.CompareHashAndPassword([]byte(*user.PasswordHash), []byte(password)) != nil {
		return store.User{}, s.failAttempt(ctx, id.Key(), c.IP)
	}
	if !user.IsActive {
		return store.User{}, ErrAccountInactive
	}

	if err := s.q.ClearLoginAttempts(ctx, store.ClearLoginAttemptsParams{Identity: id.Key(), IpAddress: c.IP}); err != nil {
		return store.User{}, err
	}
	if err := s.q.TouchLastLogin(ctx, user.ID); err != nil {
		return store.User{}, err
	}
	return user, nil
}

// ConfirmPassword re-checks a signed-in user's password before a sensitive
// step, such as approving a withdrawal. Wrong guesses count towards the
// same lockout as logins, keyed by the user.
func (s *Service) ConfirmPassword(ctx context.Context, userID int64, password string, c ClientInfo) error {
	key := fmt.Sprintf("user:%d", userID)
	if err := s.checkThrottle(ctx, key, c.IP); err != nil {
		return err
	}
	user, err := s.q.GetUserByID(ctx, userID)
	if err != nil {
		return err
	}
	if user.PasswordHash == nil || bcrypt.CompareHashAndPassword([]byte(*user.PasswordHash), []byte(password)) != nil {
		return s.failAttempt(ctx, key, c.IP)
	}
	return s.q.ClearLoginAttempts(ctx, store.ClearLoginAttemptsParams{Identity: key, IpAddress: c.IP})
}

// LookupIdentity finds the user for a phone number or email address.
func (s *Service) LookupIdentity(ctx context.Context, id Identity) (store.User, error) {
	return s.lookup(ctx, id)
}

// Refresh rotates a refresh token. Presenting a token that was already
// rotated or revoked revokes its whole family, which logs out a thief and
// the real user alike and forces a fresh login.
func (s *Service) Refresh(ctx context.Context, refreshToken string, c ClientInfo) (Session, error) {
	if refreshToken == "" {
		return Session{}, ErrInvalidToken
	}
	var (
		sess         Session
		revokeFamily *uuid.UUID
	)
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		rt, err := q.GetRefreshTokenByHash(ctx, hashRefreshToken(refreshToken))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidToken
		}
		if err != nil {
			return err
		}
		if rt.RevokedAt != nil {
			// A token rotated moments ago is most likely a second tab racing
			// the first, not theft, so it fails without killing the family.
			if s.now().Sub(*rt.RevokedAt) > reuseGrace {
				revokeFamily = &rt.FamilyID
			}
			return ErrInvalidToken
		}
		if !s.now().Before(rt.ExpiresAt) {
			return ErrInvalidToken
		}
		n, err := q.RevokeRefreshToken(ctx, rt.ID)
		if err != nil {
			return err
		}
		if n == 0 { // lost a race with a concurrent refresh, e.g. two tabs
			return ErrInvalidToken
		}
		user, err := q.GetUserByID(ctx, rt.UserID)
		if err != nil {
			return err
		}
		if !user.IsActive || user.PasswordResetRequired {
			revokeFamily = &rt.FamilyID
			return ErrInvalidToken
		}
		sess, err = s.startSession(ctx, q, user, rt.FamilyID, c)
		return err
	})
	// The transaction rolled back, so revoke outside it.
	if revokeFamily != nil {
		if rerr := s.q.RevokeRefreshTokenFamily(ctx, *revokeFamily); rerr != nil {
			return Session{}, rerr
		}
	}
	return sess, err
}

// Logout revokes the presented refresh token's family. Unknown tokens are ignored.
func (s *Service) Logout(ctx context.Context, refreshToken string) error {
	if refreshToken == "" {
		return nil
	}
	rt, err := s.q.GetRefreshTokenByHash(ctx, hashRefreshToken(refreshToken))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.q.RevokeRefreshTokenFamily(ctx, rt.FamilyID)
}

func (s *Service) lookup(ctx context.Context, id Identity) (store.User, error) {
	if id.Phone != "" {
		return s.q.GetUserByPhone(ctx, &id.Phone)
	}
	return s.q.GetUserByEmail(ctx, id.Email)
}

func (s *Service) checkThrottle(ctx context.Context, key string, ip *netip.Addr) error {
	since := s.now().Add(-LockoutWindow)
	n, err := s.q.CountRecentLoginAttempts(ctx, store.CountRecentLoginAttemptsParams{Identity: key, IpAddress: ip, Since: since})
	if err != nil {
		return err
	}
	if n < MaxLoginAttempts {
		return nil
	}
	oldest, err := s.q.OldestRecentLoginAttempt(ctx, store.OldestRecentLoginAttemptParams{Identity: key, IpAddress: ip, Since: since})
	if err != nil {
		return err
	}
	return &LockedOutError{RetryAfter: max(oldest.Add(LockoutWindow).Sub(s.now()), time.Second)}
}

func (s *Service) failAttempt(ctx context.Context, key string, ip *netip.Addr) error {
	if err := s.q.RecordLoginAttempt(ctx, store.RecordLoginAttemptParams{Identity: key, IpAddress: ip}); err != nil {
		return err
	}
	return ErrInvalidCredentials
}

func (s *Service) startSession(ctx context.Context, q *store.Queries, user store.User, family uuid.UUID, c ClientInfo) (Session, error) {
	roles, err := q.ListUserPlatformRoles(ctx, user.ID)
	if err != nil {
		return Session{}, err
	}
	admin := false
	for _, r := range roles {
		admin = admin || r == store.PlatformRoleAdmin
	}
	access, accessExp, err := s.tokens.Issue(Claims{UserID: user.ID, Admin: admin})
	if err != nil {
		return Session{}, err
	}
	refresh, hash, err := newRefreshToken()
	if err != nil {
		return Session{}, err
	}
	refreshExp := s.now().Add(s.refreshTTL)
	var ua *string
	if c.UserAgent != "" {
		ua = &c.UserAgent
	}
	err = q.CreateRefreshToken(ctx, store.CreateRefreshTokenParams{
		UserID:    user.ID,
		TokenHash: hash,
		FamilyID:  family,
		ExpiresAt: refreshExp,
		UserAgent: ua,
		IpAddress: c.IP,
	})
	if err != nil {
		return Session{}, err
	}
	return Session{
		User:             user,
		AccessToken:      access,
		AccessExpiresAt:  accessExp,
		RefreshToken:     refresh,
		RefreshExpiresAt: refreshExp,
	}, nil
}
