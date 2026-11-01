package mobile

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/swahome/legion-api/internal/auth"
	"github.com/swahome/legion-api/internal/payments"
	"github.com/swahome/legion-api/internal/store"
)

// MPesa payments and bank transaction alerts (milestone 6): the legacy
// transaction_alerts/* reconciliation actions and the deposits/* actions
// that start an MPesa prompt. The rules live in internal/payments, shared
// with the web.

// WithPayments switches on the payment actions.
func (s *Server) WithPayments(p *payments.Service) *Server {
	s.pay = p
	return s
}

// unreconciledAlerts is get_group_unreconcilled_deposits (direction 1) and
// get_group_unreconcilled_withdrawals (-1). Like the web, only officials
// see the group's bank feed.
func (s *Server) unreconciledAlerts(direction int16) groupFn {
	key, typ := "unreconciled_deposits", 1
	if direction < 0 {
		key, typ = "unreconciled_withdrawals", 2
	}
	return s.officialsOnly(func(ctx context.Context, c *call, g *member) (any, error) {
		rows, err := s.q.ListAlerts(ctx, store.ListAlertsParams{
			GroupID: &g.group.ID, Direction: &direction, PageSize: 500,
			Status: store.NullAlertStatus{AlertStatus: store.AlertStatusUnreconciled, Valid: true},
		})
		if err != nil {
			return nil, err
		}
		posts := []any{}
		for _, r := range rows {
			details := r.AccountName
			if r.BankName != "" {
				details = r.BankName + " - " + r.AccountName
			}
			posts = append(posts, map[string]any{
				"type":               typ,
				"transaction_type":   typ,
				"description":        r.Particulars,
				"amount":             r.Amount,
				"transactionAlertId": r.ID,
				"isReconciled":       0,
				"particulars":        r.Particulars,
				"accountNUmber":      r.AccountNumber,
				"transaction_id":     r.ProviderRef,
				"account_details":    details,
				"transactionDate":    mobileShortTime(r.TransactionAt.In(g.loc)),
			})
		}
		return map[string]any{"status": 1, "message": "successful", key: posts}, nil
	})
}

// alertDepositFor maps reconcile_deposits' deposit_for_type to a line kind.
// 6 bank loan, 7 transfer, 8 stock sale, 9 asset sale and 10 money market
// are not in the new books yet.
var alertDepositFor = map[string]string{"1": "contribution", "2": "fine", "3": "misc", "4": "income", "5": "loan"}

// alertWithdrawalFor maps reconcile_withdrawals' withdrawal_for_type: 1 expense
// and 7 contribution refund.
var alertWithdrawalFor = map[string]string{"1": "expense", "7": "contribution_refund"}

// reconcileAlert is reconcile_deposits and reconcile_withdrawals:
// transaction_alert_id and a breakdown whose amounts add up to the alert.
func (s *Server) reconcileAlert(in bool) groupFn {
	listKey, typeKey, kinds := "reconcile_deposits_break_down", "deposit_for_type", alertDepositFor
	if !in {
		listKey, typeKey, kinds = "reconcile_withdrawals_break_down", "withdrawal_for_type", alertWithdrawalFor
	}
	return s.officialsOnly(func(ctx context.Context, c *call, g *member) (any, error) {
		alertID := c.req.Int("transaction_alert_id")
		items, _ := c.req[listKey].([]any)
		if alertID <= 0 || len(items) == 0 {
			return timed(status(0, "Form validation failed"), s.now()), nil
		}
		var lines []payments.Line
		for _, it := range items {
			m, ok := it.(map[string]any)
			if !ok {
				return timed(status(0, "Form validation failed"), s.now()), nil
			}
			q := request(m)
			kind, ok := kinds[q.Str(typeKey)]
			if !ok {
				return timed(status(0, "This kind of reconciliation is not available in the app yet. Use the web."), s.now()), nil
			}
			l := payments.Line{Kind: kind, Amount: strings.ReplaceAll(q.Str("amount"), ",", ""), Description: q.Str("description")}
			id := func(key string) *int64 {
				if v := q.Int(key); v > 0 {
					return &v
				}
				return nil
			}
			l.MemberID, l.ContributionID, l.LoanID = id("member_id"), id("contribution_id"), id("loan_id")
			l.DepositorID, l.IncomeCategoryID, l.ExpenseCategoryID = id("depositor_id"), id("income_category_id"), id("expense_category_id")
			// fine_category_id is either an id or "contribution-ID" (a late
			// contribution fine) / "fine_category-ID".
			what, num, found := strings.Cut(q.Str("fine_category_id"), "-")
			if !found {
				num, what = what, "fine_category"
			}
			if v, _ := strconv.ParseInt(num, 10, 64); v > 0 {
				if what == "contribution" {
					l.ContributionID = &v
				} else {
					l.FineCategoryID = &v
				}
			}
			lines = append(lines, l)
		}
		err := s.inTx(ctx, func(q *store.Queries) error {
			return payments.Reconcile(ctx, q, g.group.ID, g.user.ID, alertID, lines, false, "")
		})
		var pe payments.Error
		switch {
		case errors.Is(err, payments.ErrNotFound):
			return timed(status(0, "Transaction alert error"), s.now()), nil
		case errors.As(err, &pe):
			msg := pe.Msg
			switch pe.Code {
			case "already_reconciled":
				msg = "Transaction already reconciled"
			case "amount_mismatch":
				msg = "The amount reconciled has to be equal to the amount deposited"
			}
			return timed(status(0, msg), s.now()), nil
		case err != nil:
			return nil, err
		}
		return timed(status(1, "Reconcillation successfully recorded"), s.now()), nil
	})
}

// startPayment sends the MPesa prompt and answers in the legacy words.
func (s *Server) startPayment(ctx context.Context, g *member, phone string, lines []payments.Line, amount string) (any, error) {
	if !s.pay.Enabled() {
		return timed(status(0, "Service unavailable. Try again later."), s.now()), nil
	}
	if phone == "" && g.user.Phone != nil {
		phone = *g.user.Phone
	}
	e164, err := auth.NormalizePhone(phone, "254")
	if err != nil {
		return timed(status(0, "Form validation failed"), s.now()), nil
	}
	id, err := s.pay.StartPayment(ctx, payments.PaymentInput{GroupID: g.group.ID, MembershipID: g.m.ID, UserID: g.user.ID,
		Phone: e164, Lines: lines, Amount: amount, Channel: "app"})
	var pe payments.Error
	switch {
	case errors.As(err, &pe) && pe.Code == "no_payment_account":
		return timed(status(0, "Group does not have an active bank account. Contact Admin for support"), s.now()), nil
	case errors.As(err, &pe):
		return timed(status(0, pe.Msg), s.now()), nil
	case err != nil && id == 0:
		return nil, err
	}
	pr, err := s.q.GetPaymentRequest(ctx, store.GetPaymentRequestParams{GroupID: g.group.ID, ID: id})
	if err != nil {
		return nil, err
	}
	if pr.Status == store.PaymentRequestStatusFailed {
		return timed(status(0, "Server error: "+pr.ResultDescription), s.now()), nil
	}
	return timed(status(1, "Payment in progress. Please wait to enter pin"), s.now()), nil
}

// makeGroupPayment is deposits/make_new_group_payment and
// make_payment_to_group_bank_express (the same request): payment_for (1
// contribution, 2 fine, 3 loan, 4 other), amount, phone_number and the
// item's id.
func (s *Server) makeGroupPayment(ctx context.Context, c *call, g *member) (any, error) {
	if g.m.Status != store.MembershipStatusActive {
		return timed(status(11, "Your membership is suspended."), s.now()), nil
	}
	amount := strings.ReplaceAll(c.req.Str("amount"), ",", "")
	if _, ok := payments.NormAmount(amount); !ok || c.req.Str("phone_number") == "" {
		return timed(status(0, "Form validation failed"), s.now()), nil
	}
	l, ok := lineFor(c.req.Str("payment_for"), c.req.Int("contribution_id"), c.req.Str("fine_category_id"),
		c.req.Int("loan_id"), c.req.Str("description"))
	if !ok {
		return timed(status(0, "Form validation failed"), s.now()), nil
	}
	l.Amount = amount
	return s.startPayment(ctx, g, c.req.Str("phone_number"), []payments.Line{l}, "")
}

func lineFor(paymentFor string, contributionID int64, fine string, loanID int64, desc string) (payments.Line, bool) {
	ptr := func(v int64) *int64 {
		if v > 0 {
			return &v
		}
		return nil
	}
	switch paymentFor {
	case "1":
		return payments.Line{Kind: "contribution", ContributionID: ptr(contributionID)}, contributionID > 0
	case "2":
		what, num, found := strings.Cut(fine, "-")
		if !found {
			num, what = what, "fine_category"
		}
		v, _ := strconv.ParseInt(num, 10, 64)
		if v <= 0 {
			return payments.Line{}, false
		}
		if what == "contribution" {
			return payments.Line{Kind: "fine", ContributionID: &v}, true
		}
		return payments.Line{Kind: "fine", FineCategoryID: &v}, true
	case "3":
		return payments.Line{Kind: "loan", LoanID: ptr(loanID)}, loanID > 0
	case "4", "":
		return payments.Line{Kind: "misc", Description: desc}, true
	}
	return payments.Line{}, false
}

// makeArrearsPayment is deposits/make_group_arrears_payment: an amount
// spread over the member's arrears in the legacy order.
func (s *Server) makeArrearsPayment(ctx context.Context, c *call, g *member) (any, error) {
	if g.m.Status != store.MembershipStatusActive {
		return timed(status(11, "Your membership is suspended."), s.now()), nil
	}
	amount := strings.ReplaceAll(c.req.Str("amount"), ",", "")
	if _, ok := payments.NormAmount(amount); !ok {
		return timed(status(0, "Amount to pay is invalid"), s.now()), nil
	}
	phone := c.req.Str("phone_number")
	if phone == "" {
		phone = c.req.Str("phone")
	}
	return s.startPayment(ctx, g, phone, nil, amount)
}

// paymentRequest is transaction_alerts/payment_request: amount, phone and
// one of loan_id, contribution_id or fine_category; with none of them the
// amount goes to the member's arrears.
func (s *Server) paymentRequest(ctx context.Context, c *call, g *member) (any, error) {
	amount := strings.ReplaceAll(c.req.Str("amount"), ",", "")
	if _, ok := payments.NormAmount(amount); !ok {
		return timed(status(0, "Form validation failed"), s.now()), nil
	}
	var lines []payments.Line
	switch {
	case c.req.Int("loan_id") > 0:
		l, _ := lineFor("3", 0, "", c.req.Int("loan_id"), "")
		lines = []payments.Line{l}
	case c.req.Int("contribution_id") > 0:
		l, _ := lineFor("1", c.req.Int("contribution_id"), "", 0, "")
		lines = []payments.Line{l}
	case c.req.Str("fine_category") != "":
		l, ok := lineFor("2", 0, c.req.Str("fine_category"), 0, "")
		if !ok {
			return timed(status(0, "Form validation failed"), s.now()), nil
		}
		lines = []payments.Line{l}
	}
	if len(lines) == 1 {
		lines[0].Amount = amount
		amount = ""
	}
	return s.startPayment(ctx, g, c.req.Str("phone"), lines, amount)
}
