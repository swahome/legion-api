package books

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/swahome/legion-api/internal/loans"
	"github.com/swahome/legion-api/internal/store"
)

// LoanCharge is one installment, fine or transfer with what has been paid
// towards it.
type LoanCharge struct {
	store.LoanChargesRow
	Paid   int64  // cents settled by repayments, oldest first
	Status string // paid, partly_paid, overdue, due (not yet due)
}

// LoanStatementLine is one line of a loan statement, oldest first.
type LoanStatementLine struct {
	Date    time.Time
	Kind    string // installment, late_fine, balance_fine, transfer_out, repayment
	Ref     int64  // charge or repayment id
	Debit   int64  // cents owed
	Credit  int64  // cents paid
	Balance int64  // running balance after the line
	Note    string
}

// LoanDetail is a loan with its schedule, repayments, guarantors and
// totals, as of Today.
type LoanDetail struct {
	Loan       store.GetLoanViewRow
	Charges    []LoanCharge
	Repayments []store.LoanRepaymentsRow
	Guarantors []store.LoanGuarantorsRow
	Statement  []LoanStatementLine

	Payable, Principal, Interest, Fines, Paid, Balance, Overdue, Overpaid int64
	NextDue                                                               *LoanCharge
	Status                                                                string
}

// GetLoanDetail loads a loan of the group and works out what is paid and
// owed on it. It returns ErrNotFound when the group has no such loan.
func GetLoanDetail(ctx context.Context, q *store.Queries, groupID, id int64, today time.Time) (*LoanDetail, error) {
	l, err := q.GetLoanView(ctx, store.GetLoanViewParams{GroupID: groupID, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	d := &LoanDetail{Loan: l}
	live := l.VoidedAt == nil
	charges, err := q.LoanCharges(ctx, store.LoanChargesParams{LoanID: id, IncludeVoided: !live})
	if err != nil {
		return nil, err
	}
	if d.Repayments, err = q.LoanRepayments(ctx, store.LoanRepaymentsParams{LoanID: id, IncludeVoided: !live}); err != nil {
		return nil, err
	}
	if d.Guarantors, err = q.LoanGuarantors(ctx, id); err != nil {
		return nil, err
	}
	for _, r := range d.Repayments {
		if r.VoidedAt == nil || !live {
			d.Paid += Cents(r.Amount)
		}
	}
	alloc := make([]loans.Charge, 0, len(charges))
	for _, c := range charges {
		alloc = append(alloc, loans.Charge{ID: c.ID, Kind: string(c.Kind), DueOn: c.DueOn, Amount: Cents(c.Amount)})
	}
	settled, overpaid := loans.Allocate(alloc, d.Paid, l.FineDeferment)
	d.Overpaid = overpaid
	for _, c := range charges {
		lc := LoanCharge{LoanChargesRow: c, Paid: settled[c.ID]}
		amount := Cents(c.Amount)
		switch {
		case lc.Paid >= amount:
			lc.Status = "paid"
		case c.DueOn.Before(today):
			lc.Status = "overdue"
			d.Overdue += amount - lc.Paid
		case lc.Paid > 0:
			lc.Status = "partly_paid"
		default:
			lc.Status = "due"
		}
		d.Payable += amount
		d.Principal += Cents(c.Principal)
		d.Interest += Cents(c.Interest)
		if c.Kind == store.LoanChargeKindLateFine || c.Kind == store.LoanChargeKindBalanceFine {
			d.Fines += amount
		}
		d.Charges = append(d.Charges, lc)
	}
	d.Balance = d.Payable - d.Paid
	for i := range d.Charges {
		c := &d.Charges[i]
		if c.Status != "paid" && c.Kind == store.LoanChargeKindInstallment {
			d.NextDue = c
			break
		}
	}
	switch {
	case !live:
		d.Status = loans.StatusVoided
	case d.Balance <= 0:
		d.Status = loans.StatusPaid
	case l.IsBadDebt:
		d.Status = loans.StatusBadDebt
	case d.Overdue > 0:
		d.Status = loans.StatusArrears
	default:
		d.Status = loans.StatusActive
	}
	d.Statement = statement(d)
	return d, nil
}

// statement lists charges (on their due date) and repayments by date with
// a running balance, the way the legacy loan statement read.
func statement(d *LoanDetail) []LoanStatementLine {
	var lines []LoanStatementLine
	for _, c := range d.Charges {
		note := ""
		if c.Number != nil {
			note = "Installment " + itoa(int(*c.Number))
		}
		lines = append(lines, LoanStatementLine{Date: c.DueOn, Kind: string(c.Kind), Ref: c.ID, Debit: Cents(c.Amount), Note: note})
	}
	for _, r := range d.Repayments {
		lines = append(lines, LoanStatementLine{Date: r.PaidOn, Kind: "repayment", Ref: r.ID, Credit: Cents(r.Amount), Note: r.Description})
	}
	sort.SliceStable(lines, func(i, j int) bool {
		if !lines[i].Date.Equal(lines[j].Date) {
			return lines[i].Date.Before(lines[j].Date)
		}
		return lines[i].Debit > 0 && lines[j].Credit > 0
	})
	var bal int64
	for i := range lines {
		bal += lines[i].Debit - lines[i].Credit
		lines[i].Balance = bal
	}
	return lines
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
