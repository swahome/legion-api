package mobile

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/swahome/legion-api/internal/account"
	"github.com/swahome/legion-api/internal/auth"
	"github.com/swahome/legion-api/internal/store"
)

// Milestone 5: sign-up, password reset, creating a group and inviting
// members from the app. The app's codes are 4 digits.
const appCodeDigits = 4

// registerUser is register_user: sign-up after verify_pin proved an
// identity nobody uses yet (the pin row holds unique_code).
func (s *Server) registerUser(ctx context.Context, c *call) (any, error) {
	if reply := s.required(c, [2]string{"first_name", "First Name"}, [2]string{"last_name", "Last Name"},
		[2]string{"identity", "Phone Number or Email"}, [2]string{"password", "Password"}); reply != nil {
		return reply, nil
	}
	pw := c.req.Str("password")
	if len(pw) < account.MinPasswordLen {
		return s.formFailed(map[string]string{"password": "The Password field must be at least 8 characters in length."}), nil
	}
	id, err := auth.ParseIdentity(c.req.Str("identity"), s.auth.DefaultCallingCode())
	if err != nil {
		return s.formFailed(map[string]string{"identity": "Enter a valid Email or Phone Number"}), nil
	}
	p, err := s.q.GetMobilePin(ctx, id.Key())
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (p.VerifiedAt == nil || p.UniqueCode != c.req.Str("unique_code") || p.ExpiresAt.Before(s.now()))) {
		return timed(status(9, "Authentication failed. Generate a new pin"), s.now()), nil
	}
	if err != nil {
		return nil, err
	}
	hash, err := account.HashPassword(pw)
	if err != nil {
		return nil, err
	}
	var user store.User
	exists := false
	err = s.inTx(ctx, func(q *store.Queries) error {
		if _, err := s.auth.LookupIdentity(ctx, id); err == nil {
			exists = true
			return nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		p := store.CreateSignupUserParams{FirstName: strings.TrimSpace(c.req.Str("first_name")),
			LastName: strings.TrimSpace(c.req.Str("last_name")), PasswordHash: &hash, IsValidated: true}
		if id.Phone != "" {
			p.Phone = &id.Phone
		} else {
			p.Email = &id.Email
		}
		var err error
		if user, err = q.CreateSignupUser(ctx, p); err != nil {
			return err
		}
		if doc := c.req.Str("document_number"); doc != "" {
			if err := q.SetUserIDNumber(ctx, store.SetUserIDNumberParams{ID: user.ID, IDNumber: &doc}); err != nil {
				return err
			}
		}
		return q.DeleteMobilePin(ctx, id.Key())
	})
	if err != nil {
		return nil, err
	}
	if exists {
		return timed(status(0, "User already exists. Proceed to next step"), s.now()), nil
	}
	resp, err := s.signIn(ctx, user)
	if err != nil {
		return nil, err
	}
	resp["message"] = "Registration request successful"
	u := resp["user"].(map[string]any)
	u["username"] = ""
	u["is_validated"] = 1
	delete(resp, "user_is_validated")
	return resp, nil
}

// appIdentity reads the phone or email the reset screens send.
func (s *Server) appIdentity(c *call) (auth.Identity, string, bool) {
	raw := c.req.Str("phone")
	if raw == "" || raw == "0" {
		raw = c.req.Str("email")
	}
	if raw == "" {
		raw = c.req.Str("identity")
	}
	id, err := auth.ParseIdentity(raw, s.auth.DefaultCallingCode())
	return id, raw, err == nil
}

// forgotPassword is forgot_password: texts a reset code.
func (s *Server) forgotPassword(ctx context.Context, c *call) (any, error) {
	id, raw, ok := s.appIdentity(c)
	if !ok {
		return timed(status(0, "Enter a valid email address or phone number"), s.now()), nil
	}
	err := s.inTx(ctx, func(q *store.Queries) error {
		u, err := s.auth.LookupIdentity(ctx, id)
		if err != nil {
			return err
		}
		if !u.IsActive {
			return pgx.ErrNoRows
		}
		return s.acct.SendCode(ctx, q, u, account.ResetPassword, appCodeDigits)
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return timed(status(4, raw+" is not registered. Kindly counter check"), s.now()), nil
	case errors.Is(err, account.ErrTooManyCodes), errors.Is(err, account.ErrNoPhone):
		return timed(status(0, "Error occured sending reset password code. Try again"), s.now()), nil
	case err != nil:
		return nil, err
	}
	s.smsKick()
	return timed(status(1, "Reset password code sent"), s.now()), nil
}

// validateForgotPasswordCode is validate_forgot_password_code: the code
// becomes a secret_key for reset_password.
func (s *Server) validateForgotPasswordCode(ctx context.Context, c *call) (any, error) {
	id, raw, ok := s.appIdentity(c)
	if !ok {
		return timed(status(0, "Enter a valid email address or phone number"), s.now()), nil
	}
	u, err := s.auth.LookupIdentity(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return timed(status(4, raw+" is not registered. Kindly counter check"), s.now()), nil
	}
	if err != nil {
		return nil, err
	}
	var key string
	err = s.codeTx(ctx, func(q *store.Queries) error {
		var err error
		key, err = s.acct.ExchangeResetCode(ctx, q, u.ID, c.req.Str("pin"))
		return err
	})
	switch {
	case errors.Is(err, account.ErrCodeInvalid):
		return timed(status(0, "Invalid pin submitted. Kindly retry"), s.now()), nil
	case errors.Is(err, account.ErrCodeExpired):
		return timed(status(0, "Reset pin has already expired. Kindly click resend code to start again"), s.now()), nil
	case err != nil:
		return nil, err
	}
	resp := timed(status(1, "Proceed to reset password"), s.now())
	resp["secret_key"] = key
	return resp, nil
}

// codeTx commits even when fn fails on a wrong code, so the try counts.
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

// resetPassword is reset_password: sets the password with the secret_key
// and signs the user in.
func (s *Server) resetPassword(ctx context.Context, c *call) (any, error) {
	id, _, ok := s.appIdentity(c)
	if !ok {
		return status(0, "Enter a valid email address or phone number"), nil
	}
	errs := map[string]string{}
	pw := c.req.Str("password")
	switch {
	case c.req.Str("secret_key") == "":
		errs["secret_key"] = "The Secret Key field is required."
	case pw == "":
		errs["password"] = "The Password field is required."
	case len(pw) < account.MinPasswordLen:
		errs["password"] = "The Password field must be at least 8 characters in length."
	case c.req.Str("confirm_password") != pw:
		errs["confirm_password"] = "The Confirm Password field does not match the Password field."
	}
	if len(errs) > 0 {
		return s.formFailed(errs), nil
	}
	var user store.User
	err := s.inTx(ctx, func(q *store.Queries) error {
		uid, err := s.acct.UseResetToken(ctx, q, c.req.Str("secret_key"))
		if err != nil {
			return err
		}
		u, err := q.GetUserByID(ctx, uid)
		if err != nil {
			return err
		}
		if (id.Phone != "" && deref(u.Phone) != id.Phone) || (id.Email != "" && !strings.EqualFold(deref(u.Email), id.Email)) {
			return account.ErrCodeExpired
		}
		if err := s.acct.SetPassword(ctx, q, u, pw, true); err != nil {
			return err
		}
		if err := q.SetUserValidated(ctx, u.ID); err != nil {
			return err
		}
		user, err = q.GetUserByID(ctx, u.ID)
		return err
	})
	if errors.Is(err, account.ErrCodeExpired) {
		return timed(status(9, "Password reset expired. Try again"), s.now()), nil
	}
	if err != nil {
		return nil, err
	}
	s.smsKick()
	resp, err := s.signIn(ctx, user)
	if err != nil {
		return nil, err
	}
	resp["message"] = "Password successfully changed"
	return resp, nil
}

// createGroup is create_group: a new group in setup with the caller as its
// first official. country_id is a country's id from the app's picker.
func (s *Server) createGroup(ctx context.Context, c *call) (any, error) {
	if reply := s.required(c, [2]string{"group_name", "Group Name"}); reply != nil {
		return reply, nil
	}
	user, err := s.q.GetUserByID(ctx, c.userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return timed(status(4, "User account not found"), s.now()), nil
	}
	if err != nil {
		return nil, err
	}
	in := account.GroupInput{Name: c.req.Str("group_name")}
	if cid := c.req.Int("country_id"); cid > 0 {
		country, err := s.q.GetCountry(ctx, cid)
		if errors.Is(err, pgx.ErrNoRows) {
			return s.formFailed(map[string]string{"country_id": "The Country field must contain a valid country."}), nil
		}
		if err != nil {
			return nil, err
		}
		in.CountryCode = country.Code
	}
	var g store.Group
	err = s.inTx(ctx, func(q *store.Queries) error {
		var err error
		g, _, err = account.CreateGroup(ctx, q, user, in)
		return err
	})
	if errors.Is(err, account.ErrGroupName) {
		return s.formFailed(map[string]string{"group_name": "The Group Name field is required."}), nil
	}
	if err != nil {
		return nil, err
	}
	checkin, err := s.checkinData(ctx, user.ID, g.ID)
	if err != nil {
		return nil, err
	}
	for _, grp := range checkin["user_groups"].([]any) {
		grp.(map[string]any)["is_initiator"] = 1
	}
	resp := timed(status(1, g.Name+" successfully registered. Kindly proceed to Group Setup"), s.now())
	resp["user"] = userPayload(user)
	for k, v := range checkin {
		resp[k] = v
	}
	return resp, nil
}

// completeGroupSetup is complete_group_setup: the group goes live and the
// members added during setup get their invites.
func (s *Server) completeGroupSetup(ctx context.Context, c *call, g *member) (any, error) {
	err := s.inTx(ctx, func(q *store.Queries) error {
		if code := c.req.Str("referral_code"); code != "" {
			if err := q.SetGroupSetting(ctx, store.SetGroupSettingParams{ID: g.group.ID, Key: "referral_code", Value: code}); err != nil {
				return err
			}
		}
		if g.group.Status != store.GroupStatusSetup {
			return nil
		}
		_, err := s.acct.CompleteSetup(ctx, q, g.group.ID, g.user)
		return err
	})
	if err != nil && !errors.Is(err, account.ErrGroupNotInSetup) {
		return nil, err
	}
	s.smsKick()
	return status(1, "Success. Group profile successfully set. Proceed to Dashboard"), nil
}

// ---- Members ----

// addGroupMembers is setup_tasks/add_group_members: members as an array
// of {first_name, last_name, phone, email, group_role_id}. People added
// while the group is in setup are texted when setup is complete; in a live
// group they are texted straight away.
func (s *Server) addGroupMembers(ctx context.Context, c *call, g *member) (any, error) {
	var people []request
	for _, v := range c.req {
		if list, ok := v.([]any); ok {
			for _, x := range list {
				if m, ok := x.(map[string]any); ok {
					people = append(people, request(m))
				}
			}
		}
	}
	for _, p := range people {
		if p.Str("first_name") == "" {
			return s.formFailed(map[string]string{"usernames": "Ensure all usernames are valid"}), nil
		}
	}
	ok, failed := 0, 0
	fails := map[string]string{}
	for i, p := range people {
		key := fmt.Sprint(i)
		phone, perr := auth.NormalizePhone(p.Str("phone"), s.auth.DefaultCallingCode())
		var email *string
		if e := strings.ToLower(p.Str("email")); strings.Contains(e, "@") {
			email = &e
		}
		if perr != nil {
			failed++
			fails[key] = "user does not have a valid email address or phone number"
			continue
		}
		var role *int64
		if r := appID(p.Str("group_role_id")); r != nil {
			if in, err := s.q.GroupRoleInGroup(ctx, store.GroupRoleInGroupParams{GroupID: g.group.ID, ID: *r}); err != nil {
				return nil, err
			} else if in {
				role = r
			}
		}
		err := s.inTx(ctx, func(q *store.Queries) error {
			mid, err := account.AddMember(ctx, q, g.group.ID, p.Str("first_name"), p.Str("last_name"), phone, email, role, false)
			if err != nil {
				return err
			}
			if g.group.Status != store.GroupStatusActive {
				return nil
			}
			if m, err := q.GetMember(ctx, store.GetMemberParams{GroupID: g.group.ID, ID: mid}); err != nil || m.UserID == g.user.ID {
				return err
			}
			_, err = s.acct.Invite(ctx, q, g.group.ID, mid, g.user)
			return err
		})
		switch {
		case errors.Is(err, account.ErrAlreadyMember):
			failed++
			fails[key] = phone + " is already a member of the group"
		case errors.Is(err, account.ErrEmailTaken):
			failed++
			fails[key] = "That email address belongs to someone with a different phone number"
		case err != nil:
			return nil, err
		default:
			ok++
		}
	}
	s.smsKick()
	switch {
	case ok > 0 && failed > 0:
		resp := timed(status(1, fmt.Sprintf("%d member(s) successfully invited but %d failed", ok, failed)), s.now())
		resp["fails"] = fails
		return resp, nil
	case ok > 0:
		return timed(status(1, fmt.Sprintf("%d member(s) successfully invited", ok)), s.now()), nil
	}
	resp := timed(status(1, "No member was invited"), s.now())
	resp["fails"] = fails
	return resp, nil
}

// accepted is the legacy invitation_accepted: the member has signed in.
func accepted(u store.User) bool { return u.LastLoginAt != nil }

func (s *Server) invitedMember(ctx context.Context, c *call, g *member) (store.GetMemberRow, store.User, map[string]any, error) {
	mid := appID(c.req.Str("member_id"))
	if mid == nil {
		return store.GetMemberRow{}, store.User{}, status(0, "Invalid request: Missing member_id"), nil
	}
	m, err := s.q.GetMember(ctx, store.GetMemberParams{GroupID: g.group.ID, ID: *mid})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && m.Status == store.MembershipStatusRemoved) {
		return m, store.User{}, status(0, "Member invitation failed: Could not find invited member profile"), nil
	}
	if err != nil {
		return m, store.User{}, nil, err
	}
	u, err := s.q.GetUserByID(ctx, m.UserID)
	return m, u, nil, err
}

// resendGroupInvitation is setup_tasks/resend_group_invitation.
func (s *Server) resendGroupInvitation(ctx context.Context, c *call, g *member) (any, error) {
	m, u, reply, err := s.invitedMember(ctx, c, g)
	if err != nil || reply != nil {
		return reply, err
	}
	if accepted(u) {
		return status(0, "Member invitation failed: Member has already accepted group invitation"), nil
	}
	return s.sendInvite(ctx, g, m.ID, "success", "Member invitation failed: Something went wrong when sending the group invitation")
}

// sendInvitation is members/send_invitation.
func (s *Server) sendInvitation(ctx context.Context, c *call, g *member) (any, error) {
	mid := appID(c.req.Str("member_id"))
	if mid == nil {
		return status(0, "Member you are trying to edit is not available in the group"), nil
	}
	m, err := s.q.GetMember(ctx, store.GetMemberParams{GroupID: g.group.ID, ID: *mid})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && m.Status != store.MembershipStatusActive) {
		return status(0, "Member you are trying to edit is not available in the group"), nil
	}
	if err != nil {
		return nil, err
	}
	return s.sendInvite(ctx, g, m.ID, "Invitation sent successfully", "Could not send user invitation")
}

func (s *Server) sendInvite(ctx context.Context, g *member, mid int64, okMsg, failMsg string) (any, error) {
	var sent bool
	err := s.inTx(ctx, func(q *store.Queries) error {
		var err error
		sent, err = s.acct.Invite(ctx, q, g.group.ID, mid, g.user)
		return err
	})
	if err != nil {
		return nil, err
	}
	if !sent {
		return status(0, failMsg), nil
	}
	s.smsKick()
	return status(1, okMsg), nil
}

// cancelGroupInvitation is setup_tasks/cancel_group_invitation: removes a
// member who has not signed in yet.
func (s *Server) cancelGroupInvitation(ctx context.Context, c *call, g *member) (any, error) {
	m, u, reply, err := s.invitedMember(ctx, c, g)
	if err != nil || reply != nil {
		return reply, err
	}
	if accepted(u) {
		return status(0, "Member invitation cancelation failed: Member has already accepted group invitation"), nil
	}
	if m.UserID == g.user.ID {
		return status(0, "Member invitation cancelation failed: You cannot remove yourself"), nil
	}
	err = s.q.UpdateMembership(ctx, store.UpdateMembershipParams{GroupID: g.group.ID, ID: m.ID, ClearRole: true,
		Status: store.NullMembershipStatus{MembershipStatus: store.MembershipStatusRemoved, Valid: true}})
	if err != nil {
		return nil, err
	}
	return status(1, "Member invitation successfully canceled"), nil
}

// reassignMemberGroupRole is setup_tasks/reassign_member_group_role, only
// while the group is in setup (a live group changes signatories through
// its settings).
func (s *Server) reassignMemberGroupRole(ctx context.Context, c *call, g *member) (any, error) {
	if g.group.Status != store.GroupStatusSetup {
		return timed(status(0, "Member update failed. Group already active. Go to settings and initiate member signatory change request"), s.now()), nil
	}
	m, _, reply, err := s.invitedMember(ctx, c, g)
	if err != nil || reply != nil {
		return reply, err
	}
	p := store.UpdateMembershipParams{GroupID: g.group.ID, ID: m.ID}
	if role := appID(c.req.Str("group_role_id")); role != nil {
		in, err := s.q.GroupRoleInGroup(ctx, store.GroupRoleInGroupParams{GroupID: g.group.ID, ID: *role})
		if err != nil {
			return nil, err
		}
		if !in {
			return timed(status(0, "Member update failed. Could not reasign role"), s.now()), nil
		}
		holder, err := s.q.RoleHolder(ctx, store.RoleHolderParams{GroupID: g.group.ID, GroupRoleID: role})
		if err == nil && holder != m.ID {
			return timed(status(0, "Member update failed. Role already in use"), s.now()), nil
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		p.GroupRoleID = role
	} else {
		p.ClearRole = true
	}
	if err := s.q.UpdateMembership(ctx, p); err != nil {
		return nil, err
	}
	return timed(status(1, "Group member role successfully reassigned"), s.now()), nil
}

// setupTasksIndex is setup_tasks/index, which the legacy app left empty.
func (s *Server) setupTasksIndex(ctx context.Context, c *call) (any, error) {
	return map[string]any{}, nil
}
