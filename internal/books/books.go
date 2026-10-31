// Package books writes the group's money movements: deposits, withdrawals,
// transfers between accounts and fines. Each write keeps the deposit or
// withdrawal, its cash book entry (account_entries) and the member's ledger
// entry together. The web API and the mobile API both write through here.
//
// Every function takes Queries bound to the caller's transaction.
package books

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/swahome/legion-api/internal/store"
)

// Error is a validation failure the caller can show: a stable code and a
// sentence.
type Error struct{ Code, Msg string }

func (e Error) Error() string { return e.Msg }

// ErrNotFound means the row to void does not exist in the group.
var ErrNotFound = errors.New("books: not found")

var amountPattern = regexp.MustCompile(`^\d{1,12}(\.\d{1,2})?$`)

// ValidAmount reports whether s is a plain non-negative amount with at most
// two decimals, as stored.
func ValidAmount(s string) bool { return amountPattern.MatchString(s) }

func numeric(s string) pgtype.Numeric {
	var n pgtype.Numeric
	_ = n.Scan(s)
	return n
}

// Format normalises a numeric rendered as text to two decimals.
func Format(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "0.00"
	}
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" {
		whole = "0"
	}
	frac = (frac + "00")[:2]
	out := whole + "." + frac
	if neg && out != "0.00" {
		out = "-" + out
	}
	return out
}

// Cents parses a decimal money string ("-4200.5") into whole cents.
func Cents(s string) int64 {
	s = Format(s)
	neg := strings.HasPrefix(s, "-")
	whole, frac, _ := strings.Cut(strings.TrimPrefix(s, "-"), ".")
	var n int64
	for _, c := range whole + frac {
		n = n*10 + int64(c-'0')
	}
	if neg {
		return -n
	}
	return n
}

// Nairobi is the groups' time zone, used for "not in the future".
func Nairobi() *time.Location {
	loc, err := time.LoadLocation("Africa/Nairobi")
	if err != nil {
		return time.FixedZone("EAT", 3*3600)
	}
	return loc
}

// CheckAmountDate validates the fields every movement of money has. An
// empty amount or a nil date counts as missing.
func CheckAmountDate(amount string, date *time.Time, method *string, noun string) error {
	if amount == "" || !ValidAmount(amount) || Cents(amount) <= 0 {
		return Error{"invalid_amount", "amount must be more than zero."}
	}
	if date == nil {
		return Error{"missing_fields", "date is required."}
	}
	y, m, d := time.Now().In(Nairobi()).Date()
	if date.After(time.Date(y, m, d, 0, 0, 0, 0, time.UTC)) {
		return Error{"invalid_date", "A " + noun + " cannot be dated in the future."}
	}
	if method != nil && !store.PaymentMethod(*method).Valid() {
		return Error{"invalid_method", "Unknown payment method."}
	}
	return nil
}

// OpenAccount loads one of the group's accounts that can take new entries.
func OpenAccount(ctx context.Context, q *store.Queries, groupID, id int64) (store.GroupAccount, error) {
	acct, err := q.GetGroupAccount(ctx, store.GetGroupAccountParams{GroupID: groupID, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return acct, Error{"invalid_account", "Pick one of the group's accounts."}
	}
	if err != nil {
		return acct, err
	}
	if acct.IsClosed {
		return acct, Error{"account_closed", "That account is closed."}
	}
	return acct, nil
}

// Found turns an existence check into an Error when the row is missing:
// Found(code, msg)(q.SomethingInGroup(ctx, ...)).
func Found(code, msg string) func(bool, error) error {
	return func(ok bool, err error) error {
		if err != nil {
			return err
		}
		if !ok {
			return Error{code, msg}
		}
		return nil
	}
}

// CheckBalance refuses to take an account below zero.
func CheckBalance(ctx context.Context, q *store.Queries, acct store.GroupAccount, amount string) error {
	bal, err := q.AccountBalance(ctx, store.AccountBalanceParams{GroupID: acct.GroupID, ID: acct.ID})
	if err != nil {
		return err
	}
	if Cents(amount) > Cents(bal) {
		return Error{"insufficient_balance", acct.Name + " holds " + Format(bal) + ", which is not enough for this."}
	}
	return nil
}

// ---- Deposits ----

// Deposit is money into a group account.
type Deposit struct {
	Kind             store.DepositKind
	AccountID        int64
	Method           *string
	Amount           string
	Date             *time.Time
	Description      string
	MemberID         *int64
	ContributionID   *int64
	FineCategoryID   *int64
	IncomeCategoryID *int64
	DepositorID      *int64
}

// RecordableDeposits are the kinds that can be recorded by hand so far, with
// the ledger entry a member payment writes. Loan repayments arrive with
// loans (milestone 4); stock, asset and money market sales with milestone 8.
var RecordableDeposits = map[store.DepositKind]store.LedgerEntryType{
	store.DepositKindContribution: store.LedgerEntryTypeContributionPayment,
	store.DepositKindFine:         store.LedgerEntryTypeFinePayment,
	store.DepositKindMisc:         store.LedgerEntryTypeMiscPayment,
	store.DepositKindIncome:       "",
}

// CheckDeposit validates a deposit and the references it makes, and
// returns the account it goes into.
func CheckDeposit(ctx context.Context, q *store.Queries, groupID int64, d Deposit) (store.GroupAccount, error) {
	if _, ok := RecordableDeposits[d.Kind]; !ok {
		return store.GroupAccount{}, Error{"invalid_kind", "kind must be contribution, fine, misc or income."}
	}
	if err := CheckAmountDate(d.Amount, d.Date, d.Method, "deposit"); err != nil {
		return store.GroupAccount{}, err
	}
	acct, err := OpenAccount(ctx, q, groupID, d.AccountID)
	if err != nil {
		return acct, err
	}
	member := d.Kind != store.DepositKindIncome
	switch {
	case member && d.MemberID == nil:
		return acct, Error{"missing_fields", "member_id is required for a member payment."}
	case !member && d.MemberID != nil:
		return acct, Error{"invalid_body", "Income is not linked to a member; use a depositor."}
	case d.Kind == store.DepositKindContribution && d.ContributionID == nil:
		return acct, Error{"missing_fields", "contribution_id is required."}
	case d.Kind == store.DepositKindFine && (d.FineCategoryID == nil) == (d.ContributionID == nil):
		return acct, Error{"missing_fields", "A fine payment needs either fine_category_id or contribution_id (for a contribution's late fine)."}
	case d.Kind == store.DepositKindIncome && d.IncomeCategoryID == nil:
		return acct, Error{"missing_fields", "income_category_id is required."}
	case d.Kind == store.DepositKindMisc && (d.ContributionID != nil || d.FineCategoryID != nil):
		return acct, Error{"invalid_body", "A miscellaneous payment has no contribution or fine."}
	case d.Kind != store.DepositKindFine && d.Kind != store.DepositKindContribution && d.ContributionID != nil,
		d.Kind != store.DepositKindFine && d.FineCategoryID != nil,
		d.Kind != store.DepositKindIncome && (d.IncomeCategoryID != nil || d.DepositorID != nil):
		return acct, Error{"invalid_body", "That field does not apply to this kind of deposit."}
	}
	checks := []struct {
		id        *int64
		code, msg string
		f         func(int64) (bool, error)
	}{
		{d.MemberID, "invalid_member", "That member is not in this group.", func(id int64) (bool, error) {
			return q.ActiveMembershipInGroup(ctx, store.ActiveMembershipInGroupParams{GroupID: groupID, ID: id})
		}},
		{d.ContributionID, "invalid_contribution", "That contribution is not in this group.", func(id int64) (bool, error) {
			return q.ContributionInGroup(ctx, store.ContributionInGroupParams{GroupID: groupID, ID: id})
		}},
		{d.FineCategoryID, "invalid_fine_category", "That fine category is not in this group.", func(id int64) (bool, error) {
			return q.FineCategoryInGroup(ctx, store.FineCategoryInGroupParams{GroupID: groupID, ID: id})
		}},
		{d.IncomeCategoryID, "invalid_income_category", "That income category is not in this group.", func(id int64) (bool, error) {
			return q.IncomeCategoryInGroup(ctx, store.IncomeCategoryInGroupParams{GroupID: groupID, ID: id})
		}},
		{d.DepositorID, "invalid_depositor", "That depositor is not in this group.", func(id int64) (bool, error) {
			return q.DepositorInGroup(ctx, store.DepositorInGroupParams{GroupID: groupID, ID: id})
		}},
	}
	for _, c := range checks {
		if c.id == nil {
			continue
		}
		if err := Found(c.code, c.msg)(c.f(*c.id)); err != nil {
			return acct, err
		}
	}
	return acct, nil
}

// RecordDeposit validates and writes a deposit, its cash book entry and,
// for a member payment, the member's ledger entry.
func RecordDeposit(ctx context.Context, q *store.Queries, groupID, userID int64, d Deposit) (int64, error) {
	d.Description = strings.TrimSpace(d.Description)
	acct, err := CheckDeposit(ctx, q, groupID, d)
	if err != nil {
		return 0, err
	}
	p := store.CreateDepositParams{
		GroupID: groupID, AccountID: &acct.ID, AccountKind: acct.Kind, Kind: d.Kind,
		MembershipID: d.MemberID, ContributionID: d.ContributionID, FineCategoryID: d.FineCategoryID,
		IncomeCategoryID: d.IncomeCategoryID, DepositorID: d.DepositorID, Amount: numeric(d.Amount),
		DepositDate: *d.Date, Description: d.Description, CreatedBy: &userID,
	}
	if d.Method != nil {
		p.Method = store.NullPaymentMethod{PaymentMethod: store.PaymentMethod(*d.Method), Valid: true}
	}
	id, err := q.CreateDeposit(ctx, p)
	if err != nil {
		return 0, err
	}
	if err := q.CreateAccountEntry(ctx, store.CreateAccountEntryParams{
		GroupID: groupID, AccountID: &acct.ID, AccountKind: acct.Kind, Direction: 1, Amount: p.Amount,
		EntryDate: p.DepositDate, Description: p.Description, DepositID: &id, MembershipID: d.MemberID,
	}); err != nil {
		return 0, err
	}
	if entry := RecordableDeposits[d.Kind]; entry != "" {
		if err := q.CreatePaymentLedgerEntry(ctx, store.CreatePaymentLedgerEntryParams{
			GroupID: groupID, MembershipID: d.MemberID, ContributionID: d.ContributionID,
			FineCategoryID: d.FineCategoryID, EntryType: entry, Amount: p.Amount, EntryDate: p.DepositDate,
			Description: p.Description, DepositID: &id,
		}); err != nil {
			return 0, err
		}
	}
	return id, nil
}

// VoidDeposit voids a deposit with its cash book and ledger entries. It
// returns ErrNotFound, or Error already_voided.
func VoidDeposit(ctx context.Context, q *store.Queries, groupID, id, userID int64) error {
	if _, err := q.GetDeposit(ctx, store.GetDepositParams{GroupID: groupID, ID: id}); errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	n, err := q.VoidDeposit(ctx, store.VoidDepositParams{GroupID: groupID, ID: id, VoidedBy: &userID})
	if err != nil {
		return err
	}
	if n == 0 {
		return Error{"already_voided", "This deposit is already voided."}
	}
	if err := q.VoidDepositEntries(ctx, &id); err != nil {
		return err
	}
	// A loan repayment's deposit takes the repayment with it.
	if err := q.VoidRepaymentByDeposit(ctx, store.VoidRepaymentByDepositParams{DepositID: &id, VoidedBy: &userID}); err != nil {
		return err
	}
	return q.VoidDepositLedger(ctx, &id)
}

// ---- Withdrawals ----

// Withdrawal is money that has left an account (or, for a withdrawal
// request, is to leave it).
type Withdrawal struct {
	Kind              store.WithdrawalKind
	Amount            string
	MemberID          *int64
	ContributionID    *int64
	ExpenseCategoryID *int64
}

// RecordableWithdrawals can be recorded by hand so far. Loan disbursements
// and repayments arrive with loans, dividends with investments. Funds
// transfers are account transfers.
var RecordableWithdrawals = map[store.WithdrawalKind]bool{
	store.WithdrawalKindExpense:            true,
	store.WithdrawalKindContributionRefund: true,
}

// CheckWithdrawal validates the references a withdrawal makes.
func CheckWithdrawal(ctx context.Context, q *store.Queries, groupID int64, m Withdrawal) error {
	switch m.Kind {
	case store.WithdrawalKindExpense:
		if m.ExpenseCategoryID == nil {
			return Error{"missing_fields", "expense_category_id is required for an expense."}
		}
		if m.MemberID != nil || m.ContributionID != nil {
			return Error{"invalid_body", "An expense is not linked to a member or contribution."}
		}
		return Found("invalid_expense_category", "That expense category is not in this group.")(
			q.ExpenseCategoryInGroup(ctx, store.ExpenseCategoryInGroupParams{GroupID: groupID, ID: *m.ExpenseCategoryID}))
	case store.WithdrawalKindContributionRefund:
		if m.MemberID == nil || m.ContributionID == nil {
			return Error{"missing_fields", "member_id and contribution_id are required for a refund."}
		}
		if m.ExpenseCategoryID != nil {
			return Error{"invalid_body", "A refund has no expense category."}
		}
		if err := Found("invalid_member", "That member is not in this group.")(
			q.ActiveMembershipInGroup(ctx, store.ActiveMembershipInGroupParams{GroupID: groupID, ID: *m.MemberID})); err != nil {
			return err
		}
		if err := Found("invalid_contribution", "That contribution is not in this group.")(
			q.ContributionInGroup(ctx, store.ContributionInGroupParams{GroupID: groupID, ID: *m.ContributionID})); err != nil {
			return err
		}
		paid, err := q.ContributionPaidByMember(ctx, store.ContributionPaidByMemberParams{
			GroupID: groupID, MembershipID: m.MemberID, ContributionID: m.ContributionID,
		})
		if err != nil {
			return err
		}
		if Cents(m.Amount) > Cents(paid) {
			return Error{"refund_too_large", "The member has only paid " + Format(paid) + " into this contribution."}
		}
		return nil
	}
	return Error{"invalid_kind", "kind must be expense or contribution_refund."}
}

// WriteWithdrawal writes a checked withdrawal, its cash book entry and, for
// a refund, the member's ledger entry.
func WriteWithdrawal(ctx context.Context, q *store.Queries, acct store.GroupAccount, m Withdrawal,
	method *string, date time.Time, description string, userID int64) (int64, error) {
	p := store.CreateWithdrawalParams{
		GroupID: acct.GroupID, AccountID: &acct.ID, AccountKind: acct.Kind, Kind: m.Kind,
		MembershipID: m.MemberID, ContributionID: m.ContributionID, ExpenseCategoryID: m.ExpenseCategoryID,
		Amount: numeric(m.Amount), WithdrawalDate: date, Description: description, CreatedBy: &userID,
	}
	if method != nil {
		p.Method = store.NullPaymentMethod{PaymentMethod: store.PaymentMethod(*method), Valid: true}
	}
	id, err := q.CreateWithdrawal(ctx, p)
	if err != nil {
		return 0, err
	}
	if err := q.CreateAccountEntry(ctx, store.CreateAccountEntryParams{
		GroupID: acct.GroupID, AccountID: &acct.ID, AccountKind: acct.Kind, Direction: -1, Amount: p.Amount,
		EntryDate: date, Description: description, WithdrawalID: &id, MembershipID: m.MemberID,
	}); err != nil {
		return 0, err
	}
	if m.Kind == store.WithdrawalKindContributionRefund {
		if err := q.CreateRefundLedgerEntry(ctx, store.CreateRefundLedgerEntryParams{
			GroupID: acct.GroupID, MembershipID: m.MemberID, ContributionID: m.ContributionID,
			Amount: p.Amount, EntryDate: date, Description: description, WithdrawalID: &id,
		}); err != nil {
			return 0, err
		}
	}
	return id, nil
}

// RecordWithdrawal validates and writes a withdrawal that has happened.
func RecordWithdrawal(ctx context.Context, q *store.Queries, groupID, userID, accountID int64, m Withdrawal,
	method *string, date *time.Time, description string) (int64, error) {
	if !RecordableWithdrawals[m.Kind] {
		return 0, Error{"invalid_kind", "kind must be expense or contribution_refund."}
	}
	if err := CheckAmountDate(m.Amount, date, method, "withdrawal"); err != nil {
		return 0, err
	}
	acct, err := OpenAccount(ctx, q, groupID, accountID)
	if err != nil {
		return 0, err
	}
	if err := CheckWithdrawal(ctx, q, groupID, m); err != nil {
		return 0, err
	}
	return WriteWithdrawal(ctx, q, acct, m, method, *date, strings.TrimSpace(description), userID)
}

// VoidWithdrawal voids a withdrawal with its cash book and ledger entries.
// A loan's disbursement is voided with the loan (VoidLoan), not alone.
func VoidWithdrawal(ctx context.Context, q *store.Queries, groupID, id, userID int64) error {
	wd, err := q.GetWithdrawal(ctx, store.GetWithdrawalParams{GroupID: groupID, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if wd.LoanID != nil && wd.Kind == store.WithdrawalKindLoanDisbursement && wd.VoidedAt == nil {
		return Error{"loan_disbursement", "This is a loan being paid out: void the loan instead."}
	}
	return voidWithdrawal(ctx, q, groupID, id, userID)
}

func voidWithdrawal(ctx context.Context, q *store.Queries, groupID, id, userID int64) error {
	n, err := q.VoidWithdrawal(ctx, store.VoidWithdrawalParams{GroupID: groupID, ID: id, VoidedBy: &userID})
	if err != nil {
		return err
	}
	if n == 0 {
		return Error{"already_voided", "This withdrawal is already voided."}
	}
	if err := q.VoidWithdrawalEntries(ctx, &id); err != nil {
		return err
	}
	return q.VoidWithdrawalLedger(ctx, &id)
}

// ---- Transfers between the group's own accounts ----

// TransferAccounts checks both sides of a transfer.
func TransferAccounts(ctx context.Context, q *store.Queries, groupID, fromID, toID int64) (from, to store.GroupAccount, err error) {
	if fromID == toID {
		return from, to, Error{"same_account", "Pick two different accounts."}
	}
	if from, err = OpenAccount(ctx, q, groupID, fromID); err != nil {
		return from, to, err
	}
	to, err = OpenAccount(ctx, q, groupID, toID)
	return from, to, err
}

// WriteTransfer writes a checked transfer and its two cash book entries.
func WriteTransfer(ctx context.Context, q *store.Queries, from, to store.GroupAccount, amount string,
	date time.Time, description string, userID int64) (int64, error) {
	n := numeric(amount)
	id, err := q.CreateAccountTransfer(ctx, store.CreateAccountTransferParams{
		GroupID: from.GroupID, FromAccountID: &from.ID, ToAccountID: &to.ID, Amount: n,
		TransferDate: date, Description: description, CreatedBy: &userID,
	})
	if err != nil {
		return 0, err
	}
	for _, leg := range []struct {
		acct store.GroupAccount
		dir  int16
	}{{from, -1}, {to, 1}} {
		if err := q.CreateAccountEntry(ctx, store.CreateAccountEntryParams{
			GroupID: from.GroupID, AccountID: &leg.acct.ID, AccountKind: leg.acct.Kind, Direction: leg.dir,
			Amount: n, EntryDate: date, Description: description, AccountTransferID: &id,
		}); err != nil {
			return 0, err
		}
	}
	return id, nil
}

// RecordTransfer validates and writes a transfer between two accounts.
func RecordTransfer(ctx context.Context, q *store.Queries, groupID, userID, fromID, toID int64, amount string,
	date *time.Time, description string) (int64, error) {
	if err := CheckAmountDate(amount, date, nil, "transfer"); err != nil {
		return 0, err
	}
	from, to, err := TransferAccounts(ctx, q, groupID, fromID, toID)
	if err != nil {
		return 0, err
	}
	return WriteTransfer(ctx, q, from, to, amount, *date, strings.TrimSpace(description), userID)
}

// VoidTransfer voids a transfer and both its cash book entries.
func VoidTransfer(ctx context.Context, q *store.Queries, groupID, id, userID int64) error {
	ok, err := q.AccountTransferExists(ctx, store.AccountTransferExistsParams{GroupID: groupID, ID: id})
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	n, err := q.VoidAccountTransfer(ctx, store.VoidAccountTransferParams{GroupID: groupID, ID: id, VoidedBy: &userID})
	if err != nil {
		return err
	}
	if n == 0 {
		return Error{"already_voided", "This transfer is already voided."}
	}
	return q.VoidAccountTransferEntries(ctx, &id)
}

// ---- Fines ----

// MaxFinedMembers caps one "fine members" request.
const MaxFinedMembers = 500

// FineMembers raises the same fine on each member (duplicates ignored) and
// returns the new fine ids.
func FineMembers(ctx context.Context, q *store.Queries, groupID, userID int64, memberIDs []int64,
	fineCategoryID int64, amount string, date *time.Time, description string) ([]int64, error) {
	if err := CheckAmountDate(amount, date, nil, "fine"); err != nil {
		return nil, err
	}
	if len(memberIDs) == 0 || len(memberIDs) > MaxFinedMembers {
		return nil, Error{"missing_fields", fmt.Sprintf("Pick the members to fine (up to %d at a time).", MaxFinedMembers)}
	}
	if err := Found("invalid_fine_category", "Pick one of the group's fine categories.")(
		q.FineCategoryInGroup(ctx, store.FineCategoryInGroupParams{GroupID: groupID, ID: fineCategoryID})); err != nil {
		return nil, err
	}
	description = strings.TrimSpace(description)
	var ids []int64
	seen := map[int64]bool{}
	for _, mid := range memberIDs {
		if seen[mid] {
			continue
		}
		seen[mid] = true
		if err := Found("invalid_member", "One of those members is not in this group.")(
			q.ActiveMembershipInGroup(ctx, store.ActiveMembershipInGroupParams{GroupID: groupID, ID: mid})); err != nil {
			return nil, err
		}
		m, cat := mid, fineCategoryID
		id, err := q.CreateManualFine(ctx, store.CreateManualFineParams{
			GroupID: groupID, MembershipID: &m, Amount: amount, FineDate: *date,
			Description: description, FineCategoryID: &cat, CreatedBy: &userID,
		})
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}
