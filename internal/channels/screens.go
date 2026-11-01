package channels

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/swahome/legion-api/internal/books"
	"github.com/swahome/legion-api/internal/store"
)

// screens maps a screen name to the function that draws it. Filled in
// init so screens can name each other.
var screens map[string]func(c *convo) (view, error)

func init() {
	screens = map[string]func(c *convo) (view, error){
		"start": startScreen,
		"main":  mainScreen,

		"account":    accountScreen,
		"contrib":    contributionBalanceScreen,
		"fines":      fineBalanceScreen,
		"loans_owed": loansOwedScreen,
		"group_cash": groupCashScreen,
		"statement":  statementScreen,

		"pay":         payScreen,
		"pay_contrib": payContributionScreen,
		"pay_fine":    payFineScreen,
		"pay_loan":    payLoanScreen,
		"pay_amount":  payAmountScreen,
		"pay_confirm": payConfirmScreen,

		"wd":             withdrawScreen,
		"wd_expense":     withdrawExpenseScreen,
		"wd_member":      withdrawMemberScreen,
		"wd_contrib":     withdrawContributionScreen,
		"wd_from":        withdrawFromScreen,
		"wd_to":          withdrawToScreen,
		"wd_amount":      withdrawAmountScreen,
		"wd_phone":       withdrawPhoneScreen,
		"wd_confirm":     withdrawConfirmScreen,
		"approvals":      approvalsScreen,
		"approval":       approvalScreen,
		"approval_no":    approvalDeclineScreen,
		"loans":          loansScreen,
		"loan_types":     loanTypesScreen,
		"loan_amount":    loanAmountScreen,
		"loan_term":      loanTermScreen,
		"loan_confirm":   loanConfirmScreen,
		"loan_apps":      loanApplicationsScreen,
		"loan_waiting":   loanWaitingScreen,
		"loan_answer":    loanAnswerScreen,
		"loan_answer_no": loanDeclineScreen,
	}
}

// member is the caller's membership in the chosen group.
type member struct {
	store.ChannelUserGroupsRow
	settings map[string]any
}

// official is the legacy "is_admin || group_role_id" check: officials and
// role holders (signatories) can ask for withdrawals.
func (m *member) official() bool { return m.IsOfficial || m.GroupRoleID != nil }

// flag reads a legacy group setting such as
// restrict_wallet_approval_to_group_admins ("1" or 1).
func (m *member) flag(key string) bool {
	switch v := m.settings[key].(type) {
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

func (c *convo) groups() ([]store.ChannelUserGroupsRow, error) {
	if c.user == nil {
		return nil, nil
	}
	return c.q.ChannelUserGroups(c.ctx, c.user.ID)
}

// member loads the chosen group. If the caller has left it (or it was
// suspended) since choosing, they start again.
func (c *convo) member() (*member, error) {
	if c.g != nil {
		return c.g, nil
	}
	if c.user == nil {
		c.st = State{Screen: "start"}
		return nil, errRedirect
	}
	gs, err := c.groups()
	if err != nil {
		return nil, err
	}
	for _, g := range gs {
		if g.GroupID == c.st.GroupID {
			c.g = &member{ChannelUserGroupsRow: g}
			_ = json.Unmarshal(g.Settings, &c.g.settings)
			return c.g, nil
		}
	}
	c.st = State{Screen: "start"}
	return nil, errRedirect
}

func (c *convo) loc() *time.Location {
	if c.g != nil {
		if l, err := time.LoadLocation(c.g.Timezone); err == nil {
			return l
		}
	}
	return books.Nairobi()
}

func (c *convo) todayDate() time.Time {
	t := c.today.In(c.loc())
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func (c *convo) userID() int64 { return c.user.ID }

// ---- start and main menu ----

func startScreen(c *convo) (view, error) {
	if c.user == nil {
		msg := "This number is not registered on " + c.e.brand() + "."
		if c.e.WebURL != "" {
			msg += " Sign up at " + c.e.WebURL + " or on the app."
		}
		c.finish(msg)
		return view{}, nil
	}
	gs, err := c.groups()
	if err != nil {
		return view{}, err
	}
	switch len(gs) {
	case 0:
		c.finish("Hello " + c.user.FirstName + ". You are not an active member of any group yet. Use the web or the app to join one.")
		return view{}, nil
	case 1:
		c.choose(gs[0].GroupID)
		return view{}, errRedirect
	}
	v := view{text: "Welcome " + c.user.FirstName + ". Choose a group:"}
	for _, g := range gs {
		id := g.GroupID
		v.options = append(v.options, opt(g.Name, func(c *convo) error {
			c.choose(id)
			c.st.Back = []Frame{{Screen: "start"}}
			return nil
		}))
	}
	return v, nil
}

func (c *convo) choose(groupID int64) {
	c.st = State{Screen: "main", GroupID: groupID}
	c.g = nil
}

func mainScreen(c *convo) (view, error) {
	g, err := c.member()
	if err != nil {
		return view{}, err
	}
	v := view{text: g.Name + ". Choose:"}
	v.options = append(v.options, to("My account", "account"), to("Make a payment", "pay"))
	if g.official() {
		v.options = append(v.options, to("Request a withdrawal", "wd"), to("Approve withdrawals", "approvals"))
	}
	v.options = append(v.options, to("Loans", "loans"))
	if gs, err := c.groups(); err != nil {
		return view{}, err
	} else if len(gs) > 1 {
		v.options = append(v.options, opt("Switch group", func(c *convo) error {
			c.st = State{Screen: "start"}
			c.g = nil
			return nil
		}))
	}
	return v, nil
}

// ---- my account ----

func accountScreen(c *convo) (view, error) {
	if _, err := c.member(); err != nil {
		return view{}, err
	}
	return view{text: "My account:", options: []option{
		to("Contributions", "contrib"),
		to("Fines", "fines"),
		to("Loans", "loans_owed"),
		to("Group account balances", "group_cash"),
		to("My last payments", "statement"),
	}}, nil
}

func (c *convo) totals() (store.MobileMemberTotalsRow, error) {
	rows, err := c.q.MobileMemberTotals(c.ctx, store.MobileMemberTotalsParams{GroupID: c.g.GroupID, MembershipID: &c.g.MembershipID})
	if err != nil || len(rows) == 0 {
		return store.MobileMemberTotalsRow{ContributionsPaid: "0", ContributionArrears: "0", FinesPaid: "0", FineArrears: "0"}, err
	}
	return rows[0], nil
}

// balanceText is the legacy arrears line: owed, overpaid or clear.
func (c *convo) balanceText(arrears, what string) (string, bool) {
	switch n := books.Cents(arrears); {
	case n > 0:
		return "You owe: " + c.amount(arrears), true
	case n < 0:
		return "Overpaid: " + c.amount(books.Format(arrears)[1:]), false
	}
	return "No " + what + " owed.", false
}

func contributionBalanceScreen(c *convo) (view, error) {
	if _, err := c.member(); err != nil {
		return view{}, err
	}
	t, err := c.totals()
	if err != nil {
		return view{}, err
	}
	owed, _ := c.balanceText(t.ContributionArrears, "contributions")
	v := view{text: "Contributions paid: " + c.amount(t.ContributionsPaid) + "\n" + owed}
	v.options = []option{to("Pay a contribution", "pay_contrib")}
	return v, nil
}

func fineBalanceScreen(c *convo) (view, error) {
	if _, err := c.member(); err != nil {
		return view{}, err
	}
	t, err := c.totals()
	if err != nil {
		return view{}, err
	}
	owed, due := c.balanceText(t.FineArrears, "fines")
	v := view{text: "Fines paid: " + c.amount(t.FinesPaid) + "\n" + owed}
	if due {
		v.options = []option{to("Pay a fine", "pay_fine")}
	}
	return v, nil
}

func loansOwedScreen(c *convo) (view, error) {
	g, err := c.member()
	if err != nil {
		return view{}, err
	}
	loans, err := c.q.MemberOpenLoans(c.ctx, store.MemberOpenLoansParams{GroupID: g.GroupID, MembershipID: &g.MembershipID})
	if err != nil {
		return view{}, err
	}
	if len(loans) == 0 {
		return view{text: "You have no loans to repay."}, nil
	}
	var total int64
	text := "Your loans:"
	for _, l := range loans {
		total += books.Cents(l.Balance)
		text += "\n" + l.Name + ": " + c.amount(l.Balance)
	}
	if len(loans) > 1 {
		text += "\nTotal: " + c.amount(centsText(total))
	}
	return view{text: text, options: []option{to("Repay a loan", "pay_loan")}}, nil
}

// groupCashScreen is the legacy "Group account balances": cash at bank
// (banks and saccos), cash at hand (mobile money and petty cash), total.
func groupCashScreen(c *convo) (view, error) {
	g, err := c.member()
	if err != nil {
		return view{}, err
	}
	accts, err := c.q.ListGroupAccounts(c.ctx, g.GroupID)
	if err != nil {
		return view{}, err
	}
	var bank, hand int64
	for _, a := range accts {
		if a.IsClosed {
			continue
		}
		switch a.Kind {
		case store.AccountKindBank, store.AccountKindSacco:
			bank += books.Cents(a.Balance)
		default:
			hand += books.Cents(a.Balance)
		}
	}
	return view{text: "Group account balances\nCash at bank: " + c.amount(centsText(bank)) +
		"\nCash at hand: " + c.amount(centsText(hand)) + "\nTotal: " + c.amount(centsText(bank+hand))}, nil
}

var depositKindNames = map[store.DepositKind]string{
	store.DepositKindContribution: "Contribution", store.DepositKindFine: "Fine", store.DepositKindMisc: "Other payment",
	store.DepositKindLoanRepayment: "Loan repayment", store.DepositKindIncome: "Income",
}

func statementScreen(c *convo) (view, error) {
	g, err := c.member()
	if err != nil {
		return view{}, err
	}
	rows, err := c.q.MemberRecentDeposits(c.ctx, store.MemberRecentDepositsParams{GroupID: g.GroupID, MembershipID: &g.MembershipID, Limit: 5})
	if err != nil {
		return view{}, err
	}
	if len(rows) == 0 {
		return view{text: "You have no payments recorded yet."}, nil
	}
	text := "Your last payments:"
	for _, r := range rows {
		name := r.Name
		if name == "" {
			name = depositKindNames[r.Kind]
		}
		text += "\n" + r.DepositDate.Format("02 Jan 06") + " " + name + " " + commas(r.Amount)
	}
	return view{text: text}, nil
}

// ---- helpers ----

func centsText(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := fmt.Sprintf("%d.%02d", n/100, n%100)
	if neg {
		s = "-" + s
	}
	return s
}

func id(n int64) string { return strconv.FormatInt(n, 10) }

func notFound(err error) bool { return errors.Is(err, pgx.ErrNoRows) }
