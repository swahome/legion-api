package mobile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/swahome/legion-api/internal/auth"
	"github.com/swahome/legion-api/internal/books"
	"github.com/swahome/legion-api/internal/store"
)

// Withdrawal requests (the legacy e-wallet's "request funds transfer"):
// officials ask, every signatory approves, then the money is paid. The
// rules live in internal/books, shared with the web.

// withdrawalForNames is withdrawal_request_transaction_names.
var withdrawalForNames = map[store.WithdrawalKind]string{
	store.WithdrawalKindExpense:            "Expense Payment",
	store.WithdrawalKindContributionRefund: "Contribution Refund",
	store.WithdrawalKindLoanDisbursement:   "Loan Disbursement",
	store.WithdrawalKindFundsTransfer:      "Account Transfer (Wallet - Bank)",
}

var recipientNames = map[string]string{"mobile": "Mobile Wallet", "paybill": "Paybill", "bank": "Bank Account"}

// requestFilters maps the app's status numbers to request statuses.
var requestFilters = map[string][]string{
	"1": {"pending"}, "2": {"approved"}, "3": {"declined", "cancelled"},
	"14": {"approved"}, "15": {"paid"}, "16": {"failed"},
}

// commaMoney is number_to_currency: "1,500.00".
func commaMoney(v string) string {
	s := books.Format(v)
	neg := strings.HasPrefix(s, "-")
	whole, frac, _ := strings.Cut(strings.TrimPrefix(s, "-"), ".")
	var b strings.Builder
	for i, c := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	out := b.String() + "." + frac
	if neg {
		out = "-" + out
	}
	return out
}

func recipientText(r store.MobileWithdrawalRequestsRow) string {
	if r.Kind == store.WithdrawalKindFundsTransfer {
		return r.ToAccountName
	}
	var rc books.Recipient
	_ = json.Unmarshal(r.Recipient, &rc)
	out := recipientNames[rc.Type] + " "
	switch rc.Type {
	case "mobile":
		out += " -- Phone Number: " + strings.TrimPrefix(rc.Phone, "+")
	case "paybill":
		out += rc.Paybill + " Account " + rc.AccountNumber
	case "bank":
		out += " -- Bank Name: " + r.RecipientBankName + ", Account: " + rc.AccountNumber
	}
	if r.MembershipID != nil {
		out += " - " + r.MemberName
	}
	return out
}

// requestState is the list's status text and code.
func requestState(r store.MobileWithdrawalRequestsRow) (string, int) {
	switch r.Status {
	case store.WithdrawalRequestStatusApproved:
		return "Approved by signatories- pending disbursement", 2
	case store.WithdrawalRequestStatusDeclined, store.WithdrawalRequestStatusCancelled:
		if r.DeclineReason != "" {
			return ucfirst(strings.ToLower(r.DeclineReason)), 3
		}
		return "Declined by signatories", 3
	case store.WithdrawalRequestStatusPaid:
		return "Disbursed", 5
	case store.WithdrawalRequestStatusFailed:
		return "Disbursement failed", 6
	}
	return "Pending signatories approval", 1
}

func ucfirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// responded is has_responded and response_status for the caller.
func responded(r store.MobileWithdrawalRequestsRow, g *member, notSignatory int) (int, int) {
	if r.RequestedBy != nil && *r.RequestedBy == g.m.ID {
		return 1, boolInt(r.MyDecision == "approved")
	}
	switch {
	case r.MyDecision == "approved":
		return 1, 1
	case r.MyDecision == "declined":
		return 1, 0
	case r.MyDecision == "":
		return notSignatory, 0
	case r.Status != store.WithdrawalRequestStatusPending:
		return 1, 0
	}
	return 0, 0
}

func (s *Server) withdrawalRequestList(ctx context.Context, c *call, g *member) (any, error) {
	codes := c.req.Strs("status")
	if len(codes) == 0 {
		codes = []string{"1"}
	}
	var statuses []string
	for _, code := range codes {
		statuses = append(statuses, requestFilters[code]...)
	}
	if len(statuses) == 0 {
		return timed(status(0, "Invalid status"), s.now()), nil
	}
	lower, upper := c.req.Int("lower_limit"), c.req.Int("upper_limit")
	if upper <= lower {
		upper = lower + 10
	}
	rows, err := s.q.MobileWithdrawalRequests(ctx, store.MobileWithdrawalRequestsParams{
		Caller: g.m.ID, GroupID: g.group.ID, Statuses: statuses, MemberIds: c.req.IDs("members"),
		Sort: sortOf(c), Lim: int32(min(upper-lower, 500)), Off: int32(lower),
	})
	if err != nil {
		return nil, err
	}
	posts := make([]any, 0, len(rows))
	for _, r := range rows {
		state, code := requestState(r)
		has, resp := responded(r, g, 0)
		if code != 1 {
			has = 1
		}
		posts = append(posts, map[string]any{
			"id": fmt.Sprint(r.ID), "date": r.CreatedAt.In(g.loc).Format("02-Jan-2006 3:04PM"),
			"name": r.RequestedByName, "withdrawal_for": withdrawalForNames[r.Kind], "amount": r.Amount,
			"approved": r.Approved, "declined": r.Declined, "pending": r.Pending,
			"status": state, "status_code": code, "is_owner": boolInt(r.RequestedBy != nil && *r.RequestedBy == g.m.ID),
			"recipient": recipientText(r), "has_responded": has, "response_status": resp, "description": "",
		})
	}
	return timed(map[string]any{"status": 1, "message": "Withdrawal requests", "posts": posts}, s.now()), nil
}

func (s *Server) oneRequest(ctx context.Context, c *call, g *member) (*store.MobileWithdrawalRequestsRow, error) {
	id, err := strconv.ParseInt(c.req.Str("id"), 10, 64)
	if err != nil {
		return nil, nil
	}
	rows, err := s.q.MobileWithdrawalRequests(ctx, store.MobileWithdrawalRequestsParams{
		Caller: g.m.ID, GroupID: g.group.ID, ID: &id, Lim: 1,
	})
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return &rows[0], nil
}

func (s *Server) viewWithdrawalRequest(ctx context.Context, c *call, g *member) (any, error) {
	if _, err := strconv.ParseInt(c.req.Str("id"), 10, 64); err != nil {
		return timed(status(0, "Invalid withdrawal request id"), s.now()), nil
	}
	r, err := s.oneRequest(ctx, c, g)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return timed(status(0, "Withdrawal request not found"), s.now()), nil
	}
	cur := g.group.Currency
	amt := cur + " " + commaMoney(r.Amount)
	var desc string
	switch r.Kind {
	case store.WithdrawalKindExpense:
		desc = withdrawalForNames[r.Kind] + " " + r.ExpenseCategoryName + " of  " + amt + " "
	case store.WithdrawalKindContributionRefund:
		desc = withdrawalForNames[r.Kind] + " to " + r.MemberName + " of  " + amt + " from " + r.ContributionName + " contribution "
	case store.WithdrawalKindFundsTransfer:
		desc = withdrawalForNames[r.Kind] + " of " + amt + " from " + r.AccountName + " to " + r.ToAccountName + " "
	default:
		desc = withdrawalForNames[r.Kind] + " of " + amt + " "
	}
	if r.Description != "" && r.Kind != store.WithdrawalKindFundsTransfer {
		desc += " : " + r.Description
	}
	approvals, err := s.q.ListWithdrawalApprovals(ctx, r.ID)
	if err != nil {
		return nil, err
	}
	approved, declined, pending := []any{}, []any{}, []any{}
	for _, a := range approvals {
		name := a.MemberName
		you := a.MembershipID == g.m.ID
		switch a.Decision {
		case store.ApprovalDecisionApproved:
			if you {
				name += "- You"
			}
			approved = append(approved, name)
		case store.ApprovalDecisionDeclined:
			if you {
				name += "- You: Comments --- " + a.Comment
			} else {
				name += " : Comments --- " + a.Comment
			}
			declined = append(declined, name)
		default:
			if you {
				name += "- You"
			}
			pending = append(pending, name)
		}
	}
	approval := "Pending Approval"
	switch r.Status {
	case store.WithdrawalRequestStatusApproved, store.WithdrawalRequestStatusPaid, store.WithdrawalRequestStatusFailed:
		approval = "Approved"
	case store.WithdrawalRequestStatusDeclined, store.WithdrawalRequestStatusCancelled:
		approval = "Declined: Reason -- " + r.DeclineReason
	}
	has, resp := responded(*r, g, 1)
	if r.Status == store.WithdrawalRequestStatusDeclined || r.Status == store.WithdrawalRequestStatusCancelled {
		has = 1
	}
	return timed(map[string]any{
		"status": 1, "message": "Withdrawal request",
		"withdrawal_for": withdrawalForNames[r.Kind], "date": r.CreatedAt.In(g.loc).Format("02-Jan-2006 3:04PM"),
		"request_by": r.RequestedByName, "amount": r.Amount, "description": desc, "recipient": recipientText(*r),
		"approval_status": approval, "approved_members": approved, "declined_members": declined, "pending_approvals": pending,
		"is_owner": boolInt(r.RequestedBy != nil && *r.RequestedBy == g.m.ID), "has_responded": has, "response_status": resp,
	}, s.now()), nil
}

// requestFundsTransfer is withdrawals/request_funds_transfer. withdrawal_for
// is 1 expense, 2 or 3 a contribution refund (3 was the merry-go-round
// payout), 4 a loan disbursement (with loans, milestone 4) or 5 a transfer
// between the group's accounts. The money comes from transfer_from, or the
// group's default bank account (the legacy wallet).
func (s *Server) requestFundsTransfer(ctx context.Context, c *call, g *member) (any, error) {
	if reply := s.required(c, [2]string{"withdrawal_for", "Withdrawal For"}, [2]string{"amount", "Amount"}); reply != nil {
		return reply, nil
	}
	kinds := map[string]store.WithdrawalKind{
		"1": store.WithdrawalKindExpense, "2": store.WithdrawalKindContributionRefund, "3": store.WithdrawalKindContributionRefund,
		"5": store.WithdrawalKindFundsTransfer,
	}
	kind, ok := kinds[c.req.Str("withdrawal_for")]
	if !ok {
		if c.req.Str("withdrawal_for") == "4" {
			return status(0, "Loan disbursements from the app are not available yet"), nil
		}
		return s.formFailed(map[string]string{"withdrawal_for": "Kindly select a valid withdrawal type"}), nil
	}
	req := books.NewRequest{
		Kind: kind, Amount: appAmount(c.req.Str("amount")), Description: c.req.Str("description"),
		RequestedBy: &g.m.ID, OfficialsOnly: g.flag("restrict_wallet_approval_to_group_admins"),
	}
	from := appAccount(c.req.Str("transfer_from"))
	if from == 0 {
		accts, _, err := s.groupAccounts(ctx, g)
		if err != nil {
			return nil, err
		}
		var banks []int64
		for _, a := range accts {
			if a.Kind == store.AccountKindBank && !a.IsClosed {
				if a.IsDefault {
					banks = []int64{a.ID}
					break
				}
				banks = append(banks, a.ID)
			}
		}
		if len(banks) != 1 {
			return s.formFailed(map[string]string{"transfer_from": "Select the account to pay from"}), nil
		}
		from = banks[0]
	}
	req.AccountID = from
	switch kind {
	case store.WithdrawalKindExpense:
		req.ExpenseCategoryID = appID(c.req.Str("expense_category_id"))
	case store.WithdrawalKindContributionRefund:
		req.MemberID, req.ContributionID = appID(c.req.Str("member_id")), appID(c.req.Str("contribution_id"))
	case store.WithdrawalKindFundsTransfer:
		to := appAccount(c.req.Str("transfer_to"))
		req.ToAccountID = &to
	}
	if kind != store.WithdrawalKindFundsTransfer {
		rc := &books.Recipient{}
		switch c.req.Str("recipient") {
		case "1":
			rc.Type = "mobile"
			if p := c.req.Str("phone"); p != "" {
				n, err := auth.NormalizePhone(p, s.auth.DefaultCallingCode())
				if err != nil {
					return s.formFailed(map[string]string{"phone": "Enter a valid phone number"}), nil
				}
				rc.Phone = n
			}
		case "2":
			rc.Type, rc.Paybill, rc.AccountNumber = "paybill", c.req.Str("paybill_number"), c.req.Str("paybill_account_number")
		case "3":
			rc.Type, rc.AccountNumber = "bank", c.req.Str("account_number")
			if id := appID(c.req.Str("bank_id")); id != nil {
				rc.BankID = *id
			}
		default:
			return s.formFailed(map[string]string{"recipient": "Select who is being paid"}), nil
		}
		req.Recipient = rc
	}
	var id int64
	err := s.inTx(ctx, func(q *store.Queries) (err error) {
		id, err = books.CreateRequest(ctx, q, g.group.ID, g.user.ID, req)
		return err
	})
	var cf books.Conflict
	if errors.As(err, &cf) {
		if cf.Code == "not_enough_signatories" {
			return status(0, "Group does not have enough signatories"), nil
		}
		return status(0, cf.Msg), nil
	}
	if be, ok := asBooksError(err); ok {
		field := errorFields[be.Code]
		switch be.Code {
		case "invalid_account", "account_closed":
			field = "transfer_from"
		case "same_account":
			field = "transfer_to"
		case "invalid_recipient":
			field = "recipient"
		}
		if field == "" {
			field = "form"
		}
		return s.formFailed(map[string]string{field: be.Msg}), nil
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"status": 1, "message": "Request Successfully submitted", "request_id": fmt.Sprint(id)}, nil
}

// respondToWithdrawalRequest approves or declines (with a reason). The app
// never asked for a password here; when it sends one, it is checked.
func (s *Server) respondToWithdrawalRequest(ctx context.Context, c *call, g *member) (any, error) {
	r, err := s.oneRequest(ctx, c, g)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return timed(status(0, "Withdrawal request not found"), s.now()), nil
	}
	approve, decline := c.req.Str("approve") != "" && c.req.Str("approve") != "0", c.req.Str("decline") != "" && c.req.Str("decline") != "0"
	if !approve && !decline {
		return timed(status(0, "You must approve or decline withdrawal"), s.now()), nil
	}
	if !approve && c.req.Str("reason") == "" {
		return timed(status(0, "Request could not be handled at the moment. Kindly check your request and try again later"), s.now()), nil
	}
	if pw := c.req.Str("password"); pw != "" {
		var locked *auth.LockedOutError
		if err := s.auth.ConfirmPassword(ctx, g.user.ID, pw, clientInfo(c.r)); errors.Is(err, auth.ErrInvalidCredentials) {
			return timed(status(0, "You entered the wrong password."), s.now()), nil
		} else if errors.As(err, &locked) {
			return timed(status(0, "Too many wrong passwords. Try again later."), s.now()), nil
		} else if err != nil {
			return nil, err
		}
	}
	decision := store.ApprovalDecisionApproved
	if !approve {
		decision = store.ApprovalDecisionDeclined
	}
	err = s.inTx(ctx, func(q *store.Queries) error {
		return books.DecideRequest(ctx, q, g.group.ID, r.ID, g.m.ID, g.user.ID, decision, c.req.Str("reason"))
	})
	var cf books.Conflict
	if errors.As(err, &cf) {
		return timed(status(0, cf.Msg), s.now()), nil
	}
	if be, ok := asBooksError(err); ok {
		return timed(status(0, be.Msg), s.now()), nil
	}
	if err != nil {
		return nil, err
	}
	if approve {
		return timed(status(1, "You have successfully approved request"), s.now()), nil
	}
	return timed(status(1, "You have successfully declined withdrawal request"), s.now()), nil
}

// cancelWithdrawalRequest lets whoever asked withdraw a pending request.
func (s *Server) cancelWithdrawalRequest(ctx context.Context, c *call, g *member) (any, error) {
	r, err := s.oneRequest(ctx, c, g)
	if err != nil {
		return nil, err
	}
	switch {
	case r == nil:
		return status(0, "Withdrawal request not found"), nil
	case r.Status == store.WithdrawalRequestStatusApproved || r.Status == store.WithdrawalRequestStatusPaid:
		return status(0, "Request has already been approved therefore can not be canceled"), nil
	case r.Status == store.WithdrawalRequestStatusDeclined || r.Status == store.WithdrawalRequestStatusCancelled:
		return status(0, "Request has already been declined therefore can not be canceled"), nil
	case r.CreatedBy == nil || *r.CreatedBy != g.user.ID:
		return status(0, "You are not allowed to cancel a request you did not request."), nil
	case c.req.Str("reason") == "":
		return status(0, "Kindly give a reason why you want to cancel this request"), nil
	}
	err = s.inTx(ctx, func(q *store.Queries) error {
		return books.CancelRequest(ctx, q, g.group.ID, r.ID, c.req.Str("reason"), store.WithdrawalRequestStatusPending)
	})
	var cf books.Conflict
	if errors.As(err, &cf) {
		return status(0, cf.Msg), nil
	}
	if err != nil {
		return nil, err
	}
	return status(1, "Successfully cancelled this request"), nil
}
