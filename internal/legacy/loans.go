package legacy

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/swahome/legion-api/internal/loans"
)

// Loans: loan types, loans, their installments and fines (loan_invoices),
// repayments and guarantors. See docs/schema/loans.md.

var legacyInterestTypes = map[int64]string{1: "fixed_balance", 2: "reducing_balance", 3: "custom", 4: "fixed_amount"}
var legacyRatePer = map[int64]string{1: "day", 2: "week", 3: "month", 4: "year", 5: "term"}
var legacyFrequencies = map[int64]string{1: "daily", 2: "weekly", 3: "monthly"}
var legacyFineFrequencies = map[int64]string{1: "daily", 2: "weekly", 3: "monthly", 4: "yearly"}
var legacyRateOn = map[int64]string{1: "installment_balance", 2: "loan_amount", 3: "unpaid_balance", 4: "installment_interest"}

// legacyAmount reads a legacy money column ("25,000", "1425.06") as a
// two-decimal string, or "" when it is blank, junk or not positive.
func legacyAmount(s string) string {
	f, err := strconv.ParseFloat(strings.ReplaceAll(clean(s), ",", ""), 64)
	if err != nil || f <= 0 || math.IsInf(f, 0) {
		return ""
	}
	return strconv.FormatFloat(f, 'f', 2, 64)
}

func legacyRate(s string) float64 {
	f, err := strconv.ParseFloat(clean(s), 64)
	if err != nil || f < 0 {
		return 0
	}
	return f
}

// LegacyLateFine reads a loan's or loan type's late-payment fine
// (enable_loan_fines, loan_fine_type 1 fixed, 2 percentage, 3 one-off).
func LegacyLateFine(r Row) (*loans.FineRule, bool) {
	if !r.Flag("enable_loan_fines") {
		return nil, true
	}
	var f loans.FineRule
	switch r.Int("loan_fine_type") {
	case 1:
		f = loans.FineRule{Kind: "fixed", Amount: legacyAmount(r.Str("fixed_fine_amount")),
			Frequency: legacyFineFrequencies[r.Int("fixed_amount_fine_frequency")], Scope: "installment"}
		if r.Int("fixed_amount_fine_frequency_on") == 2 {
			f.Scope = "loan"
		}
	case 2:
		f = loans.FineRule{Kind: "percentage", Rate: legacyRate(r.Str("percentage_fine_rate")),
			RateOn: legacyRateOn[r.Int("percentage_fine_on")], Frequency: legacyFineFrequencies[r.Int("percentage_fine_frequency")]}
	case 3:
		f = loans.FineRule{Kind: "fixed", Amount: legacyAmount(r.Str("one_off_fixed_amount")), Frequency: "once"}
		if r.Int("one_off_fine_type") == 2 {
			f = loans.FineRule{Kind: "percentage", Rate: legacyRate(r.Str("one_off_percentage_rate")),
				RateOn: legacyRateOn[r.Int("one_off_percentage_rate_on")], Frequency: "once"}
		}
	default:
		return nil, false
	}
	if err := f.Check(false); err != nil {
		return nil, false
	}
	return &f, true
}

// LegacyBalanceFine reads the outstanding-balance fine
// (enable_outstanding_loan_balance_fines, outstanding_loan_balance_fine_type
// 1 fixed, 2 percentage, 3 one-off).
func LegacyBalanceFine(r Row) (*loans.FineRule, bool) {
	if !r.Flag("enable_outstanding_loan_balance_fines") {
		return nil, true
	}
	var f loans.FineRule
	switch r.Int("outstanding_loan_balance_fine_type") {
	case 1:
		f = loans.FineRule{Kind: "fixed", Amount: legacyAmount(r.Str("outstanding_loan_balance_fine_fixed_amount")),
			Frequency: legacyFineFrequencies[r.Int("outstanding_loan_balance_fixed_fine_frequency")]}
	case 2:
		on := legacyRateOn[r.Int("outstanding_loan_balance_percentage_fine_on")]
		if on == "installment_balance" || on == "installment_interest" {
			on = "unpaid_balance"
		}
		f = loans.FineRule{Kind: "percentage", Rate: legacyRate(r.Str("outstanding_loan_balance_percentage_fine_rate")),
			RateOn: on, Frequency: legacyFineFrequencies[r.Int("outstanding_loan_balance_percentage_fine_frequency")]}
	case 3:
		f = loans.FineRule{Kind: "fixed", Amount: legacyAmount(r.Str("outstanding_loan_balance_fine_one_off_amount")), Frequency: "once"}
	default:
		return nil, false
	}
	if err := f.Check(true); err != nil {
		return nil, false
	}
	return &f, true
}

// LegacyProcessingFee reads enable_loan_processing_fee and its settings.
func LegacyProcessingFee(r Row) (*loans.ProcessingFee, bool) {
	if !r.Flag("enable_loan_processing_fee") {
		return nil, true
	}
	var f loans.ProcessingFee
	switch r.Int("loan_processing_fee_type") {
	case 1:
		f = loans.ProcessingFee{Kind: "fixed", Amount: legacyAmount(r.Str("loan_processing_fee_fixed_amount"))}
	case 2:
		f = loans.ProcessingFee{Kind: "percentage", Rate: legacyRate(r.Str("loan_processing_fee_percentage_rate")), On: "amount"}
		if r.Int("loan_processing_fee_percentage_charged_on") == 2 {
			f.On = "amount_plus_interest"
		}
	default:
		return nil, false
	}
	if err := f.Check(); err != nil {
		return nil, false
	}
	return &f, true
}

// rules converts the three rule sets of a row, reporting unreadable ones.
func (p *Plan) rules(table string, r Row) (late, balance, fee []byte) {
	lf, ok := LegacyLateFine(r)
	if !ok {
		p.Report.issue("%s: late fine settings incomplete, fine dropped", table)
	}
	bf, ok := LegacyBalanceFine(r)
	if !ok {
		p.Report.issue("%s: outstanding balance fine settings incomplete, fine dropped", table)
	}
	pf, ok := LegacyProcessingFee(r)
	if !ok {
		p.Report.issue("%s: processing fee settings incomplete, fee dropped", table)
	}
	return loans.Encode(lf), loans.Encode(bf), loans.Encode(pf)
}

func clampInt(v, lo, hi int64) int64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func positiveInt(r Row, col string) *int64 {
	if v := r.Int(col); v > 0 {
		return &v
	}
	return nil
}

func amountPtr(r Row, col string) *string {
	if v := legacyAmount(r.Str(col)); v != "" {
		return &v
	}
	return nil
}

var LoanTypeCols = []string{"id", "group_id", "name", "interest_type", "interest_rate", "rate_per", "fixed_interest",
	"fixed_interest_rate", "frequency", "fixed_term", "min_term", "max_term", "grace_periods", "amount_basis",
	"min_amount", "max_amount", "savings_multiple", "guarantors", "min_guarantors", "max_guarantors",
	"late_fine", "balance_fine", "processing_fee", "fine_deferment", "needs_approval", "is_hidden", "settings",
	"created_by", "created_at", "updated_at", "deleted_at"}

var loanTypeColumns = set("id", "group_id", "name", "interest_type", "interest_rate", "loan_interest_rate_per",
	"fixed_interest_type", "fixed_interest_amount", "fixed_percentage_interest", "loan_repayment_frequency",
	"loan_repayment_period_type", "fixed_repayment_period", "minimum_repayment_period", "maximum_repayment_period",
	"grace_period", "loan_amount_type", "minimum_loan_amount", "maximum_loan_amount", "loan_times_number",
	"enable_loan_guarantors", "loan_guarantors_type", "minimum_guarantors", "maximum_guarantors",
	"enable_loan_fine_deferment", "enable_loan_approvals", "is_hidden", "is_deleted", "active",
	"created_by", "created_on", "modified_by", "modified_on", "old_id")

// loanTypes keeps legacy ids; it returns loan type id -> group.
func (p *Plan) loanTypes(rows []Row, ref refSets, now time.Time) map[int64]int64 {
	ids := map[int64]int64{}
	for _, r := range byID(rows) {
		id, gid := r.Int("id"), r.Int("group_id")
		if id <= 0 || !ref.groups[gid] {
			p.Report.issue("loan_types: group missing, skipped")
			continue
		}
		name := clean(r.Str("name"))
		if name == "" {
			name = "Loan"
			p.Report.issue("loan_types: blank name, defaulted")
		}
		it := legacyInterestTypes[r.Int("interest_type")]
		if it == "" || it == "custom" {
			it = "fixed_balance"
		}
		per := legacyRatePer[r.Int("loan_interest_rate_per")]
		if per == "" {
			per = "month"
		}
		freq := legacyFrequencies[r.Int("loan_repayment_frequency")]
		if freq == "" {
			freq = "monthly"
		}
		fixedAmt, fixedRate := "0", 0.0
		if r.Int("fixed_interest_type") == 1 {
			if v := legacyAmount(r.Str("fixed_interest_amount")); v != "" {
				fixedAmt = v
			}
		} else {
			fixedRate = legacyRate(r.Str("fixed_percentage_interest"))
		}
		var fixedTerm, minTerm, maxTerm *int64
		if r.Int("loan_repayment_period_type") == 1 {
			fixedTerm = positiveInt(r, "fixed_repayment_period")
		} else {
			minTerm, maxTerm = positiveInt(r, "minimum_repayment_period"), positiveInt(r, "maximum_repayment_period")
		}
		if fixedTerm == nil && minTerm == nil && maxTerm == nil {
			p.Report.issue("loan_types: no repayment period, left open")
		}
		basis := "range"
		var multiple *string
		if r.Int("loan_amount_type") == 2 {
			basis = "savings"
			if v := legacyAmount(r.Str("loan_times_number")); v != "" {
				multiple = &v
			}
		}
		guar := "none"
		if r.Flag("enable_loan_guarantors") {
			guar = "always"
			if r.Int("loan_guarantors_type") == 2 {
				guar = "over_max"
			}
		}
		late, balance, fee := p.rules("loan_types", r)
		created := timeOr(epoch(r.Str("created_on")), now)
		var deleted *time.Time
		if r.Flag("is_deleted") {
			d := timeOr(epoch(r.Str("modified_on")), created)
			deleted = &d
		}
		p.LoanTypes = append(p.LoanTypes, []any{id, gid, name, it, strconv.FormatFloat(legacyRate(r.Str("interest_rate")), 'f', 4, 64),
			per, fixedAmt, strconv.FormatFloat(fixedRate, 'f', 4, 64), freq, fixedTerm, minTerm, maxTerm,
			clampInt(r.Int("grace_period"), 1, 24), basis, amountPtr(r, "minimum_loan_amount"), amountPtr(r, "maximum_loan_amount"),
			multiple, guar, positiveInt(r, "minimum_guarantors"), positiveInt(r, "maximum_guarantors"),
			late, balance, fee, r.Flag("enable_loan_fine_deferment"), r.Str("enable_loan_approvals") != "0",
			r.Flag("is_hidden") || r.Str("active") == "0", extras(r, loanTypeColumns), p.createdBy(r, ref.users), created,
			timeOr(epoch(r.Str("modified_on")), created), deleted})
		ids[id] = gid
	}
	return ids
}

var LoanCols = []string{"id", "group_id", "membership_id", "loan_type_id", "amount", "disbursed_on", "account_id",
	"interest_type", "interest_rate", "rate_per", "reducing_method", "fixed_interest", "fixed_interest_rate", "frequency",
	"installments", "grace_periods", "first_due_on", "ends_on", "late_fine", "balance_fine", "processing_fee",
	"fine_deferment", "next_balance_fine_on", "is_bad_debt", "settings", "is_backdated", "created_by", "created_at",
	"updated_at", "voided_at"}

var loanColumns = set("id", "group_id", "member_id", "loan_type", "loan_type_id", "loan_amount", "disbursement_date",
	"account_id", "interest_type", "interest_rate", "loan_interest_rate_per", "reducing_interest_option",
	"fixed_interest_type", "fixed_interest_amount", "fixed_percentage_interest", "loan_repayment_frequency",
	"repayment_period", "grace_period", "grace_period_date", "loan_end_date", "enable_loan_fine_deferment",
	"outstanding_loan_balance_fine_date", "is_a_bad_loan", "is_a_back_dating_record", "active",
	"created_by", "created_on", "modified_by", "modified_on", "old_id", "is_fully_paid", "grace_period_end_date")

// loanFacts is what later passes need to know about an imported loan.
type loanFacts struct {
	group, member int64
}

// loanSet is the imported loans.
type loanSet map[int64]loanFacts

func (s loanSet) in(id, gid int64) *int64 {
	if f, ok := s[id]; ok && f.group == gid {
		return &id
	}
	return nil
}

// loans keeps legacy ids. The installments are imported as they are (the
// legacy app rewrote them after repayments on reducing balance loans), so
// only the loan's end date is taken from them when the loan has none.
func (p *Plan) loans(rows []Row, invoices []Row, ref refSets, ix *accountIndex, loanTypes map[int64]int64, now time.Time) loanSet {
	lastDue := map[int64]time.Time{}
	for _, r := range invoices {
		if r.Int("type") == 1 && r.Flag("active") {
			if d := epochDate(r.Str("due_date")); d != nil && d.After(lastDue[r.Int("loan_id")]) {
				lastDue[r.Int("loan_id")] = *d
			}
		}
	}
	out := loanSet{}
	for _, r := range byID(rows) {
		id, gid := r.Int("id"), r.Int("group_id")
		if !ref.groups[gid] {
			p.Report.issue("loans: group missing, skipped")
			continue
		}
		mid := r.Int("member_id")
		if ref.members[mid] != gid {
			p.Report.issue("loans: member missing or in another group, skipped")
			continue
		}
		amount := legacyAmount(r.Str("loan_amount"))
		if amount == "" {
			p.Report.issue("loans: zero or bad amount, skipped")
			continue
		}
		disbursed := epochDate(r.Str("disbursement_date"))
		if disbursed == nil {
			disbursed = epochDate(r.Str("created_on"))
		}
		if disbursed == nil {
			p.Report.issue("loans: no disbursement date, skipped")
			continue
		}
		var typeID *int64
		for _, col := range []string{"loan_type_id", "loan_type"} {
			if v := r.Int(col); v > 0 && loanTypes[v] == gid {
				typeID = &v
				break
			}
		}
		it := legacyInterestTypes[r.Int("interest_type")]
		if it == "" {
			it = "fixed_balance"
			p.Report.issue("loans: no interest type, taken as fixed balance")
		}
		per := legacyRatePer[r.Int("loan_interest_rate_per")]
		if per == "" {
			per = "month"
		}
		freq := legacyFrequencies[r.Int("loan_repayment_frequency")]
		if freq == "" {
			freq = "monthly"
		}
		reducing := "equal_installments"
		if r.Int("reducing_interest_option") == 1 {
			reducing = "equal_principal"
		}
		fixedAmt, fixedRate := "0", 0.0
		if r.Int("fixed_interest_type") == 1 {
			if v := legacyAmount(r.Str("fixed_interest_amount")); v != "" {
				fixedAmt = v
			}
		} else {
			fixedRate = legacyRate(r.Str("fixed_percentage_interest"))
		}
		installments := int64(math.Round(legacyRate(r.Str("repayment_period"))))
		if installments < 1 || installments > loans.MaxInstallments {
			p.Report.issue("loans: repayment period out of range, set to 1")
			installments = 1
		}
		grace := r.Int("grace_period")
		var firstDue *time.Time
		if grace <= 0 {
			firstDue = epochDate(r.Str("grace_period_date"))
		}
		ends := epochDate(r.Str("loan_end_date"))
		if d, ok := lastDue[id]; ok && (ends == nil || d.After(*ends)) {
			ends = &d
		}
		if ends == nil {
			d := loans.AddPeriods(*disbursed, loans.Frequency(freq), int(installments))
			ends = &d
		}
		late, balance, fee := p.rules("loans", r)
		var nextBalanceFine *time.Time
		if balance != nil {
			nextBalanceFine = epochDate(r.Str("outstanding_loan_balance_fine_date"))
		}
		acctKind := ""
		if prefix, _, ok := strings.Cut(clean(r.Str("account_id")), "-"); ok {
			acctKind = legacyAccountPrefixes[prefix]
		}
		var acct *int64
		if acctKind != "" {
			acct = p.accountFor("loans", ix, gid, acctKind, r.Str("account_id"))
		}
		created := timeOr(epoch(r.Str("created_on")), now)
		var voided *time.Time
		if !r.Flag("active") {
			v := timeOr(epoch(r.Str("modified_on")), created)
			voided = &v
		}
		p.Loans = append(p.Loans, []any{id, gid, mid, typeID, amount, *disbursed, acct, it,
			strconv.FormatFloat(legacyRate(r.Str("interest_rate")), 'f', 4, 64), per, reducing, fixedAmt,
			strconv.FormatFloat(fixedRate, 'f', 4, 64), freq, installments, clampInt(grace, 1, 24), firstDue, *ends,
			late, balance, fee, r.Flag("enable_loan_fine_deferment"), nextBalanceFine, r.Flag("is_a_bad_loan"),
			extras(r, loanColumns), r.Flag("is_a_back_dating_record"), p.createdBy(r, ref.users), created,
			timeOr(epoch(r.Str("modified_on")), created), voided})
		out[id] = loanFacts{group: gid, member: mid}
	}
	return out
}

var LoanChargeCols = []string{"id", "group_id", "loan_id", "kind", "number", "invoice_on", "due_on", "next_fine_on",
	"principal", "interest", "amount", "parent_id", "no_fines", "sent_at", "refs", "legacy_type", "created_by",
	"created_at", "voided_at"}

var legacyChargeKinds = map[int64]string{1: "installment", 2: "late_fine", 3: "balance_fine", 5: "transfer_out"}

func moneyOrZero(s string) string {
	f, err := strconv.ParseFloat(strings.ReplaceAll(clean(s), ",", ""), 64)
	if err != nil || f < 0 {
		return "0.00"
	}
	return strconv.FormatFloat(f, 'f', 2, 64)
}

// loanCharges imports loan_invoices. Installments are numbered by due date
// within each loan.
func (p *Plan) loanCharges(rows []Row, ref refSets, loanSet loanSet, now time.Time) {
	known := map[int64]bool{}
	type pending struct {
		row  []any
		loan int64
		due  time.Time
		id   int64
		live bool
	}
	var installments []pending
	var others [][]any
	for _, r := range byID(rows) {
		id, gid, lid := r.Int("id"), r.Int("group_id"), r.Int("loan_id")
		kind, ok := legacyChargeKinds[r.Int("type")]
		if !ok {
			p.Report.issue("loan_invoices: unknown type %d, skipped", r.Int("type"))
			continue
		}
		if loanSet.in(lid, gid) == nil {
			p.Report.issue("loan_invoices: loan missing or in another group, skipped")
			continue
		}
		due := epochDate(r.Str("due_date"))
		if due == nil {
			due = epochDate(r.Str("invoice_date"))
		}
		if due == nil {
			p.Report.issue("loan_invoices: no date, skipped")
			continue
		}
		inv := epochDate(r.Str("invoice_date"))
		if inv == nil {
			inv = due
		}
		var nextFine *time.Time
		if kind == "installment" {
			nextFine = epochDate(r.Str("fine_date"))
		}
		created := timeOr(epoch(r.Str("created_on")), now)
		var sent, voided *time.Time
		if r.Flag("is_sent") {
			sent = &created
		}
		if !r.Flag("active") {
			v := timeOr(epoch(r.Str("modified_on")), created)
			voided = &v
		}
		row := []any{id, gid, lid, kind, nil, *inv, *due, nextFine, moneyOrZero(r.Str("principle_amount_payable")),
			moneyOrZero(r.Str("interest_amount_payable")), moneyOrZero(r.Str("amount_payable")), nil,
			r.Flag("disable_fines"), sent, refsJSON(r, "contribution_transfer_id", "transfer_to", "invoice_no"),
			r.Int("type"), p.createdBy(r, ref.users), created, voided}
		known[id] = true
		if kind == "installment" {
			installments = append(installments, pending{row, lid, *due, id, voided == nil})
		} else {
			others = append(others, row)
		}
		if parent := r.Int("fine_parent_loan_invoice_id"); parent > 0 {
			row[11] = parent // checked once every charge is known
		}
	}
	sort.SliceStable(installments, func(i, j int) bool {
		a, b := installments[i], installments[j]
		if a.loan != b.loan {
			return a.loan < b.loan
		}
		if !a.due.Equal(b.due) {
			return a.due.Before(b.due)
		}
		return a.id < b.id
	})
	n, prev := 0, int64(0)
	for _, x := range installments {
		if x.loan != prev {
			n, prev = 0, x.loan
		}
		if x.live { // live installments are numbered
			n++
			x.row[4] = n
		}
		p.LoanCharges = append(p.LoanCharges, x.row)
	}
	p.LoanCharges = append(p.LoanCharges, others...)
	for _, row := range p.LoanCharges {
		if parent, ok := row[11].(int64); ok && !known[parent] {
			row[11] = nil
			p.Report.issue("loan_invoices: fined installment missing, link cleared")
		}
	}
}

var LoanRepaymentCols = []string{"id", "group_id", "loan_id", "membership_id", "deposit_id", "account_id", "method",
	"amount", "paid_on", "source", "description", "refs", "is_backdated", "created_at", "voided_at"}

// loanRepayments imports loan_repayments, linking each to the deposit that
// recorded it (deposits.loan_repayment_id).
func (p *Plan) loanRepayments(rows []Row, ref refSets, ix *accountIndex, loanSet loanSet, depositOf, depositGroup map[int64]int64, now time.Time) {
	for _, r := range byID(rows) {
		id := r.Int("id")
		m, ok := p.common("loan_repayments", r, "receipt_date", ref, now)
		if !ok {
			continue
		}
		lid := loanSet.in(r.Int("loan_id"), m.gid)
		if lid == nil {
			p.Report.issue("loan_repayments: loan missing or in another group, skipped")
			continue
		}
		var deposit *int64
		source := "transfer"
		if d, ok := depositOf[id]; ok && depositGroup[d] == m.gid {
			deposit, source = &d, "deposit"
		} else if ok {
			p.Report.issue("loan_repayments: deposit in another group, not linked")
		}
		var method *string
		if v, ok := depositMethods[r.Int("payment_method")]; ok {
			method = &v
		}
		var acct *int64
		if prefix, _, ok := strings.Cut(clean(r.Str("account_id")), "-"); ok && legacyAccountPrefixes[prefix] != "" {
			acct, _ = ix.resolve(m.gid, legacyAccountPrefixes[prefix], r.Str("account_id"))
		}
		p.LoanRepayments = append(p.LoanRepayments, []any{id, m.gid, *lid, m.member, deposit, acct, method,
			m.amount, m.date, source, "", refsJSON(r, "incoming_contribution_transfer_id", "incoming_loan_transfer_invoice_id",
				"old_loan_repayment_id", "transfer_from"), r.Flag("is_a_back_dating_record"), m.created, m.voided})
	}
}

var LoanGuarantorCols = []string{"id", "group_id", "loan_id", "membership_id", "amount", "comment", "created_by", "created_at"}

func (p *Plan) loanGuarantors(rows []Row, ref refSets, loanSet loanSet, now time.Time) {
	for _, r := range byID(rows) {
		gid := r.Int("group_id")
		lid := loanSet.in(r.Int("loan_id"), gid)
		mid := r.Int("member_id")
		if lid == nil || ref.members[mid] != gid {
			p.Report.issue("loan_guarantors: loan or member missing, skipped")
			continue
		}
		if !r.Flag("active") {
			p.Report.issue("loan_guarantors: inactive, skipped")
			continue
		}
		p.LoanGuarantors = append(p.LoanGuarantors, []any{r.Int("id"), gid, *lid, mid, amountPtr(r, "guaranteed_amount"),
			clean(r.Str("comment")), p.createdBy(r, ref.users), timeOr(epoch(r.Str("created_on")), now)})
	}
}
