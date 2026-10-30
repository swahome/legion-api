package httpapi

import (
	"context"
	"errors"
	"math"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/swahome/legion-api/internal/auth"
)

const refreshCookie = "legion_refresh"

// The refresh cookie is only sent to the auth endpoints that need it.
const refreshCookiePath = "/v1/auth"

type loginRequest struct {
	Identity string `json:"identity"` // phone number or email
	Password string `json:"password"`
}

type sessionResponse struct {
	AccessToken string       `json:"access_token"`
	TokenType   string       `json:"token_type"`
	ExpiresIn   int          `json:"expires_in"`
	User        userResponse `json:"user"`
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Identity == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "Phone number or email and password are required.")
		return
	}
	sess, err := s.auth.Login(r.Context(), req.Identity, req.Password, clientInfo(r))
	var locked *auth.LockedOutError
	switch {
	case err == nil:
		s.writeSession(w, sess)
	case errors.Is(err, auth.ErrInvalidCredentials):
		writeError(w, http.StatusUnauthorized, "invalid_credentials", "Incorrect phone number, email or password.")
	case errors.As(err, &locked):
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(locked.RetryAfter.Seconds()))))
		writeError(w, http.StatusTooManyRequests, "too_many_attempts", "Too many failed attempts. Try again later.")
	case errors.Is(err, auth.ErrPasswordResetRequired):
		writeError(w, http.StatusForbidden, "password_reset_required", "You need to reset your password before logging in.")
	case errors.Is(err, auth.ErrAccountInactive):
		writeError(w, http.StatusForbidden, "account_inactive", "This account has been deactivated.")
	default:
		writeInternal(w, r, err)
	}
}

func (s *Server) refresh(w http.ResponseWriter, r *http.Request) {
	sess, err := s.auth.Refresh(r.Context(), readRefreshCookie(r), clientInfo(r))
	switch {
	case err == nil:
		s.writeSession(w, sess)
	case errors.Is(err, auth.ErrInvalidToken):
		s.clearRefreshCookie(w)
		writeError(w, http.StatusUnauthorized, "invalid_refresh_token", "Your session has expired. Please log in again.")
	default:
		writeInternal(w, r, err)
	}
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if err := s.auth.Logout(r.Context(), readRefreshCookie(r)); err != nil {
		writeInternal(w, r, err)
		return
	}
	s.clearRefreshCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) writeSession(w http.ResponseWriter, sess auth.Session) {
	http.SetCookie(w, &http.Cookie{
		Name:     refreshCookie,
		Value:    sess.RefreshToken,
		Path:     refreshCookiePath,
		Expires:  sess.RefreshExpiresAt,
		HttpOnly: true,
		Secure:   s.cookieSecure,
		SameSite: http.SameSiteStrictMode,
	})
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, sessionResponse{
		AccessToken: sess.AccessToken,
		TokenType:   "Bearer",
		ExpiresIn:   int(time.Until(sess.AccessExpiresAt).Round(time.Second).Seconds()),
		User:        toUserResponse(sess.User),
	})
}

func (s *Server) clearRefreshCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     refreshCookie,
		Value:    "",
		Path:     refreshCookiePath,
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.cookieSecure,
		SameSite: http.SameSiteStrictMode,
	})
}

func readRefreshCookie(r *http.Request) string {
	c, err := r.Cookie(refreshCookie)
	if err != nil {
		return ""
	}
	return c.Value
}

// clientInfo relies on middleware.RealIP having set RemoteAddr from
// X-Forwarded-For / X-Real-IP. Only deploy behind a proxy that sets them.
func clientInfo(r *http.Request) auth.ClientInfo {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	var ip *netip.Addr
	if a, err := netip.ParseAddr(strings.TrimSpace(host)); err == nil {
		a = a.Unmap()
		ip = &a
	}
	return auth.ClientInfo{IP: ip, UserAgent: r.UserAgent()}
}

type ctxKey int

const claimsKey ctxKey = iota

func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		tok, ok := strings.CutPrefix(h, "Bearer ")
		if !ok || tok == "" {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "Missing access token.")
			return
		}
		claims, err := s.auth.Tokens().Parse(tok)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "invalid_token", "Access token is invalid or expired.")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), claimsKey, claims)))
	})
}

func claimsFrom(ctx context.Context) auth.Claims {
	c, _ := ctx.Value(claimsKey).(auth.Claims)
	return c
}
