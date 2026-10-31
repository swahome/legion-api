package books

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/swahome/legion-api/internal/loans"
	"github.com/swahome/legion-api/internal/store"
)

// Loans: loan types, recording a loan (its disbursement, schedule, processing
// fee and guarantors), repayments and voiding. See docs/schema/loans.md.

// NumText renders a numeric column as a decimal string ("" for NULL).
func NumText(n pgtype.Numeric) string {
	if !n.Valid {
		return ""
	}
	v, err := n.Value()
	if err != nil || v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

func numFloat(n pgtype.Numeric) float64 {
	f, _ := strconv.ParseFloat(NumText(n), 64)
	return f
}

func numericOrNull(s *string) pgtype.Numeric {
	if s == nil {
		return pgtype.Numeric{}
	}
	return numeric(*s)
}

func rate(f float64) pgtype.Numeric { return numeric(strconv.FormatFloat(f, 'f', 4, 64)) }

// ---- Loan types ----

// LoanType is a loan product as an official sets it up.
type LoanType struct {
	Name            string
	InterestType    string
	InterestRate    float64
	RatePer         string
	ReducingMethod  string
	FixedInterest   string // fixed_amount: per installment...
	FixedRate       float64
	Frequency       string
	FixedTerm       *int32
	MinTerm         *int32
	MaxTerm         *int32
	GracePeriods    int32
	AmountBasis     string
	MinAmount       *string
	MaxAmount       *string
	SavingsMultiple *string
	Guarantors      string
	MinGuarantors   *int32
	MaxGuarantors   *int32
	LateFine        *loans.FineRule
	BalanceFine     *loans.FineRule
	ProcessingFee   *loans.ProcessingFee
	FineDeferment   bool
	NeedsApproval   bool
	IsHidden        bool
	ForDebtors      bool // offered to debtors (people outside the group), not members; set when created
}

// LoanTypeFrom reads a stored loan type back into the editable form.
func LoanTypeFrom(t store.LoanType) (LoanType, error) {
	lt := LoanType{
		Name: t.Name, InterestType: string(t.InterestType), InterestRate: numFloat(t.InterestRate),
		RatePer: string(t.RatePer), ReducingMethod: t.ReducingMethod, FixedInterest: Format(NumText(t.FixedInterest)),
		FixedRate: numFloat(t.FixedInterestRate), Frequency: string(t.Frequency), FixedTerm: t.FixedTerm,
		MinTerm: t.MinTerm, MaxTerm: t.MaxTerm, GracePeriods: t.GracePeriods, AmountBasis: t.AmountBasis,
		Guarantors: t.Guarantors, MinGuarantors: t.MinGuarantors, MaxGuarantors: t.MaxGuarantors,
		FineDeferment: t.FineDeferment, NeedsApproval: t.NeedsApproval, IsHidden: t.IsHidden, ForDebtors: t.ForDebtors,
	}
	opt := func(n pgtype.Numeric) *string {
		if !n.Valid {
			return nil
		}
		s := Format(NumText(n))
		return &s
	}
	lt.MinAmount, lt.MaxAmount, lt.SavingsMultiple = opt(t.MinAmount), opt(t.MaxAmount), opt(t.SavingsMultiple)
	var err error
	if lt.LateFine, err = loans.DecodeFine(t.LateFine); err != nil {
		return lt, err
	}
	if lt.BalanceFine, err = loans.DecodeFine(t.BalanceFine); err != nil {
		return lt, err
	}
	lt.ProcessingFee, err = loans.DecodeFee(t.ProcessingFee)
	return lt, err
}

func checkAmountPtr(s *string, field string) error {
	if s == nil {
		return nil
	}
	*s = strings.TrimSpace(*s)
	if !ValidAmount(*s) {
		return Error{"invalid_" + field, field + " must be an amount."}
	}
	return nil
}

// Check validates a loan type and fills defaults.
func (t *LoanType) Check() error {
	t.Name = strings.TrimSpace(t.Name)
	if t.Name == "" || len(t.Name) > 100 {
		return Error{"invalid_name", "Give the loan type a name of up to 100 characters."}
	}
	if t.RatePer == "" {
		t.RatePer = "month"
	}
	if t.Frequency == "" {
		t.Frequency = "monthly"
	}
	if t.ReducingMethod == "" {
		t.ReducingMethod = string(loans.EqualInstallments)
	}
	if t.GracePeriods == 0 {
		t.GracePeriods = 1
	}
	if t.FixedInterest == "" {
		t.FixedInterest = "0"
	}
	if !ValidAmount(t.FixedInterest) {
		return Error{"invalid_fixed_interest", "fixed_interest must be an amount."}
	}
	if t.ReducingMethod != string(loans.EqualInstallments) && t.ReducingMethod != string(loans.EqualPrincipal) {
		return Error{"invalid_reducing_method", "reducing_method must be equal_installments or equal_principal."}
	}
	// The interest settings are checked by building a sample schedule.
	terms := loans.Terms{Amount: 100000, InterestType: loans.InterestType(t.InterestType), Rate: t.InterestRate,
		RatePer: loans.RatePer(t.RatePer), Frequency: loans.Frequency(t.Frequency), Installments: 1,
		GracePeriods: int(t.GracePeriods), Reducing: loans.ReducingMethod(t.ReducingMethod),
		FixedInterest: Cents(t.FixedInterest), FixedPercent: t.FixedRate, DisbursementOn: time.Now()}
	if t.InterestType == string(loans.FixedAmount) && terms.FixedInterest == 0 && t.FixedRate == 0 {
		return Error{"invalid_terms", "A fixed interest loan needs fixed_interest or fixed_interest_rate."}
	}
	if err := terms.Check(); err != nil {
		return Error{"invalid_terms", sentence(err)}
	}
	switch {
	case t.FixedTerm != nil && (t.MinTerm != nil || t.MaxTerm != nil):
		return Error{"invalid_term", "Give either fixed_term or a min_term and max_term range, not both."}
	case t.FixedTerm != nil && (*t.FixedTerm < 1 || *t.FixedTerm > loans.MaxInstallments):
		return Error{"invalid_term", fmt.Sprintf("fixed_term must be between 1 and %d installments.", loans.MaxInstallments)}
	case t.MinTerm != nil && *t.MinTerm < 1, t.MaxTerm != nil && *t.MaxTerm > loans.MaxInstallments,
		t.MinTerm != nil && t.MaxTerm != nil && *t.MinTerm > *t.MaxTerm:
		return Error{"invalid_term", "The repayment period range is not valid."}
	}
	for _, a := range []struct {
		v     *string
		field string
	}{{t.MinAmount, "min_amount"}, {t.MaxAmount, "max_amount"}, {t.SavingsMultiple, "savings_multiple"}} {
		if err := checkAmountPtr(a.v, a.field); err != nil {
			return err
		}
	}
	switch t.AmountBasis {
	case "", "range":
		t.AmountBasis, t.SavingsMultiple = "range", nil
		if t.MinAmount != nil && t.MaxAmount != nil && Cents(*t.MinAmount) > Cents(*t.MaxAmount) {
			return Error{"invalid_amount_range", "min_amount is more than max_amount."}
		}
	case "savings":
		if t.ForDebtors {
			return Error{"invalid_amount_basis", "A debtor has no savings in the group: give an amount range instead."}
		}
		if t.SavingsMultiple == nil || Cents(*t.SavingsMultiple) <= 0 {
			return Error{"invalid_savings_multiple", "A loan based on savings needs savings_multiple, such as 3 for three times the member's savings."}
		}
	default:
		return Error{"invalid_amount_basis", "amount_basis must be range or savings."}
	}
	switch t.Guarantors {
	case "", "none":
		t.Guarantors, t.MinGuarantors, t.MaxGuarantors = "none", nil, nil
	case "always", "over_max":
		if t.Guarantors == "over_max" && t.MaxAmount == nil {
			return Error{"invalid_guarantors", "Guarantors over the maximum need a max_amount."}
		}
		if t.MinGuarantors == nil || *t.MinGuarantors < 1 {
			return Error{"invalid_guarantors", "Say how many guarantors a loan needs (min_guarantors)."}
		}
		if t.MaxGuarantors != nil && *t.MaxGuarantors < *t.MinGuarantors {
			return Error{"invalid_guarantors", "max_guarantors is less than min_guarantors."}
		}
	default:
		return Error{"invalid_guarantors", "guarantors must be none, always or over_max."}
	}
	if t.LateFine != nil {
		if err := t.LateFine.Check(false); err != nil {
			return Error{"invalid_late_fine", sentence(err)}
		}
	}
	if t.BalanceFine != nil {
		if err := t.BalanceFine.Check(true); err != nil {
			return Error{"invalid_balance_fine", sentence(err)}
		}
	}
	if t.ProcessingFee != nil {
		if err := t.ProcessingFee.Check(); err != nil {
			return Error{"invalid_processing_fee", sentence(err)}
		}
	}
	return nil
}

func sentence(err error) string {
	s := err.Error()
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:] + "."
}

// SaveLoanType creates (id 0) or updates a loan type and returns its id.
func SaveLoanType(ctx context.Context, q *store.Queries, groupID, userID, id int64, t LoanType) (int64, error) {
	if err := t.Check(); err != nil {
		return 0, err
	}
	taken, err := q.LoanTypeNameTaken(ctx, store.LoanTypeNameTakenParams{GroupID: groupID, Name: t.Name, ExceptID: id})
	if err != nil {
		return 0, err
	}
	if taken {
		return 0, Error{"name_taken", "The group already has a loan type called " + t.Name + "."}
	}
	if id == 0 {
		return q.CreateLoanType(ctx, store.CreateLoanTypeParams{
			GroupID: groupID, Name: t.Name, InterestType: store.InterestType(t.InterestType),
			RatePer: store.InterestRatePer(t.RatePer), ReducingMethod: t.ReducingMethod,
			Frequency: store.RepaymentFrequency(t.Frequency), FixedTerm: t.FixedTerm, MinTerm: t.MinTerm,
			MaxTerm: t.MaxTerm, GracePeriods: t.GracePeriods, AmountBasis: t.AmountBasis, Guarantors: t.Guarantors,
			MinGuarantors: t.MinGuarantors, MaxGuarantors: t.MaxGuarantors, LateFine: loans.Encode(t.LateFine),
			BalanceFine: loans.Encode(t.BalanceFine), ProcessingFee: loans.Encode(t.ProcessingFee),
			FineDeferment: t.FineDeferment, NeedsApproval: t.NeedsApproval, IsHidden: t.IsHidden, CreatedBy: &userID,
			ForDebtors:   t.ForDebtors,
			InterestRate: rate(t.InterestRate), FixedInterest: numeric(t.FixedInterest), FixedInterestRate: rate(t.FixedRate),
			MinAmount: numericOrNull(t.MinAmount), MaxAmount: numericOrNull(t.MaxAmount),
			SavingsMultiple: numericOrNull(t.SavingsMultiple),
		})
	}
	return id, q.UpdateLoanType(ctx, store.UpdateLoanTypeParams{
		GroupID: groupID, ID: id, Name: t.Name, InterestType: store.InterestType(t.InterestType),
		RatePer: store.InterestRatePer(t.RatePer), ReducingMethod: t.ReducingMethod,
		Frequency: store.RepaymentFrequency(t.Frequency), FixedTerm: t.FixedTerm, MinTerm: t.MinTerm,
		MaxTerm: t.MaxTerm, GracePeriods: t.GracePeriods, AmountBasis: t.AmountBasis, Guarantors: t.Guarantors,
		MinGuarantors: t.MinGuarantors, MaxGuarantors: t.MaxGuarantors, LateFine: loans.Encode(t.LateFine),
		BalanceFine: loans.Encode(t.BalanceFine), ProcessingFee: loans.Encode(t.ProcessingFee),
		FineDeferment: t.FineDeferment, NeedsApproval: t.NeedsApproval, IsHidden: t.IsHidden,
		InterestRate: rate(t.InterestRate), FixedInterest: numeric(t.FixedInterest), FixedInterestRate: rate(t.FixedRate),
		MinAmount: numericOrNull(t.MinAmount), MaxAmount: numericOrNull(t.MaxAmount),
		SavingsMultiple: numericOrNull(t.SavingsMultiple),
	})
}

// ---- Recording a loan ----

// Guarantor stands behind part of a loan.
type Guarantor struct {
	MemberID int64
	Amount   *string
	Comment  string
}

// NewLoan is a loan an official records as paid out. With a loan type the
// terms come from the type; Installments must then fit the type's term and
// the amount its limits. Without one every term is given here.
type NewLoan struct {
	MemberID     int64 // the borrower: a member...
	DebtorID     int64 // ...or a debtor (someone outside the group), never both
	ForDebtors   bool  // previewing a debtor loan before the debtor is picked
	LoanTypeID   *int64
	AccountID    int64
	Method       *string
	Amount       string
	Date         *time.Time // disbursement date
	Installments int
	FirstDueOn   *time.Time
	Description  string
	Guarantors   []Guarantor
	// Without a loan type:
	InterestType   string
	InterestRate   float64
	RatePer        string
	ReducingMethod string
	FixedInterest  string
	FixedRate      float64
	Frequency      string
	GracePeriods   int
	LateFine       *loans.FineRule
	BalanceFine    *loans.FineRule
	ProcessingFee  *loans.ProcessingFee
	FineDeferment  bool
	// SkipProcessingFee records the loan without its processing fee (legacy
	// disable_automatic_loan_processing_income).
	SkipProcessingFee bool
}

// Terms resolves a loan's terms against its loan type. It returns the
// schedule terms and the rules the loan keeps.
func (n *NewLoan) resolve(ctx context.Context, q *store.Queries, groupID int64) (loans.Terms, error) {
	if n.LoanTypeID != nil {
		lt, err := q.GetLoanType(ctx, store.GetLoanTypeParams{GroupID: groupID, ID: *n.LoanTypeID})
		if errors.Is(err, pgx.ErrNoRows) {
			return loans.Terms{}, Error{"invalid_loan_type", "That loan type is not in this group."}
		}
		if err != nil {
			return loans.Terms{}, err
		}
		t, err := LoanTypeFrom(lt)
		if err != nil {
			return loans.Terms{}, err
		}
		if t.ForDebtors != (n.DebtorID != 0 || n.ForDebtors) {
			if t.ForDebtors {
				return loans.Terms{}, Error{"invalid_loan_type", t.Name + " is a loan type for debtors, not members."}
			}
			return loans.Terms{}, Error{"invalid_loan_type", t.Name + " is a loan type for members, not debtors."}
		}
		if err := n.fitType(ctx, q, groupID, t); err != nil {
			return loans.Terms{}, err
		}
		n.InterestType, n.InterestRate, n.RatePer, n.ReducingMethod = t.InterestType, t.InterestRate, t.RatePer, t.ReducingMethod
		n.FixedInterest, n.FixedRate, n.Frequency, n.GracePeriods = t.FixedInterest, t.FixedRate, t.Frequency, int(t.GracePeriods)
		n.LateFine, n.BalanceFine, n.ProcessingFee, n.FineDeferment = t.LateFine, t.BalanceFine, t.ProcessingFee, t.FineDeferment
	} else {
		if n.RatePer == "" {
			n.RatePer = "month"
		}
		if n.Frequency == "" {
			n.Frequency = "monthly"
		}
		if n.ReducingMethod == "" {
			n.ReducingMethod = string(loans.EqualInstallments)
		}
		if n.GracePeriods == 0 {
			n.GracePeriods = 1
		}
		if n.FixedInterest == "" {
			n.FixedInterest = "0"
		}
		if !ValidAmount(n.FixedInterest) {
			return loans.Terms{}, Error{"invalid_fixed_interest", "fixed_interest must be an amount."}
		}
		for _, f := range []struct {
			r     *loans.FineRule
			bal   bool
			field string
		}{{n.LateFine, false, "late_fine"}, {n.BalanceFine, true, "balance_fine"}} {
			if f.r != nil {
				if err := f.r.Check(f.bal); err != nil {
					return loans.Terms{}, Error{"invalid_" + f.field, sentence(err)}
				}
			}
		}
		if n.ProcessingFee != nil {
			if err := n.ProcessingFee.Check(); err != nil {
				return loans.Terms{}, Error{"invalid_processing_fee", sentence(err)}
			}
		}
	}
	terms := loans.Terms{
		Amount: Cents(n.Amount), InterestType: loans.InterestType(n.InterestType), Rate: n.InterestRate,
		RatePer: loans.RatePer(n.RatePer), Frequency: loans.Frequency(n.Frequency), Installments: n.Installments,
		GracePeriods: n.GracePeriods, FirstDueDate: n.FirstDueOn, Reducing: loans.ReducingMethod(n.ReducingMethod),
		FixedInterest: Cents(n.FixedInterest), FixedPercent: n.FixedRate, DisbursementOn: *n.Date,
	}
	if terms.InterestType == loans.FixedAmount && terms.FixedInterest == 0 && terms.FixedPercent == 0 {
		return terms, Error{"invalid_terms", "A fixed interest loan needs fixed_interest or fixed_interest_rate."}
	}
	if err := terms.Check(); err != nil {
		return terms, Error{"invalid_terms", sentence(err)}
	}
	return terms, nil
}

// fitType checks the amount, period and guarantors against the loan type.
func (n *NewLoan) fitType(ctx context.Context, q *store.Queries, groupID int64, t LoanType) error {
	amount := Cents(n.Amount)
	switch {
	case t.FixedTerm != nil && n.Installments == 0:
		n.Installments = int(*t.FixedTerm)
	case t.FixedTerm != nil && n.Installments != int(*t.FixedTerm):
		return Error{"invalid_installments", fmt.Sprintf("%s loans are repaid in %d installments.", t.Name, *t.FixedTerm)}
	case t.MinTerm != nil && n.Installments < int(*t.MinTerm), t.MaxTerm != nil && n.Installments > int(*t.MaxTerm):
		lo, hi := "1", fmt.Sprint(loans.MaxInstallments)
		if t.MinTerm != nil {
			lo = fmt.Sprint(*t.MinTerm)
		}
		if t.MaxTerm != nil {
			hi = fmt.Sprint(*t.MaxTerm)
		}
		return Error{"invalid_installments", fmt.Sprintf("%s loans are repaid in %s to %s installments.", t.Name, lo, hi)}
	}
	if t.AmountBasis == "savings" {
		saved, err := q.MemberSavings(ctx, store.MemberSavingsParams{GroupID: groupID, MembershipID: &n.MemberID})
		if err != nil {
			return err
		}
		limit := Cents(saved) * Cents(*t.SavingsMultiple) / 100
		if amount > limit {
			return Error{"amount_over_limit", fmt.Sprintf("%s loans are up to %s times the member's savings: at most %s.",
				t.Name, Format(*t.SavingsMultiple), Format(fmt.Sprintf("%d.%02d", limit/100, limit%100)))}
		}
	} else {
		if t.MinAmount != nil && amount < Cents(*t.MinAmount) {
			return Error{"amount_under_limit", fmt.Sprintf("%s loans are at least %s.", t.Name, Format(*t.MinAmount))}
		}
		if t.MaxAmount != nil && Cents(*t.MaxAmount) > 0 && amount > Cents(*t.MaxAmount) && t.Guarantors != "over_max" {
			return Error{"amount_over_limit", fmt.Sprintf("%s loans are at most %s.", t.Name, Format(*t.MaxAmount))}
		}
	}
	need := t.Guarantors == "always" || (t.Guarantors == "over_max" && t.MaxAmount != nil && amount > Cents(*t.MaxAmount))
	if need && t.MinGuarantors != nil && len(n.Guarantors) < int(*t.MinGuarantors) {
		return Error{"guarantors_required", fmt.Sprintf("This loan needs at least %d guarantors.", *t.MinGuarantors)}
	}
	if t.MaxGuarantors != nil && len(n.Guarantors) > int(*t.MaxGuarantors) {
		return Error{"too_many_guarantors", fmt.Sprintf("%s loans take at most %d guarantors.", t.Name, *t.MaxGuarantors)}
	}
	return nil
}

// checkGuarantors checks that each guarantor is another active member of
// the group, listed once, with a sensible amount.
func checkGuarantors(ctx context.Context, q *store.Queries, groupID, memberID int64, gs []Guarantor) error {
	seen := map[int64]bool{}
	for i, g := range gs {
		if g.MemberID == memberID {
			return Error{"invalid_guarantor", "A member cannot guarantee their own loan."}
		}
		if seen[g.MemberID] {
			return Error{"invalid_guarantor", "Each guarantor is listed once."}
		}
		seen[g.MemberID] = true
		if err := checkAmountPtr(gs[i].Amount, "guaranteed_amount"); err != nil {
			return err
		}
		if err := Found("invalid_guarantor", "A guarantor is not a member of this group.")(
			q.ActiveMembershipInGroup(ctx, store.ActiveMembershipInGroupParams{GroupID: groupID, ID: g.MemberID})); err != nil {
			return err
		}
	}
	return nil
}

// RecordLoan records a loan that has been paid out: the loan, the
// disbursement (a loan_disbursement withdrawal out of the account), its
// installments, the processing fee (loan processing income into the same
// account) and the guarantors.
func RecordLoan(ctx context.Context, q *store.Queries, groupID, userID int64, n NewLoan) (int64, error) {
	n.Description = strings.TrimSpace(n.Description)
	if err := CheckAmountDate(n.Amount, n.Date, n.Method, "loan"); err != nil {
		return 0, err
	}
	if err := n.checkBorrower(ctx, q, groupID); err != nil {
		return 0, err
	}
	if err := checkGuarantors(ctx, q, groupID, n.MemberID, n.Guarantors); err != nil {
		return 0, err
	}
	acct, err := OpenAccount(ctx, q, groupID, n.AccountID)
	if err != nil {
		return 0, err
	}
	terms, err := n.resolve(ctx, q, groupID)
	if err != nil {
		return 0, err
	}
	schedule, err := terms.Schedule()
	if err != nil {
		return 0, Error{"invalid_terms", sentence(err)}
	}
	var nextBalanceFine *time.Time
	ends := schedule[len(schedule)-1].DueDate
	if n.BalanceFine != nil {
		d := ends.AddDate(0, 0, 1)
		nextBalanceFine = &d
	}
	member, debtor := n.borrower()
	loanID, err := q.CreateLoan(ctx, store.CreateLoanParams{
		GroupID: groupID, MembershipID: member, DebtorID: debtor, LoanTypeID: n.LoanTypeID, DisbursedOn: *n.Date, AccountID: &acct.ID,
		InterestType: store.InterestType(n.InterestType), RatePer: store.InterestRatePer(n.RatePer),
		ReducingMethod: n.ReducingMethod, Frequency: store.RepaymentFrequency(n.Frequency),
		Installments: int32(n.Installments), GracePeriods: int32(n.GracePeriods), FirstDueOn: n.FirstDueOn, EndsOn: ends,
		LateFine: loans.Encode(n.LateFine), BalanceFine: loans.Encode(n.BalanceFine),
		ProcessingFee: loans.Encode(n.ProcessingFee), FineDeferment: n.FineDeferment, NextBalanceFineOn: nextBalanceFine,
		Description: n.Description, CreatedBy: &userID, Amount: numeric(n.Amount), InterestRate: rate(n.InterestRate),
		FixedInterest: numeric(n.FixedInterest), FixedInterestRate: rate(n.FixedRate),
	})
	if err != nil {
		return 0, err
	}
	desc := n.Description
	if desc == "" {
		desc = "Loan disbursement"
	}
	out, feeKind := store.WithdrawalKindLoanDisbursement, store.DepositKindLoanProcessingIncome
	if debtor != nil {
		out, feeKind = store.WithdrawalKindExternalLoanDisbursement, store.DepositKindExternalLendingProcessingIncome
	}
	wid, err := WriteWithdrawal(ctx, q, acct, Withdrawal{Kind: out, Amount: n.Amount, MemberID: member},
		n.Method, *n.Date, desc, userID)
	if err != nil {
		return 0, err
	}
	if err := q.SetWithdrawalLoan(ctx, store.SetWithdrawalLoanParams{ID: wid, LoanID: &loanID}); err != nil {
		return 0, err
	}
	for _, inst := range schedule {
		num := int32(inst.Number)
		next := inst.FineDate
		if _, err := q.CreateLoanCharge(ctx, store.CreateLoanChargeParams{
			GroupID: groupID, LoanID: loanID, Kind: store.LoanChargeKindInstallment, Number: &num,
			InvoiceOn: inst.InvoiceDate, DueOn: inst.DueDate, NextFineOn: &next, CreatedBy: &userID,
			Principal: numeric(loans.Money(inst.Principal)), Interest: numeric(loans.Money(inst.Interest)),
			Amount: numeric(loans.Money(inst.Amount())),
		}); err != nil {
			return 0, err
		}
	}
	if n.ProcessingFee != nil && !n.SkipProcessingFee {
		if fee := n.ProcessingFee.Charge(terms.Amount, loans.Interest(schedule)); fee > 0 {
			did, err := writeDeposit(ctx, q, acct, feeKind, n.Method, loans.Money(fee),
				*n.Date, "Loan processing fee", member, userID)
			if err != nil {
				return 0, err
			}
			if err := q.SetDepositLoan(ctx, store.SetDepositLoanParams{ID: did, LoanID: &loanID}); err != nil {
				return 0, err
			}
		}
	}
	for _, g := range n.Guarantors {
		if err := q.CreateLoanGuarantor(ctx, store.CreateLoanGuarantorParams{
			GroupID: groupID, LoanID: loanID, MembershipID: g.MemberID, Comment: strings.TrimSpace(g.Comment),
			CreatedBy: &userID, Amount: numericOrNull(g.Amount),
		}); err != nil {
			return 0, err
		}
	}
	return loanID, nil
}

// Is reports whether an optional id is id.
func Is(p *int64, id int64) bool { return p != nil && *p == id }

// borrower is the loan's membership_id and debtor_id; one is nil.
func (n *NewLoan) borrower() (member, debtor *int64) {
	if n.DebtorID != 0 {
		return nil, &n.DebtorID
	}
	return &n.MemberID, nil
}

// checkBorrower checks the loan goes to one active member or one active
// debtor of the group.
func (n *NewLoan) checkBorrower(ctx context.Context, q *store.Queries, groupID int64) error {
	switch {
	case (n.MemberID == 0) == (n.DebtorID == 0):
		return Error{"missing_fields", "A loan goes to a member (member_id) or a debtor (debtor_id)."}
	case n.DebtorID != 0:
		return Found("invalid_debtor", "That debtor is not active in this group.")(
			q.ActiveDebtorInGroup(ctx, store.ActiveDebtorInGroupParams{GroupID: groupID, ID: n.DebtorID}))
	}
	return Found("invalid_member", "That member is not in this group.")(
		q.ActiveMembershipInGroup(ctx, store.ActiveMembershipInGroupParams{GroupID: groupID, ID: n.MemberID}))
}

// writeDeposit writes a deposit and its cash book entry with no ledger row.
func writeDeposit(ctx context.Context, q *store.Queries, acct store.GroupAccount, kind store.DepositKind, method *string,
	amount string, date time.Time, description string, memberID *int64, userID int64) (int64, error) {
	p := store.CreateDepositParams{
		GroupID: acct.GroupID, AccountID: &acct.ID, AccountKind: acct.Kind, Kind: kind, MembershipID: memberID,
		Amount: numeric(amount), DepositDate: date, Description: description, CreatedBy: &userID,
	}
	if method != nil {
		p.Method = store.NullPaymentMethod{PaymentMethod: store.PaymentMethod(*method), Valid: true}
	}
	id, err := q.CreateDeposit(ctx, p)
	if err != nil {
		return 0, err
	}
	return id, q.CreateAccountEntry(ctx, store.CreateAccountEntryParams{
		GroupID: acct.GroupID, AccountID: &acct.ID, AccountKind: acct.Kind, Direction: 1, Amount: p.Amount,
		EntryDate: date, Description: description, DepositID: &id, MembershipID: memberID,
	})
}

// LockLoan loads a live loan for update, or ErrNotFound.
func LockLoan(ctx context.Context, q *store.Queries, groupID, id int64) (store.Loan, error) {
	l, err := q.GetLoanForUpdate(ctx, store.GetLoanForUpdateParams{GroupID: groupID, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return l, ErrNotFound
	}
	if err != nil {
		return l, err
	}
	if l.VoidedAt != nil {
		return l, Error{"loan_voided", "This loan has been voided."}
	}
	return l, nil
}

// Repayment is money a member paid towards a loan.
type Repayment struct {
	AccountID   int64
	Method      *string
	Amount      string
	Date        *time.Time
	Description string
}

// RecordRepayment writes a repayment: a loan_repayment deposit into the
// account (with its cash book entry) and the loan_repayments row. It returns
// the repayment id.
func RecordRepayment(ctx context.Context, q *store.Queries, groupID, userID, loanID int64, r Repayment) (int64, error) {
	if err := CheckAmountDate(r.Amount, r.Date, r.Method, "repayment"); err != nil {
		return 0, err
	}
	l, err := LockLoan(ctx, q, groupID, loanID)
	if err != nil {
		return 0, err
	}
	if r.Date.Before(l.DisbursedOn) {
		return 0, Error{"invalid_date", "A repayment cannot be dated before the loan was paid out."}
	}
	acct, err := OpenAccount(ctx, q, groupID, r.AccountID)
	if err != nil {
		return 0, err
	}
	desc := strings.TrimSpace(r.Description)
	kind := store.DepositKindLoanRepayment
	if l.DebtorID != nil {
		kind = store.DepositKindExternalLendingRepayment
	}
	did, err := writeDeposit(ctx, q, acct, kind, r.Method, r.Amount, *r.Date, desc, l.MembershipID, userID)
	if err != nil {
		return 0, err
	}
	if err := q.SetDepositLoan(ctx, store.SetDepositLoanParams{ID: did, LoanID: &loanID}); err != nil {
		return 0, err
	}
	p := store.CreateLoanRepaymentParams{
		GroupID: groupID, LoanID: loanID, MembershipID: l.MembershipID, DepositID: &did, AccountID: &acct.ID,
		PaidOn: *r.Date, Source: "deposit", Description: desc, CreatedBy: &userID, Amount: numeric(r.Amount),
	}
	if r.Method != nil {
		p.Method = store.NullPaymentMethod{PaymentMethod: store.PaymentMethod(*r.Method), Valid: true}
	}
	return q.CreateLoanRepayment(ctx, p)
}

// VoidRepayment voids a repayment and the deposit that recorded it.
func VoidRepayment(ctx context.Context, q *store.Queries, groupID, loanID, id, userID int64) error {
	rp, err := q.GetLoanRepayment(ctx, store.GetLoanRepaymentParams{GroupID: groupID, ID: id})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && rp.LoanID != loanID) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if rp.DepositID != nil {
		return VoidDeposit(ctx, q, groupID, *rp.DepositID, userID)
	}
	n, err := q.VoidLoanRepayment(ctx, store.VoidLoanRepaymentParams{GroupID: groupID, ID: id, VoidedBy: &userID})
	if err != nil {
		return err
	}
	if n == 0 {
		return Error{"already_voided", "This repayment is already voided."}
	}
	return nil
}

// VoidLoan voids a loan with everything that belongs to it: the
// disbursement, processing fee, installments, fines and repayments (legacy
// void_loan did the same).
func VoidLoan(ctx context.Context, q *store.Queries, groupID, id, userID int64) error {
	if _, err := LockLoan(ctx, q, groupID, id); err != nil {
		var be Error
		if errors.As(err, &be) {
			return Error{"already_voided", "This loan is already voided."}
		}
		return err
	}
	rows, err := q.LoanMoneyRows(ctx, &id)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if r.Kind == "deposit" {
			err = VoidDeposit(ctx, q, groupID, r.ID, userID)
		} else {
			err = voidWithdrawal(ctx, q, groupID, r.ID, userID)
		}
		if err != nil {
			return err
		}
	}
	if err := q.VoidLoanRepayments(ctx, store.VoidLoanRepaymentsParams{LoanID: id, VoidedBy: &userID}); err != nil {
		return err
	}
	if err := q.VoidLoanCharges(ctx, id); err != nil {
		return err
	}
	_, err = q.VoidLoan(ctx, store.VoidLoanParams{GroupID: groupID, ID: id, VoidedBy: &userID})
	return err
}

// VoidLoanFine voids a late or outstanding-balance fine on a loan.
func VoidLoanFine(ctx context.Context, q *store.Queries, groupID, loanID, chargeID int64) error {
	if _, err := LockLoan(ctx, q, groupID, loanID); err != nil {
		return err
	}
	n, err := q.VoidLoanCharge(ctx, store.VoidLoanChargeParams{GroupID: groupID, LoanID: loanID, ID: chargeID})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// PreviewLoan resolves a loan's terms against its type without recording
// anything, and returns the processing fee it would carry.
func PreviewLoan(ctx context.Context, q *store.Queries, groupID int64, n NewLoan) (loans.Terms, int64, error) {
	if n.Amount == "" || !ValidAmount(n.Amount) || Cents(n.Amount) <= 0 {
		return loans.Terms{}, 0, Error{"invalid_amount", "amount must be more than zero."}
	}
	terms, err := n.resolve(ctx, q, groupID)
	if err != nil {
		return terms, 0, err
	}
	var fee int64
	if n.ProcessingFee != nil && !n.SkipProcessingFee {
		sched, err := terms.Schedule()
		if err != nil {
			return terms, 0, Error{"invalid_terms", sentence(err)}
		}
		fee = n.ProcessingFee.Charge(terms.Amount, loans.Interest(sched))
	}
	return terms, fee, nil
}
