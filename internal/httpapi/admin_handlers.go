package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/crypto/bcrypt"

	"github.com/swahome/legion-api/internal/auth"
	"github.com/swahome/legion-api/internal/store"
)

// requirePlatformAdmin limits a route to active platform admins. It checks
// the database rather than the token's adm claim, so removing the role or
// deactivating the account takes effect at once.
func (s *Server) requirePlatformAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok, err := s.q.IsPlatformAdmin(r.Context(), claimsFrom(r.Context()).UserID)
		if err != nil {
			writeInternal(w, r, err)
			return
		}
		if !ok {
			writeError(w, http.StatusForbidden, "forbidden", "Only platform admins can do this.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// audit records an admin change. A failure is logged, not returned: the
// change itself has already been made.
func (s *Server) audit(r *http.Request, action, targetType string, targetID int64, detail any) {
	b, _ := json.Marshal(detail)
	if b == nil || string(b) == "null" {
		b = []byte("{}")
	}
	uid := claimsFrom(r.Context()).UserID
	if err := s.q.InsertAudit(r.Context(), store.InsertAuditParams{
		ActorID: &uid, Action: action, TargetType: targetType, TargetID: &targetID, Detail: b,
	}); err != nil {
		slog.ErrorContext(r.Context(), "audit log write failed", "err", err, "action", action)
	}
}

type page struct{ size, offset int32 }

// pageParams reads ?page (from 1) and ?page_size (1-200, default 50).
func pageParams(r *http.Request) page {
	size, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
	if size < 1 || size > 200 {
		size = 50
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if n < 1 {
		n = 1
	}
	return page{size: int32(size), offset: int32((n - 1) * size)}
}

type paged[T any] struct {
	Items    []T   `json:"items"`
	Total    int64 `json:"total"`
	Page     int32 `json:"page"`
	PageSize int32 `json:"page_size"`
}

func newPaged[T any](items []T, total int64, p page) paged[T] {
	return paged[T]{Items: items, Total: total, Page: p.offset/p.size + 1, PageSize: p.size}
}

func queryPtr(r *http.Request, key string) *string {
	v := strings.TrimSpace(r.URL.Query().Get(key))
	if v == "" {
		return nil
	}
	return &v
}

// ---- dashboard ----

func (s *Server) adminStats(w http.ResponseWriter, r *http.Request) {
	st, err := s.q.AdminStats(r.Context())
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"groups": st.Groups, "active_groups": st.ActiveGroups, "setup_groups": st.SetupGroups,
		"suspended_groups": st.SuspendedGroups, "closed_groups": st.ClosedGroups, "groups_today": st.GroupsToday,
		"users": st.Users, "users_today": st.UsersToday, "users_active_30d": st.UsersActive30d,
		"memberships": st.Memberships,
		"sms_total":   st.SmsTotal, "sms_sent_today": st.SmsSentToday, "sms_queued": st.SmsQueued, "sms_failed_7d": st.SmsFailed7d,
		"transaction_alerts": st.TransactionAlerts, "alerts_unassigned": st.AlertsUnassigned,
		"accounts_connected": st.AccountsConnected,
		"payments_paid":      st.PaymentsPaid, "payments_paid_amount": st.PaymentsPaidAmount,
		"invoices_today": st.InvoicesToday, "fine_invoices_today": st.FineInvoicesToday,
		"open_withdrawal_requests": st.OpenWithdrawalRequests, "audit_entries": st.AuditEntries,
	})
}

func (s *Server) adminAudit(w http.ResponseWriter, r *http.Request) {
	rows, err := s.q.ListAudit(r.Context(), 100)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	type entry struct {
		ID         int64           `json:"id"`
		ActorID    *int64          `json:"actor_id"`
		ActorName  string          `json:"actor_name"`
		Action     string          `json:"action"`
		TargetType string          `json:"target_type"`
		TargetID   *int64          `json:"target_id"`
		Detail     json.RawMessage `json:"detail"`
		CreatedAt  time.Time       `json:"created_at"`
	}
	out := make([]entry, 0, len(rows))
	for _, a := range rows {
		name := strings.TrimSpace(deref(a.ActorFirstName) + " " + deref(a.ActorLastName))
		out = append(out, entry{a.ID, a.ActorID, name, a.Action, a.TargetType, a.TargetID, a.Detail, a.CreatedAt})
	}
	writeJSON(w, http.StatusOK, out)
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// ---- groups ----

type adminGroup struct {
	ID                int64      `json:"id"`
	LegacyID          *int32     `json:"legacy_id"`
	Name              string     `json:"name"`
	Slug              string     `json:"slug"`
	Status            string     `json:"status"`
	Currency          string     `json:"currency"`
	CountryCode       string     `json:"country_code"`
	Email             *string    `json:"email"`
	Phone             *string    `json:"phone"`
	ActiveMembers     int64      `json:"active_members"`
	RegisteredMembers int64      `json:"registered_members"`
	LastSeenAt        *time.Time `json:"last_seen_at"`
	Contact           *contact   `json:"contact"`
	CreatedAt         time.Time  `json:"created_at"`
}

// contact is the person to call about a group: its owner, else whoever
// created it.
type contact struct {
	UserID int64   `json:"user_id"`
	Name   string  `json:"name"`
	Phone  *string `json:"phone"`
	Email  *string `json:"email"`
}

func newContact(id int64, name string, phone, email *string) *contact {
	if id == 0 {
		return nil
	}
	return &contact{id, strings.TrimSpace(name), phone, email}
}

// adminGroupsParams reads the filters shared by the group list and its export.
func adminGroupsParams(w http.ResponseWriter, r *http.Request) (store.AdminListGroupsParams, bool) {
	params := store.AdminListGroupsParams{Search: queryPtr(r, "q"), View: r.URL.Query().Get("view"), Sort: r.URL.Query().Get("sort")}
	if v := queryPtr(r, "status"); v != nil {
		st := store.GroupStatus(*v)
		if !st.Valid() {
			writeError(w, http.StatusBadRequest, "invalid_status", "Unknown group status.")
			return params, false
		}
		params.Status = store.NullGroupStatus{GroupStatus: st, Valid: true}
	}
	if params.View != "" && params.View != "inactive" {
		writeError(w, http.StatusBadRequest, "invalid_view", "View must be inactive or left out.")
		return params, false
	}
	return params, true
}

func toAdminGroup(g store.AdminListGroupsRow) adminGroup {
	return adminGroup{ID: g.ID, LegacyID: g.LegacyID, Name: g.Name, Slug: g.Slug, Status: string(g.Status),
		Currency: g.Currency, CountryCode: g.CountryCode, Email: g.GroupEmail, Phone: g.GroupPhone,
		ActiveMembers: g.ActiveMembers, RegisteredMembers: g.RegisteredMembers, LastSeenAt: g.LastSeenAt,
		Contact: newContact(g.ContactID, g.ContactName, g.ContactPhone, g.ContactEmail), CreatedAt: g.CreatedAt}
}

// adminListGroups lists groups. ?status, ?q (name, id or phone),
// ?view=inactive (no sign-in for 90 days), ?sort=newest.
func (s *Server) adminListGroups(w http.ResponseWriter, r *http.Request) {
	params, ok := adminGroupsParams(w, r)
	if !ok {
		return
	}
	p := pageParams(r)
	params.PageSize, params.PageOffset = p.size, p.offset
	rows, err := s.q.AdminListGroups(r.Context(), params)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	items := make([]adminGroup, 0, len(rows))
	var total int64
	for _, g := range rows {
		total = g.Total
		items = append(items, toAdminGroup(g))
	}
	writeJSON(w, http.StatusOK, newPaged(items, total, p))
}

// adminGetGroup is the admin's summary of one group. Its members,
// contributions and accounts come from the group's own endpoints, which
// platform admins can read.
func (s *Server) adminGetGroup(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "groupID")
	if !ok {
		writeError(w, http.StatusNotFound, "group_not_found", "Group not found.")
		return
	}
	g, err := s.q.AdminGetGroup(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "group_not_found", "Group not found.")
		return
	}
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	var settings map[string]any
	_ = json.Unmarshal(g.Settings, &settings)
	off := false
	if v, ok := settings["disable_notifications"]; ok {
		off = v == "1" || v == true || v == "true"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": g.ID, "legacy_id": g.LegacyID, "name": g.Name, "slug": g.Slug, "status": g.Status,
		"currency": g.Currency, "country_code": g.CountryCode, "timezone": g.Timezone,
		"email": g.Email, "phone": g.Phone, "address": g.Address,
		"created_at": g.CreatedAt, "updated_at": g.UpdatedAt, "created_by_name": g.CreatedByName,
		"contact":        newContact(g.ContactID, g.ContactName, g.ContactPhone, g.ContactEmail),
		"active_members": g.ActiveMembers, "registered_members": g.RegisteredMembers, "last_seen_at": g.LastSeenAt,
		"contributions": g.Contributions, "accounts": g.Accounts, "loans": g.Loans,
		"notifications_off": off,
	})
}

// adminUpdateGroup changes a group's status, name, contact details or
// its notifications switch. Fields left out stay as they are.
func (s *Server) adminUpdateGroup(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "groupID")
	if !ok {
		writeError(w, http.StatusNotFound, "group_not_found", "Group not found.")
		return
	}
	var req struct {
		Status           *string          `json:"status"`
		Name             *string          `json:"name"`
		Email            Optional[string] `json:"email"`
		Phone            Optional[string] `json:"phone"`
		NotificationsOff *bool            `json:"notifications_off"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Status != nil && !store.GroupStatus(*req.Status).Valid() {
		writeError(w, http.StatusBadRequest, "invalid_status", "Status must be setup, active, suspended or closed.")
		return
	}
	if req.Name != nil {
		n := strings.TrimSpace(*req.Name)
		if n == "" {
			writeError(w, http.StatusBadRequest, "invalid_name", "The group needs a name.")
			return
		}
		req.Name = &n
	}
	email, phone := trimOpt(req.Email.Value), trimOpt(req.Phone.Value)
	if phone != nil {
		p, err := auth.NormalizePhone(*phone, s.defaultCallingCode)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_phone", "That phone number doesn't look right.")
			return
		}
		phone = &p
	}
	err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		if req.Status != nil {
			if _, err := q.SetGroupStatus(r.Context(), store.SetGroupStatusParams{ID: id, Status: store.GroupStatus(*req.Status)}); err != nil {
				return err
			}
		}
		if req.Name != nil || req.Email.Set || req.Phone.Set || req.NotificationsOff != nil {
			_, err := q.AdminUpdateGroupProfile(r.Context(), store.AdminUpdateGroupProfileParams{
				ID: id, Name: req.Name, SetEmail: req.Email.Set, Email: email, SetPhone: req.Phone.Set, Phone: phone,
				NotificationsOff: req.NotificationsOff,
			})
			return err
		}
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "group_not_found", "Group not found.")
		return
	}
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	if req.Status != nil {
		s.audit(r, "group.status", "group", id, map[string]string{"status": *req.Status})
	}
	if req.Name != nil || req.Email.Set || req.Phone.Set || req.NotificationsOff != nil {
		s.audit(r, "group.update", "group", id, map[string]any{"name": req.Name, "email": email, "phone": phone, "notifications_off": req.NotificationsOff})
	}
	s.adminGetGroup(w, r)
}

func trimOpt(p *string) *string {
	if p == nil {
		return nil
	}
	v := strings.TrimSpace(*p)
	if v == "" {
		return nil
	}
	return &v
}

// ---- users ----

type adminUser struct {
	ID            int64          `json:"id"`
	FirstName     string         `json:"first_name"`
	MiddleName    string         `json:"middle_name"`
	LastName      string         `json:"last_name"`
	Phone         *string        `json:"phone"`
	Email         *string        `json:"email"`
	IDNumber      *string        `json:"id_number,omitempty"`
	IsActive      bool           `json:"is_active"`
	LastLoginAt   *time.Time     `json:"last_login_at"`
	CreatedAt     time.Time      `json:"created_at"`
	PlatformRoles []string       `json:"platform_roles"`
	Groups        *int64         `json:"groups,omitempty"`
	GroupList     []userGroupRef `json:"group_list,omitempty"`
	Memberships   []any          `json:"memberships,omitempty"`
}

type userGroupRef struct {
	GroupID    int64  `json:"group_id"`
	GroupName  string `json:"group_name"`
	IsOfficial bool   `json:"is_official"`
}

// adminUsersParams reads the filters shared by the user list and its export.
func adminUsersParams(w http.ResponseWriter, r *http.Request) (store.AdminListUsersParams, bool) {
	params := store.AdminListUsersParams{Search: queryPtr(r, "q")}
	if v := queryPtr(r, "role"); v != nil {
		role := store.PlatformRole(*v)
		if !role.Valid() {
			writeError(w, http.StatusBadRequest, "invalid_role", "Unknown platform role.")
			return params, false
		}
		params.Role = store.NullPlatformRole{PlatformRole: role, Valid: true}
	}
	if v := queryPtr(r, "active"); v != nil {
		b := *v == "true"
		params.IsActive = &b
	}
	return params, true
}

// usersWithGroups lists users with the groups each one is in.
func (s *Server) usersWithGroups(ctx context.Context, params store.AdminListUsersParams) ([]adminUser, int64, error) {
	rows, err := s.q.AdminListUsers(ctx, params)
	if err != nil {
		return nil, 0, err
	}
	ids := make([]int64, 0, len(rows))
	for _, u := range rows {
		ids = append(ids, u.ID)
	}
	gs, err := s.q.AdminUsersGroupNames(ctx, ids)
	if err != nil {
		return nil, 0, err
	}
	byUser := map[int64][]userGroupRef{}
	for _, g := range gs {
		byUser[g.UserID] = append(byUser[g.UserID], userGroupRef{g.GroupID, g.GroupName, g.IsOfficial})
	}
	items := make([]adminUser, 0, len(rows))
	var total int64
	for _, u := range rows {
		total = u.Total
		n := u.Groups
		list := byUser[u.ID]
		if list == nil {
			list = []userGroupRef{}
		}
		items = append(items, adminUser{ID: u.ID, FirstName: u.FirstName, MiddleName: u.MiddleName,
			LastName: u.LastName, Phone: u.Phone, Email: u.Email, IsActive: u.IsActive,
			LastLoginAt: u.LastLoginAt, CreatedAt: u.CreatedAt, PlatformRoles: u.PlatformRoles, Groups: &n, GroupList: list})
	}
	return items, total, nil
}

func (s *Server) adminListUsers(w http.ResponseWriter, r *http.Request) {
	p := pageParams(r)
	params, ok := adminUsersParams(w, r)
	if !ok {
		return
	}
	params.PageSize, params.PageOffset = p.size, p.offset
	items, total, err := s.usersWithGroups(r.Context(), params)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newPaged(items, total, p))
}

func (s *Server) adminGetUser(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "userID")
	if !ok {
		writeError(w, http.StatusNotFound, "user_not_found", "User not found.")
		return
	}
	u, err := s.loadAdminUser(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "user_not_found", "User not found.")
		return
	}
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) loadAdminUser(ctx context.Context, id int64) (adminUser, error) {
	u, err := s.q.GetUserByID(ctx, id)
	if err != nil {
		return adminUser{}, err
	}
	roles, err := s.q.ListUserPlatformRoles(ctx, id)
	if err != nil {
		return adminUser{}, err
	}
	ms, err := s.q.AdminUserMemberships(ctx, id)
	if err != nil {
		return adminUser{}, err
	}
	out := adminUser{ID: u.ID, FirstName: u.FirstName, MiddleName: u.MiddleName, LastName: u.LastName,
		Phone: u.Phone, Email: u.Email, IDNumber: u.IDNumber, IsActive: u.IsActive,
		LastLoginAt: u.LastLoginAt, CreatedAt: u.CreatedAt,
		PlatformRoles: make([]string, 0, len(roles)), Memberships: make([]any, 0, len(ms))}
	for _, role := range roles {
		out.PlatformRoles = append(out.PlatformRoles, string(role))
	}
	for _, m := range ms {
		out.Memberships = append(out.Memberships, map[string]any{
			"membership_id": m.ID, "group_id": m.GroupID, "group_name": m.GroupName,
			"is_official": m.IsOfficial, "role_name": m.RoleName, "status": m.Status, "joined_at": m.JoinedAt,
		})
	}
	return out, nil
}

type adminUserRequest struct {
	FirstName     Optional[string]   `json:"first_name"`
	MiddleName    Optional[string]   `json:"middle_name"`
	LastName      Optional[string]   `json:"last_name"`
	Phone         Optional[string]   `json:"phone"`
	Email         Optional[string]   `json:"email"`
	IDNumber      Optional[string]   `json:"id_number"`
	IsActive      Optional[bool]     `json:"is_active"`
	PlatformRoles Optional[[]string] `json:"platform_roles"`
	// Password is only read on create. Without one, the user must set a
	// password before logging in.
	Password string `json:"password"`
}

var (
	errInvalidPhone = errors.New("invalid phone")
	errInvalidEmail = errors.New("invalid email")
	errInvalidRole  = errors.New("invalid platform role")
	errLastAdmin    = errors.New("last admin")
	errNoIdentity   = errors.New("no phone or email")
)

func trimmed(o Optional[string]) *string {
	if !o.Set || o.Value == nil {
		return nil
	}
	v := strings.TrimSpace(*o.Value)
	return &v
}

// identity validates the phone and email fields. A nil result with clear
// set means the field was sent as null or empty.
func (s *Server) identity(req adminUserRequest) (phone, email *string, clearPhone, clearEmail bool, err error) {
	if p := trimmed(req.Phone); p != nil && *p != "" {
		n, err := auth.NormalizePhone(*p, s.defaultCallingCode)
		if err != nil {
			return nil, nil, false, false, errInvalidPhone
		}
		phone = &n
	} else if req.Phone.Set {
		clearPhone = true
	}
	if e := trimmed(req.Email); e != nil && *e != "" {
		v := strings.ToLower(*e)
		if !strings.Contains(v, "@") || strings.ContainsAny(v, " \t") {
			return nil, nil, false, false, errInvalidEmail
		}
		email = &v
	} else if req.Email.Set {
		clearEmail = true
	}
	return phone, email, clearPhone, clearEmail, nil
}

func parseRoles(in []string) ([]store.PlatformRole, error) {
	out := make([]store.PlatformRole, 0, len(in))
	for _, v := range in {
		role := store.PlatformRole(v)
		if !role.Valid() {
			return nil, errInvalidRole
		}
		out = append(out, role)
	}
	return out, nil
}

func (s *Server) adminCreateUser(w http.ResponseWriter, r *http.Request) {
	var req adminUserRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	first := trimmed(req.FirstName)
	if first == nil || *first == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "First name is required.")
		return
	}
	phone, email, _, _, err := s.identity(req)
	if err == nil && phone == nil && email == nil {
		err = errNoIdentity
	}
	var roles []store.PlatformRole
	if err == nil && req.PlatformRoles.Value != nil {
		roles, err = parseRoles(*req.PlatformRoles.Value)
	}
	if err == nil && req.Password != "" && len(req.Password) < 8 {
		writeError(w, http.StatusBadRequest, "weak_password", "Passwords need at least 8 characters.")
		return
	}
	if s.writeAdminUserError(w, r, err) {
		return
	}
	var hash *string
	if req.Password != "" {
		b, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
		if err != nil {
			writeInternal(w, r, err)
			return
		}
		h := string(b)
		hash = &h
	}
	var id int64
	err = pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		u, err := q.AdminCreateUser(r.Context(), store.AdminCreateUserParams{
			FirstName: *first, MiddleName: deref(trimmed(req.MiddleName)), LastName: deref(trimmed(req.LastName)),
			Email: email, Phone: phone, IDNumber: emptyNil(trimmed(req.IDNumber)),
			PasswordHash: hash, PasswordResetRequired: false,
		})
		if err != nil {
			return err
		}
		id = u.ID
		for _, role := range roles {
			if err := q.AddPlatformRole(r.Context(), store.AddPlatformRoleParams{UserID: id, Role: role}); err != nil {
				return err
			}
		}
		return nil
	})
	if s.writeAdminUserError(w, r, err) {
		return
	}
	s.audit(r, "user.create", "user", id, map[string]any{"platform_roles": req.PlatformRoles.Value})
	u, err := s.loadAdminUser(r.Context(), id)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, u)
}

func emptyNil(p *string) *string {
	if p == nil || *p == "" {
		return nil
	}
	return p
}

func (s *Server) adminUpdateUser(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "userID")
	if !ok {
		writeError(w, http.StatusNotFound, "user_not_found", "User not found.")
		return
	}
	var req adminUserRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Password != "" {
		writeError(w, http.StatusBadRequest, "invalid_body", "Use the password endpoint to change a password.")
		return
	}
	if f := trimmed(req.FirstName); f != nil && *f == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "First name cannot be empty.")
		return
	}
	phone, email, clearPhone, clearEmail, err := s.identity(req)
	var roles []store.PlatformRole
	if err == nil && req.PlatformRoles.Set {
		roles, err = parseRoles(deref2(req.PlatformRoles.Value))
	}
	if s.writeAdminUserError(w, r, err) {
		return
	}
	self := claimsFrom(r.Context()).UserID == id
	err = pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		u, err := q.AdminUpdateUser(r.Context(), store.AdminUpdateUserParams{
			ID: id, FirstName: trimmed(req.FirstName), MiddleName: trimmed(req.MiddleName),
			LastName: trimmed(req.LastName), Email: email, ClearEmail: clearEmail,
			Phone: phone, ClearPhone: clearPhone, IDNumber: trimmed(req.IDNumber), IsActive: req.IsActive.Value,
		})
		if err != nil {
			return err
		}
		if req.PlatformRoles.Set {
			if err := q.ClearPlatformRoles(r.Context(), id); err != nil {
				return err
			}
			for _, role := range roles {
				if err := q.AddPlatformRole(r.Context(), store.AddPlatformRoleParams{UserID: id, Role: role}); err != nil {
					return err
				}
			}
		}
		// Never leave the platform without an admin who can log in, and
		// never let admins lock themselves out by accident.
		n, err := q.CountActiveAdmins(r.Context())
		if err != nil {
			return err
		}
		if n == 0 || (self && (!u.IsActive || !hasRole(roles, req.PlatformRoles.Set, store.PlatformRoleAdmin))) {
			return errLastAdmin
		}
		if !u.IsActive {
			return q.RevokeUserRefreshTokens(r.Context(), id)
		}
		return nil
	})
	if s.writeAdminUserError(w, r, err) {
		return
	}
	s.audit(r, "user.update", "user", id, req)
	u, err := s.loadAdminUser(r.Context(), id)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func deref2[T any](p *[]T) []T {
	if p == nil {
		return nil
	}
	return *p
}

// hasRole reports whether the update keeps role; an update that does not
// touch roles keeps them all.
func hasRole(roles []store.PlatformRole, set bool, role store.PlatformRole) bool {
	if !set {
		return true
	}
	for _, r := range roles {
		if r == role {
			return true
		}
	}
	return false
}

func (s *Server) adminSetPassword(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "userID")
	if !ok {
		writeError(w, http.StatusNotFound, "user_not_found", "User not found.")
		return
	}
	var req struct {
		Password      string `json:"password"`
		RequireChange bool   `json:"require_change"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.Password) < 8 {
		writeError(w, http.StatusBadRequest, "weak_password", "Passwords need at least 8 characters.")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	if _, err := s.q.GetUserByID(r.Context(), id); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "user_not_found", "User not found.")
		return
	} else if err != nil {
		writeInternal(w, r, err)
		return
	}
	err = pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		if err := q.SetUserPassword(r.Context(), store.SetUserPasswordParams{
			ID: id, PasswordHash: ptr(string(hash)), PasswordResetRequired: req.RequireChange,
		}); err != nil {
			return err
		}
		// A reset signs the user out everywhere.
		return q.RevokeUserRefreshTokens(r.Context(), id)
	})
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	s.audit(r, "user.password", "user", id, map[string]bool{"require_change": req.RequireChange})
	w.WriteHeader(http.StatusNoContent)
}

func ptr[T any](v T) *T { return &v }

func (s *Server) writeAdminUserError(w http.ResponseWriter, r *http.Request, err error) bool {
	var pgErr *pgconn.PgError
	switch {
	case err == nil:
		return false
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "user_not_found", "User not found.")
	case errors.Is(err, errInvalidPhone):
		writeError(w, http.StatusBadRequest, "invalid_phone", "Phone number is not valid.")
	case errors.Is(err, errInvalidEmail):
		writeError(w, http.StatusBadRequest, "invalid_email", "Email address is not valid.")
	case errors.Is(err, errInvalidRole):
		writeError(w, http.StatusBadRequest, "invalid_role", "Unknown platform role.")
	case errors.Is(err, errNoIdentity):
		writeError(w, http.StatusBadRequest, "missing_fields", "A phone number or an email address is required.")
	case errors.Is(err, errLastAdmin):
		writeError(w, http.StatusConflict, "last_admin", "You can't remove your own admin access or deactivate yourself, and the platform needs at least one active admin.")
	case errors.As(err, &pgErr) && pgErr.Code == "23505":
		writeError(w, http.StatusConflict, "identity_taken", "Another user already has that phone number or email address.")
	case errors.As(err, &pgErr) && pgErr.Code == "23514":
		writeError(w, http.StatusBadRequest, "missing_fields", "A user needs a phone number or an email address.")
	default:
		writeInternal(w, r, err)
	}
	return true
}

// ---- transactions ----

func (s *Server) adminListTransactions(w http.ResponseWriter, r *http.Request) {
	p := pageParams(r)
	params := store.AdminListTransactionsParams{PageSize: p.size, PageOffset: p.offset,
		IncludeVoided: r.URL.Query().Get("include_voided") == "true"}
	if v := queryPtr(r, "group_id"); v != nil {
		id, err := strconv.ParseInt(*v, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_group", "group_id must be a number.")
			return
		}
		params.GroupID = &id
	}
	if v := queryPtr(r, "type"); v != nil {
		t := store.LedgerEntryType(*v)
		if !t.Valid() {
			writeError(w, http.StatusBadRequest, "invalid_type", "Unknown transaction type.")
			return
		}
		params.EntryType = store.NullLedgerEntryType{LedgerEntryType: t, Valid: true}
	}
	for key, dst := range map[string]**time.Time{"from": &params.FromDate, "to": &params.ToDate} {
		if v := queryPtr(r, key); v != nil {
			t, err := time.Parse(time.DateOnly, *v)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid_date", key+" must be a date like 2026-01-31.")
				return
			}
			*dst = &t
		}
	}
	rows, err := s.q.AdminListTransactions(r.Context(), params)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	type txn struct {
		ID               int64      `json:"id"`
		GroupID          int64      `json:"group_id"`
		GroupName        string     `json:"group_name"`
		MembershipID     *int64     `json:"membership_id"`
		MemberName       string     `json:"member_name"`
		Type             string     `json:"type"`
		Label            string     `json:"label"`
		Amount           string     `json:"amount"`
		Date             Date       `json:"date"`
		ContributionID   *int64     `json:"contribution_id"`
		ContributionName *string    `json:"contribution_name"`
		Description      string     `json:"description"`
		VoidedAt         *time.Time `json:"voided_at"`
	}
	items := make([]txn, 0, len(rows))
	var total int64
	for _, e := range rows {
		total = e.Total
		items = append(items, txn{e.ID, e.GroupID, e.GroupName, e.MembershipID,
			strings.TrimSpace(deref(e.FirstName) + " " + deref(e.LastName)), string(e.EntryType),
			entryLabels[e.EntryType], *numericMoney(e.Amount), Date{e.EntryDate}, e.ContributionID,
			e.ContributionName, e.Description, e.VoidedAt})
	}
	writeJSON(w, http.StatusOK, newPaged(items, total, p))
}
