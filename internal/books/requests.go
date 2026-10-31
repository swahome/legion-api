package books

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/swahome/legion-api/internal/store"
)

// Withdrawal requests: the legacy e-wallet's "request funds transfer",
// approved by every signatory (see docs/schema/money.md).

// Conflict is a refusal because of the request's or group's state, not the
// input: an HTTP 409 on the web.
type Conflict struct{ Code, Msg string }

func (e Conflict) Error() string { return e.Msg }

// Recipient is who a withdrawal request pays.
type Recipient struct {
	Type          string `json:"type"`
	Phone         string `json:"phone,omitempty"`
	Paybill       string `json:"paybill,omitempty"`
	AccountNumber string `json:"account_number,omitempty"`
	AccountName   string `json:"account_name,omitempty"`
	BankID        int64  `json:"bank_id,omitempty"`
}

// Check trims the recipient and checks it names someone to pay.
func (rc *Recipient) Check() error {
	rc.Phone, rc.Paybill = strings.TrimSpace(rc.Phone), strings.TrimSpace(rc.Paybill)
	rc.AccountNumber, rc.AccountName = strings.TrimSpace(rc.AccountNumber), strings.TrimSpace(rc.AccountName)
	switch rc.Type {
	case "mobile":
		if rc.Phone == "" {
			return Error{"invalid_recipient", "A mobile money recipient needs a phone number."}
		}
	case "paybill":
		if rc.Paybill == "" {
			return Error{"invalid_recipient", "A paybill recipient needs the paybill number."}
		}
	case "bank":
		if rc.BankID == 0 || rc.AccountNumber == "" {
			return Error{"invalid_recipient", "A bank recipient needs the bank and account number."}
		}
	default:
		return Error{"invalid_recipient", "recipient.type must be mobile, paybill or bank."}
	}
	return nil
}

// NewRequest is a request to pay money out.
type NewRequest struct {
	Kind              store.WithdrawalKind
	AccountID         int64
	ToAccountID       *int64 // funds transfers
	Amount            string
	Description       string
	ExpenseCategoryID *int64
	MemberID          *int64
	ContributionID    *int64
	Recipient         *Recipient
	RequestedBy       *int64 // the asking official's membership
	OfficialsOnly     bool   // the group's restrict_wallet_approval_to_group_admins
}

const refAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

func referenceNumber() string {
	b := make([]byte, 10)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = refAlphabet[int(b[i])%len(refAlphabet)]
	}
	return string(b)
}

// CreateRequest validates a request, sends it to every signatory and
// counts the requester's own approval.
func CreateRequest(ctx context.Context, q *store.Queries, groupID, userID int64, req NewRequest) (int64, error) {
	if req.Amount == "" || !ValidAmount(req.Amount) || Cents(req.Amount) <= 0 {
		return 0, Error{"invalid_amount", "amount must be more than zero."}
	}
	req.Description = strings.TrimSpace(req.Description)
	acct, err := OpenAccount(ctx, q, groupID, req.AccountID)
	if err != nil {
		return 0, err
	}
	switch {
	case req.Kind == store.WithdrawalKindFundsTransfer:
		if req.ToAccountID == nil {
			return 0, Error{"missing_fields", "to_account_id is required for a transfer."}
		}
		if req.ExpenseCategoryID != nil || req.MemberID != nil || req.ContributionID != nil || req.Recipient != nil {
			return 0, Error{"invalid_body", "A transfer only names the two accounts."}
		}
		if _, _, err := TransferAccounts(ctx, q, groupID, acct.ID, *req.ToAccountID); err != nil {
			return 0, err
		}
	case RecordableWithdrawals[req.Kind]:
		if req.ToAccountID != nil {
			return 0, Error{"invalid_body", "to_account_id is only for transfers."}
		}
		if err := CheckWithdrawal(ctx, q, groupID, Withdrawal{Kind: req.Kind, Amount: req.Amount, MemberID: req.MemberID,
			ContributionID: req.ContributionID, ExpenseCategoryID: req.ExpenseCategoryID}); err != nil {
			return 0, err
		}
		if req.Recipient == nil {
			return 0, Error{"missing_fields", "recipient is required: who is being paid."}
		}
		if err := req.Recipient.Check(); err != nil {
			return 0, err
		}
	default:
		return 0, Error{"invalid_kind", "kind must be expense, contribution_refund or funds_transfer."}
	}
	if err := CheckBalance(ctx, q, acct, req.Amount); err != nil {
		return 0, err
	}
	signatories, err := q.GroupSignatories(ctx, store.GroupSignatoriesParams{GroupID: groupID, OfficialsOnly: req.OfficialsOnly})
	if err != nil {
		return 0, err
	}
	if len(signatories) < 2 {
		return 0, Conflict{"not_enough_signatories", "The group needs at least two signatories (members with a group role) to approve withdrawals."}
	}
	rcp := []byte("{}")
	if req.Recipient != nil {
		rcp, _ = json.Marshal(req.Recipient)
	}
	id, err := q.CreateWithdrawalRequest(ctx, store.CreateWithdrawalRequestParams{
		GroupID: groupID, ReferenceNumber: referenceNumber(), Kind: req.Kind, RequestedBy: req.RequestedBy,
		AccountID: &acct.ID, ToAccountID: req.ToAccountID, Amount: numeric(req.Amount), Description: req.Description,
		ExpenseCategoryID: req.ExpenseCategoryID, MembershipID: req.MemberID, ContributionID: req.ContributionID,
		Recipient: rcp, CreatedBy: &userID,
	})
	if err != nil {
		return 0, err
	}
	for _, m := range signatories {
		p := store.CreateWithdrawalApprovalParams{
			GroupID: groupID, WithdrawalRequestID: id, MembershipID: m, Decision: store.ApprovalDecisionPending,
		}
		if req.RequestedBy != nil && *req.RequestedBy == m {
			p.Decision, p.DecidedBy = store.ApprovalDecisionApproved, &userID
		}
		if err := q.CreateWithdrawalApproval(ctx, p); err != nil {
			return 0, err
		}
	}
	return id, nil
}

// LockRequest loads a request for update, or ErrNotFound.
func LockRequest(ctx context.Context, q *store.Queries, groupID, id int64) (store.WithdrawalRequest, error) {
	x, err := q.GetWithdrawalRequestForUpdate(ctx, store.GetWithdrawalRequestForUpdateParams{GroupID: groupID, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return x, ErrNotFound
	}
	return x, err
}

// DecideRequest records a signatory's answer and settles the request when
// that answer decides it: one decline declines it, the last approval
// approves it. The caller checks who is answering (and their password).
func DecideRequest(ctx context.Context, q *store.Queries, groupID, requestID, membershipID, userID int64,
	decision store.ApprovalDecision, reason string) error {
	reason = strings.TrimSpace(reason)
	if decision == store.ApprovalDecisionDeclined && reason == "" {
		return Error{"missing_fields", "Say why you are declining."}
	}
	x, err := LockRequest(ctx, q, groupID, requestID)
	if err != nil {
		return err
	}
	if x.Status != store.WithdrawalRequestStatusPending {
		return Conflict{"request_closed", "This request is already " + string(x.Status) + "."}
	}
	n, err := q.DecideWithdrawalApproval(ctx, store.DecideWithdrawalApprovalParams{
		Decision: decision, Comment: reason, DecidedBy: &userID, WithdrawalRequestID: x.ID, MembershipID: membershipID,
	})
	if err != nil {
		return err
	}
	if n == 0 {
		return Conflict{"not_a_signatory", "You are not a signatory on this request, or you have already answered."}
	}
	t, err := q.ApprovalTally(ctx, x.ID)
	if err != nil {
		return err
	}
	switch {
	case t.Declined > 0:
		return q.SetWithdrawalRequestStatus(ctx, store.SetWithdrawalRequestStatusParams{
			GroupID: groupID, ID: x.ID, Status: store.WithdrawalRequestStatusDeclined, DeclineReason: &reason,
		})
	case t.Approved == t.Total:
		return q.SetWithdrawalRequestStatus(ctx, store.SetWithdrawalRequestStatusParams{
			GroupID: groupID, ID: x.ID, Status: store.WithdrawalRequestStatusApproved,
		})
	}
	return nil
}

// CancelRequest withdraws a request in one of the allowed states.
func CancelRequest(ctx context.Context, q *store.Queries, groupID, requestID int64, reason string,
	allowed ...store.WithdrawalRequestStatus) error {
	x, err := LockRequest(ctx, q, groupID, requestID)
	if err != nil {
		return err
	}
	ok := false
	for _, s := range allowed {
		ok = ok || x.Status == s
	}
	if !ok {
		return Conflict{"request_closed", "This request is already " + string(x.Status) + "."}
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "Cancelled"
	}
	return q.SetWithdrawalRequestStatus(ctx, store.SetWithdrawalRequestStatusParams{
		GroupID: groupID, ID: x.ID, Status: store.WithdrawalRequestStatusCancelled, DeclineReason: &reason,
	})
}
