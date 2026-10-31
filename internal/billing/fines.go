package billing

import (
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"
)

// FineRule is a contribution fine setting in the legacy encoding kept by
// contribution_fine_rules:
//
//	chargeable_on: "1".."56" days after the contribution date,
//	    "last_day_of_the_month" or "first_day_of_the_month" (the next one).
//	frequency: "0" once per contribution, then "1" daily, "2" weekly,
//	    "3" monthly, "4" quarterly, "5" half-yearly, "6" yearly.
//	mode: "1" a fine for each unpaid invoice, "2" one fine on the balance.
//	percentage_of: "1" the contribution amount, "2" the unpaid balance,
//	    "3" the unpaid balance plus unpaid fines of this contribution.
//	fine_limit: most fines one invoice can collect (mode 1); 0 = no limit.
type FineRule struct {
	Kind         string // fixed | percentage
	FixedCents   int64
	Rate         *big.Rat // percent, e.g. 10 for 10%
	PercentageOf string
	ChargeableOn string
	Mode         string
	Frequency    string
	Limit        int
}

const (
	ModeEachInvoice = "1"
	ModeBalance     = "2"
)

// Repeats reports whether the rule charges again every period while unpaid.
func (f FineRule) Repeats() bool { return f.Frequency != "" && f.Frequency != "0" }

// offsetDays is the number of days after the contribution date, or -1 for
// the month-based options.
func (f FineRule) offsetDays() int {
	switch f.ChargeableOn {
	case "last_day_of_the_month", "first_day_of_the_month":
		return -1
	}
	n, err := strconv.Atoi(strings.TrimSpace(f.ChargeableOn))
	if err != nil || n < 0 {
		return 1
	}
	return n
}

// FirstFineDate is the fine date for the contribution due on `due`.
func (f FineRule) FirstFineDate(due time.Time) time.Time {
	due = Day(due)
	switch f.ChargeableOn {
	case "last_day_of_the_month":
		return Date(due.Year(), due.Month(), daysIn(due.Year(), due.Month()))
	case "first_day_of_the_month":
		return Date(due.Year(), due.Month()+1, 1)
	}
	return due.AddDate(0, 0, f.offsetDays())
}

// NextFineDate is the fine date after one charged on `fined`. A repeating
// rule charges again one period later; a one-off rule waits for the next
// contribution date of the schedule.
func (f FineRule) NextFineDate(fined time.Time, frequency string, r Rule, anchor time.Time) (time.Time, error) {
	fined = Day(fined)
	switch f.Frequency {
	case "1":
		return fined.AddDate(0, 0, 1), nil
	case "2":
		return fined.AddDate(0, 0, 7), nil
	case "3":
		return addMonths(fined, 1), nil
	case "4":
		return addMonths(fined, 3), nil
	case "5":
		return addMonths(fined, 6), nil
	case "6":
		return addMonths(fined, 12), nil
	}
	// From the contribution this fine belonged to, find the next one.
	var from time.Time
	switch f.ChargeableOn {
	case "first_day_of_the_month":
		from = fined
	case "last_day_of_the_month":
		from = fined.AddDate(0, 0, 1)
	default:
		from = fined.AddDate(0, 0, 1-f.offsetDays())
	}
	for i := 0; i < 400; i++ {
		due, err := Next(frequency, r, anchor, from)
		if err != nil {
			return time.Time{}, err
		}
		if d := f.FirstFineDate(due); d.After(fined) {
			return d, nil
		}
		from = due.AddDate(0, 0, 1)
	}
	return time.Time{}, fmt.Errorf("no fine date found after %s", fined.Format(time.DateOnly))
}

// Amount is the fine on a base in cents: the fixed amount, or the rate
// applied to the base, rounded half up to the cent.
func (f FineRule) Amount(baseCents int64) int64 {
	if f.Kind == "fixed" {
		return f.FixedCents
	}
	if f.Rate == nil || baseCents <= 0 {
		return 0
	}
	v := new(big.Rat).Mul(new(big.Rat).SetInt64(baseCents), f.Rate)
	v.Quo(v, big.NewRat(100, 1))
	// Round half up: floor(v + 1/2).
	v.Add(v, big.NewRat(1, 2))
	return new(big.Int).Quo(v.Num(), v.Denom()).Int64()
}

// ParseRate reads a percentage such as "2.5000".
func ParseRate(s string) (*big.Rat, error) {
	r, ok := new(big.Rat).SetString(strings.TrimSpace(s))
	if !ok || r.Sign() < 0 {
		return nil, fmt.Errorf("bad percentage %q", s)
	}
	return r, nil
}

// ParseCents reads a decimal amount such as "1500.50" into cents.
func ParseCents(s string) (int64, error) {
	r, ok := new(big.Rat).SetString(strings.TrimSpace(s))
	if !ok {
		return 0, fmt.Errorf("bad amount %q", s)
	}
	r.Mul(r, big.NewRat(100, 1))
	neg := r.Sign() < 0
	r.Abs(r)
	r.Add(r, big.NewRat(1, 2))
	n := new(big.Int).Quo(r.Num(), r.Denom()).Int64()
	if neg {
		n = -n
	}
	return n, nil
}

// FormatCents writes cents as a decimal string ("1500.50").
func FormatCents(c int64) string {
	sign := ""
	if c < 0 {
		sign, c = "-", -c
	}
	return fmt.Sprintf("%s%d.%02d", sign, c/100, c%100)
}

// addMonths moves n months on, keeping to the month's last day when the
// day doesn't exist there (31 October + 1 month = 30 November).
func addMonths(t time.Time, n int) time.Time {
	ms := monthStart(t.Year(), t.Month()+time.Month(n))
	return Date(ms.Year(), ms.Month(), min(t.Day(), daysIn(ms.Year(), ms.Month())))
}
