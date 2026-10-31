package loans

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// FineRule is a loan's late-payment fine (charged on installments not paid
// by their due date) or outstanding-balance fine (charged on what is still
// owed after the last installment's due date). It is stored as JSON in
// loan_types and loans.
//
// Legacy encodings it replaces (Loan.php): loan_fine_type 1 fixed amount,
// 2 percentage, 3 one-off per installment (fixed or percentage), with
// *_frequency 1-4 and fixed_amount_fine_frequency_on 1 each installment, 2
// the total outstanding balance.
type FineRule struct {
	Kind      string  `json:"kind"`              // fixed | percentage
	Amount    string  `json:"amount,omitempty"`  // fixed: amount per fine
	Rate      float64 `json:"rate,omitempty"`    // percentage: percent of RateOn
	RateOn    string  `json:"rate_on,omitempty"` // installment_balance | loan_amount | unpaid_balance | installment_interest
	Frequency string  `json:"frequency"`         // once | daily | weekly | monthly | yearly
	// Late fines only: "installment" fines each late installment; "loan"
	// charges one fine a day for the loan however many installments are late.
	Scope string `json:"scope,omitempty"`
}

var fineFrequencies = map[string]bool{"once": true, "daily": true, "weekly": true, "monthly": true, "yearly": true}
var rateBases = map[string]bool{"installment_balance": true, "loan_amount": true, "unpaid_balance": true, "installment_interest": true}

// Check validates a rule. balance is true for an outstanding-balance fine,
// which has no installment to be a percentage of.
func (r *FineRule) Check(balance bool) error {
	r.Amount = strings.TrimSpace(r.Amount)
	if !fineFrequencies[r.Frequency] {
		return fmt.Errorf("fine frequency must be once, daily, weekly, monthly or yearly")
	}
	switch r.Kind {
	case "fixed":
		if !amountPattern.MatchString(r.Amount) || Cents(r.Amount) <= 0 {
			return fmt.Errorf("a fixed fine needs an amount more than zero")
		}
		r.Rate, r.RateOn = 0, ""
	case "percentage":
		if r.Rate <= 0 || r.Rate > 100 {
			return fmt.Errorf("a percentage fine needs a rate between 0 and 100")
		}
		if !rateBases[r.RateOn] {
			return fmt.Errorf("rate_on must be installment_balance, loan_amount, unpaid_balance or installment_interest")
		}
		if balance && (r.RateOn == "installment_balance" || r.RateOn == "installment_interest") {
			return fmt.Errorf("an outstanding balance fine is a percentage of the loan amount or the unpaid balance")
		}
		r.Amount = ""
	default:
		return fmt.Errorf("fine kind must be fixed or percentage")
	}
	switch {
	case balance:
		r.Scope = ""
	case r.Scope == "":
		r.Scope = "installment"
	case r.Scope != "installment" && r.Scope != "loan":
		return fmt.Errorf("fine scope must be installment or loan")
	}
	if r.Scope == "loan" && r.Kind != "fixed" {
		return fmt.Errorf("a fine on the whole loan is a fixed amount")
	}
	return nil
}

// Bases are the amounts a percentage fine can be worked out on, in cents.
type Bases struct {
	InstallmentBalance  int64 // what is left of the late installment
	LoanAmount          int64
	UnpaidBalance       int64 // what is left of the whole loan
	InstallmentInterest int64
}

// Charge is the fine, in cents, truncated to the cent.
func (r FineRule) Charge(b Bases) int64 {
	if r.Kind == "fixed" {
		return Cents(r.Amount)
	}
	base := map[string]int64{
		"installment_balance": b.InstallmentBalance, "loan_amount": b.LoanAmount,
		"unpaid_balance": b.UnpaidBalance, "installment_interest": b.InstallmentInterest,
	}[r.RateOn]
	return floorCents(float64(base) * r.Rate / 100)
}

// Next is the next date the fine applies after one charged on d, or nil
// for a one-off fine.
func (r FineRule) Next(d time.Time) *time.Time {
	var n time.Time
	switch r.Frequency {
	case "daily":
		n = d.AddDate(0, 0, 1)
	case "weekly":
		n = d.AddDate(0, 0, 7)
	case "monthly":
		n = d.AddDate(0, 1, 0)
	case "yearly":
		n = d.AddDate(1, 0, 0)
	default:
		return nil
	}
	return &n
}

// ProcessingFee is charged when a loan is paid out and recorded as loan
// processing income into the account the loan came from (legacy
// calculate_and_record_loan_processing_fee).
type ProcessingFee struct {
	Kind   string  `json:"kind"`             // fixed | percentage
	Amount string  `json:"amount,omitempty"` // fixed
	Rate   float64 `json:"rate,omitempty"`   // percentage...
	On     string  `json:"on,omitempty"`     // ...of "amount" or "amount_plus_interest"
}

// Check validates a fee.
func (f *ProcessingFee) Check() error {
	f.Amount = strings.TrimSpace(f.Amount)
	switch f.Kind {
	case "fixed":
		if !amountPattern.MatchString(f.Amount) || Cents(f.Amount) <= 0 {
			return fmt.Errorf("a fixed processing fee needs an amount more than zero")
		}
		f.Rate, f.On = 0, ""
	case "percentage":
		if f.Rate <= 0 || f.Rate > 100 {
			return fmt.Errorf("a percentage processing fee needs a rate between 0 and 100")
		}
		if f.On != "amount" && f.On != "amount_plus_interest" {
			return fmt.Errorf("processing fee on must be amount or amount_plus_interest")
		}
		f.Amount = ""
	default:
		return fmt.Errorf("processing fee kind must be fixed or percentage")
	}
	return nil
}

// Charge is the fee in cents for a loan of amount with this total interest.
func (f ProcessingFee) Charge(amount, interest int64) int64 {
	if f.Kind == "fixed" {
		return Cents(f.Amount)
	}
	base := amount
	if f.On == "amount_plus_interest" {
		base += interest
	}
	return floorCents(float64(base) * f.Rate / 100)
}

// DecodeFine reads a stored rule; nil or JSON null is no rule.
func DecodeFine(raw []byte) (*FineRule, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var r FineRule
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// DecodeFee reads a stored processing fee.
func DecodeFee(raw []byte) (*ProcessingFee, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var f ProcessingFee
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, err
	}
	return &f, nil
}

// Encode stores a rule or fee; a nil pointer is SQL NULL.
func Encode[T any](v *T) []byte {
	if v == nil {
		return nil
	}
	b, _ := json.Marshal(v)
	return b
}

var amountPattern = regexp.MustCompile(`^\d{1,12}(\.\d{1,2})?$`)

// Cents parses a decimal amount ("1250.5", "-3.00") to cents.
func Cents(s string) int64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return int64(math.Round(f * 100))
}

// Money renders cents as a decimal with two places.
func Money(c int64) string {
	sign := ""
	if c < 0 {
		sign, c = "-", -c
	}
	return fmt.Sprintf("%s%d.%02d", sign, c/100, c%100)
}
