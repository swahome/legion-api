package books

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/swahome/legion-api/internal/loans"
	"github.com/swahome/legion-api/internal/store"
)

// ChargeLoanFines raises the late-payment and outstanding-balance fines
// that have fallen due on a loan up to today, the way the legacy
// fix_late_loan_invoices_fine and queue_outstanding_loan_balance_fines jobs
// did:
//
//   - a late fine is charged on each installment still not fully paid on
//     its next fine date (the day after it fell due, then every period of
//     the rule's frequency, or just once). A "loan" scope rule charges one
//     fine per loan per date however many installments are late;
//   - an outstanding-balance fine is charged on the loan while anything is
//     still owed on its next balance fine date.
//
// What is paid is worked out as of each fine date. Fine dates older than
// oldest are not charged, only moved on, so a long outage or an old import
// is never back-billed. Each fine is a loan_charges row with generated_by
// 'fines'; a unique index makes a repeat run for the same date add nothing.
// The caller runs it in a transaction. It returns how many fines it raised.
func ChargeLoanFines(ctx context.Context, q *store.Queries, groupID, loanID int64, today, oldest time.Time) (int64, error) {
	l, err := q.GetLoanForUpdate(ctx, store.GetLoanForUpdateParams{GroupID: groupID, ID: loanID})
	if err != nil {
		return 0, err
	}
	if l.VoidedAt != nil || l.IsBadDebt {
		return 0, nil
	}
	late, err := loans.DecodeFine(l.LateFine)
	if err != nil {
		return 0, err
	}
	balance, err := loans.DecodeFine(l.BalanceFine)
	if err != nil {
		return 0, err
	}
	rows, err := q.LoanCharges(ctx, store.LoanChargesParams{LoanID: loanID})
	if err != nil {
		return 0, err
	}
	reps, err := q.LoanRepayments(ctx, store.LoanRepaymentsParams{LoanID: loanID})
	if err != nil {
		return 0, err
	}
	b := &fineBook{loanAmount: Cents(NumText(l.Amount)), defer_: l.FineDeferment}
	for _, c := range rows {
		b.add(loans.Charge{ID: c.ID, Kind: string(c.Kind), DueOn: c.DueOn, Amount: Cents(c.Amount)}, c.InvoiceOn)
	}
	for _, r := range reps {
		b.payments = append(b.payments, payment{r.PaidOn, Cents(r.Amount)})
	}
	userless := "fines"
	raise := func(kind store.LoanChargeKind, on time.Time, parent *int64, cents int64) error {
		id, err := q.CreateLoanCharge(ctx, store.CreateLoanChargeParams{
			GroupID: groupID, LoanID: loanID, Kind: kind, InvoiceOn: on, DueOn: on, ParentID: parent,
			GeneratedBy: &userless, Principal: numeric("0"), Interest: numeric("0"), Amount: numeric(loans.Money(cents)),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // already raised for this date
		}
		if err != nil {
			return err
		}
		b.add(loans.Charge{ID: id, Kind: string(kind), DueOn: on, Amount: cents}, on)
		b.raised++
		return nil
	}

	if late != nil {
		for _, c := range rows {
			if c.Kind != store.LoanChargeKindInstallment || c.NoFines || c.NextFineOn == nil || c.NextFineOn.After(today) {
				continue
			}
			next, err := fineDates(*late, *c.NextFineOn, today, oldest, func(d time.Time) (bool, error) {
				owed := b.chargeBalance(c.ID, d)
				if owed <= 0 {
					return false, nil // paid by then: no more late fines on it
				}
				cents := late.Charge(loans.Bases{
					InstallmentBalance: owed, LoanAmount: b.loanAmount, UnpaidBalance: b.unpaid(d),
					InstallmentInterest: Cents(c.Interest),
				})
				if cents <= 0 {
					return true, nil
				}
				parent := &c.ID
				if late.Scope == "loan" {
					parent = nil
				}
				return true, raise(store.LoanChargeKindLateFine, d, parent, cents)
			})
			if err != nil {
				return b.raised, err
			}
			if err := q.SetChargeNextFine(ctx, store.SetChargeNextFineParams{ID: c.ID, NextFineOn: next}); err != nil {
				return b.raised, err
			}
		}
	}

	if balance != nil && l.NextBalanceFineOn != nil && !l.NextBalanceFineOn.After(today) {
		next, err := fineDates(*balance, *l.NextBalanceFineOn, today, oldest, func(d time.Time) (bool, error) {
			owed := b.unpaid(d)
			if owed <= 0 && b.total(today) <= 0 {
				return false, nil // nothing left on the loan
			}
			if owed <= 0 {
				return true, nil
			}
			cents := balance.Charge(loans.Bases{LoanAmount: b.loanAmount, UnpaidBalance: owed})
			if cents <= 0 {
				return true, nil
			}
			return true, raise(store.LoanChargeKindBalanceFine, d, nil, cents)
		})
		if err != nil {
			return b.raised, err
		}
		if err := q.SetLoanNextBalanceFine(ctx, store.SetLoanNextBalanceFineParams{ID: loanID, NextBalanceFineOn: next}); err != nil {
			return b.raised, err
		}
	}
	return b.raised, nil
}

// fineDates walks a rule's fine dates from first up to today, calling
// charge on each date from oldest on. charge says whether fining goes on.
// It returns the next fine date after today, or nil when there is none.
func fineDates(rule loans.FineRule, first, today, oldest time.Time, charge func(time.Time) (bool, error)) (*time.Time, error) {
	d := first
	for i := 0; !d.After(today); i++ {
		if i > 5000 {
			return nil, fmt.Errorf("fine date does not advance past %s", d.Format(time.DateOnly))
		}
		if !d.Before(oldest) {
			more, err := charge(d)
			if err != nil || !more {
				return nil, err
			}
		}
		n := rule.Next(d)
		if n == nil {
			return nil, nil
		}
		d = *n
	}
	return &d, nil
}

type payment struct {
	on    time.Time
	cents int64
}

// fineBook holds a loan's charges and repayments while fines are raised,
// so each fine date sees what was paid by then and the fines raised before.
type fineBook struct {
	loanAmount int64
	defer_     bool
	charges    []loans.Charge
	invoiced   []time.Time // invoice date of each charge
	payments   []payment
	raised     int64
}

func (b *fineBook) add(c loans.Charge, invoiceOn time.Time) {
	b.charges = append(b.charges, c)
	b.invoiced = append(b.invoiced, invoiceOn)
}

func (b *fineBook) paid(d time.Time) int64 {
	var p int64
	for _, r := range b.payments {
		if !r.on.After(d) {
			p += r.cents
		}
	}
	return p
}

// chargeBalance is what is left of one charge on d.
func (b *fineBook) chargeBalance(id int64, d time.Time) int64 {
	settled, _ := loans.Allocate(b.charges, b.paid(d), b.defer_)
	for _, c := range b.charges {
		if c.ID == id {
			return c.Amount - settled[id]
		}
	}
	return 0
}

// unpaid is what had been invoiced by d less what had been paid by d (the
// legacy get_outstanding_balance).
func (b *fineBook) unpaid(d time.Time) int64 {
	var owed int64
	for i, c := range b.charges {
		if !b.invoiced[i].After(d) {
			owed += c.Amount
		}
	}
	return owed - b.paid(d)
}

// total is the whole loan's balance with the payments made by d.
func (b *fineBook) total(d time.Time) int64 {
	var owed int64
	for _, c := range b.charges {
		owed += c.Amount
	}
	return owed - b.paid(d)
}
