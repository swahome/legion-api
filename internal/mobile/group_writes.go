package mobile

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/swahome/legion-api/internal/auth"
	"github.com/swahome/legion-api/internal/store"
)

// Group profile, settings, roles, members and contributions from the app:
// the writes the legacy groups, group_roles, members and contributions
// modules had. All of them are for officials here.

// setGroupFields updates the group and the settings JSON in one go.
func (s *Server) setGroupFields(ctx context.Context, g *member, p store.SetGroupFieldsParams, settings map[string]any) error {
	if settings == nil {
		settings = map[string]any{}
	}
	b, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	p.ID, p.Settings = g.group.ID, b
	return s.q.SetGroupFields(ctx, p)
}

func (s *Server) updateGroupName(ctx context.Context, c *call, g *member) (any, error) {
	name := strings.ToUpper(c.req.Str("name"))
	if name == "" {
		return status(0, "Group name not found"), nil
	}
	if err := s.setGroupFields(ctx, g, store.SetGroupFieldsParams{Name: &name}, nil); err != nil {
		return nil, err
	}
	return map[string]any{"status": 1, "message": "Successful", "name": name}, nil
}

func (s *Server) updateGroupPhone(ctx context.Context, c *call, g *member) (any, error) {
	phone, err := auth.NormalizePhone(c.req.Str("phone"), s.auth.DefaultCallingCode())
	if err != nil || c.req.Str("phone") == "" {
		return status(0, "Group phone number is invalid"), nil
	}
	if err := s.setGroupFields(ctx, g, store.SetGroupFieldsParams{Phone: &phone}, nil); err != nil {
		return nil, err
	}
	return map[string]any{"status": 1, "message": "Successful", "phone": strings.TrimPrefix(phone, "+")}, nil
}

func (s *Server) updateGroupEmail(ctx context.Context, c *call, g *member) (any, error) {
	email := strings.ToLower(c.req.Str("email"))
	if !validEmail(email) {
		return status(0, "Group Email is invalid"), nil
	}
	if err := s.setGroupFields(ctx, g, store.SetGroupFieldsParams{Email: &email}, nil); err != nil {
		return nil, err
	}
	return map[string]any{"status": 1, "message": "Successful", "email": email}, nil
}

// updateGroupCountry and updateGroupCurrency take a country id, as the
// legacy option lists gave them.
func (s *Server) updateGroupCountry(currency bool) groupFn {
	key := "country_id"
	if currency {
		key = "currency_id"
	}
	return func(ctx context.Context, c *call, g *member) (any, error) {
		id := appID(c.req.Str(key))
		if id == nil {
			return status(0, "Group Email is invalid"), nil
		}
		country, err := s.q.GetCountryByID(ctx, *id)
		if errors.Is(err, pgx.ErrNoRows) {
			return status(0, "Country selected not found"), nil
		}
		if err != nil {
			return nil, err
		}
		if currency {
			cur := strings.TrimSpace(country.CurrencyCode)
			if err := s.setGroupFields(ctx, g, store.SetGroupFieldsParams{Currency: &cur}, nil); err != nil {
				return nil, err
			}
			return map[string]any{"status": 1, "message": "Successful", "currency_id": c.req.Str(key), "currency": cur}, nil
		}
		code := strings.TrimSpace(country.Code)
		if err := s.setGroupFields(ctx, g, store.SetGroupFieldsParams{CountryCode: &code}, nil); err != nil {
			return nil, err
		}
		return map[string]any{"status": 1, "message": "Successful", "country_id": c.req.Str(key), "country_name": country.Name}, nil
	}
}

// groupSettingFlags are the on/off settings update_group_settings sets;
// a missing flag is off, as in legacy.
var groupSettingFlags = []string{"enable_send_monthly_email_statements", "enable_bulk_transaction_alerts_reconciliation",
	"enable_member_information_privacy", "disable_arrears", "disable_ignore_contribution_transfers",
	"disable_member_directory", "disable_member_edit_profile", "enable_absolute_loan_recalculation"}

var memberOrderBy = map[string]bool{"users.first_name": true, "users.last_name": true, "members.created_on": true,
	"members.date_of_birth": true, "members.membership_number": true}

func (s *Server) updateGroupSettings(ctx context.Context, c *call, g *member) (any, error) {
	errs := map[string]string{}
	settings := map[string]any{}
	if v := c.req.Str("member_listing_order_by"); v != "" {
		if !memberOrderBy[v] {
			errs["member_listing_order_by"] = "Invalid member listing order"
		}
		settings["member_listing_order_by"] = v
	}
	if v := strings.ToUpper(c.req.Str("order_members_by")); v != "" {
		if v != "ASC" && v != "DESC" {
			errs["order_members_by"] = "Invalid order"
		}
		settings["order_members_by"] = v
	}
	for _, f := range groupSettingFlags {
		v := c.req.Str(f)
		settings[f] = boolInt(v != "" && v != "0")
	}
	if len(errs) > 0 {
		return map[string]any{"status": 0, "time": s.now().Unix(), "error": "Form validation failed", "validation_errors": errs}, nil
	}
	if err := s.setGroupFields(ctx, g, store.SetGroupFieldsParams{}, settings); err != nil {
		return nil, err
	}
	return status(1, "Changes saved successfully"), nil
}

// ---- Group roles ----

func (s *Server) saveGroupRole(edit bool) groupFn {
	return func(ctx context.Context, c *call, g *member) (any, error) {
		var id int64
		if edit {
			r, err := s.q.GetGroupRole(ctx, store.GetGroupRoleParams{GroupID: g.group.ID, ID: c.req.Int("id")})
			if errors.Is(err, pgx.ErrNoRows) {
				return status(0, "Could not find group role to edit"), nil
			}
			if err != nil {
				return nil, err
			}
			if !r.IsEditable {
				return status(0, "You are not allowed to edit this group role"), nil
			}
			id = r.ID
		}
		name, desc := c.req.Str("name"), c.req.Str("description")
		if name == "" {
			return timed(map[string]any{"status": 0, "message": "Form validation failed",
				"validation_errors": map[string]string{"name": "The Group Role Name field is required."}}, s.now()), nil
		}
		taken, err := s.q.GroupRoleNameTaken(ctx, store.GroupRoleNameTakenParams{GroupID: g.group.ID, Name: name, ExceptID: id})
		if err != nil {
			return nil, err
		}
		if taken {
			return timed(map[string]any{"status": 0, "message": "Form validation failed",
				"validation_errors": map[string]string{"name": "Another Group Role by the name `" + name + "` already exists"}}, s.now()), nil
		}
		if edit {
			if err := s.q.UpdateGroupRole(ctx, store.UpdateGroupRoleParams{GroupID: g.group.ID, ID: id, Name: name, Description: desc}); err != nil {
				return nil, err
			}
			return map[string]any{"status": 1, "success": name + " as a group role was successfully edited"}, nil
		}
		newID, err := s.q.CreateEditableGroupRole(ctx, store.CreateEditableGroupRoleParams{GroupID: g.group.ID, Name: name, Description: desc})
		if err != nil {
			return nil, err
		}
		return map[string]any{"status": 1, "success": name + " as a group role was successfully created", "id": newID}, nil
	}
}

func (s *Server) hideGroupRole(hide bool) groupFn {
	return func(ctx context.Context, c *call, g *member) (any, error) {
		r, err := s.q.GetGroupRole(ctx, store.GetGroupRoleParams{GroupID: g.group.ID, ID: c.req.Int("id")})
		if errors.Is(err, pgx.ErrNoRows) {
			return status(0, "Could not find group role to edit"), nil
		}
		if err != nil {
			return nil, err
		}
		switch {
		case !r.IsEditable:
			return status(0, "You are not allowed to edit this group role"), nil
		case hide && !r.IsActive:
			return status(0, "Group role already hidden"), nil
		case !hide && r.IsActive:
			return status(0, "Group role already active"), nil
		}
		if err := s.q.SetGroupRoleActive(ctx, store.SetGroupRoleActiveParams{GroupID: g.group.ID, ID: r.ID, IsActive: !hide}); err != nil {
			return nil, err
		}
		if hide {
			return status(1, "Group role successfully hidden"), nil
		}
		return status(1, "Group role successfully activated"), nil
	}
}

// unassignedRoles maps each active role (by name, or by id) to 0 when a
// member holds it and 1 when it is free.
func (s *Server) unassignedRoles(byID bool) groupFn {
	return func(ctx context.Context, c *call, g *member) (any, error) {
		rows, err := s.q.GroupRoleAssignments(ctx, g.group.ID)
		if err != nil {
			return nil, err
		}
		roles := object{}
		seen := map[string]bool{}
		for _, r := range rows {
			key := r.Name
			if byID {
				key = strconv.FormatInt(r.ID, 10)
			}
			if seen[key] {
				continue // PHP's += kept the first
			}
			seen[key] = true
			roles = append(roles, field{key, boolInt(!r.Assigned)})
		}
		return map[string]any{"status": 1, "message": "success", "time": s.now().Unix(), "roles_status": roles,
			"member_has_role": boolInt(g.m.GroupRoleID != nil)}, nil
	}
}

// ---- Members ----

func memberName(m store.GetGroupMemberUserRow) string {
	return strings.TrimSpace(m.FirstName + " " + m.LastName)
}

func (s *Server) suspendMember(suspend bool) groupFn {
	verb := "suspend"
	if !suspend {
		verb = "unsuspended"
	}
	return func(ctx context.Context, c *call, g *member) (any, error) {
		id := c.req.Int("member_id")
		if id <= 0 {
			return timed(status(0, "Kindly select member to "+verb), s.now()), nil
		}
		m, err := s.q.GetGroupMemberUser(ctx, store.GetGroupMemberUserParams{GroupID: g.group.ID, ID: id})
		if errors.Is(err, pgx.ErrNoRows) {
			return timed(status(0, "Member selected to "+verb+" not found"), s.now()), nil
		}
		if err != nil {
			return nil, err
		}
		st, msg := store.MembershipStatusActive, memberName(m)+" unsuspended successfully."
		if suspend {
			if m.ID == g.m.ID {
				return timed(status(0, "You can not suspend yourself"), s.now()), nil
			}
			st, msg = store.MembershipStatusSuspended, memberName(m)+" suspended successfully."
		}
		if err := s.q.SetMembershipStatus(ctx, store.SetMembershipStatusParams{GroupID: g.group.ID, ID: m.ID, Status: st}); err != nil {
			return nil, err
		}
		return timed(status(1, msg), s.now()), nil
	}
}

// deleteMember stays switched off, as the legacy app had it.
func (s *Server) deleteMember(ctx context.Context, c *call, g *member) (any, error) {
	return timed(status(1, "This action has been disabled"), s.now()), nil
}

func (s *Server) editGroupMember(ctx context.Context, c *call, g *member) (any, error) {
	m, err := s.q.GetGroupMemberUser(ctx, store.GetGroupMemberUserParams{GroupID: g.group.ID, ID: c.req.Int("member_id")})
	if errors.Is(err, pgx.ErrNoRows) {
		return status(0, "Member you are trying to edit is not available in the group"), nil
	}
	if err != nil {
		return nil, err
	}
	if m.UserID == g.user.ID {
		return status(0, "You can not edit your user profile. Go to settings > My profile"), nil
	}
	if m.LastLoginAt != nil {
		return status(0, "You can not edit an active member. Kindly communicate to the member to update their profile"), nil
	}
	or := func(v string, old *string) string {
		if v != "" {
			return v
		}
		return deref(old)
	}
	first, last := c.req.Str("first_name"), c.req.Str("last_name")
	if first == "" {
		first = m.FirstName
	}
	if last == "" {
		last = m.LastName
	}
	email := strings.ToLower(or(c.req.Str("email"), m.Email))
	var phone *string
	if p, err := auth.NormalizePhone(or(c.req.Str("phone"), m.Phone), s.auth.DefaultCallingCode()); err == nil {
		phone = &p
	}
	var emailPtr *string
	if validEmail(email) {
		emailPtr = &email
	}
	if first == "" || last == "" || (emailPtr == nil && phone == nil) {
		return status(0, "First name, last name and user identity are required"), nil
	}
	if phone != nil {
		if taken, err := s.phoneTaken(ctx, *phone, m.UserID); err != nil {
			return nil, err
		} else if taken {
			return status(0, "That phone number is used by another account"), nil
		}
	}
	if emailPtr != nil {
		if taken, err := s.emailTaken(ctx, email, m.UserID); err != nil {
			return nil, err
		} else if taken {
			return status(0, "That email address is used by another account"), nil
		}
	}
	if err := s.q.UpdateUserContact(ctx, store.UpdateUserContactParams{ID: m.UserID, FirstName: first, LastName: last,
		Email: emailPtr, Phone: phone}); err != nil {
		return nil, err
	}
	return status(1, "successful"), nil
}

// ---- Contributions ----

func (s *Server) hideContribution(hide bool) groupFn {
	return func(ctx context.Context, c *call, g *member) (any, error) {
		id := c.req.Int("id")
		if id <= 0 {
			return status(0, "Bad request: Missing contribution id"), nil
		}
		ct, err := s.q.GetContribution(ctx, store.GetContributionParams{GroupID: g.group.ID, ID: id})
		if errors.Is(err, pgx.ErrNoRows) {
			return status(0, "Group contribution not available"), nil
		}
		if err != nil {
			return nil, err
		}
		switch {
		case hide && ct.IsHidden:
			return status(0, "Sorry, the Contribution is already hidden"), nil
		case !hide && !ct.IsHidden:
			return status(0, "Sorry, the Contribution is not hidden"), nil
		}
		if err := s.q.SetContributionHidden(ctx, store.SetContributionHiddenParams{GroupID: g.group.ID, ID: id, IsHidden: hide}); err != nil {
			return nil, err
		}
		if hide {
			return status(1, ct.Name+" was successfully hidden"), nil
		}
		return status(1, ct.Name+" was successfully activated"), nil
	}
}

// deleteContribution checks the password when the app sends one (Sam: no
// password is needed for officials' actions), and refuses a contribution
// with money on it.
func (s *Server) deleteContribution(ctx context.Context, c *call, g *member) (any, error) {
	id := c.req.Int("id")
	if id <= 0 {
		return status(0, "Bad request: Missing contribution id"), nil
	}
	if _, err := s.q.GetContribution(ctx, store.GetContributionParams{GroupID: g.group.ID, ID: id}); errors.Is(err, pgx.ErrNoRows) {
		return status(0, "Group contribution not available"), nil
	} else if err != nil {
		return nil, err
	}
	if pw := c.req.Str("password"); pw != "" {
		err := s.auth.ConfirmPassword(ctx, g.user.ID, pw, clientInfo(c.r))
		var locked *auth.LockedOutError
		switch {
		case errors.Is(err, auth.ErrInvalidCredentials):
			return status(0, "You entered the wrong password."), nil
		case errors.As(err, &locked):
			return status(0, "Too many wrong passwords. Try again later."), nil
		case err != nil:
			return nil, err
		}
	}
	used, err := s.q.ContributionHasMoney(ctx, store.ContributionHasMoneyParams{GroupID: g.group.ID, ID: id})
	if err != nil {
		return nil, err
	}
	if used {
		return status(0, "The contribution has transactions associated to it, void all transactions associated to this account before deleting it"), nil
	}
	if err := s.q.DeleteContribution(ctx, store.DeleteContributionParams{GroupID: g.group.ID, ID: id}); err != nil {
		return nil, err
	}
	return status(1, "Contribution deleted successfully"), nil
}

// getGroupContributionsToPay is the open (active) contributions, in the
// get_group_contributions shape.
func (s *Server) getGroupContributionsToPay(ctx context.Context, c *call, g *member) (any, error) {
	resp, err := s.getGroupContributions(ctx, c, g)
	if err != nil {
		return nil, err
	}
	m := resp.(map[string]any)
	var open []any
	for _, x := range m["contributions"].([]any) {
		if x.(map[string]any)["active"] == 1 {
			open = append(open, x)
		}
	}
	if open == nil {
		open = []any{}
	}
	m["contributions"] = open
	return m, nil
}
