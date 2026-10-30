package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/swahome/legion-api/internal/auth"
	"github.com/swahome/legion-api/internal/store"
)

type groupResponse struct {
	ID            int64   `json:"id"`
	Name          string  `json:"name"`
	Slug          string  `json:"slug"`
	Status        string  `json:"status"`
	Currency      string  `json:"currency"`
	Email         *string `json:"email,omitempty"`
	Phone         *string `json:"phone,omitempty"`
	Address       *string `json:"address,omitempty"`
	MyMembership  *int64  `json:"my_membership_id"`
	IsOfficial    bool    `json:"is_official"`
	PlatformAdmin bool    `json:"platform_admin"`
	Summary       struct {
		ActiveMembers          int64  `json:"active_members"`
		Officials              int64  `json:"officials"`
		ActiveContributions    int64  `json:"active_contributions"`
		TotalContributionsPaid string `json:"total_contributions_paid"`
	} `json:"summary"`
}

func (s *Server) getGroup(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	sum, err := s.q.GroupSummary(r.Context(), a.Group.ID)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	g := a.Group
	resp := groupResponse{
		ID: g.ID, Name: g.Name, Slug: g.Slug, Status: string(g.Status), Currency: g.Currency,
		MyMembership: a.MembershipID, IsOfficial: a.IsOfficial, PlatformAdmin: a.PlatformAdmin,
	}
	if a.IsOfficial {
		resp.Email, resp.Phone, resp.Address = g.Email, g.Phone, g.Address
	}
	resp.Summary.ActiveMembers = sum.ActiveMembers
	resp.Summary.Officials = sum.Officials
	resp.Summary.ActiveContributions = sum.ActiveContributions
	resp.Summary.TotalContributionsPaid = money(sum.TotalContributionsPaid)
	writeJSON(w, http.StatusOK, resp)
}

type roleResponse struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

func (s *Server) listRoles(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	roles, err := s.q.ListGroupRoles(r.Context(), a.Group.ID)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	out := make([]roleResponse, 0, len(roles))
	for _, ro := range roles {
		out = append(out, roleResponse{ID: ro.ID, Name: ro.Name, Description: ro.Description})
	}
	writeJSON(w, http.StatusOK, map[string]any{"roles": out})
}

type memberResponse struct {
	ID               int64     `json:"id"`
	UserID           int64     `json:"user_id"`
	FirstName        string    `json:"first_name"`
	MiddleName       string    `json:"middle_name"`
	LastName         string    `json:"last_name"`
	IsOfficial       bool      `json:"is_official"`
	RoleID           *int64    `json:"role_id"`
	RoleName         *string   `json:"role_name"`
	MembershipNumber *string   `json:"membership_number"`
	Status           string    `json:"status"`
	JoinedAt         time.Time `json:"joined_at"`
	// Contact details are only shown to officials and to the member themself.
	Phone           *string `json:"phone,omitempty"`
	Email           *string `json:"email,omitempty"`
	DateOfBirth     *Date   `json:"date_of_birth,omitempty"`
	PhysicalAddress *string `json:"physical_address,omitempty"`
	PostalAddress   *string `json:"postal_address,omitempty"`
}

// listMembers is the member directory. Ordinary members see names and
// offices only, and not at all when the group turned the directory off
// (legacy investment_groups.disable_member_directory).
func (s *Server) listMembers(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	if !a.IsOfficial && a.setting("disable_member_directory") == "1" {
		writeError(w, http.StatusForbidden, "directory_disabled", "This group has turned off the member directory.")
		return
	}
	p := store.ListMembersParams{GroupID: a.Group.ID}
	switch st := r.URL.Query().Get("status"); st {
	case "", "active":
		p.Status = store.NullMembershipStatus{MembershipStatus: store.MembershipStatusActive, Valid: true}
	case "all":
		if !a.IsOfficial {
			p.Status = store.NullMembershipStatus{MembershipStatus: store.MembershipStatusActive, Valid: true}
		}
	case "suspended", "removed":
		if !a.IsOfficial {
			writeError(w, http.StatusForbidden, "officials_only", "Only group officials can see former members.")
			return
		}
		p.Status = store.NullMembershipStatus{MembershipStatus: store.MembershipStatus(st), Valid: true}
	default:
		writeError(w, http.StatusBadRequest, "invalid_status", "status must be active, suspended, removed or all.")
		return
	}
	if q := strings.TrimSpace(r.URL.Query().Get("q")); q != "" {
		p.Search = &q
	}
	rows, err := s.q.ListMembers(r.Context(), p)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	out := make([]memberResponse, 0, len(rows))
	for _, m := range rows {
		mr := memberResponse{
			ID: m.ID, UserID: m.UserID, FirstName: m.FirstName, MiddleName: m.MiddleName, LastName: m.LastName,
			IsOfficial: m.IsOfficial, RoleID: m.GroupRoleID, RoleName: m.RoleName,
			MembershipNumber: m.MembershipNumber, Status: string(m.Status), JoinedAt: m.JoinedAt,
		}
		if a.canSeeMember(m.ID) {
			mr.Phone, mr.Email = m.Phone, m.Email
		}
		out = append(out, mr)
	}
	writeJSON(w, http.StatusOK, map[string]any{"members": out})
}

func (s *Server) getMember(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	mid, ok := pathID(r, "memberID")
	if !ok || !a.canSeeMember(mid) {
		writeError(w, http.StatusNotFound, "member_not_found", "Member not found.")
		return
	}
	m, err := s.loadMember(r.Context(), a.Group.ID, mid)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "member_not_found", "Member not found.")
		return
	}
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) loadMember(ctx context.Context, groupID, id int64) (memberResponse, error) {
	m, err := s.q.GetMember(ctx, store.GetMemberParams{GroupID: groupID, ID: id})
	if err != nil {
		return memberResponse{}, err
	}
	return memberResponse{
		ID: m.ID, UserID: m.UserID, FirstName: m.FirstName, MiddleName: m.MiddleName, LastName: m.LastName,
		IsOfficial: m.IsOfficial, RoleID: m.GroupRoleID, RoleName: m.RoleName,
		MembershipNumber: m.MembershipNumber, Status: string(m.Status), JoinedAt: m.JoinedAt,
		Phone: m.Phone, Email: m.Email, DateOfBirth: datePtr(m.DateOfBirth),
		PhysicalAddress: m.PhysicalAddress, PostalAddress: m.PostalAddress,
	}, nil
}

type addMemberRequest struct {
	FirstName        string  `json:"first_name"`
	LastName         string  `json:"last_name"`
	Phone            string  `json:"phone"`
	Email            string  `json:"email"`
	IsOfficial       bool    `json:"is_official"`
	RoleID           *int64  `json:"role_id"`
	MembershipNumber *string `json:"membership_number"`
}

// addMember adds a person to the group by phone number. An existing user
// with that phone (or email) is reused; otherwise a user is created who
// must set a password before logging in.
func (s *Server) addMember(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	var req addMemberRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.FirstName, req.LastName = strings.TrimSpace(req.FirstName), strings.TrimSpace(req.LastName)
	if req.FirstName == "" || strings.TrimSpace(req.Phone) == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "First name and phone number are required.")
		return
	}
	phone, err := auth.NormalizePhone(req.Phone, s.defaultCallingCode)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_phone", "Phone number is not valid.")
		return
	}
	var email *string
	if e := strings.ToLower(strings.TrimSpace(req.Email)); e != "" {
		if _, err := auth.ParseIdentity(e, s.defaultCallingCode); err != nil || !strings.Contains(e, "@") {
			writeError(w, http.StatusBadRequest, "invalid_email", "Email address is not valid.")
			return
		}
		email = &e
	}

	var memberID int64
	err = pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		uid, err := s.findOrCreateUser(r.Context(), q, req.FirstName, req.LastName, phone, email)
		if err != nil {
			return err
		}
		existing, err := q.GetMembershipByUser(r.Context(), store.GetMembershipByUserParams{GroupID: a.Group.ID, UserID: uid})
		switch {
		case err == nil && existing.Status == store.MembershipStatusActive:
			return errAlreadyMember
		case err == nil:
			memberID = existing.ID
			if err := q.ReactivateMembership(r.Context(), existing.ID); err != nil {
				return err
			}
			return q.UpdateMembership(r.Context(), store.UpdateMembershipParams{
				GroupID: a.Group.ID, ID: existing.ID, IsOfficial: &req.IsOfficial,
				GroupRoleID: req.RoleID, MembershipNumber: req.MembershipNumber,
			})
		case errors.Is(err, pgx.ErrNoRows):
			memberID, err = q.CreateMembership(r.Context(), store.CreateMembershipParams{
				GroupID: a.Group.ID, UserID: uid, IsOfficial: req.IsOfficial,
				GroupRoleID: req.RoleID, MembershipNumber: req.MembershipNumber,
			})
			return err
		default:
			return err
		}
	})
	if s.writeMemberWriteError(w, r, err) {
		return
	}
	m, err := s.loadMember(r.Context(), a.Group.ID, memberID)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, m)
}

var (
	errAlreadyMember = errors.New("already a member")
	errLastOfficial  = errors.New("last official")
	errEmailTaken    = errors.New("email belongs to another user")
)

func (s *Server) findOrCreateUser(ctx context.Context, q *store.Queries, first, last, phone string, email *string) (int64, error) {
	u, err := q.GetUserByPhone(ctx, &phone)
	if err == nil {
		return u.ID, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	if email != nil {
		if _, err := q.GetUserByEmail(ctx, *email); err == nil {
			// A different person (different phone) already uses this email.
			return 0, errEmailTaken
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return 0, err
		}
	}
	return q.CreateUserWithoutPassword(ctx, store.CreateUserWithoutPasswordParams{
		FirstName: first, LastName: last, Phone: &phone, Email: email,
	})
}

type updateMemberRequest struct {
	IsOfficial       *bool           `json:"is_official"`
	RoleID           Optional[int64] `json:"role_id"`
	MembershipNumber *string         `json:"membership_number"`
	Status           *string         `json:"status"`
}

func (s *Server) updateMember(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	mid, ok := pathID(r, "memberID")
	if !ok {
		writeError(w, http.StatusNotFound, "member_not_found", "Member not found.")
		return
	}
	var req updateMemberRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	p := store.UpdateMembershipParams{
		GroupID: a.Group.ID, ID: mid, IsOfficial: req.IsOfficial, MembershipNumber: req.MembershipNumber,
	}
	if req.RoleID.Set {
		p.GroupRoleID, p.ClearRole = req.RoleID.Value, req.RoleID.Value == nil
	}
	if req.Status != nil {
		switch st := store.MembershipStatus(*req.Status); st {
		case store.MembershipStatusActive, store.MembershipStatusSuspended, store.MembershipStatusRemoved:
			p.Status = store.NullMembershipStatus{MembershipStatus: st, Valid: true}
		default:
			writeError(w, http.StatusBadRequest, "invalid_status", "status must be active, suspended or removed.")
			return
		}
	}

	err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		// Lock the group's official memberships so two officials cannot
		// demote each other at the same time and leave nobody in charge.
		var before, after int
		err := tx.QueryRow(r.Context(), `
			SELECT count(*) FROM (SELECT 1 FROM memberships
			WHERE group_id = $1 AND status = 'active' AND is_official FOR UPDATE) o`, a.Group.ID).Scan(&before)
		if err != nil {
			return err
		}
		if _, err := q.GetMember(r.Context(), store.GetMemberParams{GroupID: a.Group.ID, ID: mid}); err != nil {
			return err
		}
		if err := q.UpdateMembership(r.Context(), p); err != nil {
			return err
		}
		err = tx.QueryRow(r.Context(), `SELECT count(*) FROM memberships
			WHERE group_id = $1 AND status = 'active' AND is_official`, a.Group.ID).Scan(&after)
		if err != nil {
			return err
		}
		if before > 0 && after == 0 {
			return errLastOfficial
		}
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "member_not_found", "Member not found.")
		return
	}
	if s.writeMemberWriteError(w, r, err) {
		return
	}
	m, err := s.loadMember(r.Context(), a.Group.ID, mid)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// writeMemberWriteError maps member write failures to responses. It
// returns true when it wrote one.
func (s *Server) writeMemberWriteError(w http.ResponseWriter, r *http.Request, err error) bool {
	var pgErr *pgconn.PgError
	switch {
	case err == nil:
		return false
	case errors.Is(err, errAlreadyMember):
		writeError(w, http.StatusConflict, "already_member", "This person is already a member of the group.")
	case errors.Is(err, errLastOfficial):
		writeError(w, http.StatusConflict, "last_official", "A group needs at least one active official.")
	case errors.Is(err, errEmailTaken):
		writeError(w, http.StatusConflict, "email_taken", "That email address belongs to someone with a different phone number.")
	case errors.As(err, &pgErr) && pgErr.Code == "23503":
		writeError(w, http.StatusBadRequest, "invalid_role", "That role does not belong to this group.")
	default:
		writeInternal(w, r, err)
	}
	return true
}
