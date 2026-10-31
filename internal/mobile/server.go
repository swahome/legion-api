package mobile

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/swahome/legion-api/internal/auth"
	"github.com/swahome/legion-api/internal/store"
)

const (
	// Builds at or below this version are told to update, as before.
	minVersionCode = 73
	// Above this version, responses drop the outer {"response": ...}.
	unwrapVersionCode = 77

	tokenTTL      = 100 * 24 * time.Hour // a new token, as legacy Chamasoft
	tokenSlideTTL = 30 * 24 * time.Hour  // each use keeps it alive at least this long
	maxBodyBytes  = 2 << 20
)

// Server answers the mobile app. Actions are registered by their legacy
// path, like "login" or "contributions/get_group_contributions".
type Server struct {
	pool    *pgxpool.Pool
	q       *store.Queries
	keys    *Keys
	auth    *auth.Service
	otp     OTPSender
	now     func() time.Time
	actions map[string]action
}

// action handles one decrypted request and returns the legacy "response".
type action struct {
	public bool // reachable without an access token (login, PIN sign-in)
	fn     func(ctx context.Context, c *call) (any, error)
}

// call is one decrypted request.
type call struct {
	req     request
	userID  int64 // from the access token; 0 on public actions
	r       *http.Request
	version int
}

func New(pool *pgxpool.Pool, keys *Keys, authSvc *auth.Service, otp OTPSender) *Server {
	if otp == nil {
		otp = noOTPSender{}
	}
	s := &Server{pool: pool, q: store.New(pool), keys: keys, auth: authSvc, otp: otp, now: time.Now}
	s.actions = map[string]action{
		"login":                 {public: true, fn: s.login},
		"generate_pin":          {public: true, fn: s.generatePin},
		"resend_pin":            {public: true, fn: s.resendPin},
		"verify_pin":            {public: true, fn: s.verifyPin},
		"get_user_checkin_data": {fn: s.getUserCheckinData},

		"get_group_data":                                {fn: s.groupAction(0, s.getGroupData)},
		"get_member_dashboard_data":                     {fn: s.groupAction(0, s.getMemberDashboardData)},
		"get_group_dashboard_data":                      {fn: s.groupAction(0, s.getGroupDashboardData)},
		"get_group_member_contribution_summary":         {fn: s.groupAction(0, s.getGroupMemberContributionSummary)},
		"countries/get_country_options":                 {fn: s.userAction(s.getCountryOptions)},
		"countries/get_currency_options":                {fn: s.userAction(s.getCurrencyOptions)},
		"banks/get_bank_options":                        {fn: s.groupAction(6, s.getBankOptions)},
		"bank_branches/get_bank_branches_options":       {fn: s.groupAction(6, s.getBankBranchOptions)},
		"group_roles/group_roles_list":                  {fn: s.groupAction(6, s.groupRolesList)},
		"members/get_members_and_details":               {fn: s.groupAction(6, s.getMembersAndDetails)},
		"members/get_group_member_options":              {fn: s.groupAction(6, s.getGroupMemberOptions)},
		"contributions/get_group_contributions":         {fn: s.groupAction(6, s.getGroupContributions)},
		"contributions/get_group_contribution":          {fn: s.groupAction(6, s.getGroupContribution)},
		"contributions/get_group_contribution_options":  {fn: s.groupAction(6, s.getGroupContributionOptions)},
		"contributions/get_member_contribution_options": {fn: s.groupAction(6, s.getMemberContributionOptions)},
		"statements/contribution_statement":             {fn: s.groupAction(0, s.contributionStatement)},
	}
	return s
}

// Routes serves mobile/<action> and mobile/<module>/<action>. Mount it
// where the app's base URL points, e.g. /v1/legacy.
func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()
	r.Post("/mobile/{action}", s.serve)
	r.Post("/mobile/{module}/{action}", s.serve)
	return r
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "action")
	if m := chi.URLParam(r, "module"); m != "" {
		name = m + "/" + name
	}
	version, _ := strconv.Atoi(firstHeader(r, "Versioncode", "VersionCode"))
	out := &responder{w: w, keys: s.keys, version: version}

	act, known := s.actions[name]
	c := &call{r: r, version: version}

	// The checks run in the legacy order, so the app sees the same
	// answers: token, Secret header, version, body.
	if !act.public || !known {
		token := bearer(r)
		if token == "" {
			out.send(http.StatusOK, status("401", "You are currently using an outdated application. Update to the latest version"))
			return
		}
		uid, err := s.q.UseMobileToken(r.Context(), store.UseMobileTokenParams{
			TokenHash: hashToken(token), MinExpiresAt: s.now().Add(tokenSlideTTL),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			out.send(http.StatusOK, status(400, "Authentication token expired"))
			return
		}
		if err != nil {
			out.internal(r, err)
			return
		}
		c.userID = uid
	}
	secret := r.Header.Get("Secret")
	if secret == "" {
		out.send(http.StatusOK, status("401", "You are using an outdated application. Update to the latest application"))
		return
	}
	if version <= minVersionCode {
		// Sent unencrypted, as before: an old build may not read the new scheme.
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": 0, "message": "You are using an outdated application. Update to the latest application"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil || len(body) == 0 {
		out.send(http.StatusOK, status(2, "Empty file sent from the client"))
		return
	}
	plain, err := s.keys.DecryptRequest(secret, body)
	if err == nil {
		err = json.Unmarshal(plain, &c.req)
	}
	if err != nil || c.req == nil {
		out.send(http.StatusOK, status(3, "You are currently using an outdated application. Kindly update to latest version. Your request could not be handled at the moment. if the error persist, contact admin"))
		return
	}
	if c.userID != 0 && c.req.Int("user_id") != c.userID {
		out.send(http.StatusOK, status(400, "user Authentication token is invalid."))
		return
	}
	if !known {
		out.send(http.StatusNotFound, map[string]any{"status": 0, "error": "404 Method Not Found for URI: mobile/" + name})
		return
	}
	resp, err := act.fn(r.Context(), c)
	if err != nil {
		out.internal(r, err)
		return
	}
	out.send(http.StatusOK, resp)
}

type responder struct {
	w       http.ResponseWriter
	keys    *Keys
	version int
}

// send encrypts resp, wrapped in {"response": ...} for builds up to 77.
func (o *responder) send(code int, resp any) {
	var v any = resp
	if o.version <= unwrapVersionCode {
		v = map[string]any{"response": resp}
	}
	b, err := o.keys.EncryptResponse(v)
	if err != nil {
		slog.Error("mobile: encrypt response", "err", err)
		http.Error(o.w, "", http.StatusInternalServerError)
		return
	}
	o.w.Header().Set("Content-Type", "application/json")
	o.w.WriteHeader(code)
	_, _ = o.w.Write(b)
}

func (o *responder) internal(r *http.Request, err error) {
	slog.ErrorContext(r.Context(), "mobile: internal error", "err", err, "path", r.URL.Path)
	o.send(http.StatusOK, status(0, "Something went wrong. Try again later."))
}

func status(code any, msg string) map[string]any {
	return map[string]any{"status": code, "message": msg}
}

func bearer(r *http.Request) string {
	v := strings.TrimSpace(r.Header.Get("Authorization"))
	for _, p := range []string{"Bearer", "Basic"} {
		v = strings.TrimSpace(strings.TrimPrefix(v, p))
	}
	return strings.Trim(v, `"`)
}

func firstHeader(r *http.Request, names ...string) string {
	for _, n := range names {
		if v := r.Header.Get(n); v != "" {
			return v
		}
	}
	return ""
}

func hashToken(t string) []byte {
	h := sha256.Sum256([]byte(t))
	return h[:]
}

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

// request is a decrypted request body. The app sends numbers both as JSON
// numbers and as strings, so the getters accept either.
type request map[string]any

func (q request) Str(key string) string {
	switch v := q[key].(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		if v {
			return "1"
		}
	}
	return ""
}

func (q request) Int(key string) int64 {
	n, _ := strconv.ParseInt(q.Str(key), 10, 64)
	return n
}
