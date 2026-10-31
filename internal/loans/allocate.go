package loans

import (
	"sort"
	"time"
)

// Charge is something owed on a loan, for allocation.
type Charge struct {
	ID     int64
	Kind   string // installment, late_fine, balance_fine, transfer_out
	DueOn  time.Time
	Amount int64 // cents
}

// IsFine reports whether the charge is a fine.
func (c Charge) IsFine() bool { return c.Kind == "late_fine" || c.Kind == "balance_fine" }

// Allocate spreads what has been paid on a loan over its charges, oldest
// first (legacy pay_invoices walked the loan statement in date order). With
// fine deferment every installment is settled before any fine
// (enable_loan_fine_deferment). The result maps charge id to the amount of
// it paid; whatever is left over is an overpayment.
func Allocate(charges []Charge, paid int64, deferFines bool) (settled map[int64]int64, overpaid int64) {
	order := make([]Charge, len(charges))
	copy(order, charges)
	sort.SliceStable(order, func(i, j int) bool {
		a, b := order[i], order[j]
		if deferFines && a.IsFine() != b.IsFine() {
			return !a.IsFine()
		}
		if !a.DueOn.Equal(b.DueOn) {
			return a.DueOn.Before(b.DueOn)
		}
		// On the same day an installment is settled before the fine on it.
		if a.IsFine() != b.IsFine() {
			return !a.IsFine()
		}
		return a.ID < b.ID
	})
	settled = make(map[int64]int64, len(order))
	for _, c := range order {
		take := c.Amount
		if take > paid {
			take = paid
		}
		if take < 0 {
			take = 0
		}
		settled[c.ID] = take
		paid -= take
	}
	return settled, paid
}

// Status of a loan, from its balance and dates.
const (
	StatusActive  = "active"  // something still owed, nothing overdue
	StatusArrears = "arrears" // an installment or fine is overdue
	StatusPaid    = "paid"    // nothing owed
	StatusBadDebt = "bad_debt"
	StatusVoided  = "voided"
)
