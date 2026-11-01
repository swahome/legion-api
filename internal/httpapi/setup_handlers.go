package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/swahome/legion-api/internal/account"
	"github.com/swahome/legion-api/internal/auth"
	"github.com/swahome/legion-api/internal/store"
)

// The group setup wizard. Sign-up creates a group in 'setup'; its official
// then adds members, contributions with their fines, accounts and loan
// types through the usual endpoints, reviews it all (GET .../setup), and
// confirms (POST .../setup/complete). Only then does the group go live:
// billing starts and the members are texted their invites.

type groupDetailsRequest struct {
	Name        *string `json:"name"`
	CountryCode *string `json:"country_code"`
	Currency    *string `json:"currency"`
	Email       *string `json:"email"`
	Phone       *string `json:"phone"`
	Address     *string `json:"address"`
	Size        *int    `json:"size"`
	Type        *string `json:"type"` // a key of GET /v1/group-types
}

type groupDetails struct {
	ID          int64   `json:"id"`
	Name        string  `json:"name"`
	Slug        string  `json:"slug"`
	Status      string  `json:"status"`
	CountryCode string  `json:"country_code"`
	Currency    string  `json:"currency"`
	Email       *string `json:"email"`
	Phone       *string `json:"phone"`
	Address     *string `json:"address"`
	Size        *int    `json:"size"`
	Type        *string `json:"type"`
	TypeName    *string `json:"type_name"`
}

func toGroupDetails(g store.Group) groupDetails {
	d := groupDetails{ID: g.ID, Name: g.Name, Slug: g.Slug, Status: string(g.Status), CountryCode: g.CountryCode,
		Currency: g.Currency, Email: g.Email, Phone: g.Phone, Address: g.Address}
	var settings map[string]any
	_ = json.Unmarshal(g.Settings, &settings)
	if v, ok := settings["size"].(string); ok {
		if n, err := strconv.Atoi(v); err == nil {
			d.Size = &n
		}
	}
	if v, ok := settings["group_type"].(string); ok && v != "" {
		d.Type = &v
		if n, ok := account.GroupTypes[v]; ok {
			d.TypeName = &n
		}
	}
	return d
}

type createGroupResponse struct {
	Group        groupDetails `json:"group"`
	MembershipID int64        `json:"membership_id"`
}

// createGroup starts a new group in setup with the caller as its official.
func (s *Server) createGroup(w http.ResponseWriter, r *http.Request) {
	var req groupDetailsRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	in, ok := s.groupInput(w, req)
	if !ok {
		return
	}
	uid := claimsFrom(r.Context()).UserID
	var g store.Group
	var mid int64
	err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		u, err := q.GetUserByID(r.Context(), uid)
		if err != nil {
			return err
		}
		if !u.IsValidated {
			return errPhoneNotVerified
		}
		g, mid, err = account.CreateGroup(r.Context(), q, u, in)
		return err
	})
	if writeGroupInputError(w, r, err) {
		return
	}
	writeJSON(w, http.StatusCreated, createGroupResponse{Group: toGroupDetails(g), MembershipID: mid})
}

var errPhoneNotVerified = errors.New("phone not verified")

func (s *Server) groupInput(w http.ResponseWriter, req groupDetailsRequest) (account.GroupInput, bool) {
	in := account.GroupInput{Name: deref(req.Name), CountryCode: deref(req.CountryCode), Currency: deref(req.Currency),
		Email: deref(req.Email), Type: deref(req.Type)}
	if req.Size != nil {
		in.Size = *req.Size
	}
	if p := strings.TrimSpace(deref(req.Phone)); p != "" {
		phone, err := auth.NormalizePhone(p, s.defaultCallingCode)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_phone", "The group phone number is not valid.")
			return in, false
		}
		in.Phone = phone
	}
	if e := strings.TrimSpace(in.Email); e != "" && !strings.Contains(e, "@") {
		writeError(w, http.StatusBadRequest, "invalid_email", "The group email address is not valid.")
		return in, false
	}
	return in, true
}

func writeGroupInputError(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, errPhoneNotVerified):
		writeError(w, http.StatusForbidden, "phone_not_verified", "Verify your phone number with the texted code before creating a group.")
	case errors.Is(err, account.ErrGroupName):
		writeError(w, http.StatusBadRequest, "missing_fields", "Give the group a name of up to 120 characters.")
	case errors.Is(err, account.ErrUnknownCountry):
		writeError(w, http.StatusBadRequest, "invalid_country", "Unknown country.")
	case errors.Is(err, account.ErrBadCurrency):
		writeError(w, http.StatusBadRequest, "invalid_currency", "Currency must be a 3-letter code such as KES.")
	case errors.Is(err, account.ErrGroupType):
		writeError(w, http.StatusBadRequest, "invalid_type", "Unknown group type.")
	case errors.Is(err, errCurrencyLocked):
		writeError(w, http.StatusConflict, "currency_locked", "The country and currency can only be changed during setup.")
	default:
		writeInternal(w, r, err)
	}
	return true
}

var errCurrencyLocked = errors.New("currency locked")

// listGroupTypes is GET /v1/group-types, the kinds of group sign-up offers.
func (s *Server) listGroupTypes(w http.ResponseWriter, r *http.Request) {
	out := make([]namedKey, 0, len(account.GroupTypes))
	for i := 1; i <= len(account.GroupTypes); i++ {
		k := strconv.Itoa(i)
		out = append(out, namedKey{Key: k, Name: account.GroupTypes[k]})
	}
	writeJSON(w, http.StatusOK, out)
}

type namedKey struct {
	Key  string `json:"key"`
	Name string `json:"name"`
}

// updateGroup edits the group's details (officials). Country and currency
// are fixed once the group is live, since amounts already recorded are in
// the old currency.
func (s *Server) updateGroup(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	var req groupDetailsRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	cur := toGroupDetails(a.Group)
	in := account.GroupInput{Name: a.Group.Name, CountryCode: a.Group.CountryCode, Currency: a.Group.Currency, CountryKnown: true}
	if req.Name != nil {
		in.Name = *req.Name
	}
	if req.CountryCode != nil {
		if !strings.EqualFold(strings.TrimSpace(*req.CountryCode), a.Group.CountryCode) {
			in.CountryCode, in.CountryKnown = *req.CountryCode, false
			if req.Currency == nil {
				in.Currency = "" // follow the new country
			}
		}
	}
	if req.Currency != nil {
		in.Currency = *req.Currency
	}
	if req.Type != nil {
		in.Type = *req.Type
	}
	var phone *string
	if req.Phone != nil {
		if p := strings.TrimSpace(*req.Phone); p != "" {
			n, err := auth.NormalizePhone(p, s.defaultCallingCode)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid_phone", "The group phone number is not valid.")
				return
			}
			phone = &n
		}
	}
	var email *string
	if req.Email != nil {
		if e := strings.ToLower(strings.TrimSpace(*req.Email)); e != "" {
			if !strings.Contains(e, "@") {
				writeError(w, http.StatusBadRequest, "invalid_email", "The group email address is not valid.")
				return
			}
			email = &e
		}
	}
	var address *string
	if req.Address != nil {
		if v := strings.TrimSpace(*req.Address); v != "" {
			address = &v
		}
	}
	var g store.Group
	err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		if err := account.CheckGroupInput(r.Context(), q, &in); err != nil {
			return err
		}
		if a.Group.Status != store.GroupStatusSetup && (in.CountryCode != cur.CountryCode || in.Currency != cur.Currency) {
			return errCurrencyLocked
		}
		settings := map[string]any{}
		if req.Size != nil {
			settings["size"] = strconv.Itoa(max(*req.Size, 0))
		}
		if req.Type != nil {
			settings["group_type"] = in.Type
		}
		raw, _ := json.Marshal(settings)
		var err error
		g, err = q.UpdateGroupProfile(r.Context(), store.UpdateGroupProfileParams{
			ID: a.Group.ID, Name: &in.Name, CountryCode: &in.CountryCode, Currency: &in.Currency,
			SetEmail: req.Email != nil, Email: email, SetPhone: req.Phone != nil, Phone: phone,
			SetAddress: req.Address != nil, Address: address, Settings: raw,
		})
		return err
	})
	if writeGroupInputError(w, r, err) {
		return
	}
	writeJSON(w, http.StatusOK, toGroupDetails(g))
}

type setupStep struct {
	Key   string `json:"key"`
	Done  bool   `json:"done"`
	Count int64  `json:"count"`
}

type setupResponse struct {
	Group           groupDetails `json:"group"`
	Steps           []setupStep  `json:"steps"`
	MembersToInvite int          `json:"members_to_invite"`
	CompletedAt     *time.Time   `json:"completed_at"`
}

// getSetup is the wizard's progress and review: what each step holds.
func (s *Server) getSetup(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	c, err := s.q.SetupCounts(r.Context(), a.Group.ID)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	pending, err := s.q.MembersToInvite(r.Context(), store.MembersToInviteParams{GroupID: a.Group.ID, ExceptUser: claimsFrom(r.Context()).UserID})
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, setupResponse{
		Group: toGroupDetails(a.Group),
		Steps: []setupStep{
			{Key: "details", Done: true, Count: 1},
			{Key: "members", Done: c.Members > 1, Count: c.Members},
			{Key: "contributions", Done: c.Contributions > 0, Count: c.Contributions},
			{Key: "fines", Done: c.FineRules > 0, Count: c.FineRules},
			{Key: "accounts", Done: c.Accounts > 0, Count: c.Accounts},
			{Key: "loan_types", Done: c.LoanTypes > 0, Count: c.LoanTypes},
		},
		MembersToInvite: len(pending),
		CompletedAt:     a.Group.SetupCompletedAt,
	})
}

// completeSetup is the review step's confirm: the group goes live and the
// members added during setup are texted their invites.
func (s *Server) completeSetup(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	uid := claimsFrom(r.Context()).UserID
	var sent int
	err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		by, err := q.GetUserByID(r.Context(), uid)
		if err != nil {
			return err
		}
		sent, err = s.acct.CompleteSetup(r.Context(), q, a.Group.ID, by)
		return err
	})
	if errors.Is(err, account.ErrGroupNotInSetup) {
		writeError(w, http.StatusConflict, "setup_complete", "This group's setup is already complete.")
		return
	}
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	s.smsKick()
	g, err := s.q.GetGroup(r.Context(), a.Group.ID)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"group": toGroupDetails(g), "invites_sent": sent})
}

// inviteMember texts a member their invite again, with a fresh link.
func (s *Server) inviteMember(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	mid, ok := pathID(r, "memberID")
	if !ok {
		writeError(w, http.StatusNotFound, "member_not_found", "Member not found.")
		return
	}
	if a.Group.Status == store.GroupStatusSetup {
		writeError(w, http.StatusConflict, "group_in_setup", "Invites go out when setup is finished.")
		return
	}
	var sent bool
	err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		m, err := q.GetMember(r.Context(), store.GetMemberParams{GroupID: a.Group.ID, ID: mid})
		if err != nil {
			return err
		}
		if m.Status != store.MembershipStatusActive {
			return errMemberInactive
		}
		by, err := q.GetUserByID(r.Context(), claimsFrom(r.Context()).UserID)
		if err != nil {
			return err
		}
		sent, err = s.acct.Invite(r.Context(), q, a.Group.ID, mid, by)
		return err
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "member_not_found", "Member not found.")
		return
	case errors.Is(err, errMemberInactive):
		writeError(w, http.StatusConflict, "member_inactive", "Only active members can be invited.")
		return
	case err != nil:
		writeInternal(w, r, err)
		return
	}
	if !sent {
		writeError(w, http.StatusBadRequest, "no_phone", "This member has no phone number to text.")
		return
	}
	s.smsKick()
	w.WriteHeader(http.StatusAccepted)
}

var errMemberInactive = errors.New("member not active")

type smsResponse struct {
	ID             int64      `json:"id"`
	To             string     `json:"to"`
	Body           string     `json:"body"`
	Kind           string     `json:"kind"`
	UserID         *int64     `json:"user_id"`
	Group          *namedRef  `json:"group"`
	Status         string     `json:"status"`
	Attempts       int32      `json:"attempts"`
	ProviderStatus *string    `json:"provider_status"`
	Cost           *string    `json:"cost"`
	Error          *string    `json:"error"`
	CreatedAt      time.Time  `json:"created_at"`
	SentAt         *time.Time `json:"sent_at"`
}

// adminListSMS is the outbox for platform admins: ?status=, ?group_id=,
// ?phone=, ?page=.
func (s *Server) adminListSMS(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p := store.ListSMSParams{Lim: 50}
	if v := q.Get("status"); v != "" {
		st := store.SmsStatus(v)
		if !st.Valid() {
			writeError(w, http.StatusBadRequest, "invalid_status", "status must be queued, sending, sent or failed.")
			return
		}
		p.Status = store.NullSmsStatus{SmsStatus: st, Valid: true}
	}
	if v := q.Get("group_id"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_group", "group_id must be a number.")
			return
		}
		p.GroupID = &id
	}
	if v := q.Get("phone"); v != "" {
		n, err := auth.NormalizePhone(v, s.defaultCallingCode)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_phone", "Phone number is not valid.")
			return
		}
		p.Phone = &n
	}
	if page, err := strconv.Atoi(q.Get("page")); err == nil && page > 1 {
		p.Off = int32(page-1) * p.Lim
	}
	rows, err := s.q.ListSMS(r.Context(), p)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	out := make([]smsResponse, 0, len(rows))
	for _, m := range rows {
		sr := smsResponse{ID: m.ID, To: m.ToPhone, Body: m.Body, Kind: m.Kind, UserID: m.UserID, Status: string(m.Status),
			Attempts: m.Attempts, ProviderStatus: m.ProviderStatus, Cost: m.Cost, Error: m.Error, CreatedAt: m.CreatedAt, SentAt: m.SentAt}
		if m.GroupID != nil {
			sr.Group = &namedRef{ID: *m.GroupID, Name: m.GroupName}
		}
		out = append(out, sr)
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": out})
}
