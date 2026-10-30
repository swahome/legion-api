package legacy

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/swahome/legion-api/internal/auth"
)

// DumpTables are the legacy tables the import reads.
var DumpTables = []string{
	"users_groups",
	"users", "investment_groups", "group_roles", "members",
	"contributions", "regular_contribution_settings", "one_time_contribution_settings",
	"contribution_member_pairings", "contribution_fine_settings", "statements",
	"member_contributions_fines_balances",
}

// Options control how legacy values are mapped.
type Options struct {
	// SyntheticPhones replaces every user's phone with +2547000NNNNN built
	// from the user id. Use it for anonymized dumps whose phones are
	// placeholders; never for a production export.
	SyntheticPhones bool
	// DefaultCallingCode normalises phones that have no calling code.
	DefaultCallingCode string
}

// Plan is the transformed data, ready to load. Each slice holds rows in
// the column order given by the matching *Cols variable.
type Plan struct {
	Users, PlatformRoles, Groups, GroupRoles, Memberships            [][]any
	Contributions, Schedules, ContributionMembers, FineRules, Ledger [][]any

	Report Report
}

// Report counts what was imported and every row that was dropped or changed.
type Report struct {
	Imported map[string]int
	Issues   map[string]int // "table: reason" -> rows
}

func (r *Report) issue(format string, args ...any) {
	r.Issues[fmt.Sprintf(format, args...)]++
}

var nairobi = mustLoadLocation("Africa/Nairobi")

func mustLoadLocation(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		// Kenya has been UTC+3 with no DST since 1960.
		return time.FixedZone("EAT", 3*3600)
	}
	return loc
}

// epoch converts a legacy unix-seconds value; 0, blank and junk give nil.
func epoch(s string) *time.Time {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n <= 0 {
		return nil
	}
	t := time.Unix(n, 0).UTC()
	return &t
}

// epochDate is the Nairobi calendar date of a legacy timestamp.
func epochDate(s string) *time.Time {
	t := epoch(s)
	if t == nil {
		return nil
	}
	y, m, d := t.In(nairobi).Date()
	day := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	return &day
}

func timeOr(t *time.Time, fallback time.Time) time.Time {
	if t != nil {
		return *t
	}
	return fallback
}

// clean strips NUL bytes, which Postgres text cannot hold, and trims space.
func clean(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(s, "\x00", ""))
}

func cleanPtr(p *string) *string {
	if p == nil {
		return nil
	}
	s := clean(*p)
	if s == "" {
		return nil
	}
	return &s
}

// extras collects the non-empty columns not in skip into a JSON object.
func extras(r Row, skip map[string]bool) []byte {
	m := map[string]string{}
	for k, v := range r {
		if skip[k] || v == nil {
			continue
		}
		if s := clean(*v); s != "" {
			m[k] = s
		}
	}
	b, _ := json.Marshal(m)
	return b
}

func set(cols ...string) map[string]bool {
	m := map[string]bool{}
	for _, c := range cols {
		m[c] = true
	}
	return m
}

// byID sorts rows by their legacy id so output is deterministic.
func byID(rows []Row) []Row {
	out := append([]Row(nil), rows...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Int("id") < out[j].Int("id") })
	return out
}

// Transform maps the legacy rows to the new schema. roles is the
// users_groups table, which may come from a separate export.
func Transform(t Tables, roles []Row, opt Options, now time.Time) (*Plan, error) {
	p := &Plan{Report: Report{Imported: map[string]int{}, Issues: map[string]int{}}}
	if opt.DefaultCallingCode == "" {
		opt.DefaultCallingCode = "254"
	}

	users := p.users(t["users"], opt, now)
	p.platformRoles(roles, users)
	groups := p.groups(t["investment_groups"], users, now)
	roleGroup := p.groupRoles(t["group_roles"], groups, now)
	members := p.memberships(t["members"], users, groups, roleGroup, now)
	contribs := p.contributions(t["contributions"], groups, now)
	p.schedules(t, contribs, now)
	p.contributionMembers(t["contribution_member_pairings"], contribs, members, now)
	p.fineRules(t["contribution_fine_settings"], contribs, now)
	p.ledger(t["statements"], groups, members, contribs, now)

	for name, rows := range map[string][][]any{
		"users": p.Users, "user_platform_roles": p.PlatformRoles, "groups": p.Groups,
		"group_roles": p.GroupRoles, "memberships": p.Memberships, "contributions": p.Contributions,
		"contribution_schedules": p.Schedules, "contribution_members": p.ContributionMembers,
		"contribution_fine_rules": p.FineRules, "ledger_entries": p.Ledger,
	} {
		p.Report.Imported[name] = len(rows)
	}
	return p, nil
}

var UserCols = []string{"id", "legacy_id", "first_name", "middle_name", "last_name", "email", "phone",
	"id_number", "password_hash", "password_reset_required", "is_active", "last_login_at", "created_at", "updated_at"}

func (p *Plan) users(rows []Row, opt Options, now time.Time) map[int64]bool {
	ids := map[int64]bool{}
	phones := map[string]bool{}
	emails := map[string]bool{}
	for _, r := range byID(rows) {
		id := r.Int("id")
		if id <= 0 {
			p.Report.issue("users: missing id")
			continue
		}
		var phone *string
		if opt.SyntheticPhones {
			s := fmt.Sprintf("+2547%08d", id)
			phone = &s
		} else if raw := clean(r.Str("phone")); raw != "" {
			cc := strings.TrimPrefix(clean(r.Str("calling_code")), "+")
			if cc == "" || len(cc) < 2 {
				cc = opt.DefaultCallingCode
			}
			num := raw
			if !strings.HasPrefix(num, "+") && !strings.HasPrefix(num, cc) {
				num = "+" + cc + strings.TrimLeft(num, "0")
			}
			if e164, err := auth.NormalizePhone(num, opt.DefaultCallingCode); err == nil {
				phone = &e164
			} else {
				p.Report.issue("users: phone not normalisable, cleared")
			}
		}
		if phone != nil && phones[*phone] {
			p.Report.issue("users: duplicate phone, cleared on later user")
			phone = nil
		}
		email := cleanPtr(r.Ptr("email"))
		if email != nil {
			low := strings.ToLower(*email)
			if emails[low] {
				p.Report.issue("users: duplicate email, cleared on later user")
				email = nil
			} else {
				emails[low] = true
			}
		}
		if phone == nil && email == nil {
			p.Report.issue("users: no phone or email, skipped")
			continue
		}
		if phone != nil {
			phones[*phone] = true
		}

		hash := cleanPtr(r.Ptr("password"))
		reset := false
		if hash == nil || !strings.HasPrefix(*hash, "$2") {
			if hash != nil {
				p.Report.issue("users: non-bcrypt password, reset required")
			} else {
				p.Report.issue("users: no password, reset required")
			}
			hash, reset = nil, true
		}
		if !r.Flag("active") {
			p.Report.issue("users: inactive")
		}
		created := timeOr(epoch(r.Str("created_on")), now)
		p.Users = append(p.Users, []any{
			id, id, clean(r.Str("first_name")), clean(r.Str("middle_name")), clean(r.Str("last_name")),
			email, phone, cleanPtr(r.Ptr("id_number")), hash, reset, r.Flag("active"),
			epoch(r.Str("last_login")), created, timeOr(epoch(r.Str("modified_on")), created),
		})
		ids[id] = true
	}
	return ids
}

var PlatformRoleCols = []string{"user_id", "role"}

// Legacy Ion Auth group ids. 2 ("member") is implicit and not stored.
var legacyPlatformRoles = map[int64]string{
	1: "admin", 3: "group_account_manager", 4: "banker", 5: "partner", 6: "sales",
	7: "diocese_admin", 8: "deanery_admin", 9: "parish_admin", 10: "sub_parish_admin",
}

func (p *Plan) platformRoles(rows []Row, users map[int64]bool) {
	seen := map[string]bool{}
	for _, r := range byID(rows) {
		uid, gid := r.Int("user_id"), r.Int("group_id")
		if gid == 2 {
			continue
		}
		role, ok := legacyPlatformRoles[gid]
		if !ok {
			p.Report.issue("user_platform_roles: unknown role id %d, skipped", gid)
			continue
		}
		if !users[uid] {
			p.Report.issue("user_platform_roles: user missing, skipped")
			continue
		}
		key := fmt.Sprint(uid, role)
		if seen[key] {
			continue
		}
		seen[key] = true
		p.PlatformRoles = append(p.PlatformRoles, []any{uid, role})
	}
}

var GroupCols = []string{"id", "legacy_id", "name", "slug", "email", "phone", "address",
	"status", "owner_user_id", "settings", "created_at", "updated_at"}

var groupColumnsUsed = set("id", "name", "slug", "email", "phone", "address", "active", "owner",
	"created_on", "modified_on", "created_by", "modified_by", "old_id")

func (p *Plan) groups(rows []Row, users map[int64]bool, now time.Time) map[int64]bool {
	ids := map[int64]bool{}
	slugs := map[string]bool{}
	for _, r := range byID(rows) {
		id := r.Int("id")
		name := clean(r.Str("name"))
		if name == "" {
			name = fmt.Sprintf("Group %d", id)
			p.Report.issue("groups: blank name, defaulted")
		}
		slug := strings.ToLower(clean(r.Str("slug")))
		if slug == "" || slugs[slug] {
			slug = fmt.Sprintf("group-%d", id)
			p.Report.issue("groups: missing or duplicate slug, regenerated")
		}
		slugs[slug] = true
		status := "active"
		if !r.Flag("active") {
			status = "suspended"
		}
		var owner *int64
		if o := r.Int("owner"); o > 0 {
			if users[o] {
				owner = &o
			} else {
				p.Report.issue("groups: owner user missing, cleared")
			}
		}
		created := timeOr(epoch(r.Str("created_on")), now)
		p.Groups = append(p.Groups, []any{
			id, id, name, slug, cleanPtr(r.Ptr("email")), cleanPtr(r.Ptr("phone")), cleanPtr(r.Ptr("address")),
			status, owner, extras(r, groupColumnsUsed), created, timeOr(epoch(r.Str("modified_on")), created),
		})
		ids[id] = true
	}
	return ids
}

var GroupRoleCols = []string{"id", "legacy_id", "group_id", "name", "description", "is_editable", "is_active", "created_at", "updated_at"}

// groupRoles returns role id -> group id, for the composite membership FK.
func (p *Plan) groupRoles(rows []Row, groups map[int64]bool, now time.Time) map[int64]int64 {
	out := map[int64]int64{}
	for _, r := range byID(rows) {
		id, gid := r.Int("id"), r.Int("group_id")
		if !groups[gid] {
			p.Report.issue("group_roles: group missing, skipped")
			continue
		}
		name := clean(r.Str("name"))
		if name == "" {
			name = "Role"
		}
		created := timeOr(epoch(r.Str("created_on")), now)
		p.GroupRoles = append(p.GroupRoles, []any{
			id, id, gid, name, clean(r.Str("description")),
			r[`is_editable`] == nil || r.Flag("is_editable"),
			r["active"] == nil || r.Flag("active"),
			created, timeOr(epoch(r.Str("modified_on")), created),
		})
		out[id] = gid
	}
	return out
}

var MembershipCols = []string{"id", "legacy_id", "group_id", "user_id", "is_official", "group_role_id",
	"membership_number", "date_of_birth", "physical_address", "postal_address", "status",
	"joined_at", "updated_at", "profile"}

var memberColumnsUsed = set("id", "user_id", "group_id", "is_admin", "active", "created_by", "created_on",
	"modified_by", "modified_on", "group_role_id", "membership_number", "date_of_birth",
	"physical_address", "postal_address", "is_deleted", "old_id")

// memberships returns member id -> group id.
func (p *Plan) memberships(rows []Row, users, groups map[int64]bool, roleGroup map[int64]int64, now time.Time) map[int64]int64 {
	// One membership per (group, user): prefer an active, non-deleted row,
	// then the most recent.
	best := map[[2]int64]Row{}
	rank := func(r Row) int {
		if r.Flag("is_deleted") || !r.Flag("active") {
			return 0
		}
		return 1
	}
	for _, r := range byID(rows) {
		uid, gid := r.Int("user_id"), r.Int("group_id")
		if !users[uid] {
			p.Report.issue("memberships: user missing, skipped")
			continue
		}
		if !groups[gid] {
			p.Report.issue("memberships: group missing, skipped")
			continue
		}
		k := [2]int64{gid, uid}
		if cur, ok := best[k]; ok {
			p.Report.issue("memberships: duplicate user in group, kept one")
			if rank(r) < rank(cur) {
				continue
			}
		}
		best[k] = r
	}
	kept := make([]Row, 0, len(best))
	for _, r := range best {
		kept = append(kept, r)
	}
	out := map[int64]int64{}
	for _, r := range byID(kept) {
		id, gid := r.Int("id"), r.Int("group_id")
		var role *int64
		if rid := r.Int("group_role_id"); rid > 0 {
			if roleGroup[rid] == gid {
				role = &rid
			} else {
				p.Report.issue("memberships: role missing or from another group, cleared")
			}
		}
		status := "active"
		switch {
		case r.Flag("is_deleted"):
			status = "removed"
		case !r.Flag("active"):
			status = "suspended"
		}
		joined := timeOr(epoch(r.Str("created_on")), now)
		p.Memberships = append(p.Memberships, []any{
			id, id, gid, r.Int("user_id"), r.Flag("is_admin"), role,
			cleanPtr(r.Ptr("membership_number")), epochDate(r.Str("date_of_birth")),
			cleanPtr(r.Ptr("physical_address")), cleanPtr(r.Ptr("postal_address")), status,
			joined, timeOr(epoch(r.Str("modified_on")), joined), extras(r, memberColumnsUsed),
		})
		out[id] = gid
	}
	return out
}

var ContributionCols = []string{"id", "legacy_id", "group_id", "name", "kind", "category", "amount",
	"is_active", "is_hidden", "is_closed", "is_non_refundable", "is_equity", "hidden_in_statement",
	"show_arrears_cumulatively", "applies_to_all_members", "deleted_at", "created_at", "updated_at"}

var contributionKinds = map[int64]string{1: "regular", 2: "one_time", 3: "non_scheduled"}
var contributionCategories = map[int64]string{
	1: "share_capital", 2: "savings", 3: "welfare", 4: "membership_fee", 5: "investment", 6: "other",
}

type contribution struct {
	groupID int64
	kind    string
	row     Row
}

func (p *Plan) contributions(rows []Row, groups map[int64]bool, now time.Time) map[int64]*contribution {
	out := map[int64]*contribution{}
	for _, r := range byID(rows) {
		id, gid := r.Int("id"), r.Int("group_id")
		if !groups[gid] {
			p.Report.issue("contributions: group missing, skipped")
			continue
		}
		kind, ok := contributionKinds[r.Int("type")]
		if !ok {
			p.Report.issue("contributions: unknown type, treated as non_scheduled")
			kind = "non_scheduled"
		}
		var category *string
		if c, ok := contributionCategories[r.Int("category")]; ok {
			category = &c
		}
		amount := clean(r.Str("amount"))
		if _, err := strconv.ParseFloat(amount, 64); err != nil || strings.HasPrefix(amount, "-") {
			amount = "0"
		}
		created := timeOr(epoch(r.Str("created_on")), now)
		updated := timeOr(epoch(r.Str("modified_on")), created)
		var deleted *time.Time
		if r.Flag("is_deleted") {
			deleted = &updated
		}
		name := clean(r.Str("name"))
		if name == "" {
			name = fmt.Sprintf("Contribution %d", id)
		}
		// applies_to_all_members is filled in by schedules().
		p.Contributions = append(p.Contributions, []any{
			id, id, gid, name, kind, category, amount,
			r.Flag("active"), r.Flag("is_hidden"), r.Flag("is_closed"), r.Flag("is_non_refundable"),
			r.Flag("is_equity"), r.Flag("hidden_in_statement"), r.Flag("display_contribution_arrears_cumulatively"),
			true, deleted, created, updated,
		})
		out[id] = &contribution{groupID: gid, kind: kind, row: r}
	}
	return out
}

var ScheduleCols = []string{"contribution_id", "frequency", "rule", "invoice_date", "contribution_date",
	"invoice_days_before", "last_invoiced_on", "invoicing_active", "sms_notifications",
	"email_notifications", "sms_template", "disable_overpayments", "fines_enabled", "updated_at"}

var frequencies = map[int64]string{
	1: "monthly", 2: "bimonthly", 3: "quarterly", 4: "biannually", 5: "annually",
	6: "weekly", 7: "fortnightly", 8: "daily", 9: "twice_monthly",
}

var scheduleRuleColumns = []string{
	"month_day_monthly", "week_day_monthly", "week_day_weekly", "week_day_fortnight",
	"week_number_fortnight", "month_day_multiple", "week_day_multiple", "start_month_multiple",
	"after_first_starting_day", "after_second_starting_day", "after_second_contribution_date",
	"after_first_contribution_day_option", "after_first_day_week_multiple",
	"after_second_day_week_multiple", "after_second_contribution_day_option",
}

func (p *Plan) schedules(t Tables, contribs map[int64]*contribution, now time.Time) {
	pick := func(table string) map[int64]Row {
		out := map[int64]Row{}
		for _, r := range byID(t[table]) {
			cid := r.Int("contribution_id")
			if contribs[cid] == nil {
				p.Report.issue("%s: contribution missing, skipped", table)
				continue
			}
			if r.Flag("is_deleted") {
				continue
			}
			if _, dup := out[cid]; dup {
				p.Report.issue("%s: several per contribution, kept latest", table)
			}
			out[cid] = r
		}
		return out
	}
	regular, oneTime := pick("regular_contribution_settings"), pick("one_time_contribution_settings")

	memberList := map[int64]bool{}
	for i, row := range p.Contributions {
		cid := row[0].(int64)
		c := contribs[cid]
		var s Row
		var invoicingActive bool
		var frequency *string
		switch c.kind {
		case "regular":
			s, invoicingActive = regular[cid], c.row.Flag("regular_invoicing_active")
			if s != nil {
				if f, ok := frequencies[s.Int("contribution_frequency")]; ok {
					frequency = &f
				} else {
					p.Report.issue("contribution_schedules: regular with no frequency")
				}
			}
		case "one_time":
			s, invoicingActive = oneTime[cid], c.row.Flag("one_time_invoicing_active")
		}
		if s == nil {
			continue
		}
		rule := map[string]string{}
		for _, col := range scheduleRuleColumns {
			if v := cleanPtr(s.Ptr(col)); v != nil {
				rule[col] = *v
			}
		}
		ruleJSON, _ := json.Marshal(rule)
		var daysBefore *int64
		if s["invoice_days"] != nil {
			d := s.Int("invoice_days")
			daysBefore = &d
		}
		p.Schedules = append(p.Schedules, []any{
			cid, frequency, ruleJSON, epochDate(s.Str("invoice_date")), epochDate(s.Str("contribution_date")),
			daysBefore, epochDate(s.Str("last_invoice_date")), invoicingActive,
			s.Flag("sms_notifications_enabled"), s.Flag("email_notifications_enabled"),
			cleanPtr(s.Ptr("sms_template")), s.Flag("disable_overpayments"), s.Flag("enable_fines"),
			timeOr(epoch(s.Str("modified_on")), now),
		})
		if s.Flag("enable_contribution_member_list") {
			memberList[cid] = true
			p.Contributions[i][14] = false // applies_to_all_members
		}
	}
}

var ContributionMemberCols = []string{"contribution_id", "membership_id", "created_at"}

func (p *Plan) contributionMembers(rows []Row, contribs map[int64]*contribution, members map[int64]int64, now time.Time) {
	seen := map[[2]int64]bool{}
	for _, r := range byID(rows) {
		cid, mid := r.Int("contribution_id"), r.Int("member_id")
		if r.Flag("is_deleted") {
			continue
		}
		c := contribs[cid]
		if c == nil {
			p.Report.issue("contribution_members: contribution missing, skipped")
			continue
		}
		if gid, ok := members[mid]; !ok || gid != c.groupID {
			p.Report.issue("contribution_members: member missing or in another group, skipped")
			continue
		}
		k := [2]int64{cid, mid}
		if seen[k] {
			continue
		}
		seen[k] = true
		p.ContributionMembers = append(p.ContributionMembers, []any{cid, mid, timeOr(epoch(r.Str("created_on")), now)})
	}
}

var FineRuleCols = []string{"id", "legacy_id", "contribution_id", "kind", "fixed_amount", "percentage_rate",
	"percentage_of", "chargeable_on", "mode", "frequency", "fine_limit", "sms_notifications",
	"email_notifications", "is_active", "created_at"}

func (p *Plan) fineRules(rows []Row, contribs map[int64]*contribution, now time.Time) {
	for _, r := range byID(rows) {
		id, cid := r.Int("id"), r.Int("contribution_id")
		if contribs[cid] == nil {
			p.Report.issue("contribution_fine_rules: contribution missing, skipped")
			continue
		}
		var kind string
		var fixed, rate *string
		var chargeable, mode, freq *string
		switch r.Int("fine_type") {
		case 1:
			kind = "fixed"
			fixed = cleanPtr(r.Ptr("fixed_amount"))
			chargeable, mode, freq = cleanPtr(r.Ptr("fixed_fine_chargeable_on")), cleanPtr(r.Ptr("fixed_fine_mode")), cleanPtr(r.Ptr("fixed_fine_frequency"))
			if fixed == nil {
				p.Report.issue("contribution_fine_rules: fixed fine without amount, skipped")
				continue
			}
		case 2:
			kind = "percentage"
			rate = cleanPtr(r.Ptr("percentage_rate"))
			chargeable, mode, freq = cleanPtr(r.Ptr("percentage_fine_chargeable_on")), cleanPtr(r.Ptr("percentage_fine_mode")), cleanPtr(r.Ptr("percentage_fine_frequency"))
			if rate == nil {
				p.Report.issue("contribution_fine_rules: percentage fine without rate, skipped")
				continue
			}
			if _, err := strconv.ParseFloat(*rate, 64); err != nil {
				p.Report.issue("contribution_fine_rules: unparseable rate, skipped")
				continue
			}
		default:
			p.Report.issue("contribution_fine_rules: unknown fine type, skipped")
			continue
		}
		ch := ""
		if chargeable != nil {
			ch = *chargeable
		}
		var limit *int64
		if r["fine_limit"] != nil {
			l := r.Int("fine_limit")
			limit = &l
		}
		p.FineRules = append(p.FineRules, []any{
			id, id, cid, kind, fixed, rate, cleanPtr(r.Ptr("percentage_fine_on")), ch, mode, freq, limit,
			r.Flag("fine_sms_notifications_enabled"), r.Flag("fine_email_notifications_enabled"),
			r.Flag("active") && !r.Flag("is_deleted"), timeOr(epoch(r.Str("created_on")), now),
		})
	}
}

var LedgerCols = []string{"id", "legacy_id", "legacy_type", "group_id", "membership_id", "contribution_id",
	"entry_type", "amount", "entry_date", "due_date", "description", "voided_at", "created_at", "is_backdated"}

// LedgerTypes maps statements.transaction_type to ledger_entry_type.
// See Transactions::$statement_transaction_names in the legacy code.
var LedgerTypes = map[int64]string{
	1: "contribution_invoice", 2: "contribution_fine_invoice", 3: "fine_invoice", 4: "misc_invoice",
	9: "contribution_payment", 10: "contribution_payment", 11: "contribution_payment", 15: "contribution_payment",
	12: "fine_payment", 13: "fine_payment", 14: "fine_payment", 16: "fine_payment",
	17: "misc_payment", 18: "misc_payment", 19: "misc_payment", 20: "misc_payment",
	21: "contribution_refund", 22: "contribution_refund", 23: "contribution_refund", 24: "contribution_refund",
	25: "contribution_transfer_out", 26: "contribution_transfer_in", 27: "contribution_transfer_out",
	28: "contribution_to_fine_transfer", 29: "loan_to_contribution_transfer",
	30: "contribution_to_loan_transfer", 31: "loan_to_fine_transfer",
}

func (p *Plan) ledger(rows []Row, groups map[int64]bool, members map[int64]int64, contribs map[int64]*contribution, now time.Time) {
	for _, r := range byID(rows) {
		id, gid := r.Int("id"), r.Int("group_id")
		typ := r.Int("transaction_type")
		entryType, ok := LedgerTypes[typ]
		if !ok {
			p.Report.issue("ledger_entries: unknown transaction_type %d, skipped", typ)
			continue
		}
		if !groups[gid] {
			p.Report.issue("ledger_entries: group missing, skipped")
			continue
		}
		var member *int64
		if mid := r.Int("member_id"); mid > 0 {
			if members[mid] == gid {
				member = &mid
			} else {
				p.Report.issue("ledger_entries: member missing or in another group, kept without member")
			}
		}
		var contrib *int64
		if cid := r.Int("contribution_id"); cid > 0 {
			if c := contribs[cid]; c != nil && c.groupID == gid {
				contrib = &cid
			} else {
				p.Report.issue("ledger_entries: contribution missing or in another group, kept without contribution")
			}
		}
		amount := clean(r.Str("amount"))
		if f, err := strconv.ParseFloat(amount, 64); err != nil || f < 0 {
			p.Report.issue("ledger_entries: bad amount, skipped")
			continue
		}
		date := epochDate(r.Str("transaction_date"))
		if date == nil {
			date = epochDate(r.Str("created_on"))
		}
		if date == nil {
			p.Report.issue("ledger_entries: no date, skipped")
			continue
		}
		due := epochDate(r.Str("contribution_invoice_due_date"))
		if due == nil {
			due = epochDate(r.Str("fine_invoice_due_date"))
		}
		created := timeOr(epoch(r.Str("created_on")), now)
		var voided *time.Time
		if !r.Flag("active") {
			v := timeOr(epoch(r.Str("modified_on")), created)
			voided = &v
		}
		p.Ledger = append(p.Ledger, []any{
			id, id, typ, gid, member, contrib, entryType, amount, *date, due,
			clean(r.Str("description")), voided, created, r.Flag("is_a_back_dating_record"),
		})
	}
}
