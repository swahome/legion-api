package mobile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/swahome/legion-api/internal/store"
)

// member is the caller inside a group, the legacy $this->user,
// $this->group and $this->member.
type member struct {
	user     store.User
	group    store.Group
	m        store.Membership
	settings map[string]any
	loc      *time.Location
}

func (g *member) official() bool { return g.m.IsOfficial || g.m.GroupRoleID != nil }

// flag reads a legacy group setting such as disable_arrears ("1" or 1).
func (g *member) flag(key string) bool {
	switch v := g.settings[key].(type) {
	case string:
		n, _ := strconv.Atoi(v)
		return n != 0
	case float64:
		return v != 0
	case bool:
		return v
	}
	return false
}

// inGroup runs the checks every legacy group action starts with: the user
// (status 4), the group (status 5) and the caller's membership (status 6,
// or notMember where the action used another code). Like the legacy code
// it also records the user as seen now.
//
// Unlike the legacy app, suspended and closed groups count as not found,
// as they do on the web.
func (s *Server) inGroup(ctx context.Context, c *call, notMember int) (*member, map[string]any, error) {
	user, err := s.q.GetUserByID(ctx, c.userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, timed(status(4, "Could not find user details"), s.now()), nil
	}
	if err != nil {
		return nil, nil, err
	}
	if err := s.q.TouchLastLogin(ctx, user.ID); err != nil {
		return nil, nil, err
	}
	group, err := s.q.GetGroup(ctx, c.req.Int("group_id"))
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (group.Status == store.GroupStatusSuspended || group.Status == store.GroupStatusClosed)) {
		return nil, timed(status(5, "Could not find group details"), s.now()), nil
	}
	if err != nil {
		return nil, nil, err
	}
	m, err := s.q.GetMembershipByUser(ctx, store.GetMembershipByUserParams{GroupID: group.ID, UserID: user.ID})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && m.Status == store.MembershipStatusRemoved) {
		return nil, timed(status(notMember, "Could not find member details"), s.now()), nil
	}
	if err != nil {
		return nil, nil, err
	}
	g := &member{user: user, group: group, m: m}
	_ = json.Unmarshal(group.Settings, &g.settings)
	if g.loc, err = time.LoadLocation(group.Timezone); err != nil {
		g.loc = nairobi
	}
	return g, nil, nil
}

var nairobi = func() *time.Location {
	l, err := time.LoadLocation("Africa/Nairobi")
	if err != nil {
		return time.FixedZone("EAT", 3*60*60)
	}
	return l
}()

// userAction wraps an action that only checks the user (status 4), like
// the legacy country lists.
func (s *Server) userAction(fn func(ctx context.Context, c *call) (any, error)) func(context.Context, *call) (any, error) {
	return func(ctx context.Context, c *call) (any, error) {
		if _, err := s.q.GetUserByID(ctx, c.userID); errors.Is(err, pgx.ErrNoRows) {
			return timed(status(4, "Could not find user details"), s.now()), nil
		} else if err != nil {
			return nil, err
		}
		if err := s.q.TouchLastLogin(ctx, c.userID); err != nil {
			return nil, err
		}
		return fn(ctx, c)
	}
}

// groupAction wraps an action that needs the caller's membership.
func (s *Server) groupAction(notMember int, fn func(ctx context.Context, c *call, g *member) (any, error)) func(context.Context, *call) (any, error) {
	return func(ctx context.Context, c *call) (any, error) {
		g, reply, err := s.inGroup(ctx, c, notMember)
		if err != nil || reply != nil {
			return reply, err
		}
		return fn(ctx, c, g)
	}
}

// getGroupData is the check-in data for one group.
func (s *Server) getGroupData(ctx context.Context, c *call, g *member) (any, error) {
	data, err := s.checkinData(ctx, g.user.ID, g.group.ID)
	if err != nil {
		return nil, err
	}
	resp := map[string]any{"status": 1, "message": "Group data"}
	for k, v := range data {
		resp[k] = v
	}
	return resp, nil
}

func (s *Server) getCountryOptions(ctx context.Context, c *call) (any, error) {
	rows, err := s.q.MobileCountries(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, map[string]any{"id": r.ID, "name": r.Name})
	}
	return map[string]any{"status": 1, "message": "successful", "countries": out}, nil
}

func (s *Server) getCurrencyOptions(ctx context.Context, c *call) (any, error) {
	rows, err := s.q.MobileCountries(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, map[string]any{"id": r.ID, "name": r.CurrencyName})
	}
	return map[string]any{"status": 1, "message": "successful", "currencies": out}, nil
}

func (s *Server) getBankOptions(ctx context.Context, c *call, g *member) (any, error) {
	rows, err := s.q.MobileBanks(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, map[string]any{"id": fmt.Sprint(r.ID), "name": r.Name, "logo": r.Logo})
	}
	return map[string]any{"status": 1, "message": "successful", "banks": out}, nil
}

func (s *Server) getBankBranchOptions(ctx context.Context, c *call, g *member) (any, error) {
	bankID, err := strconv.ParseInt(c.req.Str("bank_id"), 10, 64)
	if err != nil {
		return timed(status(0, "Request submitted is invalid"), s.now()), nil
	}
	rows, err := s.q.MobileBankBranches(ctx, bankID)
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, map[string]any{"id": r.ID, "name": r.Name})
	}
	return map[string]any{"status": 1, "message": "successful", "bank_branches": out}, nil
}

// page reads the app's lower_limit/upper_limit paging: 100 rows by default.
func page(c *call) (limit, offset int32) {
	lower, upper := c.req.Int("lower_limit"), c.req.Int("upper_limit")
	if upper == 0 {
		upper = 100
	}
	n := upper - lower
	if lower < 0 || n <= 0 || n > 1000 {
		lower, n = 0, 100
	}
	return int32(n), int32(lower)
}

func (s *Server) groupRolesList(ctx context.Context, c *call, g *member) (any, error) {
	limit, offset := page(c)
	rows, err := s.q.MobileGroupRoles(ctx, store.MobileGroupRolesParams{GroupID: g.group.ID, Lim: limit, Off: offset})
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, map[string]any{
			"id":          fmt.Sprint(r.ID),
			"description": r.Description,
			"name":        r.Name,
			"active":      boolInt(r.IsActive),
			"is_editable": boolInt(r.IsEditable),
			"in_use":      boolInt(r.InUse),
		})
	}
	return map[string]any{"status": 1, "message": "success", "group_roles": out}, nil
}
