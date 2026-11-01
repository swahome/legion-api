package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/swahome/legion-api/internal/account"
	"github.com/swahome/legion-api/internal/auth"
	"github.com/swahome/legion-api/internal/store"
)

// Milestone 5: self sign-up with a phone code, password reset by SMS and
// accepting an invite. Web codes are 6 digits.
const webCodeDigits = 6

type signupRequest struct {
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Phone     string `json:"phone"`
	Email     string `json:"email"`
	Password  string `json:"password"`
}

// signup creates an account and signs it in. The phone number is proven
// with the code texted to it (POST /v1/auth/verify-phone) before the user
// can create a group.
func (s *Server) signup(w http.ResponseWriter, r *http.Request) {
	var req signupRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	first := strings.Join(strings.Fields(req.FirstName), " ")
	last := strings.Join(strings.Fields(req.LastName), " ")
	if first == "" || strings.TrimSpace(req.Phone) == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "First name, phone number and password are required.")
		return
	}
	phone, err := auth.NormalizePhone(req.Phone, s.defaultCallingCode)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_phone", "Phone number is not valid.")
		return
	}
	var email *string
	if e := strings.ToLower(strings.TrimSpace(req.Email)); e != "" {
		if id, err := auth.ParseIdentity(e, s.defaultCallingCode); err != nil || id.Email == "" {
			writeError(w, http.StatusBadRequest, "invalid_email", "Email address is not valid.")
			return
		}
		email = &e
	}
	hash, err := account.HashPassword(req.Password)
	if err != nil {
		writeError(w, http.StatusBadRequest, "weak_password", "Use a password of at least 8 characters.")
		return
	}
	var user store.User
	err = pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		if _, err := q.GetUserByPhone(r.Context(), &phone); err == nil {
			return errPhoneTaken
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if email != nil {
			if _, err := q.GetUserByEmail(r.Context(), *email); err == nil {
				return errEmailTaken
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		user, err = q.CreateSignupUser(r.Context(), store.CreateSignupUserParams{
			FirstName: first, LastName: last, Phone: &phone, Email: email, PasswordHash: &hash,
		})
		if err != nil {
			return err
		}
		return s.acct.SendCode(r.Context(), q, user, account.VerifyPhone, webCodeDigits)
	})
	switch {
	case errors.Is(err, errPhoneTaken), isUniqueViolation(err, "users_phone_key"):
		writeError(w, http.StatusConflict, "phone_taken", "An account with this phone number already exists. Log in, or reset your password.")
		return
	case errors.Is(err, errEmailTaken), isUniqueViolation(err, "users_email_key"):
		writeError(w, http.StatusConflict, "email_taken", "An account with this email address already exists.")
		return
	case err != nil:
		writeInternal(w, r, err)
		return
	}
	s.smsKick()
	sess, err := s.auth.StartSession(r.Context(), user, clientInfo(r))
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	s.writeSessionStatus(w, sess, http.StatusCreated)
}

var errPhoneTaken = errors.New("phone belongs to another user")

type codeRequest struct {
	Code string `json:"code"`
}

// verifyPhone checks the code texted at sign-up.
func (s *Server) verifyPhone(w http.ResponseWriter, r *http.Request) {
	var req codeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	uid := claimsFrom(r.Context()).UserID
	err := s.codeTx(r.Context(), func(q *store.Queries) error {
		if err := s.acct.CheckCode(r.Context(), q, uid, account.VerifyPhone, req.Code); err != nil {
			return err
		}
		return q.SetUserValidated(r.Context(), uid)
	})
	if writeCodeError(w, r, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"phone_verified": true})
}

// resendPhoneCode texts a new sign-up code.
func (s *Server) resendPhoneCode(w http.ResponseWriter, r *http.Request) {
	uid := claimsFrom(r.Context()).UserID
	err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		u, err := q.GetUserByID(r.Context(), uid)
		if err != nil {
			return err
		}
		if u.IsValidated {
			return errAlreadyVerified
		}
		return s.acct.SendCode(r.Context(), q, u, account.VerifyPhone, webCodeDigits)
	})
	if errors.Is(err, errAlreadyVerified) {
		writeError(w, http.StatusConflict, "already_verified", "Your phone number is already verified.")
		return
	}
	if writeCodeError(w, r, err) {
		return
	}
	s.smsKick()
	w.WriteHeader(http.StatusAccepted)
}

var errAlreadyVerified = errors.New("already verified")

type forgotRequest struct {
	Identity string `json:"identity"` // phone number or email
}

// forgotPassword texts a reset code to the account's phone. It answers the
// same whether or not the account exists, so it can't be used to find out
// who has one.
func (s *Server) forgotPassword(w http.ResponseWriter, r *http.Request) {
	var req forgotRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	id, err := auth.ParseIdentity(req.Identity, s.defaultCallingCode)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_identity", "Enter a valid phone number or email address.")
		return
	}
	err = pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		u, err := lookupUser(r.Context(), q, id)
		if err != nil {
			return err
		}
		if !u.IsActive {
			return pgx.ErrNoRows
		}
		return s.acct.SendCode(r.Context(), q, u, account.ResetPassword, webCodeDigits)
	})
	switch {
	case errors.Is(err, account.ErrTooManyCodes):
		writeError(w, http.StatusTooManyRequests, "too_many_codes", "Too many codes were sent. Wait an hour and try again.")
		return
	case errors.Is(err, pgx.ErrNoRows), errors.Is(err, account.ErrNoPhone):
		slog.InfoContext(r.Context(), "forgot password: no account to text", "identity", id.Key())
	case err != nil:
		writeInternal(w, r, err)
		return
	default:
		s.smsKick()
	}
	writeJSON(w, http.StatusAccepted, map[string]string{
		"message": "If an account uses that phone number or email, we have texted a code to its phone.",
	})
}

type resetRequest struct {
	Identity string `json:"identity"`
	Code     string `json:"code"`
	Password string `json:"password"`
}

// resetPassword sets a new password with the texted code, signs out every
// other session and signs the user in.
func (s *Server) resetPassword(w http.ResponseWriter, r *http.Request) {
	var req resetRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.Password) < account.MinPasswordLen {
		writeError(w, http.StatusBadRequest, "weak_password", "Use a password of at least 8 characters.")
		return
	}
	id, err := auth.ParseIdentity(req.Identity, s.defaultCallingCode)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_code", "That code is not right.")
		return
	}
	var user store.User
	err = s.codeTx(r.Context(), func(q *store.Queries) error {
		u, err := lookupUser(r.Context(), q, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return account.ErrCodeInvalid
		}
		if err != nil {
			return err
		}
		if err := s.acct.CheckCode(r.Context(), q, u.ID, account.ResetPassword, req.Code); err != nil {
			return err
		}
		if err := s.acct.SetPassword(r.Context(), q, u, req.Password, true); err != nil {
			return err
		}
		// The code reached their phone, so the phone is theirs.
		if err := q.SetUserValidated(r.Context(), u.ID); err != nil {
			return err
		}
		user, err = q.GetUserByID(r.Context(), u.ID)
		return err
	})
	if writeCodeError(w, r, err) {
		return
	}
	s.smsKick()
	if !user.IsActive {
		writeError(w, http.StatusForbidden, "account_inactive", "This account has been deactivated.")
		return
	}
	sess, err := s.auth.StartSession(r.Context(), user, clientInfo(r))
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	s.writeSession(w, sess)
}

// codeTx runs fn in a transaction that also commits when fn fails on a
// wrong code, so the failed try counts against the code.
func (s *Server) codeTx(ctx context.Context, fn func(q *store.Queries) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	ferr := fn(s.q.WithTx(tx))
	if ferr != nil && !errors.Is(ferr, account.ErrCodeInvalid) {
		return ferr
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return ferr
}

func lookupUser(ctx context.Context, q *store.Queries, id auth.Identity) (store.User, error) {
	if id.Phone != "" {
		return q.GetUserByPhone(ctx, &id.Phone)
	}
	return q.GetUserByEmail(ctx, id.Email)
}

func writeCodeError(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, account.ErrCodeInvalid):
		writeError(w, http.StatusBadRequest, "invalid_code", "That code is not right.")
	case errors.Is(err, account.ErrCodeExpired):
		writeError(w, http.StatusBadRequest, "code_expired", "That code has expired. Ask for a new one.")
	case errors.Is(err, account.ErrTooManyCodes):
		writeError(w, http.StatusTooManyRequests, "too_many_codes", "Too many codes were sent. Wait an hour and try again.")
	case errors.Is(err, account.ErrNoPhone):
		writeError(w, http.StatusBadRequest, "no_phone", "This account has no phone number to text.")
	case errors.Is(err, account.ErrWeakPassword):
		writeError(w, http.StatusBadRequest, "weak_password", "Use a password of at least 8 characters.")
	default:
		writeInternal(w, r, err)
	}
	return true
}

// ---- Invites ----

type inviteResponse struct {
	FirstName     string `json:"first_name"`
	GroupName     string `json:"group_name"`
	InvitedBy     string `json:"invited_by"`
	NeedsPassword bool   `json:"needs_password"`
}

func (s *Server) loadInvite(w http.ResponseWriter, r *http.Request, q *store.Queries) (store.GetInviteRow, bool) {
	inv, err := q.GetInvite(r.Context(), account.TokenHash(chi.URLParam(r, "token")))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "invite_not_found", "This invite link is not valid.")
		return inv, false
	case err != nil:
		writeInternal(w, r, err)
		return inv, false
	case inv.UsedAt != nil || inv.ExpiresAt.Before(timeNow()) || !inv.UserActive || inv.MembershipStatus != store.MembershipStatusActive:
		writeError(w, http.StatusGone, "invite_expired", "This invite link has expired. Ask your group official to send a new one, or reset your password.")
		return inv, false
	}
	return inv, true
}

// getInvite says who an invite link is for, for the join page.
func (s *Server) getInvite(w http.ResponseWriter, r *http.Request) {
	inv, ok := s.loadInvite(w, r, s.q)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, inviteResponse{FirstName: inv.FirstName, GroupName: inv.GroupName,
		InvitedBy: inv.InvitedByName, NeedsPassword: !inv.HasPassword})
}

type acceptInviteRequest struct {
	Password string `json:"password"`
}

// acceptInvite sets the invited member's password and signs them in.
func (s *Server) acceptInvite(w http.ResponseWriter, r *http.Request) {
	var req acceptInviteRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.Password) < account.MinPasswordLen {
		writeError(w, http.StatusBadRequest, "weak_password", "Use a password of at least 8 characters.")
		return
	}
	var user store.User
	answered := false
	err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		inv, ok := s.loadInvite(w, r, q)
		if !ok {
			answered = true
			return errAnswered
		}
		if inv.HasPassword {
			answered = true
			writeError(w, http.StatusConflict, "password_already_set", "You already have a password. Log in, or reset your password.")
			return errAnswered
		}
		if n, err := q.UseInvite(r.Context(), inv.TokenHash); err != nil || n == 0 {
			if err == nil {
				answered = true
				writeError(w, http.StatusGone, "invite_expired", "This invite link has expired.")
				return errAnswered
			}
			return err
		}
		u, err := q.GetUserByID(r.Context(), inv.UserID)
		if err != nil {
			return err
		}
		if err := s.acct.SetPassword(r.Context(), q, u, req.Password, false); err != nil {
			return err
		}
		if err := q.SetUserValidated(r.Context(), u.ID); err != nil {
			return err
		}
		user, err = q.GetUserByID(r.Context(), u.ID)
		return err
	})
	if answered {
		return
	}
	if writeCodeError(w, r, err) {
		return
	}
	sess, err := s.auth.StartSession(r.Context(), user, clientInfo(r))
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	s.writeSession(w, sess)
}

var errAnswered = errors.New("response already written")
