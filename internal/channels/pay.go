package channels

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/swahome/legion-api/internal/payments"
	"github.com/swahome/legion-api/internal/store"
)

// Paying in by MPesa: pick what to pay, the amount, confirm. The prompt
// goes to the caller's own phone through the same payment service as the
// web and the app, after the USSD screen has closed.

func (c *convo) paymentsOn() bool { return c.e.Payments != nil && c.e.Payments.Enabled() }

func payScreen(c *convo) (view, error) {
	if _, err := c.member(); err != nil {
		return view{}, err
	}
	if !c.paymentsOn() {
		c.finish("MPesa payments are not available right now. Please try again later.")
		return view{}, nil
	}
	return view{text: "What are you paying for?", options: []option{
		to("Contribution", "pay_contrib"),
		to("Fine", "pay_fine"),
		to("Loan repayment", "pay_loan"),
		to("Other payment", "pay_amount", "kind", "misc", "item", "", "name", "other payment", "owed", ""),
	}}, nil
}

func payContributionScreen(c *convo) (view, error) {
	g, err := c.member()
	if err != nil {
		return view{}, err
	}
	cs, err := c.q.PayableContributions(c.ctx, g.GroupID)
	if err != nil {
		return view{}, err
	}
	if len(cs) == 0 {
		return view{text: "Your group has no contributions to pay into yet."}, nil
	}
	owed, err := c.q.MemberLedgerArrears(c.ctx, store.MemberLedgerArrearsParams{GroupID: g.GroupID, MembershipID: &g.MembershipID})
	if err != nil {
		return view{}, err
	}
	due := map[int64]string{}
	for _, o := range owed {
		if o.Bucket == "contribution" && o.ContributionID != nil {
			due[*o.ContributionID] = o.Balance
		}
	}
	v := view{text: "Choose the contribution:"}
	for _, ct := range cs {
		v.options = append(v.options, to(ct.Name, "pay_amount", "kind", "contribution", "item", "c"+id(ct.ID), "name", ct.Name, "owed", due[ct.ID]))
	}
	return v, nil
}

// payFineScreen lists the fines the member owes first, then the group's
// other fine categories.
func payFineScreen(c *convo) (view, error) {
	g, err := c.member()
	if err != nil {
		return view{}, err
	}
	owed, err := c.q.MemberLedgerArrears(c.ctx, store.MemberLedgerArrearsParams{GroupID: g.GroupID, MembershipID: &g.MembershipID})
	if err != nil {
		return view{}, err
	}
	v := view{text: "Choose the fine:"}
	listed := map[int64]bool{}
	for _, o := range owed {
		switch {
		case o.Bucket == "fine" && o.ContributionID != nil:
			name := o.Name + " late fine"
			v.options = append(v.options, to(name, "pay_amount", "kind", "fine", "item", "c"+id(*o.ContributionID), "name", name, "owed", o.Balance))
		case o.Bucket == "fine" && o.FineCategoryID != nil:
			listed[*o.FineCategoryID] = true
			v.options = append(v.options, to(o.Name, "pay_amount", "kind", "fine", "item", "f"+id(*o.FineCategoryID), "name", o.Name, "owed", o.Balance))
		}
	}
	cats, err := c.q.ListFineCategories(c.ctx, store.ListFineCategoriesParams{GroupID: g.GroupID})
	if err != nil {
		return view{}, err
	}
	for _, f := range cats {
		if !listed[f.ID] {
			v.options = append(v.options, to(f.Name, "pay_amount", "kind", "fine", "item", "f"+id(f.ID), "name", f.Name, "owed", ""))
		}
	}
	if len(v.options) == 0 {
		return view{text: "You have no fines to pay."}, nil
	}
	return v, nil
}

func payLoanScreen(c *convo) (view, error) {
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
	v := view{text: "Choose the loan:"}
	for _, l := range loans {
		label := l.Name + " (" + commas(l.Balance) + ")"
		v.options = append(v.options, to(label, "pay_amount", "kind", "loan", "item", "l"+id(l.ID), "name", l.Name, "owed", l.Balance))
	}
	return v, nil
}

func payAmountScreen(c *convo) (view, error) {
	if _, err := c.member(); err != nil {
		return view{}, err
	}
	text := "Enter the amount to pay for " + c.v("name") + ":"
	if owed := c.v("owed"); owed != "" {
		text = "You owe " + c.amount(owed) + " for " + c.v("name") + ". Enter the amount to pay:"
	}
	return view{text: text, input: func(c *convo, s string) error {
		amt, ok := normAmount(s)
		if !ok {
			return say("Enter an amount in numbers, like 500.")
		}
		return c.open("pay_confirm", "amount", amt)
	}}, nil
}

// line is the payment line the menu choices add up to.
func (c *convo) line() payments.Line {
	l := payments.Line{Kind: c.v("kind"), Amount: c.v("amount")}
	item := c.v("item")
	if len(item) > 1 {
		n := c.vIntOf(item[1:])
		switch item[0] {
		case 'c':
			l.ContributionID = &n
		case 'f':
			l.FineCategoryID = &n
		case 'l':
			l.LoanID = &n
		}
	}
	return l
}

func (c *convo) vIntOf(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

func payConfirmScreen(c *convo) (view, error) {
	g, err := c.member()
	if err != nil {
		return view{}, err
	}
	text := "Pay " + c.amount(c.v("amount")) + " for " + c.v("name") + " from MPesa " + localPhone(c.phone) + "?"
	return view{text: text, options: []option{
		opt("Yes, send the MPesa prompt", func(c *convo) error {
			if !c.paymentsOn() {
				return payments.ErrNotConfigured
			}
			lines := []payments.Line{c.line()}
			if _, err := payments.CheckLines(c.ctx, c.q, g.GroupID, g.MembershipID, lines); err != nil {
				return err
			}
			if _, err := c.q.DefaultPaymentAccount(c.ctx, g.GroupID); errors.Is(err, pgx.ErrNoRows) {
				return say("Your group has no account to pay into yet. Ask an official to add one.")
			} else if err != nil {
				return err
			}
			in := payments.PaymentInput{GroupID: g.GroupID, MembershipID: g.MembershipID, UserID: c.userID(),
				Phone: c.phone, Lines: []payments.Line{c.line()}, Channel: string(c.format)}
			pay, log := c.e.Payments, c.e.log()
			delay := time.Duration(0)
			if c.format == USSD {
				delay = 3 * time.Second // let the USSD screen close first
			}
			c.after = append(c.after, func() {
				c.e.later(delay, func() {
					ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
					defer cancel()
					if _, err := pay.StartPayment(ctx, in); err != nil {
						log.Error("channel payment", "group", in.GroupID, "member", in.MembershipID, "err", err)
					}
				})
			})
			c.finish("You will get an MPesa prompt on " + localPhone(c.phone) + " shortly. Enter your MPesa PIN to pay " +
				c.amount(c.v("amount")) + ". We will send you an SMS when it is received.")
			return nil
		}),
		opt("No, cancel", func(c *convo) error {
			c.finish("Payment cancelled.")
			return nil
		}),
	}}, nil
}

// localPhone shows +254712345678 as 0712345678.
func localPhone(e164 string) string {
	if len(e164) == 13 && e164[:4] == "+254" {
		return "0" + e164[4:]
	}
	return e164
}
