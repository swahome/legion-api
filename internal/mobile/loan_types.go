package mobile

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/swahome/legion-api/internal/books"
	"github.com/swahome/legion-api/internal/legacy"
	"github.com/swahome/legion-api/internal/loans"
	"github.com/swahome/legion-api/internal/store"
)

// Loan types (legacy loan_types/Mobile.php). The app reads and writes the
// legacy columns; these helpers translate them to and from books.LoanType.

var (
	interestTypeCodes = map[string]string{"fixed_balance": "1", "reducing_balance": "2", "custom": "3", "fixed_amount": "4"}
	ratePerCodes      = map[string]string{"day": "1", "week": "2", "month": "3", "year": "4", "term": "5"}
	fineFreqCodes     = map[string]string{"daily": "1", "weekly": "2", "monthly": "3", "yearly": "4"}
	rateOnCodes       = map[string]string{"installment_balance": "1", "loan_amount": "2", "unpaid_balance": "3", "installment_interest": "4"}
	interestTypeNames = map[string]string{"1": "Fixed Balance", "2": "Reducing Balance", "3": "Custom Interest Type", "4": "Fixed Interest Amount"}
	ratePerNames      = map[string]string{"1": "Per Day", "2": "Per Week", "3": "Per Month", "4": "Per Annum", "5": "For the whole loan repayment period"}
	fineFreqNames     = map[string]string{"1": "Daily", "2": "Weekly", "3": "Monthly", "4": "Yearly"}
	rateOnNames       = map[string]string{"1": "Loan Installment Balance", "2": "Loan Amount", "3": "Total Unpaid Loan Amount", "4": "Installment Interest"}
	fineTypeNames     = map[string]string{"1": "A Fixed Fine Amount", "2": "A Percentage (%) Fine", "3": "A One off Fine Amount per Installment"}
	fixedOnNames      = map[string]string{"1": "Each Outstanding Installment", "2": "Total Outstanding Balance"}
	feeOnNames        = map[string]string{"1": "Total Loan Amount", "2": "Total Loan Principle plus Interest"}
)

func rateText(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

func i32(p *int32) any {
	if p == nil {
		return 0
	}
	return strconv.Itoa(int(*p))
}

func strOr0(p *string) any {
	if p == nil {
		return 0
	}
	return books.Format(*p)
}

// lateFineFields are the legacy late-fine columns of a rule.
func lateFineFields(f *loans.FineRule) map[string]any {
	m := map[string]any{"enable_loan_fines": 0, "loan_fine_type": 0, "fixed_fine_amount": 0, "fixed_amount_fine_frequency": 0,
		"fixed_amount_fine_frequency_on": 0, "percentage_fine_rate": 0, "percentage_fine_frequency": 0, "percentage_fine_on": 0,
		"one_off_fine_type": 0, "one_off_fixed_amount": 0, "one_off_percentage_rate": 0, "one_off_percentage_rate_on": 0}
	if f == nil {
		return m
	}
	m["enable_loan_fines"] = "1"
	switch {
	case f.Frequency == "once" && f.Kind == "fixed":
		m["loan_fine_type"], m["one_off_fine_type"], m["one_off_fixed_amount"] = "3", "1", books.Format(f.Amount)
	case f.Frequency == "once":
		m["loan_fine_type"], m["one_off_fine_type"] = "3", "2"
		m["one_off_percentage_rate"], m["one_off_percentage_rate_on"] = rateText(f.Rate), rateOnCodes[f.RateOn]
	case f.Kind == "fixed":
		m["loan_fine_type"], m["fixed_fine_amount"], m["fixed_amount_fine_frequency"] = "1", books.Format(f.Amount), fineFreqCodes[f.Frequency]
		m["fixed_amount_fine_frequency_on"] = "1"
		if f.Scope == "loan" {
			m["fixed_amount_fine_frequency_on"] = "2"
		}
	default:
		m["loan_fine_type"], m["percentage_fine_rate"] = "2", rateText(f.Rate)
		m["percentage_fine_frequency"], m["percentage_fine_on"] = fineFreqCodes[f.Frequency], rateOnCodes[f.RateOn]
	}
	return m
}

func balanceFineFields(f *loans.FineRule) map[string]any {
	m := map[string]any{"enable_outstanding_loan_balance_fines": 0, "outstanding_loan_balance_fine_type": 0,
		"outstanding_loan_balance_fine_fixed_amount": 0, "outstanding_loan_balance_fixed_fine_frequency": 0,
		"outstanding_loan_balance_percentage_fine_rate": 0, "outstanding_loan_balance_percentage_fine_frequency": 0,
		"outstanding_loan_balance_percentage_fine_on": 0, "outstanding_loan_balance_fine_one_off_amount": 0}
	if f == nil {
		return m
	}
	m["enable_outstanding_loan_balance_fines"] = "1"
	switch {
	case f.Frequency == "once" && f.Kind == "fixed":
		m["outstanding_loan_balance_fine_type"], m["outstanding_loan_balance_fine_one_off_amount"] = "3", books.Format(f.Amount)
	case f.Kind == "fixed":
		m["outstanding_loan_balance_fine_type"] = "1"
		m["outstanding_loan_balance_fine_fixed_amount"] = books.Format(f.Amount)
		m["outstanding_loan_balance_fixed_fine_frequency"] = fineFreqCodes[f.Frequency]
	default:
		m["outstanding_loan_balance_fine_type"] = "2"
		m["outstanding_loan_balance_percentage_fine_rate"] = rateText(f.Rate)
		m["outstanding_loan_balance_percentage_fine_frequency"] = fineFreqCodes[f.Frequency]
		m["outstanding_loan_balance_percentage_fine_on"] = rateOnCodes[f.RateOn]
	}
	return m
}

func feeFields(f *loans.ProcessingFee) map[string]any {
	m := map[string]any{"enable_loan_processing_fee": 0, "loan_processing_fee_type": 0, "loan_processing_fee_fixed_amount": 0,
		"loan_processing_fee_percentage_rate": 0, "loan_processing_fee_percentage_charged_on": 0}
	if f == nil {
		return m
	}
	m["enable_loan_processing_fee"] = "1"
	if f.Kind == "fixed" {
		m["loan_processing_fee_type"], m["loan_processing_fee_fixed_amount"] = "1", books.Format(f.Amount)
	} else {
		m["loan_processing_fee_type"], m["loan_processing_fee_percentage_rate"] = "2", rateText(f.Rate)
		m["loan_processing_fee_percentage_charged_on"] = "1"
		if f.On == "amount_plus_interest" {
			m["loan_processing_fee_percentage_charged_on"] = "2"
		}
	}
	return m
}

func guarantorFields(t books.LoanType) map[string]any {
	m := map[string]any{"enable_loan_guarantors": 0, "loan_guarantors_type": 0, "minimum_guarantors": i32(t.MinGuarantors)}
	switch t.Guarantors {
	case "always":
		m["enable_loan_guarantors"], m["loan_guarantors_type"] = "1", "1"
	case "over_max":
		m["enable_loan_guarantors"], m["loan_guarantors_type"] = "1", "2"
	}
	return m
}

func periodType(t books.LoanType) string {
	if t.FixedTerm != nil {
		return "1"
	}
	return "2"
}

// loanTypeGeneral is the legacy loan_types row's general columns.
func loanTypeGeneral(id int64, t books.LoanType) map[string]any {
	amountType, times := "1", any(0)
	if t.AmountBasis == "savings" {
		amountType, times = "2", strOr0(t.SavingsMultiple)
	}
	return map[string]any{
		"id": fmt.Sprint(id), "name": t.Name, "loan_amount_type": amountType,
		"minimum_loan_amount": strOr0(t.MinAmount), "maximum_loan_amount": strOr0(t.MaxAmount), "savings_times": times,
		"enable_reducing_balance_installment_recalculation": 0, "interest_type": interestTypeCodes[t.InterestType],
		"interest_rate": rateText(t.InterestRate), "loan_interest_rate_per": ratePerCodes[t.RatePer],
		"loan_repayment_period_type": periodType(t), "minimum_repayment_period": i32(t.MinTerm),
		"maximum_repayment_period": i32(t.MaxTerm), "fixed_repayment_period": i32(t.FixedTerm),
		"grace_period": strconv.Itoa(int(t.GracePeriods)),
	}
}

func merge(ms ...map[string]any) map[string]any {
	out := map[string]any{}
	for _, m := range ms {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// Texts for get_group_loan_types_list.

func periodText(t books.LoanType, word string) string {
	switch {
	case t.FixedTerm != nil:
		return fmt.Sprintf("Fixed repayment %sof %d month(s)", word, *t.FixedTerm)
	case t.MinTerm != nil || t.MaxTerm != nil:
		return fmt.Sprintf("Varying repayment %sbetween %v and %v month(s)", word, i32(t.MinTerm), i32(t.MaxTerm))
	}
	return ""
}

func amountText(t books.LoanType, cur string) string {
	if t.AmountBasis == "savings" {
		return fmt.Sprint(strOr0(t.SavingsMultiple)) + " times member savings"
	}
	min, max := "0", "0"
	if t.MinAmount != nil {
		min = *t.MinAmount
	}
	if t.MaxAmount != nil {
		max = *t.MaxAmount
	}
	return "Between " + cur + " " + commaMoney(min) + " - " + cur + " " + commaMoney(max)
}

func guarantorText(t books.LoanType) string {
	switch t.Guarantors {
	case "always":
		return fmt.Sprintf("Atleast %v guarantors every time a member is applying a loan ", i32(t.MinGuarantors))
	case "over_max":
		return fmt.Sprintf("Atleast %v guarantors when loan request exceeds maximum loan amount", i32(t.MinGuarantors))
	}
	return "Not Required"
}

func lateFineText(f *loans.FineRule, cur string) string {
	if f == nil {
		return "Disabled"
	}
	m := lateFineFields(f)
	typ := m["loan_fine_type"].(string)
	out := fineTypeNames[typ] + " of "
	switch typ {
	case "1":
		out += cur + " " + commaMoney(f.Amount) + " fine " + fineFreqNames[m["fixed_amount_fine_frequency"].(string)] + " on " +
			fixedOnNames[m["fixed_amount_fine_frequency_on"].(string)]
	case "2":
		out += rateText(f.Rate) + "% fine " + fineFreqNames[m["percentage_fine_frequency"].(string)] + " on " + rateOnNames[rateOnCodes[f.RateOn]]
	default:
		if f.Kind == "fixed" {
			out += cur + " " + commaMoney(f.Amount) + " per Installment"
		} else {
			out += rateText(f.Rate) + "% on " + rateOnNames[rateOnCodes[f.RateOn]]
		}
	}
	return out
}

func balanceFineText(f *loans.FineRule, cur string) string {
	switch {
	case f == nil:
		return "Disabled"
	case f.Frequency == "once":
		return "One Off Amount of " + cur + " " + commaMoney(f.Amount)
	case f.Kind == "fixed":
		return cur + " " + commaMoney(f.Amount) + " " + fineFreqNames[fineFreqCodes[f.Frequency]]
	}
	return rateText(f.Rate) + "% fine " + fineFreqNames[fineFreqCodes[f.Frequency]] + " on " + rateOnNames[rateOnCodes[f.RateOn]]
}

func feeText(f *loans.ProcessingFee) string {
	switch {
	case f == nil:
		return "No Charge"
	case f.Kind == "fixed":
		return "Fixed Amount of " + commaMoney(f.Amount)
	case f.On == "amount_plus_interest":
		return rateText(f.Rate) + "% of " + feeOnNames["2"]
	}
	return rateText(f.Rate) + "% of " + feeOnNames["1"]
}

func interestText(t books.LoanType) string {
	return rateText(t.InterestRate) + "% per " + ratePerNames[ratePerCodes[t.RatePer]] + " on " + interestTypeNames[interestTypeCodes[t.InterestType]]
}

type loanTypeRow struct {
	id int64
	t  books.LoanType
}

func (s *Server) loanTypes(ctx context.Context, g *member, hidden bool) ([]loanTypeRow, error) {
	rows, err := s.q.ListLoanTypes(ctx, store.ListLoanTypesParams{GroupID: g.group.ID, IncludeHidden: hidden})
	if err != nil {
		return nil, err
	}
	out := make([]loanTypeRow, 0, len(rows))
	for _, r := range rows {
		t, err := books.LoanTypeFrom(r)
		if err != nil {
			return nil, err
		}
		out = append(out, loanTypeRow{r.ID, t})
	}
	return out, nil
}

func (s *Server) loanTypesList(ctx context.Context, c *call, g *member) (any, error) {
	all, err := s.loanTypes(ctx, g, true)
	if err != nil {
		return nil, err
	}
	lower, upper := c.req.Int("lower_limit"), c.req.Int("upper_limit")
	if upper <= lower {
		upper = lower + 20
	}
	cur := g.group.Currency
	out := []any{}
	for i, r := range all {
		if int64(i) < lower || int64(i) >= upper {
			continue
		}
		t := r.t
		gf := guarantorFields(t)
		out = append(out, map[string]any{
			"id": fmt.Sprint(r.id), "name": t.Name, "repayment_period": periodText(t, ""), "loan_amount": amountText(t, cur),
			"interest_rate": interestText(t), "loan_processing": feeText(t.ProcessingFee), "guarantors": guarantorText(t),
			"enable_loan_guarantors": gf["enable_loan_guarantors"], "minimum_guarantors": minGuarantorsText(t),
			"late_payment_fines": lateFineText(t.LateFine, cur), "outstanding_payment_fines": balanceFineText(t.BalanceFine, cur),
			"is_hidden": boolInt(t.IsHidden), "repayment_period_type": periodType(t), "fixed_repayment_period": fixedPeriodText(t),
			"minimum_repayment_period": i32(t.MinTerm), "maximum_repayment_period": i32(t.MaxTerm),
		})
	}
	return timed(map[string]any{"status": 1, "message": "deposit list", "loan_types": out}, s.now()), nil
}

func minGuarantorsText(t books.LoanType) any {
	if t.MinGuarantors == nil {
		return ""
	}
	return strconv.Itoa(int(*t.MinGuarantors))
}

func fixedPeriodText(t books.LoanType) any {
	if t.FixedTerm == nil {
		return ""
	}
	return strconv.Itoa(int(*t.FixedTerm))
}

// loanTypeOptions is get_group_loan_type_options: the visible types with
// every legacy setting, for the loan and application forms.
func (s *Server) loanTypeOptions(ctx context.Context, c *call, g *member) (any, error) {
	all, err := s.loanTypes(ctx, g, false)
	if err != nil {
		return nil, err
	}
	cur := g.group.Currency
	out := []any{}
	for _, r := range all {
		t := r.t
		gen := loanTypeGeneral(r.id, t)
		row := merge(lateFineFields(t.LateFine), balanceFineFields(t.BalanceFine), feeFields(t.ProcessingFee), guarantorFields(t), map[string]any{
			"id": gen["id"], "name": t.Name, "description": amountText(t, cur) + " for " + periodText(t, "period "),
			"repayment_period_type": gen["loan_repayment_period_type"], "interest_type": gen["interest_type"],
			"interest_rate": gen["interest_rate"], "loan_interest_rate_per": gen["loan_interest_rate_per"],
			"loan_repayment_period_type": gen["loan_repayment_period_type"], "minimum_repayment_period": gen["minimum_repayment_period"],
			"maximum_repayment_period": gen["maximum_repayment_period"], "fixed_repayment_period": gen["fixed_repayment_period"],
		})
		out = append(out, row)
	}
	return timed(map[string]any{"status": 1, "message": "deposit list", "loan_types": out}, s.now()), nil
}

func (s *Server) loanTypeApplicationOptions(ctx context.Context, c *call, g *member) (any, error) {
	all, err := s.loanTypes(ctx, g, false)
	if err != nil {
		return nil, err
	}
	out := []any{}
	for _, r := range all {
		out = append(out, map[string]any{"name": r.t.Name, "repayment_period_type": periodType(r.t)})
	}
	return timed(map[string]any{"status": 1, "message": "deposit list", "loan_types": out}, s.now()), nil
}

// oneLoanType loads the type named by the request's id, or nil.
func (s *Server) oneLoanType(ctx context.Context, c *call, g *member) (int64, *books.LoanType, error) {
	id, err := strconv.ParseInt(c.req.Str("id"), 10, 64)
	if err != nil {
		return 0, nil, nil
	}
	r, err := s.q.GetLoanType(ctx, store.GetLoanTypeParams{GroupID: g.group.ID, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil, nil
	}
	if err != nil {
		return 0, nil, err
	}
	t, err := books.LoanTypeFrom(r)
	return id, &t, err
}

var noLoanType = status(0, "Could not find group loan type selected")

func (s *Server) groupLoanType(ctx context.Context, c *call, g *member) (any, error) {
	id, t, err := s.oneLoanType(ctx, c, g)
	if err != nil || t == nil {
		return timed(noLoanType, s.now()), err
	}
	gen := loanTypeGeneral(id, *t)
	delete(gen, "grace_period")
	return timed(map[string]any{"status": 1, "message": "successful", "data": map[string]any{
		"loan_type":       gen,
		"fine":            merge(lateFineFields(t.LateFine), balanceFineFields(t.BalanceFine)),
		"general_details": merge(feeFields(t.ProcessingFee), guarantorFields(*t)),
	}}, s.now()), nil
}

// reqRow lets the legacy import readers parse the app's legacy columns.
func reqRow(c *call) legacy.Row {
	r := legacy.Row{}
	for k := range c.req {
		v := c.req.Str(k)
		r[k] = &v
	}
	return r
}

func posInt32(v string) *int32 {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return nil
	}
	x := int32(n)
	return &x
}

// applyGeneral sets a type's general settings from the create/edit form.
func applyGeneral(c *call, t *books.LoanType) map[string]string {
	errs := map[string]string{}
	if c.req.Str("name") == "" {
		errs["name"] = "The Loan Type Name field is required."
	}
	t.Name = strings.ToUpper(c.req.Str("name"))
	t.InterestType = map[string]string{"1": "fixed_balance", "2": "reducing_balance", "4": "fixed_amount"}[c.req.Str("interest_type")]
	if t.InterestType == "" {
		errs["interest_type"] = "The Interest Type field is required."
	}
	t.InterestRate, _ = strconv.ParseFloat(c.req.Str("interest_rate"), 64)
	t.RatePer = map[string]string{"1": "day", "2": "week", "3": "month", "4": "year", "5": "term"}[c.req.Str("interest_rate_per")]
	if t.RatePer == "" {
		t.RatePer = "month"
	}
	t.FixedTerm, t.MinTerm, t.MaxTerm = nil, nil, nil
	switch c.req.Str("repayment_period_type") {
	case "1":
		if t.FixedTerm = posInt32(c.req.Str("fixed_repayment_period")); t.FixedTerm == nil {
			errs["fixed_repayment_period"] = "The Fixed repayment period field is required."
		}
	case "2":
		t.MinTerm, t.MaxTerm = posInt32(c.req.Str("minimum_repayment_period")), posInt32(c.req.Str("maximum_repayment_period"))
		if t.MinTerm == nil {
			errs["minimum_repayment_period"] = "The Minimum repayment period field is required."
		}
		if t.MaxTerm == nil {
			errs["maximum_repayment_period"] = "The Maximum Repayment Period field is required."
		}
	default:
		errs["repayment_period_type"] = "The Repayment Period Type field is required."
	}
	switch c.req.Str("loan_amount_type") {
	case "2":
		t.AmountBasis, t.MinAmount, t.MaxAmount = "savings", nil, nil
		if v := appAmount(c.req.Str("savings_times")); v != "" {
			t.SavingsMultiple = &v
		} else {
			errs["savings_times"] = "The Member Savings field is required."
		}
	default:
		t.AmountBasis, t.SavingsMultiple = "range", nil
		min, max := appAmount(c.req.Str("minimum_loan_amount")), appAmount(c.req.Str("maximum_loan_amount"))
		if min == "" {
			errs["minimum_loan_amount"] = "The Minimum Loan Amount field is required."
		}
		if max == "" {
			errs["maximum_loan_amount"] = "The Maximum Loan Amount field is required."
		}
		t.MinAmount, t.MaxAmount = &min, &max
	}
	if gp := posInt32(c.req.Str("grace_period")); gp != nil {
		t.GracePeriods = *gp
	} else if t.GracePeriods == 0 {
		t.GracePeriods = 1
	}
	if c.req.Str("enable_loan_fine_deferment") != "" {
		t.FineDeferment = c.req.Str("enable_loan_fine_deferment") == "1"
	}
	return errs
}

// applyFines sets the late and outstanding-balance fines (update_loan_type_fines).
func applyFines(c *call, t *books.LoanType) map[string]string {
	row := reqRow(c)
	errs := map[string]string{}
	late, ok := legacy.LegacyLateFine(row)
	if !ok {
		errs["loan_fine_type"] = "Enter the late loan payment fine settings in full."
	}
	bal, ok := legacy.LegacyBalanceFine(row)
	if !ok {
		errs["outstanding_loan_balance_fine_type"] = "Enter the outstanding loan balance fine settings in full."
	}
	t.LateFine, t.BalanceFine = late, bal
	return errs
}

// applyGeneralDetails sets the processing fee and guarantors (update_general_details).
func applyGeneralDetails(c *call, t *books.LoanType) map[string]string {
	errs := map[string]string{}
	fee, ok := legacy.LegacyProcessingFee(reqRow(c))
	if !ok {
		errs["loan_processing_fee_type"] = "Enter the loan processing fee settings in full."
	}
	t.ProcessingFee = fee
	t.Guarantors, t.MinGuarantors, t.MaxGuarantors = "none", nil, nil
	if c.req.Str("enable_loan_guarantors") == "1" {
		t.Guarantors = "always"
		if c.req.Str("loan_guarantors_type") == "2" {
			t.Guarantors = "over_max"
		}
		if t.MinGuarantors = posInt32(c.req.Str("minimum_guarantors")); t.MinGuarantors == nil {
			errs["minimum_guarantors"] = "The Minimum Guarantors field is required."
		} else {
			max := *t.MinGuarantors + 2 // as legacy
			t.MaxGuarantors = &max
		}
	}
	return errs
}

func (s *Server) saveLoanTypeAs(ctx context.Context, g *member, id int64, t books.LoanType) (int64, map[string]any, error) {
	err := s.inTx(ctx, func(q *store.Queries) (err error) {
		id, err = books.SaveLoanType(ctx, q, g.group.ID, g.user.ID, id, t)
		return err
	})
	if be, ok := asBooksError(err); ok {
		field := "form"
		if be.Code == "name_taken" {
			field = "name"
		}
		return 0, s.formFailed(map[string]string{field: be.Msg}), nil
	}
	return id, nil, err
}

func (s *Server) createLoanType(ctx context.Context, c *call, g *member) (any, error) {
	t := books.LoanType{Guarantors: "none", NeedsApproval: true, ReducingMethod: string(loans.EqualInstallments), FixedInterest: "0"}
	errs := applyGeneral(c, &t)
	// The app sends fines and general details in later calls; take them now
	// when they come with the form.
	if c.req.Str("enable_loan_fines") == "1" || c.req.Str("enable_outstanding_loan_balance_fines") == "1" {
		for k, v := range applyFines(c, &t) {
			errs[k] = v
		}
	}
	if c.req.Str("enable_loan_processing_fee") == "1" || c.req.Str("enable_loan_guarantors") == "1" {
		for k, v := range applyGeneralDetails(c, &t) {
			errs[k] = v
		}
	}
	if len(errs) > 0 {
		return s.formFailed(errs), nil
	}
	id, fail, err := s.saveLoanTypeAs(ctx, g, 0, t)
	if fail != nil || err != nil {
		return fail, err
	}
	return timed(map[string]any{"status": 1, "message": "Loan type successfully added", "id": fmt.Sprint(id)}, s.now()), nil
}

// editLoanType and the two update actions change part of a type.
func (s *Server) editLoanType(apply func(*call, *books.LoanType) map[string]string, msg string) groupFn {
	return func(ctx context.Context, c *call, g *member) (any, error) {
		id, t, err := s.oneLoanType(ctx, c, g)
		if err != nil || t == nil {
			return timed(noLoanType, s.now()), err
		}
		if errs := apply(c, t); len(errs) > 0 {
			return s.formFailed(errs), nil
		}
		if _, fail, err := s.saveLoanTypeAs(ctx, g, id, *t); fail != nil || err != nil {
			return fail, err
		}
		return timed(status(1, msg), s.now()), nil
	}
}

func (s *Server) hideLoanType(hide bool) groupFn {
	return func(ctx context.Context, c *call, g *member) (any, error) {
		id, t, err := s.oneLoanType(ctx, c, g)
		if err != nil || t == nil {
			return timed(noLoanType, s.now()), err
		}
		if t.IsHidden == hide {
			if hide {
				return timed(status(0, "Loan type already inactive"), s.now()), nil
			}
			return timed(status(0, "Loan type already active"), s.now()), nil
		}
		t.IsHidden = hide
		if _, fail, err := s.saveLoanTypeAs(ctx, g, id, *t); fail != nil || err != nil {
			return fail, err
		}
		return timed(status(1, "successful"), s.now()), nil
	}
}

func (s *Server) deleteLoanType(ctx context.Context, c *call, g *member) (any, error) {
	id, t, err := s.oneLoanType(ctx, c, g)
	if err != nil || t == nil {
		return timed(noLoanType, s.now()), err
	}
	used, err := s.q.LoanTypeInUse(ctx, id)
	if err != nil {
		return nil, err
	}
	if used {
		return timed(status(0, "Loan type already in use. Can not be deleted. Option to hide the loan type"), s.now()), nil
	}
	if _, err := s.q.DeleteLoanType(ctx, store.DeleteLoanTypeParams{GroupID: g.group.ID, ID: id}); err != nil {
		return nil, err
	}
	return timed(status(1, "Loan type successfully removed"), s.now()), nil
}
