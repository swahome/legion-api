package httpapi

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// exportLimit caps an export. The whole platform has fewer groups and
// users than this; a bigger result is cut and says so in a last row.
const exportLimit = 100_000

// platformRoleInfo describes the fixed platform roles (the legacy "user
// groups"). They are fixed in code, so the admin page lists them read-only.
var platformRoleInfo = []struct{ Role, Name, Description string }{
	{"admin", "Admin", "Platform admins: the whole admin panel"},
	{"group_account_manager", "Group account manager", "Looks after a set of groups"},
	{"banker", "Banker", "Bank staff (see each bank's panel for their bank)"},
	{"partner", "Partner", "Partners and agents"},
	{"sales", "Sales representative", "Signs up new groups"},
	{"diocese_admin", "Diocese admin", "Church hierarchy: diocese"},
	{"deanery_admin", "Deanery admin", "Church hierarchy: deanery"},
	{"parish_admin", "Parish admin", "Church hierarchy: parish"},
	{"sub_parish_admin", "Sub-parish admin", "Church hierarchy: sub-parish"},
}

func (s *Server) adminListRoles(w http.ResponseWriter, r *http.Request) {
	counts, err := s.q.AdminRoleCounts(r.Context())
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	n := map[string]int64{}
	for _, c := range counts {
		n[c.Role] = c.Users
	}
	type role struct {
		Role        string `json:"role"`
		Name        string `json:"name"`
		Description string `json:"description"`
		Users       int64  `json:"users"`
	}
	out := make([]role, 0, len(platformRoleInfo))
	for _, p := range platformRoleInfo {
		out = append(out, role{p.Role, p.Name, p.Description, n[p.Role]})
	}
	writeJSON(w, http.StatusOK, out)
}

func startCSV(w http.ResponseWriter, name string) *csv.Writer {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%s.csv"`, name, time.Now().Format("2006-01-02")))
	// A byte-order mark makes Excel read the file as UTF-8.
	_, _ = w.Write([]byte{0xEF, 0xBB, 0xBF})
	return csv.NewWriter(w)
}

func timeStr(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format("2006-01-02 15:04")
}

// adminExportGroups writes the group list (same filters as the list) as CSV.
func (s *Server) adminExportGroups(w http.ResponseWriter, r *http.Request) {
	params, ok := adminGroupsParams(w, r)
	if !ok {
		return
	}
	params.PageSize = exportLimit + 1
	rows, err := s.q.AdminListGroups(r.Context(), params)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	c := startCSV(w, "groups")
	_ = c.Write([]string{"ID", "Name", "Status", "Members", "Registered", "Contact", "Contact phone", "Contact email",
		"Group phone", "Group email", "Country", "Currency", "Last seen", "Joined"})
	for i, g := range rows {
		if i == exportLimit {
			_ = c.Write([]string{"", "Export cut at " + strconv.Itoa(exportLimit) + " rows"})
			break
		}
		_ = c.Write([]string{strconv.FormatInt(g.ID, 10), g.Name, string(g.Status),
			strconv.FormatInt(g.ActiveMembers, 10), strconv.FormatInt(g.RegisteredMembers, 10),
			strings.TrimSpace(g.ContactName), deref(g.ContactPhone), deref(g.ContactEmail), deref(g.GroupPhone), deref(g.GroupEmail),
			g.CountryCode, g.Currency, timeStr(g.LastSeenAt), g.CreatedAt.Format("2006-01-02")})
	}
	c.Flush()
}

// adminExportUsers writes the user list (same filters as the list) as CSV.
func (s *Server) adminExportUsers(w http.ResponseWriter, r *http.Request) {
	params, ok := adminUsersParams(w, r)
	if !ok {
		return
	}
	params.PageSize = exportLimit + 1
	users, _, err := s.usersWithGroups(r.Context(), params)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	c := startCSV(w, "users")
	_ = c.Write([]string{"ID", "First name", "Middle name", "Last name", "Phone", "Email", "Active", "Roles", "Groups", "Last login", "Created"})
	for i, u := range users {
		if i == exportLimit {
			_ = c.Write([]string{"", "Export cut at " + strconv.Itoa(exportLimit) + " rows"})
			break
		}
		names := make([]string, 0, len(u.GroupList))
		for _, g := range u.GroupList {
			names = append(names, g.GroupName)
		}
		active := "no"
		if u.IsActive {
			active = "yes"
		}
		_ = c.Write([]string{strconv.FormatInt(u.ID, 10), u.FirstName, u.MiddleName, u.LastName, deref(u.Phone), deref(u.Email),
			active, strings.Join(u.PlatformRoles, "; "), strings.Join(names, "; "), timeStr(u.LastLoginAt), u.CreatedAt.Format("2006-01-02")})
	}
	c.Flush()
}
