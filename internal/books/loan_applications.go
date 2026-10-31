package books

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/swahome/legion-api/internal/store"
)

// A loan application goes the way the legacy toggle_loan_application_status
// took it: the guarantors the member named answer first (one decline
// declines it); then, when the loan type needs approval, every signatory
// (members with a group role, as for withdrawals) must approve, one decline
// declines it, and an applicant who is a signatory counts as approving.
// An approved application waits for an official to pay it out, which
// records the loan.

// NewApplication is a member's request for a loan.
type NewApplication struct {
	MemberID     int64
	LoanTypeID   int64
	Amount       string
	Installments int
	Description  string
	Guarantors   []Guarantor
	// ByOfficial lets an official apply with a hidden loan type.
	ByOfficial bool
	// OfficialsOnly is the group's restrict_wallet_approval_to_group_admins.
	OfficialsOnly bool
}

// Apply checks an application against its loan type and files it. It
// returns the application id.
func Apply(ctx context.Context, q *store.Queries, groupID, userID int64, a NewApplication, today time.Time) (int64, error) {
	if a.Amount == "" || !ValidAmount(a.Amount) || Cents(a.Amount) <= 0 {
		return 0, Error{"invalid_amount", "amount must be more than zero."}
	}
	a.Description = strings.TrimSpace(a.Description)
	if err := Found("invalid_member", "That member is not in this group.")(
		q.ActiveMembershipInGroup(ctx, store.ActiveMembershipInGroupParams{GroupID: groupID, ID: a.MemberID})); err != nil {
		return 0, err
	}
	lt, err := applicationType(ctx, q, groupID, a.LoanTypeID)
	if err != nil {
		return 0, err
	}
	if lt.IsHidden && !a.ByOfficial {
		return 0, Error{"invalid_loan_type", "That loan type is not open for applications."}
	}
	if err := checkGuarantors(ctx, q, groupID, a.MemberID, a.Guarantors); err != nil {
		return 0, err
	}
	n := NewLoan{MemberID: a.MemberID, LoanTypeID: &a.LoanTypeID, Amount: a.Amount, Installments: a.Installments,
		Guarantors: a.Guarantors, Date: &today}
	if _, err := n.resolve(ctx, q, groupID); err != nil {
		return 0, err
	}
	var signatories []int64
	if lt.NeedsApproval {
		if signatories, err = q.GroupSignatories(ctx, store.GroupSignatoriesParams{GroupID: groupID, OfficialsOnly: a.OfficialsOnly}); err != nil {
			return 0, err
		}
		if len(signatories) == 0 {
			return 0, Conflict{"not_enough_signatories", "The group has no signatories (members with a group role) to approve loans."}
		}
	}
	status := store.LoanApplicationStatusPendingGuarantors
	if len(a.Guarantors) == 0 {
		status = store.LoanApplicationStatusPendingApproval
	}
	id, err := q.CreateLoanApplication(ctx, store.CreateLoanApplicationParams{
		GroupID: groupID, MembershipID: a.MemberID, LoanTypeID: a.LoanTypeID, Amount: numeric(a.Amount),
		Installments: int32(n.Installments), Description: a.Description, Status: status, CreatedBy: &userID,
	})
	if err != nil {
		return 0, err
	}
	for _, g := range a.Guarantors {
		if err := q.CreateApplicationGuarantor(ctx, store.CreateApplicationGuarantorParams{
			GroupID: groupID, ApplicationID: id, MembershipID: g.MemberID, Amount: numericOrNull(g.Amount), Comment: strings.TrimSpace(g.Comment),
		}); err != nil {
			return 0, err
		}
	}
	if status == store.LoanApplicationStatusPendingApproval {
		return id, toApproval(ctx, q, groupID, id, a.MemberID, userID, lt.NeedsApproval, signatories)
	}
	return id, nil
}

func applicationType(ctx context.Context, q *store.Queries, groupID, id int64) (LoanType, error) {
	t, err := q.GetLoanType(ctx, store.GetLoanTypeParams{GroupID: groupID, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return LoanType{}, Error{"invalid_loan_type", "That loan type is not in this group."}
	}
	if err != nil {
		return LoanType{}, err
	}
	return LoanTypeFrom(t)
}

// toApproval moves an application past its guarantors: to the signatories
// when the loan type needs approval, else straight to approved.
func toApproval(ctx context.Context, q *store.Queries, groupID, id, applicant, userID int64, needsApproval bool, signatories []int64) error {
	if !needsApproval || len(signatories) == 0 {
		return setApplication(ctx, q, id, store.LoanApplicationStatusApproved, "", nil)
	}
	if err := setApplication(ctx, q, id, store.LoanApplicationStatusPendingApproval, "", nil); err != nil {
		return err
	}
	for _, m := range signatories {
		p := store.CreateApplicationApprovalParams{GroupID: groupID, ApplicationID: id, MembershipID: m, Decision: store.ApprovalDecisionPending}
		if m == applicant {
			p.Decision, p.DecidedBy = store.ApprovalDecisionApproved, &userID
		}
		if err := q.CreateApplicationApproval(ctx, p); err != nil {
			return err
		}
	}
	return settleApproval(ctx, q, id)
}

func settleApproval(ctx context.Context, q *store.Queries, id int64) error {
	t, err := q.ApplicationTally(ctx, id)
	if err != nil {
		return err
	}
	switch {
	case t.SignatoriesDeclined > 0:
		return setApplication(ctx, q, id, store.LoanApplicationStatusDeclined, "Declined by a signatory.", nil)
	case t.SignatoriesApproved == t.Signatories:
		return setApplication(ctx, q, id, store.LoanApplicationStatusApproved, "", nil)
	}
	return nil
}

func setApplication(ctx context.Context, q *store.Queries, id int64, s store.LoanApplicationStatus, reason string, loanID *int64) error {
	return q.SetLoanApplicationStatus(ctx, store.SetLoanApplicationStatusParams{ID: id, Status: s, DeclineReason: reason, LoanID: loanID})
}

// LockApplication loads an application for update, or ErrNotFound.
func LockApplication(ctx context.Context, q *store.Queries, groupID, id int64) (store.LoanApplication, error) {
	a, err := q.GetLoanApplicationForUpdate(ctx, store.GetLoanApplicationForUpdateParams{GroupID: groupID, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return a, ErrNotFound
	}
	return a, err
}

func closed(a store.LoanApplication) error {
	return Conflict{"application_closed", "This application is already " + strings.ReplaceAll(string(a.Status), "_", " ") + "."}
}

// AnswerGuarantee records a guarantor's answer. A decline needs a reason
// and declines the application; the last approval sends it on.
func AnswerGuarantee(ctx context.Context, q *store.Queries, groupID, id, membershipID, userID int64, approve bool, reason string, officialsOnly bool) error {
	reason = strings.TrimSpace(reason)
	if !approve && reason == "" {
		return Error{"missing_fields", "Say why you are declining."}
	}
	a, err := LockApplication(ctx, q, groupID, id)
	if err != nil {
		return err
	}
	if a.Status != store.LoanApplicationStatusPendingGuarantors {
		return closed(a)
	}
	decision := store.ApprovalDecisionApproved
	if !approve {
		decision = store.ApprovalDecisionDeclined
	}
	n, err := q.DecideApplicationGuarantor(ctx, store.DecideApplicationGuarantorParams{
		Decision: decision, Reason: reason, ApplicationID: id, MembershipID: membershipID,
	})
	if err != nil {
		return err
	}
	if n == 0 {
		return Conflict{"not_a_guarantor", "You are not a guarantor on this application, or you have already answered."}
	}
	if !approve {
		return setApplication(ctx, q, id, store.LoanApplicationStatusDeclined, "A guarantor declined: "+reason, nil)
	}
	t, err := q.ApplicationTally(ctx, id)
	if err != nil || t.GuarantorsApproved < t.Guarantors {
		return err
	}
	lt, err := applicationType(ctx, q, groupID, a.LoanTypeID)
	if err != nil {
		return err
	}
	var signatories []int64
	if lt.NeedsApproval {
		if signatories, err = q.GroupSignatories(ctx, store.GroupSignatoriesParams{GroupID: groupID, OfficialsOnly: officialsOnly}); err != nil {
			return err
		}
	}
	return toApproval(ctx, q, groupID, id, a.MembershipID, userID, lt.NeedsApproval, signatories)
}

// DecideApplication records a signatory's answer. The caller checks the
// password.
func DecideApplication(ctx context.Context, q *store.Queries, groupID, id, membershipID, userID int64, approve bool, comment string) error {
	comment = strings.TrimSpace(comment)
	if !approve && comment == "" {
		return Error{"missing_fields", "Say why you are declining."}
	}
	a, err := LockApplication(ctx, q, groupID, id)
	if err != nil {
		return err
	}
	if a.Status != store.LoanApplicationStatusPendingApproval {
		return closed(a)
	}
	decision := store.ApprovalDecisionApproved
	if !approve {
		decision = store.ApprovalDecisionDeclined
	}
	n, err := q.DecideApplicationApproval(ctx, store.DecideApplicationApprovalParams{
		Decision: decision, Comment: comment, DecidedBy: &userID, ApplicationID: id, MembershipID: membershipID,
	})
	if err != nil {
		return err
	}
	if n == 0 {
		return Conflict{"not_a_signatory", "You are not a signatory on this application, or you have already answered."}
	}
	if !approve {
		return setApplication(ctx, q, id, store.LoanApplicationStatusDeclined, "Declined by a signatory: "+comment, nil)
	}
	return settleApproval(ctx, q, id)
}

// CloseApplication ends an open application: the applicant cancels it, or
// an official declines it.
func CloseApplication(ctx context.Context, q *store.Queries, groupID, id int64, byOfficial bool, reason string) error {
	a, err := LockApplication(ctx, q, groupID, id)
	if err != nil {
		return err
	}
	switch a.Status {
	case store.LoanApplicationStatusPendingGuarantors, store.LoanApplicationStatusPendingApproval, store.LoanApplicationStatusApproved:
	default:
		return closed(a)
	}
	reason = strings.TrimSpace(reason)
	if byOfficial {
		if reason == "" {
			return Error{"missing_fields", "Say why the application is declined."}
		}
		return setApplication(ctx, q, id, store.LoanApplicationStatusDeclined, reason, nil)
	}
	if reason == "" {
		reason = "Cancelled by the member."
	}
	return setApplication(ctx, q, id, store.LoanApplicationStatusCancelled, reason, nil)
}

// Disbursement is how an official pays out an approved application.
type Disbursement struct {
	AccountID   int64
	Method      *string
	Date        *time.Time
	FirstDueOn  *time.Time
	Description string
	// SkipProcessingFee pays out without the type's processing fee.
	SkipProcessingFee bool
}

// DisburseApplication records the loan for an approved application with
// the guarantors who agreed, and marks the application disbursed. It
// returns the loan id.
func DisburseApplication(ctx context.Context, q *store.Queries, groupID, userID, id int64, d Disbursement) (int64, error) {
	a, err := LockApplication(ctx, q, groupID, id)
	if err != nil {
		return 0, err
	}
	if a.Status != store.LoanApplicationStatusApproved {
		if a.Status == store.LoanApplicationStatusPendingGuarantors || a.Status == store.LoanApplicationStatusPendingApproval {
			return 0, Conflict{"not_approved", "This application is not approved yet."}
		}
		return 0, closed(a)
	}
	gs, err := q.ApplicationGuarantors(ctx, id)
	if err != nil {
		return 0, err
	}
	var guarantors []Guarantor
	for _, g := range gs {
		if g.Decision == store.ApprovalDecisionApproved {
			var amount *string
			if !g.NoAmount {
				amount = &g.Amount
			}
			guarantors = append(guarantors, Guarantor{MemberID: g.MembershipID, Amount: amount, Comment: g.Comment})
		}
	}
	desc := strings.TrimSpace(d.Description)
	if desc == "" {
		desc = a.Description
	}
	typeID := a.LoanTypeID
	loanID, err := RecordLoan(ctx, q, groupID, userID, NewLoan{
		MemberID: a.MembershipID, LoanTypeID: &typeID, AccountID: d.AccountID, Method: d.Method, Amount: NumText(a.Amount),
		Date: d.Date, Installments: int(a.Installments), FirstDueOn: d.FirstDueOn, Description: desc,
		Guarantors: guarantors, SkipProcessingFee: d.SkipProcessingFee,
	})
	if err != nil {
		return 0, err
	}
	return loanID, setApplication(ctx, q, id, store.LoanApplicationStatusDisbursed, "", &loanID)
}
