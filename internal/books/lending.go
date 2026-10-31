package books

import (
	"context"
	"errors"
	"net/mail"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/swahome/legion-api/internal/store"
)

// ---- Bank loans: money the group borrowed ----

// BankLoan is a loan the group took. Balance is what was still owed when it
// was entered: the total payable for a new loan, less for an older one.
type BankLoan struct {
	Description  string
	Amount       string // received into the account
	TotalPayable string
	Balance      *string // nil: the whole total payable
	StartsOn     *time.Time
	EndsOn       *time.Time
	AccountID    int64
	Method       *string
}

func (b *BankLoan) check() error {
	if err := CheckAmountDate(b.Amount, b.StartsOn, b.Method, "bank loan"); err != nil {
		return err
	}
	return b.checkTerms()
}

// checkTerms checks everything but the amount received and start date.
func (b *BankLoan) checkTerms() error {
	b.Description = strings.TrimSpace(b.Description)
	if b.Description == "" {
		return Error{"missing_fields", "Describe the loan: who lent it and what for."}
	}
	if !ValidAmount(b.TotalPayable) || Cents(b.TotalPayable) < Cents(b.Amount) {
		return Error{"invalid_total_payable", "The total payable is at least the amount received."}
	}
	if b.Balance == nil {
		b.Balance = &b.TotalPayable
	}
	if !ValidAmount(*b.Balance) || Cents(*b.Balance) > Cents(b.TotalPayable) {
		return Error{"invalid_balance", "The balance is between zero and the total payable."}
	}
	if b.EndsOn == nil {
		return Error{"missing_fields", "ends_on is required."}
	}
	if b.EndsOn.Before(*b.StartsOn) {
		return Error{"invalid_date", "The loan cannot end before it starts."}
	}
	return nil
}

// RecordBankLoan records a loan the group received: the loan, and the money
// coming into the account as a bank_loan deposit.
func RecordBankLoan(ctx context.Context, q *store.Queries, groupID, userID int64, b BankLoan) (int64, error) {
	if err := b.check(); err != nil {
		return 0, err
	}
	acct, err := OpenAccount(ctx, q, groupID, b.AccountID)
	if err != nil {
		return 0, err
	}
	id, err := q.CreateBankLoan(ctx, store.CreateBankLoanParams{
		GroupID: groupID, Description: b.Description, StartsOn: *b.StartsOn, EndsOn: *b.EndsOn, AccountID: &acct.ID,
		CreatedBy: &userID, Amount: numeric(b.Amount), TotalPayable: numeric(b.TotalPayable),
		OpeningBalance: numeric(*b.Balance),
	})
	if err != nil {
		return 0, err
	}
	did, err := writeDeposit(ctx, q, acct, store.DepositKindBankLoan, b.Method, b.Amount, *b.StartsOn, b.Description, nil, userID)
	if err != nil {
		return 0, err
	}
	return id, q.SetDepositBankLoan(ctx, store.SetDepositBankLoanParams{ID: did, BankLoanID: &id})
}

// BankLoanEdit changes what can change once the money is in the books.
type BankLoanEdit struct {
	Description  string
	TotalPayable string
	Balance      string
	EndsOn       *time.Time
}

// EditBankLoan updates a live bank loan's terms. The amount received, date
// and account belong to its deposit: void and re-record to change them.
func EditBankLoan(ctx context.Context, q *store.Queries, groupID, id int64, e BankLoanEdit) error {
	l, err := lockBankLoan(ctx, q, groupID, id)
	if err != nil {
		return err
	}
	cur, err := q.GetBankLoan(ctx, store.GetBankLoanParams{GroupID: groupID, ID: id})
	if err != nil {
		return err
	}
	b := BankLoan{Description: e.Description, Amount: cur.Amount, TotalPayable: e.TotalPayable, Balance: &e.Balance,
		StartsOn: &l.StartsOn, EndsOn: e.EndsOn}
	if err := b.checkTerms(); err != nil {
		return err
	}
	return q.UpdateBankLoan(ctx, store.UpdateBankLoanParams{GroupID: groupID, ID: id, Description: b.Description,
		EndsOn: *b.EndsOn, TotalPayable: numeric(b.TotalPayable), OpeningBalance: numeric(*b.Balance)})
}

func lockBankLoan(ctx context.Context, q *store.Queries, groupID, id int64) (store.LockBankLoanRow, error) {
	l, err := q.LockBankLoan(ctx, store.LockBankLoanParams{GroupID: groupID, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return l, ErrNotFound
	}
	if err != nil {
		return l, err
	}
	if l.VoidedAt != nil {
		return l, Error{"loan_voided", "This bank loan has been voided."}
	}
	return l, nil
}

// BankLoanRepayment is money the group paid back.
type BankLoanRepayment struct {
	AccountID   int64
	Method      *string
	Amount      string
	Date        *time.Time
	Description string
}

// RecordBankLoanRepayment writes a bank_loan_repayment withdrawal out of the
// account against the loan. It returns the withdrawal id.
func RecordBankLoanRepayment(ctx context.Context, q *store.Queries, groupID, userID, loanID int64, r BankLoanRepayment) (int64, error) {
	if err := CheckAmountDate(r.Amount, r.Date, r.Method, "repayment"); err != nil {
		return 0, err
	}
	l, err := lockBankLoan(ctx, q, groupID, loanID)
	if err != nil {
		return 0, err
	}
	if r.Date.Before(l.StartsOn) {
		return 0, Error{"invalid_date", "A repayment cannot be dated before the loan started."}
	}
	acct, err := OpenAccount(ctx, q, groupID, r.AccountID)
	if err != nil {
		return 0, err
	}
	desc := strings.TrimSpace(r.Description)
	if desc == "" {
		desc = "Bank loan repayment"
	}
	wid, err := WriteWithdrawal(ctx, q, acct, Withdrawal{Kind: store.WithdrawalKindBankLoanRepayment, Amount: r.Amount},
		r.Method, *r.Date, desc, userID)
	if err != nil {
		return 0, err
	}
	return wid, q.SetWithdrawalBankLoan(ctx, store.SetWithdrawalBankLoanParams{ID: wid, BankLoanID: &loanID})
}

// VoidBankLoan voids a bank loan with the deposit that brought it in and
// every repayment.
func VoidBankLoan(ctx context.Context, q *store.Queries, groupID, id, userID int64) error {
	if _, err := lockBankLoan(ctx, q, groupID, id); err != nil {
		var be Error
		if errors.As(err, &be) {
			return Error{"already_voided", "This bank loan is already voided."}
		}
		return err
	}
	rows, err := q.BankLoanMoneyRows(ctx, &id)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if r.Kind == "deposit" {
			err = voidDeposit(ctx, q, groupID, r.ID, userID)
		} else {
			err = voidWithdrawal(ctx, q, groupID, r.ID, userID)
		}
		if err != nil {
			return err
		}
	}
	_, err = q.VoidBankLoan(ctx, store.VoidBankLoanParams{GroupID: groupID, ID: id, VoidedBy: &userID})
	return err
}

// ---- Debtors: people outside the group who borrow from it ----

// Debtor is a borrower who is not a member.
type Debtor struct {
	Name        string
	Phone       string
	Email       string
	IDNumber    string
	Description string
}

func (d *Debtor) check() error {
	d.Name, d.Phone, d.Email = strings.TrimSpace(d.Name), strings.TrimSpace(d.Phone), strings.TrimSpace(d.Email)
	d.IDNumber, d.Description = strings.TrimSpace(d.IDNumber), strings.TrimSpace(d.Description)
	if d.Name == "" {
		return Error{"missing_fields", "name is required."}
	}
	if d.Email != "" {
		if _, err := mail.ParseAddress(d.Email); err != nil {
			return Error{"invalid_email", "That email address does not look right."}
		}
	}
	return nil
}

// SaveDebtor creates a debtor (id 0) or updates one.
func SaveDebtor(ctx context.Context, q *store.Queries, groupID, userID, id int64, d Debtor) (int64, error) {
	if err := d.check(); err != nil {
		return 0, err
	}
	if id == 0 {
		return q.CreateDebtor(ctx, store.CreateDebtorParams{GroupID: groupID, Name: d.Name, Phone: d.Phone,
			Email: d.Email, IDNumber: d.IDNumber, Description: d.Description, CreatedBy: &userID})
	}
	n, err := q.UpdateDebtor(ctx, store.UpdateDebtorParams{GroupID: groupID, ID: id, Name: d.Name, Phone: d.Phone,
		Email: d.Email, IDNumber: d.IDNumber, Description: d.Description})
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, ErrNotFound
	}
	return id, nil
}

// DeleteDebtor removes a debtor who has no live loans.
func DeleteDebtor(ctx context.Context, q *store.Queries, groupID, id int64) error {
	has, err := q.DebtorHasLoans(ctx, &id)
	if err != nil {
		return err
	}
	if has {
		return Error{"debtor_has_loans", "This debtor has loans. Void them first, or suspend the debtor instead."}
	}
	n, err := q.DeleteDebtor(ctx, store.DeleteDebtorParams{GroupID: groupID, ID: id})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
