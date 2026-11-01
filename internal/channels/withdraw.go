package channels

import (
	"encoding/json"
	"strings"

	"github.com/swahome/legion-api/internal/auth"
	"github.com/swahome/legion-api/internal/books"
	"github.com/swahome/legion-api/internal/store"
)

// Withdrawal requests: an official asks, every signatory approves (here,
// on the web or in the app), then the money is paid. The rules are
// books.CreateRequest and books.DecideRequest, the same as everywhere.
// As in the legacy USSD menu, answering needs no password.

func (c *convo) officialOnly() (*member, error) {
	g, err := c.member()
	if err != nil {
		return nil, err
	}
	if !g.official() {
		return nil, say("Only the group's officials can do that.")
	}
	return g, nil
}

var withdrawKinds = map[string]string{
	"expense":             "Expense",
	"contribution_refund": "Contribution refund",
	"funds_transfer":      "Transfer between accounts",
}

func withdrawScreen(c *convo) (view, error) {
	if _, err := c.officialOnly(); err != nil {
		return view{}, err
	}
	return view{text: "What is the withdrawal for?", options: []option{
		to("Expense", "wd_expense", "wkind", "expense"),
		to("Contribution refund", "wd_member", "wkind", "contribution_refund"),
		to("Transfer between accounts", "wd_from", "wkind", "funds_transfer"),
	}}, nil
}

func withdrawExpenseScreen(c *convo) (view, error) {
	g, err := c.officialOnly()
	if err != nil {
		return view{}, err
	}
	cats, err := c.q.ListExpenseCategories(c.ctx, store.ListExpenseCategoriesParams{GroupID: g.GroupID})
	if err != nil {
		return view{}, err
	}
	if len(cats) == 0 {
		return view{text: "Your group has no expense categories yet. Add them on the web or the app."}, nil
	}
	v := view{text: "Choose the expense:"}
	for _, ct := range cats {
		v.options = append(v.options, to(ct.Name, "wd_from", "expense", id(ct.ID), "what", ct.Name))
	}
	return v, nil
}

func withdrawMemberScreen(c *convo) (view, error) {
	g, err := c.officialOnly()
	if err != nil {
		return view{}, err
	}
	ms, err := c.q.ListMembers(c.ctx, store.ListMembersParams{GroupID: g.GroupID,
		Status: store.NullMembershipStatus{MembershipStatus: store.MembershipStatusActive, Valid: true}})
	if err != nil {
		return view{}, err
	}
	v := view{text: "Refund which member?"}
	for _, m := range ms {
		name := strings.TrimSpace(m.FirstName + " " + m.LastName)
		phone := ""
		if m.Phone != nil {
			phone = *m.Phone
		}
		v.options = append(v.options, to(name, "wd_contrib", "member", id(m.ID), "member_name", name, "member_phone", phone))
	}
	return v, nil
}

func withdrawContributionScreen(c *convo) (view, error) {
	g, err := c.officialOnly()
	if err != nil {
		return view{}, err
	}
	cs, err := c.q.PayableContributions(c.ctx, g.GroupID)
	if err != nil {
		return view{}, err
	}
	if len(cs) == 0 {
		return view{text: "Your group has no contributions to refund."}, nil
	}
	v := view{text: "Refund " + c.v("member_name") + " from which contribution?"}
	for _, ct := range cs {
		v.options = append(v.options, to(ct.Name, "wd_from", "contribution", id(ct.ID), "what", ct.Name+" refund to "+c.v("member_name")))
	}
	return v, nil
}

// withdrawFromScreen picks the account the money leaves. A group with one
// open account skips it.
func withdrawFromScreen(c *convo) (view, error) {
	g, err := c.officialOnly()
	if err != nil {
		return view{}, err
	}
	accts, err := c.q.ListGroupAccounts(c.ctx, g.GroupID)
	if err != nil {
		return view{}, err
	}
	var open []store.ListGroupAccountsRow
	for _, a := range accts {
		if !a.IsClosed {
			open = append(open, a)
		}
	}
	next := "wd_amount"
	if c.v("wkind") == "funds_transfer" {
		next = "wd_to"
		if len(open) < 2 {
			return view{text: "Your group needs two accounts to move money between them."}, nil
		}
	}
	switch len(open) {
	case 0:
		return view{text: "Your group has no accounts yet. Add one on the web or the app."}, nil
	case 1:
		if err := c.replace(next, "from", id(open[0].ID), "from_name", open[0].Name, "from_balance", open[0].Balance); err != nil {
			return view{}, err
		}
		return view{}, errRedirect
	}
	v := view{text: "Pay from which account?"}
	for _, a := range open {
		v.options = append(v.options, to(a.Name+" ("+commas(a.Balance)+")", next, "from", id(a.ID), "from_name", a.Name, "from_balance", a.Balance))
	}
	return v, nil
}

func withdrawToScreen(c *convo) (view, error) {
	g, err := c.officialOnly()
	if err != nil {
		return view{}, err
	}
	accts, err := c.q.ListGroupAccounts(c.ctx, g.GroupID)
	if err != nil {
		return view{}, err
	}
	v := view{text: "Move the money to which account?"}
	for _, a := range accts {
		if a.IsClosed || id(a.ID) == c.v("from") {
			continue
		}
		v.options = append(v.options, to(a.Name, "wd_amount", "to", id(a.ID), "what", "Transfer to "+a.Name))
	}
	return v, nil
}

func withdrawAmountScreen(c *convo) (view, error) {
	if _, err := c.officialOnly(); err != nil {
		return view{}, err
	}
	text := c.v("from_name") + " has " + c.amount(c.v("from_balance")) + ". Enter the amount to withdraw:"
	return view{text: text, input: func(c *convo, s string) error {
		amt, ok := normAmount(s)
		if !ok {
			return say("Enter an amount in numbers, like 1500.")
		}
		if books.Cents(amt) > books.Cents(c.v("from_balance")) {
			return say("That is more than the " + c.amount(c.v("from_balance")) + " in " + c.v("from_name") + ".")
		}
		if c.v("wkind") == "funds_transfer" {
			return c.open("wd_confirm", "amount", amt)
		}
		return c.open("wd_phone", "amount", amt)
	}}, nil
}

func withdrawPhoneScreen(c *convo) (view, error) {
	if _, err := c.officialOnly(); err != nil {
		return view{}, err
	}
	v := view{text: "Send the money to which MPesa number?"}
	if p := c.v("member_phone"); p != "" {
		v.text = "Send the money to which MPesa number? Reply 1 for " + c.v("member_name") + "'s " + localPhone(p) + ", or type a number."
	}
	v.input = func(c *convo, s string) error {
		if s == "1" && c.v("member_phone") != "" {
			s = c.v("member_phone")
		}
		e164, err := auth.NormalizePhone(s, c.e.DefaultCC)
		if err != nil || len(e164) < 12 { // a plus and at least 11 digits
			return say("That is not a phone number. Type it like 0712345678.")
		}
		return c.open("wd_confirm", "phone", e164)
	}
	return v, nil
}

func withdrawConfirmScreen(c *convo) (view, error) {
	g, err := c.officialOnly()
	if err != nil {
		return view{}, err
	}
	text := "Request " + c.amount(c.v("amount")) + " from " + c.v("from_name") + " for " + c.v("what")
	if p := c.v("phone"); p != "" {
		text += ", paid to " + localPhone(p)
	}
	text += "?"
	return view{text: text, options: []option{
		opt("Yes, send for approval", func(c *convo) error {
			req := books.NewRequest{
				Kind: store.WithdrawalKind(c.v("wkind")), AccountID: c.vInt("from"), Amount: c.v("amount"),
				Description: c.v("what") + " (requested on " + channelName(c.format) + ")",
				RequestedBy: &g.MembershipID, OfficialsOnly: g.flag("restrict_wallet_approval_to_group_admins"),
			}
			switch req.Kind {
			case store.WithdrawalKindExpense:
				n := c.vInt("expense")
				req.ExpenseCategoryID = &n
			case store.WithdrawalKindContributionRefund:
				m, ct := c.vInt("member"), c.vInt("contribution")
				req.MemberID, req.ContributionID = &m, &ct
			case store.WithdrawalKindFundsTransfer:
				n := c.vInt("to")
				req.ToAccountID = &n
			}
			if req.Kind != store.WithdrawalKindFundsTransfer {
				req.Recipient = &books.Recipient{Type: "mobile", Phone: c.v("phone")}
			}
			var reqID int64
			if err := c.write(func(q *store.Queries) error {
				var err error
				reqID, err = books.CreateRequest(c.ctx, q, g.GroupID, c.userID(), req)
				return err
			}); err != nil {
				return err
			}
			ref := ""
			if x, err := c.q.GetWithdrawalRequestView(c.ctx, store.GetWithdrawalRequestViewParams{GroupID: g.GroupID, ID: reqID}); err == nil {
				ref = " Reference " + x.ReferenceNumber + "."
			}
			c.finish("Your request was sent to the signatories for approval." + ref)
			return nil
		}),
		opt("No, cancel", func(c *convo) error {
			c.finish("Withdrawal request cancelled.")
			return nil
		}),
	}}, nil
}

func channelName(f Format) string {
	if f == WhatsApp {
		return "WhatsApp"
	}
	return "USSD"
}

// ---- approving ----

func approvalsScreen(c *convo) (view, error) {
	g, err := c.member()
	if err != nil {
		return view{}, err
	}
	rows, err := c.q.ListWithdrawalRequests(c.ctx, store.ListWithdrawalRequestsParams{
		GroupID: g.GroupID, Awaiting: &g.MembershipID, PageSize: 50,
		Status: store.NullWithdrawalRequestStatus{WithdrawalRequestStatus: store.WithdrawalRequestStatusPending, Valid: true},
	})
	if err != nil {
		return view{}, err
	}
	if len(rows) == 0 {
		return view{text: "No withdrawal requests are waiting for you."}, nil
	}
	v := view{text: "Requests waiting for you:"}
	for _, r := range rows {
		v.options = append(v.options, to(commas(r.Amount)+" "+requestWhat(r), "approval", "request", id(r.ID)))
	}
	return v, nil
}

// requestWhat is a short "what for" for a request.
func requestWhat(r store.ListWithdrawalRequestsRow) string {
	switch {
	case r.ExpenseCategoryName != nil && *r.ExpenseCategoryName != "":
		return *r.ExpenseCategoryName
	case r.Kind == store.WithdrawalKindContributionRefund:
		return "refund to " + r.MemberName
	case r.Kind == store.WithdrawalKindFundsTransfer && r.ToAccountName != nil:
		return "transfer to " + *r.ToAccountName
	}
	if w, ok := withdrawKinds[string(r.Kind)]; ok {
		return w
	}
	return string(r.Kind)
}

func approvalScreen(c *convo) (view, error) {
	g, err := c.member()
	if err != nil {
		return view{}, err
	}
	x, err := c.q.GetWithdrawalRequestView(c.ctx, store.GetWithdrawalRequestViewParams{GroupID: g.GroupID, ID: c.vInt("request")})
	if notFound(err) {
		return view{}, books.ErrNotFound
	}
	if err != nil {
		return view{}, err
	}
	if x.Status != store.WithdrawalRequestStatusPending {
		return view{}, say("That request is already " + string(x.Status) + ".")
	}
	r := store.ListWithdrawalRequestsRow(x)
	text := c.amount(x.Amount) + " for " + requestWhat(r)
	var rc books.Recipient
	if json.Unmarshal(x.Recipient, &rc) == nil && rc.Phone != "" {
		text += " to " + localPhone(rc.Phone)
	}
	if x.RequestedByName != "" {
		text += ". Asked by " + x.RequestedByName
	}
	text += "."
	return view{text: text, options: []option{
		opt("Approve", func(c *convo) error {
			if err := c.write(func(q *store.Queries) error {
				return books.DecideRequest(c.ctx, q, g.GroupID, x.ID, g.MembershipID, c.userID(), store.ApprovalDecisionApproved, "")
			}); err != nil {
				return err
			}
			c.finish("You approved the request for " + c.amount(x.Amount) + ".")
			return nil
		}),
		to("Decline", "approval_no"),
	}}, nil
}

func approvalDeclineScreen(c *convo) (view, error) {
	g, err := c.member()
	if err != nil {
		return view{}, err
	}
	return view{text: "Why are you declining?", input: func(c *convo, s string) error {
		if err := c.write(func(q *store.Queries) error {
			return books.DecideRequest(c.ctx, q, g.GroupID, c.vInt("request"), g.MembershipID, c.userID(), store.ApprovalDecisionDeclined, s)
		}); err != nil {
			return err
		}
		c.finish("You declined the request. The other signatories will see your reason.")
		return nil
	}}, nil
}
