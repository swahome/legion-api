package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/swahome/legion-api/internal/store"
)

// groupAccess is what the caller may do in the group named in the URL.
type groupAccess struct {
	Group store.Group
	// MembershipID is the caller's own membership, nil for a platform
	// admin who is not a member.
	MembershipID  *int64
	IsOfficial    bool
	PlatformAdmin bool
	settings      map[string]string
}

// setting reads a legacy group flag kept in groups.settings.
func (a groupAccess) setting(key string) string { return a.settings[key] }

const accessKey ctxKey = iota + 100

// requireGroup resolves {groupID} and the caller's membership. Non-members
// get 404 so group ids cannot be probed. Platform admins get official
// access to every group, like the legacy "view group as admin".
func (s *Server) requireGroup(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		gid, err := strconv.ParseInt(chi.URLParam(r, "groupID"), 10, 64)
		if err != nil {
			writeError(w, http.StatusNotFound, "group_not_found", "Group not found.")
			return
		}
		claims := claimsFrom(ctx)
		g, err := s.q.GetGroup(ctx, gid)
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "group_not_found", "Group not found.")
			return
		}
		if err != nil {
			writeInternal(w, r, err)
			return
		}
		a := groupAccess{Group: g}
		_ = json.Unmarshal(g.Settings, &a.settings)

		m, err := s.q.GetActiveMembership(ctx, store.GetActiveMembershipParams{GroupID: gid, UserID: claims.UserID})
		switch {
		case err == nil:
			a.MembershipID, a.IsOfficial = &m.ID, m.IsOfficial
		case errors.Is(err, pgx.ErrNoRows) && claims.Admin:
			a.IsOfficial, a.PlatformAdmin = true, true
		case errors.Is(err, pgx.ErrNoRows):
			writeError(w, http.StatusNotFound, "group_not_found", "Group not found.")
			return
		default:
			writeInternal(w, r, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, accessKey, a)))
	})
}

func accessFrom(ctx context.Context) groupAccess {
	a, _ := ctx.Value(accessKey).(groupAccess)
	return a
}

// requireOfficial limits a route to the group's officials and platform admins.
func requireOfficial(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !accessFrom(r.Context()).IsOfficial {
			writeError(w, http.StatusForbidden, "officials_only", "Only group officials can do this.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// canSeeMember reports whether the caller may read a member's details and statement.
func (a groupAccess) canSeeMember(membershipID int64) bool {
	return a.IsOfficial || (a.MembershipID != nil && *a.MembershipID == membershipID)
}

func pathID(r *http.Request, name string) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, name), 10, 64)
	return id, err == nil && id > 0
}
