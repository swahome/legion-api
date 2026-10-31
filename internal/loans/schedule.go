// Package loans holds the loan arithmetic: repayment schedules, how much
// interest an installment carries, how repayments settle installments and
// fines, and the late-payment and outstanding-balance fines. It has no
// database code; internal/books writes loans and the billing job charges
// their fines. It ports the legacy Loan library (calculate_loan_balance_invoice,
// pay_invoices, get_fine_payable_new); see docs/schema/loans.md for where it
// deliberately differs.
package loans

import (
	"fmt"
	"math"
	"time"
)

// InterestType is how a loan's interest is worked out (legacy
// loans.interest_type 1-4).
type InterestType string

const (
	FixedBalance    InterestType = "fixed_balance"    // flat: rate on the amount borrowed, every installment
	ReducingBalance InterestType = "reducing_balance" // rate on what is still owed
	CustomInterest  InterestType = "custom"           // legacy rate table by days; kept for imported loans only
	FixedAmount     InterestType = "fixed_amount"     // a set interest per installment
)

// RatePer is the period the interest rate is quoted for (legacy
// loan_interest_rate_per 1-5).
type RatePer string

const (
	PerDay   RatePer = "day"
	PerWeek  RatePer = "week"
	PerMonth RatePer = "month"
	PerYear  RatePer = "year"
	PerTerm  RatePer = "term" // for the whole repayment period
)

// Frequency is how often installments fall due (legacy
// loan_repayment_frequency 1-3).
type Frequency string

const (
	Daily   Frequency = "daily"
	Weekly  Frequency = "weekly"
	Monthly Frequency = "monthly"
)

// ReducingMethod picks the reducing balance schedule (legacy
// reducing_interest_option): equal installments, or equal principal with
// interest on the falling balance.
type ReducingMethod string

const (
	EqualInstallments ReducingMethod = "equal_installments"
	EqualPrincipal    ReducingMethod = "equal_principal"
)

// Terms are what a schedule is built from. Amounts are in cents.
type Terms struct {
	Amount         int64
	InterestType   InterestType
	Rate           float64 // percent, per RatePer
	RatePer        RatePer
	Frequency      Frequency
	Installments   int
	GracePeriods   int        // the first installment is due this many periods after disbursement (at least 1)
	FirstDueDate   *time.Time // overrides GracePeriods: the first installment's due date
	Reducing       ReducingMethod
	FixedInterest  int64   // FixedAmount: cents of interest per installment...
	FixedPercent   float64 // ...or this percent of the amount, per installment
	DisbursementOn time.Time
}

// Installment is one row of a schedule.
type Installment struct {
	Number      int       `json:"number"`
	InvoiceDate time.Time `json:"invoice_date"` // a week before it is due, when the reminder goes out
	DueDate     time.Time `json:"due_date"`
	FineDate    time.Time `json:"fine_date"` // the day after it is due, when a late fine can first apply
	Principal   int64     `json:"principal"`
	Interest    int64     `json:"interest"`
}

// Amount is principal plus interest.
func (i Installment) Amount() int64 { return i.Principal + i.Interest }

// MaxInstallments bounds a schedule (a daily loan for two years).
const MaxInstallments = 730

// Check validates terms before a schedule is built.
func (t Terms) Check() error {
	switch {
	case t.Amount <= 0:
		return fmt.Errorf("the loan amount must be more than zero")
	case t.Installments < 1 || t.Installments > MaxInstallments:
		return fmt.Errorf("the repayment period must be between 1 and %d installments", MaxInstallments)
	case t.Frequency != Daily && t.Frequency != Weekly && t.Frequency != Monthly:
		return fmt.Errorf("repayment frequency must be daily, weekly or monthly")
	case t.GracePeriods < 0 || t.GracePeriods > 24:
		return fmt.Errorf("the grace period must be between 1 and 24 periods")
	case t.Rate < 0 || t.Rate > 1000 || t.FixedPercent < 0 || t.FixedPercent > 1000 || t.FixedInterest < 0:
		return fmt.Errorf("the interest rate is out of range")
	case t.FirstDueDate != nil && t.FirstDueDate.Before(t.DisbursementOn):
		return fmt.Errorf("the first installment cannot be due before the loan is disbursed")
	}
	switch t.InterestType {
	case FixedBalance, ReducingBalance:
		switch t.RatePer {
		case PerDay, PerWeek, PerMonth, PerYear, PerTerm:
		default:
			return fmt.Errorf("the interest rate period must be day, week, month, year or term")
		}
		if t.InterestType == ReducingBalance && t.Reducing == EqualPrincipal && t.Frequency != Monthly {
			return fmt.Errorf("equal principal reducing balance loans are repaid monthly")
		}
	case FixedAmount:
	case CustomInterest:
		return fmt.Errorf("custom interest is kept for imported loans only")
	default:
		return fmt.Errorf("interest type must be fixed_balance, reducing_balance or fixed_amount")
	}
	return nil
}

// PeriodRate is the interest rate (percent) for one installment period,
// converted the way the legacy app did: a month is 30 days or 4 weeks, a
// year 365 days, 52 weeks or 12 months.
func (t Terms) PeriodRate() float64 {
	r := t.Rate
	type conv struct{ mul, div float64 }
	table := map[RatePer]map[Frequency]conv{
		PerDay:   {Daily: {1, 1}, Weekly: {7, 1}, Monthly: {30, 1}},
		PerWeek:  {Daily: {1, 7}, Weekly: {1, 1}, Monthly: {1, 4}},
		PerMonth: {Daily: {1, 30}, Weekly: {1, 4}, Monthly: {1, 1}},
		PerYear:  {Daily: {1, 365}, Weekly: {1, 52}, Monthly: {1, 12}},
	}
	if t.RatePer == PerTerm {
		return r / float64(t.Installments)
	}
	c, ok := table[t.RatePer][t.Frequency]
	if !ok {
		return 0
	}
	return r * c.mul / c.div
}

// AddPeriods moves a date on by n installment periods. Months overflow the
// way PHP's strtotime and Go's AddDate both do (31 January + 1 month is
// 3 March), which keeps imported schedules and new ones in step.
func AddPeriods(d time.Time, f Frequency, n int) time.Time {
	switch f {
	case Daily:
		return d.AddDate(0, 0, n)
	case Weekly:
		return d.AddDate(0, 0, 7*n)
	default:
		return d.AddDate(0, n, 0)
	}
}

// DueDates are the installments' due dates.
func (t Terms) DueDates() []time.Time {
	out := make([]time.Time, t.Installments)
	if t.FirstDueDate != nil {
		for i := range out {
			out[i] = AddPeriods(*t.FirstDueDate, t.Frequency, i)
		}
		return out
	}
	grace := t.GracePeriods
	if grace < 1 {
		grace = 1
	}
	base := AddPeriods(t.DisbursementOn, t.Frequency, grace-1)
	for i := range out {
		out[i] = AddPeriods(base, t.Frequency, i+1)
	}
	return out
}

// floorCents truncates to whole cents, as the legacy app did, with a
// little slack so 0.29999999 cents of float error is not lost.
func floorCents(x float64) int64 { return int64(math.Floor(x + 1e-6)) }

// Schedule builds the repayment schedule. Principal always adds up to the
// amount borrowed: each installment's share is truncated to the cent and the
// last installment takes what is left (the legacy app truncated every
// installment and lost the remainder).
func (t Terms) Schedule() ([]Installment, error) {
	if err := t.Check(); err != nil {
		return nil, err
	}
	n := t.Installments
	dues := t.DueDates()
	out := make([]Installment, n)
	rate := t.PeriodRate() / 100
	evenPrincipal := func(i int) int64 {
		share := t.Amount / int64(n)
		if i == n-1 {
			return t.Amount - share*int64(n-1)
		}
		return share
	}
	switch {
	case t.InterestType == FixedBalance:
		total := float64(t.Amount) * rate * float64(n)
		share := floorCents(total / float64(n))
		for i := range out {
			out[i].Principal, out[i].Interest = evenPrincipal(i), share
		}
		// The rounding left over goes on the last installment.
		out[n-1].Interest += floorCents(total) - share*int64(n)
	case t.InterestType == FixedAmount:
		per := t.FixedInterest
		if per == 0 && t.FixedPercent > 0 {
			per = floorCents(float64(t.Amount) * t.FixedPercent / 100)
		}
		for i := range out {
			out[i].Principal, out[i].Interest = evenPrincipal(i), per
		}
	case t.InterestType == ReducingBalance && t.Reducing == EqualPrincipal:
		balance := t.Amount
		for i := range out {
			out[i].Interest = floorCents(float64(balance) * rate)
			out[i].Principal = evenPrincipal(i)
			balance -= out[i].Principal
		}
	case t.InterestType == ReducingBalance:
		balance := t.Amount
		payment := float64(t.Amount) / float64(n)
		if rate > 0 {
			payment = rate / (1 - math.Pow(1+rate, -float64(n))) * float64(t.Amount)
		}
		pay := floorCents(payment)
		for i := range out {
			interest := floorCents(float64(balance) * rate)
			principal := pay - interest
			if i == n-1 || principal > balance {
				principal = balance
			}
			out[i].Principal, out[i].Interest = principal, interest
			balance -= principal
		}
	}
	for i := range out {
		out[i].Number = i + 1
		out[i].DueDate = dues[i]
		out[i].InvoiceDate = dues[i].AddDate(0, 0, -7)
		out[i].FineDate = dues[i].AddDate(0, 0, 1)
	}
	return out, nil
}

// Interest is the schedule's total interest.
func Interest(s []Installment) int64 {
	var n int64
	for _, i := range s {
		n += i.Interest
	}
	return n
}
