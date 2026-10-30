package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/swahome/legion-api/internal/store"
)

type userResponse struct {
	ID          int64      `json:"id"`
	FirstName   string     `json:"first_name"`
	MiddleName  string     `json:"middle_name"`
	LastName    string     `json:"last_name"`
	Email       *string    `json:"email"`
	Phone       *string    `json:"phone"`
	LastLoginAt *time.Time `json:"last_login_at"`
}

func toUserResponse(u store.User) userResponse {
	return userResponse{
		ID:          u.ID,
		FirstName:   u.FirstName,
		MiddleName:  u.MiddleName,
		LastName:    u.LastName,
		Email:       u.Email,
		Phone:       u.Phone,
		LastLoginAt: u.LastLoginAt,
	}
}

type membershipResponse struct {
	MembershipID int64   `json:"membership_id"`
	GroupID      int64   `json:"group_id"`
	GroupName    string  `json:"group_name"`
	GroupSlug    string  `json:"group_slug"`
	GroupStatus  string  `json:"group_status"`
	Currency     string  `json:"currency"`
	IsOfficial   bool    `json:"is_official"`
	RoleName     *string `json:"role_name"`
	Status       string  `json:"status"`
}

type meResponse struct {
	User          userResponse         `json:"user"`
	PlatformRoles []string             `json:"platform_roles"`
	Memberships   []membershipResponse `json:"memberships"`
	// DisabledMenus lists the menu items a platform admin switched off,
	// like "group.arrears". The menus themselves are fixed in the web app.
	DisabledMenus []string `json:"disabled_menus"`
}

// disabledMenusKey is the platform setting that holds the switched-off menus.
const disabledMenusKey = "menus.disabled"

// me returns the signed-in user, their platform roles and every group they
// belong to. The web app uses the memberships list as its group switcher.
func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	uid := claimsFrom(ctx).UserID
	user, err := s.q.GetUserByID(ctx, uid)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !user.IsActive) {
		writeError(w, http.StatusUnauthorized, "invalid_token", "Access token is invalid or expired.")
		return
	}
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	roles, err := s.q.ListUserPlatformRoles(ctx, uid)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	rows, err := s.q.ListUserMemberships(ctx, uid)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	resp := meResponse{
		User:          toUserResponse(user),
		PlatformRoles: make([]string, 0, len(roles)),
		Memberships:   make([]membershipResponse, 0, len(rows)),
	}
	for _, role := range roles {
		resp.PlatformRoles = append(resp.PlatformRoles, string(role))
	}
	for _, m := range rows {
		resp.Memberships = append(resp.Memberships, membershipResponse{
			MembershipID: m.MembershipID,
			GroupID:      m.GroupID,
			GroupName:    m.GroupName,
			GroupSlug:    m.GroupSlug,
			GroupStatus:  string(m.GroupStatus),
			Currency:     m.Currency,
			IsOfficial:   m.IsOfficial,
			RoleName:     m.RoleName,
			Status:       string(m.Status),
		})
	}
	resp.DisabledMenus = []string{}
	if v, err := s.q.GetPlatformSetting(ctx, disabledMenusKey); err == nil {
		// A malformed value leaves every menu on.
		_ = json.Unmarshal(v, &resp.DisabledMenus)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
