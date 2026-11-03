package books

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/swahome/legion-api/internal/store"
)

// Contribution transfers move money a member has already paid: out of a
// contribution, or off a loan's repayments, into another contribution, a
// fine or a loan, for the same member or another. No cash moves, so there
// are no deposits or withdrawals; the ledger rows and loan rows follow the
// legacy statement types:
//
//	from a contribution: contribution_transfer_out (to a contribution or
//	  fine) or contribution_to_loan_transfer (to a loan), on the source;
//	from a loan: a transfer_out charge on the source loan, which raises
//	  what it owes again, plus loan_to_contribution_transfer or
//	  loan_to_fine_transfer as a memo row;
//	into a contribution: contribution_transfer_in;
//	into a fine: contribution_to_fine_transfer;
//	into a loan: a loan_repayments row with source 'transfer'.
type Transfer struct {
	MemberID           int64
	FromContributionID *int64
	FromLoanID         *int64
	ToMemberID         *int64 // another member; nil for the same member
	ToKind             store.TransferKind
	ToContributionID   *int64 // a contribution, or a contribution's fines
	ToFineCategoryID   *int64
	ToLoanID           *int64
	Amount             string
	Date               *time.Time
	Description        string
}

func invalid(msg string) error { return Error{"invalid_transfer", msg} }

func (t *Transfer) check(ctx context.Context, q *store.Queries, groupID int64) error {
	if err := CheckAmountDate(t.Amount, t.Date, nil, "transfer"); err != nil {
		return err
	}
	inGroup := func(id int64) error {
		return Found("invalid_member", "That member is not in this group.")(
			q.ActiveMembershipInGroup(ctx, store.ActiveMembershipInGroupParams{GroupID: groupID, ID: id}))
	}
	if err := inGroup(t.MemberID); err != nil {
		return err
	}
	to := t.MemberID
	if t.ToMemberID != nil {
		if *t.ToMemberID == t.MemberID {
			return invalid("Pick another member to transfer to.")
		}
		if err := inGroup(*t.ToMemberID); err != nil {
			return err
		}
		to = *t.ToMemberID
	}
	contribution := func(id int64) error {
		return Found("invalid_contribution", "That contribution is not in this group.")(
			q.ContributionInGroup(ctx, store.ContributionInGroupParams{GroupID: groupID, ID: id}))
	}
	memberLoan := func(id, member int64) error {
		l, err := LockLoan(ctx, q, groupID, id)
		if errors.Is(err, ErrNotFound) || (err == nil && (l.MembershipID == nil || *l.MembershipID != member)) {
			return invalid("That loan is not the member's.")
		}
		return err
	}
	switch {
	case (t.FromContributionID == nil) == (t.FromLoanID == nil):
		return invalid("Pick the contribution or loan to transfer from.")
	case t.FromContributionID != nil:
		if err := contribution(*t.FromContributionID); err != nil {
			return err
		}
		paid, err := q.ContributionPaidByMember(ctx, store.ContributionPaidByMemberParams{
			GroupID: groupID, MembershipID: &t.MemberID, ContributionID: t.FromContributionID})
		if err != nil {
			return err
		}
		if Cents(t.Amount) > Cents(paid) {
			return invalid("The member has only paid " + Format(paid) + " into that contribution.")
		}
	default:
		if err := memberLoan(*t.FromLoanID, t.MemberID); err != nil {
			return err
		}
		avail, err := q.LoanRepaidLessTransfers(ctx, *t.FromLoanID)
		if err != nil {
			return err
		}
		if Cents(t.Amount) > Cents(avail) {
			return invalid("Only " + Format(avail) + " has been repaid on that loan.")
		}
	}
	switch t.ToKind {
	case store.TransferKindContribution:
		if t.ToContributionID == nil {
			return invalid("Pick the contribution to transfer to.")
		}
		if t.FromContributionID != nil && *t.FromContributionID == *t.ToContributionID && t.ToMemberID == nil {
			return invalid("You cannot transfer to the same contribution.")
		}
		return contribution(*t.ToContributionID)
	case store.TransferKindFine:
		switch {
		case (t.ToContributionID == nil) == (t.ToFineCategoryID == nil):
			return invalid("Pick the fine to transfer to.")
		case t.ToContributionID != nil:
			return contribution(*t.ToContributionID)
		}
		return Found("invalid_fine_category", "That fine category is not in this group.")(
			q.FineCategoryInGroup(ctx, store.FineCategoryInGroupParams{GroupID: groupID, ID: *t.ToFineCategoryID}))
	case store.TransferKindLoan:
		if t.ToLoanID == nil {
			return invalid("Pick the loan to transfer to.")
		}
		if t.FromLoanID != nil && *t.FromLoanID == *t.ToLoanID {
			return invalid("You cannot transfer to the same loan.")
		}
		return memberLoan(*t.ToLoanID, to)
	}
	return invalid("Pick where to transfer to.")
}

// RecordContributionTransfer checks and writes a contribution transfer.
func RecordContributionTransfer(ctx context.Context, q *store.Queries, groupID, userID int64, t Transfer) (int64, error) {
	if err := t.check(ctx, q, groupID); err != nil {
		return 0, err
	}
	to := t.MemberID
	if t.ToMemberID != nil {
		to = *t.ToMemberID
	}
	desc := strings.TrimSpace(t.Description)
	amount := numeric(t.Amount)
	id, err := q.CreateContributionTransfer(ctx, store.CreateContributionTransferParams{
		GroupID: groupID, MembershipID: t.MemberID, FromContributionID: t.FromContributionID, FromLoanID: t.FromLoanID,
		ToMember: t.ToMemberID != nil, ToKind: t.ToKind, ToMembershipID: to, ToContributionID: t.ToContributionID,
		ToFineCategoryID: t.ToFineCategoryID, ToLoanID: t.ToLoanID, TransferDate: *t.Date, Description: desc,
		CreatedBy: By(userID), Amount: amount,
	})
	if err != nil {
		return 0, err
	}
	entry := func(member int64, contrib, fine *int64, typ store.LedgerEntryType) error {
		return q.CreateTransferLedgerEntry(ctx, store.CreateTransferLedgerEntryParams{
			GroupID: groupID, MembershipID: &member, ContributionID: contrib, FineCategoryID: fine, EntryType: typ,
			EntryDate: *t.Date, Description: desc, ContributionTransferID: &id, CreatedBy: By(userID), Amount: amount,
		})
	}
	// The source.
	if t.FromContributionID != nil {
		typ := store.LedgerEntryTypeContributionTransferOut
		if t.ToKind == store.TransferKindLoan {
			typ = store.LedgerEntryTypeContributionToLoanTransfer
		}
		if err := entry(t.MemberID, t.FromContributionID, nil, typ); err != nil {
			return 0, err
		}
	} else {
		if err := q.CreateTransferCharge(ctx, store.CreateTransferChargeParams{GroupID: groupID, LoanID: *t.FromLoanID,
			InvoiceOn: *t.Date, ContributionTransferID: &id, CreatedBy: By(userID), Amount: amount}); err != nil {
			return 0, err
		}
		switch t.ToKind {
		case store.TransferKindContribution:
			err = entry(t.MemberID, nil, nil, store.LedgerEntryTypeLoanToContributionTransfer)
		case store.TransferKindFine:
			err = entry(t.MemberID, nil, nil, store.LedgerEntryTypeLoanToFineTransfer)
		}
		if err != nil {
			return 0, err
		}
	}
	// The destination.
	switch t.ToKind {
	case store.TransferKindContribution:
		err = entry(to, t.ToContributionID, nil, store.LedgerEntryTypeContributionTransferIn)
	case store.TransferKindFine:
		err = entry(to, t.ToContributionID, t.ToFineCategoryID, store.LedgerEntryTypeContributionToFineTransfer)
	case store.TransferKindLoan:
		err = q.CreateTransferRepayment(ctx, store.CreateTransferRepaymentParams{GroupID: groupID, LoanID: *t.ToLoanID,
			MembershipID: &to, PaidOn: *t.Date, Description: desc, ContributionTransferID: &id, CreatedBy: By(userID),
			Amount: amount})
	}
	return id, err
}

// VoidContributionTransfer voids a transfer and every row it wrote.
func VoidContributionTransfer(ctx context.Context, q *store.Queries, groupID, userID, id int64) error {
	t, err := q.LockContributionTransfer(ctx, store.LockContributionTransferParams{GroupID: groupID, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return Error{"not_found", "Could not find contribution transfer"}
	}
	if err != nil {
		return err
	}
	if t.VoidedAt != nil {
		return Error{"already_voided", "This contribution transfer is already voided"}
	}
	if err := q.VoidContributionTransfer(ctx, store.VoidContributionTransferParams{GroupID: groupID, ID: id, VoidedBy: &userID}); err != nil {
		return err
	}
	if err := q.VoidTransferEntries(ctx, &id); err != nil {
		return err
	}
	if err := q.VoidTransferCharges(ctx, &id); err != nil {
		return err
	}
	return q.VoidTransferRepayments(ctx, store.VoidTransferRepaymentsParams{ContributionTransferID: &id, VoidedBy: &userID})
}
