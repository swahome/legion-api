package channels

import (
	"fmt"
	"strconv"

	"github.com/swahome/legion-api/internal/books"
	"github.com/swahome/legion-api/internal/loans"
	"github.com/swahome/legion-api/internal/payments"
	"github.com/swahome/legion-api/internal/store"
)

// Loans: apply, see your applications and balances, and answer the
// applications waiting on you as a guarantor or signatory. The rules are
// books.Apply, books.AnswerGuarantee and books.DecideApplication.

func loansScreen(c *convo) (view, error) {
	g, err := c.member()
	if err != nil {
		return view{}, err
	}
	waiting, err := c.q.CountLoanApplicationsAwaiting(c.ctx, g.MembershipID)
	if err != nil {
		return view{}, err
	}
	v := view{text: "Loans:", options: []option{
		to("Apply for a loan", "loan_types"),
		to("My loan applications", "loan_apps"),
		to("My loan balances", "loans_owed"),
	}}
	label := "Applications waiting for me"
	if waiting > 0 {
		label += fmt.Sprintf(" (%d)", waiting)
	}
	v.options = append(v.options, to(label, "loan_waiting"))
	return v, nil
}

func loanTypesScreen(c *convo) (view, error) {
	g, err := c.member()
	if err != nil {
		return view{}, err
	}
	ts, err := c.q.ListLoanTypes(c.ctx, store.ListLoanTypesParams{GroupID: g.GroupID})
	if err != nil {
		return view{}, err
	}
	if len(ts) == 0 {
		return view{text: "Your group has no loan types to apply for yet."}, nil
	}
	v := view{text: "Choose the loan:"}
	for _, t := range ts {
		t := t
		v.options = append(v.options, opt(t.Name, func(c *convo) error {
			if t.Guarantors == "always" {
				return say(t.Name + " needs guarantors. Apply for it on the app or the web.")
			}
			return c.open("loan_amount", "loan_type", id(t.ID), "loan_name", t.Name)
		}))
	}
	return v, nil
}

func (c *convo) loanType() (books.LoanType, error) {
	t, err := c.q.GetLoanType(c.ctx, store.GetLoanTypeParams{GroupID: c.g.GroupID, ID: c.vInt("loan_type")})
	if notFound(err) {
		return books.LoanType{}, books.ErrNotFound
	}
	if err != nil {
		return books.LoanType{}, err
	}
	return books.LoanTypeFrom(t)
}

func loanAmountScreen(c *convo) (view, error) {
	g, err := c.member()
	if err != nil {
		return view{}, err
	}
	t, err := c.loanType()
	if err != nil {
		return view{}, err
	}
	text := "How much " + t.Name + " do you want?"
	switch {
	case t.AmountBasis == "savings" && t.SavingsMultiple != nil:
		saved, err := c.q.MemberSavings(c.ctx, store.MemberSavingsParams{GroupID: g.GroupID, MembershipID: &g.MembershipID})
		if err != nil {
			return view{}, err
		}
		limit := books.Cents(saved) * books.Cents(*t.SavingsMultiple) / 100
		if limit <= 0 {
			return view{}, say("You need savings in the group before you can apply for " + t.Name + ".")
		}
		text += " Up to " + c.amount(centsText(limit)) + "."
	case t.MinAmount != nil && t.MaxAmount != nil:
		text += " Between " + c.amount(*t.MinAmount) + " and " + c.amount(*t.MaxAmount) + "."
	case t.MaxAmount != nil:
		text += " Up to " + c.amount(*t.MaxAmount) + "."
	case t.MinAmount != nil:
		text += " At least " + c.amount(*t.MinAmount) + "."
	}
	return view{text: text, input: func(c *convo, s string) error {
		amt, ok := normAmount(s)
		if !ok {
			return say("Enter an amount in numbers, like 10000.")
		}
		if t.FixedTerm != nil {
			return c.checkLoan(amt, int(*t.FixedTerm))
		}
		// Check the amount now, with any term the type allows.
		n := 1
		if t.MinTerm != nil {
			n = int(*t.MinTerm)
		}
		if err := c.previewLoan(amt, n); err != nil {
			return err
		}
		return c.open("loan_term", "loan_amount", amt)
	}}, nil
}

var periodNames = map[string]string{
	"weekly": "weeks", "fortnightly": "fortnights", "monthly": "months", "quarterly": "quarters",
	"half_yearly": "half years", "yearly": "years", "daily": "days",
}

func loanTermScreen(c *convo) (view, error) {
	if _, err := c.member(); err != nil {
		return view{}, err
	}
	t, err := c.loanType()
	if err != nil {
		return view{}, err
	}
	unit := periodNames[t.Frequency]
	if unit == "" {
		unit = "installments"
	}
	text := "Repay " + c.amount(c.v("loan_amount")) + " over how many " + unit + "?"
	switch {
	case t.MinTerm != nil && t.MaxTerm != nil:
		text += fmt.Sprintf(" %d to %d.", *t.MinTerm, *t.MaxTerm)
	case t.MaxTerm != nil:
		text += fmt.Sprintf(" Up to %d.", *t.MaxTerm)
	}
	return view{text: text, input: func(c *convo, s string) error {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > loans.MaxInstallments {
			return say("Enter the number of " + unit + ", like 6.")
		}
		return c.checkLoan(c.v("loan_amount"), n)
	}}, nil
}

func (c *convo) previewLoan(amount string, installments int) error {
	today := c.todayDate()
	lt := c.vInt("loan_type")
	_, _, err := books.PreviewLoan(c.ctx, c.q, c.g.GroupID, books.NewLoan{
		MemberID: c.g.MembershipID, LoanTypeID: &lt, Amount: amount, Installments: installments, Date: &today,
	})
	return err
}

// checkLoan runs the loan type's rules before showing the confirmation.
func (c *convo) checkLoan(amount string, installments int) error {
	if err := c.previewLoan(amount, installments); err != nil {
		return err
	}
	return c.open("loan_confirm", "loan_amount", amount, "installments", strconv.Itoa(installments))
}

func loanConfirmScreen(c *convo) (view, error) {
	g, err := c.member()
	if err != nil {
		return view{}, err
	}
	today := c.todayDate()
	lt := c.vInt("loan_type")
	n, _ := strconv.Atoi(c.v("installments"))
	terms, fee, err := books.PreviewLoan(c.ctx, c.q, g.GroupID, books.NewLoan{
		MemberID: g.MembershipID, LoanTypeID: &lt, Amount: c.v("loan_amount"), Installments: n, Date: &today,
	})
	if err != nil {
		return view{}, err
	}
	text := "Apply for " + c.amount(c.v("loan_amount")) + " " + c.v("loan_name")
	if sched, err := terms.Schedule(); err == nil && len(sched) > 0 {
		var total int64
		for _, in := range sched {
			total += in.Principal + in.Interest
		}
		text += fmt.Sprintf(", repaid in %d installments of about %s (total %s", len(sched),
			commas(centsText(sched[0].Principal+sched[0].Interest)), c.amount(centsText(total)))
		if fee > 0 {
			text += ", fee " + commas(centsText(fee))
		}
		text += ")"
	}
	text += "?"
	return view{text: text, options: []option{
		opt("Yes, apply", func(c *convo) error {
			if err := c.write(func(q *store.Queries) error {
				_, err := books.Apply(c.ctx, q, g.GroupID, c.userID(), books.NewApplication{
					MemberID: g.MembershipID, LoanTypeID: lt, Amount: c.v("loan_amount"), Installments: n,
					Description:   "Applied on " + channelName(c.format),
					OfficialsOnly: g.flag("restrict_wallet_approval_to_group_admins"),
				}, today)
				return err
			}); err != nil {
				return err
			}
			c.finish("Your loan application was sent. We will let you know when it is decided.")
			return nil
		}),
		opt("No, cancel", func(c *convo) error {
			c.finish("Loan application cancelled.")
			return nil
		}),
	}}, nil
}

var applicationStates = map[store.LoanApplicationStatus]string{
	store.LoanApplicationStatusPendingGuarantors: "waiting for guarantors",
	store.LoanApplicationStatusPendingApproval:   "waiting for approval",
	store.LoanApplicationStatusApproved:          "approved, to be paid out",
	store.LoanApplicationStatusDeclined:          "declined",
	store.LoanApplicationStatusCancelled:         "cancelled",
	store.LoanApplicationStatusDisbursed:         "paid out",
}

func loanApplicationsScreen(c *convo) (view, error) {
	g, err := c.member()
	if err != nil {
		return view{}, err
	}
	rows, err := c.q.ListLoanApplications(c.ctx, store.ListLoanApplicationsParams{
		Viewer: g.MembershipID, GroupID: g.GroupID, MembershipID: &g.MembershipID, Lim: 5,
	})
	if err != nil {
		return view{}, err
	}
	if len(rows) == 0 {
		return view{text: "You have no loan applications."}, nil
	}
	text := "Your loan applications:"
	for _, r := range rows {
		state := applicationStates[r.Status]
		if state == "" {
			state = string(r.Status)
		}
		text += "\n" + r.CreatedAt.In(c.loc()).Format("02 Jan 06") + " " + r.LoanTypeName + " " + commas(r.Amount) + ": " + state
	}
	return view{text: text}, nil
}

func loanWaitingScreen(c *convo) (view, error) {
	g, err := c.member()
	if err != nil {
		return view{}, err
	}
	rows, err := c.q.ListLoanApplications(c.ctx, store.ListLoanApplicationsParams{
		Viewer: g.MembershipID, GroupID: g.GroupID, AwaitingMe: true, AllVisible: true, Lim: 50,
	})
	if err != nil {
		return view{}, err
	}
	if len(rows) == 0 {
		return view{text: "No loan applications are waiting for you."}, nil
	}
	v := view{text: "Waiting for you:"}
	for _, r := range rows {
		role := "approve"
		if r.AwaitingMyGuarantee {
			role = "guarantee"
		}
		v.options = append(v.options, to(r.MemberName+" "+commas(r.Amount)+" ("+role+")", "loan_answer", "application", id(r.ID), "role", role))
	}
	return v, nil
}

func loanAnswerScreen(c *convo) (view, error) {
	g, err := c.member()
	if err != nil {
		return view{}, err
	}
	a, err := c.q.GetLoanApplication(c.ctx, store.GetLoanApplicationParams{GroupID: g.GroupID, ID: c.vInt("application")})
	if notFound(err) {
		return view{}, books.ErrNotFound
	}
	if err != nil {
		return view{}, err
	}
	guarantee := c.v("role") == "guarantee"
	text := a.MemberName + " applied for " + c.amount(a.Amount) + " " + a.LoanTypeName + fmt.Sprintf(" over %d installments.", a.Installments)
	yes := "Approve"
	if guarantee {
		text += " Will you guarantee it?"
		yes = "Yes, I guarantee it"
	}
	return view{text: text, options: []option{
		opt(yes, func(c *convo) error {
			if err := c.answerLoan(a.ID, guarantee, true, ""); err != nil {
				return err
			}
			if guarantee {
				c.finish("Thank you. You are now a guarantor on " + a.MemberName + "'s loan application.")
			} else {
				c.finish("You approved " + a.MemberName + "'s loan application.")
			}
			return nil
		}),
		to("Decline", "loan_answer_no"),
	}}, nil
}

func (c *convo) answerLoan(appID int64, guarantee, yes bool, reason string) error {
	return c.write(func(q *store.Queries) error {
		if guarantee {
			return books.AnswerGuarantee(c.ctx, q, c.g.GroupID, appID, c.g.MembershipID, c.userID(), yes, reason,
				c.g.flag("restrict_wallet_approval_to_group_admins"))
		}
		return books.DecideApplication(c.ctx, q, c.g.GroupID, appID, c.g.MembershipID, c.userID(), yes, reason)
	})
}

func loanDeclineScreen(c *convo) (view, error) {
	if _, err := c.member(); err != nil {
		return view{}, err
	}
	return view{text: "Why are you declining?", input: func(c *convo, s string) error {
		if err := c.answerLoan(c.vInt("application"), c.v("role") == "guarantee", false, s); err != nil {
			return err
		}
		c.finish("You declined the loan application.")
		return nil
	}}, nil
}

// normAmount reads "1,500" or "1500.50" as an amount above zero.
func normAmount(s string) (string, bool) { return payments.NormAmount(s) }
