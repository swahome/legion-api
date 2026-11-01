package account

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/swahome/legion-api/internal/sms"
	"github.com/swahome/legion-api/internal/store"
)

// GroupInput is what sign-up asks about a new group.
type GroupInput struct {
	Name        string
	CountryCode string // ISO alpha-2; empty takes the default country
	Currency    string // empty takes the country's currency
	Email       string
	Phone       string // E.164 or empty
	Size        int    // members expected (legacy size)
	Type        string // a key of GroupTypes
	// CountryKnown skips looking the country up: it is the group's own.
	CountryKnown bool
}

// GroupTypes are the legacy type_of_groups, keyed as stored in
// groups.settings.group_type (the legacy number) with their names.
var GroupTypes = map[string]string{
	"1": "Investment club", "2": "Chama", "3": "Sacco", "4": "Merry-go-round", "5": "Welfare Group",
	"6": "Table Banking Group", "7": "Self Help Group", "8": "Digital Lender",
	"9": "Community Based Organization", "10": "Youth group", "11": "Women group",
}

// The defaults a new group gets, as the legacy create_group_default_data.
var (
	DefaultRoles = []string{"Chair Person", "Treasurer", "Secretary", "Organizing Secretary"}

	defaultPettyCash = "Cash at Hand"

	defaultExpenseCategories = []string{
		"Stationery", "Bank Charges", "Withholding Tax", "Company Registration", "Legal Fees", "Wages",
		"Salaries", "Donation", "Miscellaneous Expenses", "Subscription Fees", "Office Expenses",
		"Insurance Fees", "Excise Duty", "Transport", "Courier", "Search Fee", "Food and Drinks",
		"Brokerage Fees", "Operating Expenses", "Benevolence", "Company Seal", "Telephone & Postages",
		"Corporate Social Responsibility",
	}
	defaultIncomeCategories = []string{"Rent", "Interest", "Sales", "Miscellaneous Income", "Dividends", "Lease"}
	defaultFineCategories   = []string{
		"Absent without apology", "Lateness to attend meeting", "Rude behavior", "Phone calls during meetings",
		"Absent with apology", "Absconding duty", "Miscellaneous fine",
	}
)

var (
	ErrGroupName      = errors.New("group name is required")
	ErrUnknownCountry = errors.New("unknown country")
	ErrBadCurrency    = errors.New("currency must be a 3-letter code")
	ErrGroupType      = errors.New("unknown group type")
)

var (
	reNonSlug  = regexp.MustCompile(`[^a-z0-9]+`)
	reCurrency = regexp.MustCompile(`^[A-Z]{3}$`)
)

// CheckGroupInput normalises and checks a new group's details. It returns
// the country code and currency to store.
func CheckGroupInput(ctx context.Context, q *store.Queries, in *GroupInput) error {
	in.Name = strings.Join(strings.Fields(in.Name), " ")
	if in.Name == "" || len(in.Name) > 120 {
		return ErrGroupName
	}
	in.Email = strings.ToLower(strings.TrimSpace(in.Email))
	var c store.Country
	var err error
	if in.CountryKnown {
		if in.Currency == "" {
			return ErrBadCurrency
		}
	} else if code := strings.ToUpper(strings.TrimSpace(in.CountryCode)); code != "" {
		c, err = q.GetActiveCountryByCode(ctx, code)
	} else {
		c, err = q.GetDefaultCountry(ctx)
	}
	switch {
	case in.CountryKnown:
	case errors.Is(err, pgx.ErrNoRows) && in.CountryCode == "":
		in.CountryCode, c.CurrencyCode = "KE", "KES" // no reference data loaded
	case errors.Is(err, pgx.ErrNoRows):
		return ErrUnknownCountry
	case err != nil:
		return err
	default:
		in.CountryCode = c.Code
	}
	in.Currency = strings.ToUpper(strings.TrimSpace(in.Currency))
	if in.Currency == "" {
		in.Currency = c.CurrencyCode
	}
	if !reCurrency.MatchString(in.Currency) {
		return ErrBadCurrency
	}
	if in.Type != "" {
		if _, ok := GroupTypes[in.Type]; !ok {
			return ErrGroupType
		}
	}
	if in.Size < 0 {
		in.Size = 0
	}
	return nil
}

// CreateGroup creates a group in setup with the user as its first official
// (Chair Person), and the default roles, petty cash account and income,
// expense and fine categories the legacy app gave every new group.
func CreateGroup(ctx context.Context, q *store.Queries, u store.User, in GroupInput) (store.Group, int64, error) {
	if err := CheckGroupInput(ctx, q, &in); err != nil {
		return store.Group{}, 0, err
	}
	slug, err := freeSlug(ctx, q, in.Name)
	if err != nil {
		return store.Group{}, 0, err
	}
	settings := map[string]any{}
	if in.Size > 0 {
		settings["size"] = fmt.Sprint(in.Size)
	}
	if in.Type != "" {
		settings["group_type"] = in.Type
	}
	rawSettings, _ := json.Marshal(settings)
	g, err := q.CreateSignupGroup(ctx, store.CreateSignupGroupParams{
		Name: in.Name, Slug: slug, Email: nonEmpty(in.Email), Phone: nonEmpty(in.Phone),
		CountryCode: in.CountryCode, Currency: in.Currency, OwnerUserID: &u.ID, Settings: rawSettings,
	})
	if err != nil {
		return g, 0, err
	}
	var chair int64
	for i, name := range DefaultRoles {
		id, err := q.CreateGroupRole(ctx, store.CreateGroupRoleParams{GroupID: g.ID, Name: name, IsEditable: false})
		if err != nil {
			return g, 0, err
		}
		if i == 0 {
			chair = id
		}
	}
	mid, err := q.CreateMembership(ctx, store.CreateMembershipParams{GroupID: g.ID, UserID: u.ID, IsOfficial: true, GroupRoleID: &chair})
	if err != nil {
		return g, 0, err
	}
	if _, err := q.CreatePettyCashAccount(ctx, store.CreatePettyCashAccountParams{GroupID: g.ID, Name: defaultPettyCash}); err != nil {
		return g, 0, err
	}
	for _, n := range defaultExpenseCategories {
		if _, err := q.CreateExpenseCategory(ctx, store.CreateExpenseCategoryParams{GroupID: g.ID, Name: n}); err != nil {
			return g, 0, err
		}
	}
	for _, n := range defaultIncomeCategories {
		if _, err := q.CreateIncomeCategory(ctx, store.CreateIncomeCategoryParams{GroupID: g.ID, Name: n}); err != nil {
			return g, 0, err
		}
	}
	var zero pgtype.Numeric
	_ = zero.Scan("0")
	for _, n := range defaultFineCategories {
		if _, err := q.CreateFineCategory(ctx, store.CreateFineCategoryParams{GroupID: g.ID, Name: n, Amount: zero}); err != nil {
			return g, 0, err
		}
	}
	return g, mid, nil
}

func freeSlug(ctx context.Context, q *store.Queries, name string) (string, error) {
	base := strings.Trim(reNonSlug.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if base == "" {
		base = "group"
	}
	for i := 1; ; i++ {
		slug := base
		if i > 1 {
			slug = fmt.Sprintf("%s-%d", base, i)
		}
		taken, err := q.SlugTaken(ctx, slug)
		if err != nil || !taken {
			return slug, err
		}
	}
}

// ErrGroupNotInSetup is returned when finishing a group that is live.
var ErrGroupNotInSetup = errors.New("group setup is already complete")

// CompleteSetup makes a setup group live and invites everyone added during
// setup except the official finishing it. It returns how many invites went
// out.
func (s *Service) CompleteSetup(ctx context.Context, q *store.Queries, groupID int64, by store.User) (int, error) {
	n, err := q.CompleteGroupSetup(ctx, groupID)
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, ErrGroupNotInSetup
	}
	if err := q.MarkMemberInvitedByUser(ctx, store.MarkMemberInvitedByUserParams{GroupID: groupID, UserID: by.ID}); err != nil {
		return 0, err
	}
	rows, err := q.MembersToInvite(ctx, store.MembersToInviteParams{GroupID: groupID, ExceptUser: by.ID})
	if err != nil {
		return 0, err
	}
	sent := 0
	for _, m := range rows {
		ok, err := s.Invite(ctx, q, groupID, m.ID, by)
		if err != nil {
			return sent, err
		}
		if ok {
			sent++
		}
	}
	return sent, nil
}

// Invite texts a member that they were added to the group. Someone who
// has never set a password gets a link to set one. It reports false when
// the member has no phone number to text.
func (s *Service) Invite(ctx context.Context, q *store.Queries, groupID, membershipID int64, by store.User) (bool, error) {
	m, err := q.GetMember(ctx, store.GetMemberParams{GroupID: groupID, ID: membershipID})
	if err != nil {
		return false, err
	}
	if err := q.MarkMemberInvited(ctx, membershipID); err != nil {
		return false, err
	}
	if m.Phone == nil {
		return false, nil
	}
	u, err := q.GetUserByID(ctx, m.UserID)
	if err != nil {
		return false, err
	}
	g, err := q.GetGroup(ctx, groupID)
	if err != nil {
		return false, err
	}
	app := AppName(ctx, q)
	inviter := strings.TrimSpace(titleCase(by.FirstName + " " + by.LastName))
	if inviter == "" {
		inviter = "your group"
	}
	var link string
	web := strings.TrimRight(s.WebURL, "/")
	needsPassword := u.PasswordHash == nil || u.PasswordResetRequired
	switch {
	case needsPassword && web != "":
		if err := q.ExpireMembershipInvites(ctx, membershipID); err != nil {
			return false, err
		}
		tok := RandomToken()
		if err := q.CreateInvite(ctx, store.CreateInviteParams{
			TokenHash: TokenHash(tok), UserID: u.ID, GroupID: groupID, MembershipID: membershipID,
			InvitedBy: &by.ID, ExpiresAt: s.now().Add(inviteTTL),
		}); err != nil {
			return false, err
		}
		link = " Set your password at " + web + "/join/" + tok + " ."
	case web != "":
		link = " Log in at " + web + " ."
	}
	body := fmt.Sprintf("Dear %s, you have been invited to %s by %s.%s %s group", greet(u), app, inviter, link, g.Name)
	_, err = sms.Queue(ctx, q, sms.Message{To: *m.Phone, Body: body, Kind: "invite", UserID: &u.ID, GroupID: &groupID})
	return err == nil, err
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
