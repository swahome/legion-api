package mobile

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/swahome/legion-api/internal/store"
)

// groupFields are the legacy investment_groups columns the check-in data
// returned (Groups_m::current_user_groups). Most live in groups.settings
// under their legacy names, as strings.
var groupFields = []string{
	"sms_balance", "account_number", "theme", "online_banking_enabled",
	"enable_member_information_privacy", "member_listing_order_by", "order_members_by",
	"enable_send_monthly_email_statements", "enable_bulk_transaction_alerts_reconciliation",
	"disable_arrears", "enable_two_factor_auth", "disable_ignore_contribution_transfers",
	"disable_member_directory", "disable_member_edit_profile", "enable_absolute_loan_recalculation",
	"referral_code", "is_collective", "group_offer_loans", "disable_e_wallet_on_mobile",
	"subscription_status", "trial_days", "billing_package_id", "billing_cycle",
	"enable_hide_group_balances_to_members",
}

// checkinData is the legacy _get_checkin_data: the user's groups with their
// role and activation details, or only the given groups. Invitations come
// with sign-up (milestone 5).
func (s *Server) checkinData(ctx context.Context, userID int64, only ...int64) (map[string]any, error) {
	rows, err := s.q.MobileUserGroups(ctx, userID)
	if err != nil {
		return nil, err
	}
	if len(only) > 0 {
		rows = slices.DeleteFunc(rows, func(g store.MobileUserGroupsRow) bool { return !slices.Contains(only, g.ID) })
	}
	ids := make([]int64, len(rows))
	for i, g := range rows {
		ids[i] = g.ID
	}
	roles, err := s.q.GroupRoleNames(ctx, ids)
	if err != nil {
		return nil, err
	}
	groups := make([]any, 0, len(rows))
	for _, g := range rows {
		groups = append(groups, groupPayload(g, roles))
	}
	return map[string]any{"user_groups": groups, "invited_user_groups": []any{}}, nil
}

func groupPayload(g store.MobileUserGroupsRow, roles []store.GroupRoleNamesRow) map[string]any {
	var settings map[string]any
	_ = json.Unmarshal(g.Settings, &settings)
	get := func(k string) any { return settings[k] } // nil when unset, as MySQL NULL

	validated := g.Status == store.GroupStatusActive
	out := map[string]any{
		"id":           fmt.Sprint(g.ID),
		"member_id":    fmt.Sprint(g.MemberID),
		"name":         g.Name,
		"slug":         g.Slug,
		"size":         get("size"),
		"is_validated": fmt.Sprint(boolInt(validated)),
		"avatar":       get("avatar"),
		"country_id":   nil,
		"country_name": g.CountryName,
		"phone":        g.Phone,
		"email":        g.Email,
	}
	if out["size"] == nil {
		out["size"] = fmt.Sprint(g.Size)
	}
	if g.CountryID != nil {
		out["country_id"] = fmt.Sprint(*g.CountryID)
	}
	for _, k := range groupFields {
		out[k] = get(k)
	}

	// group_roles is {"<id>": "<name>"}, ordered by name; PHP encoded an
	// empty list as [].
	var roleNames object
	role, roleID := "Member", any(nil)
	if g.GroupRoleID != nil {
		roleID = fmt.Sprint(*g.GroupRoleID)
	}
	for _, r := range roles {
		if r.GroupID == g.ID {
			roleNames = append(roleNames, field{fmt.Sprint(r.ID), r.Name})
			if g.GroupRoleID != nil && r.ID == *g.GroupRoleID {
				role = r.Name
			}
		}
	}
	var groupRoles any = roleNames
	if len(roleNames) == 0 {
		groupRoles = []any{}
	}
	activation, colour := "Pending", "#FF0000"
	if validated {
		activation, colour = "Active", "#00FF00"
	}
	out["group_roles"] = groupRoles
	out["activation_status"] = activation
	out["activation_status_color_code"] = colour
	out["group_currency"] = g.Currency
	out["role"] = role
	out["group_role_id"] = roleID
	out["is_on_free_plan"] = 0
	out["is_admin"] = boolInt(g.IsOfficial || g.GroupRoleID != nil)
	return out
}

// object is a JSON object that keeps its key order, like a PHP array.
type object []field

type field struct {
	key string
	val any
}

func (o object) MarshalJSON() ([]byte, error) {
	buf := []byte{'{'}
	for i, f := range o {
		if i > 0 {
			buf = append(buf, ',')
		}
		k, err := marshal(f.key)
		if err != nil {
			return nil, err
		}
		v, err := marshal(f.val)
		if err != nil {
			return nil, err
		}
		buf = append(append(append(buf, k...), ':'), v...)
	}
	return append(buf, '}'), nil
}
