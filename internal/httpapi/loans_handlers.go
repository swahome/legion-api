package httpapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/swahome/legion-api/internal/billing"
	"github.com/swahome/legion-api/internal/books"
	"github.com/swahome/legion-api/internal/loans"
	"github.com/swahome/legion-api/internal/store"
)

// Loans (milestone 4): loan types, loans an official records as paid out,
// their schedules, repayments and fines. Members see their own loans and
// the loan types on offer. See docs/schema/loans.md and docs/api/groups.md.

func centsText(c int64) string { return loans.Money(c) }

func (s *Server) today() time.Time { return billing.LocalToday(time.Now(), s.location) }

// ---- Loan types ----

type loanTypeBody struct {
	Name            string               `json:"name"`
	InterestType    string               `json:"interest_type"`
	InterestRate    float64              `json:"interest_rate"`
	RatePer         string               `json:"rate_per"`
	ReducingMethod  string               `json:"reducing_method"`
	FixedInterest   *Amount              `json:"fixed_interest"`
	FixedRate       float64              `json:"fixed_interest_rate"`
	Frequency       string               `json:"frequency"`
	FixedTerm       *int32               `json:"fixed_term"`
	MinTerm         *int32               `json:"min_term"`
	MaxTerm         *int32               `json:"max_term"`
	GracePeriods    int32                `json:"grace_periods"`
	AmountBasis     string               `json:"amount_basis"`
	MinAmount       *Amount              `json:"min_amount"`
	MaxAmount       *Amount              `json:"max_amount"`
	SavingsMultiple *Amount              `json:"savings_multiple"`
	Guarantors      string               `json:"guarantors"`
	MinGuarantors   *int32               `json:"min_guarantors"`
	MaxGuarantors   *int32               `json:"max_guarantors"`
	LateFine        *loans.FineRule      `json:"late_fine"`
	BalanceFine     *loans.FineRule      `json:"balance_fine"`
	ProcessingFee   *loans.ProcessingFee `json:"processing_fee"`
	FineDeferment   bool                 `json:"fine_deferment"`
	NeedsApproval   *bool                `json:"needs_approval"`
	IsHidden        bool                 `json:"is_hidden"`
}

func amountStr(a *Amount) *string {
	if a == nil {
		return nil
	}
	s := string(*a)
	return &s
}

func (b loanTypeBody) toBooks() books.LoanType {
	t := books.LoanType{
		Name: b.Name, InterestType: b.InterestType, InterestRate: b.InterestRate, RatePer: b.RatePer,
		ReducingMethod: b.ReducingMethod, FixedRate: b.FixedRate, Frequency: b.Frequency, FixedTerm: b.FixedTerm,
		MinTerm: b.MinTerm, MaxTerm: b.MaxTerm, GracePeriods: b.GracePeriods, AmountBasis: b.AmountBasis,
		MinAmount: amountStr(b.MinAmount), MaxAmount: amountStr(b.MaxAmount), SavingsMultiple: amountStr(b.SavingsMultiple),
		Guarantors: b.Guarantors, MinGuarantors: b.MinGuarantors, MaxGuarantors: b.MaxGuarantors,
		LateFine: b.LateFine, BalanceFine: b.BalanceFine, ProcessingFee: b.ProcessingFee,
		FineDeferment: b.FineDeferment, NeedsApproval: true, IsHidden: b.IsHidden,
	}
	if b.FixedInterest != nil {
		t.FixedInterest = string(*b.FixedInterest)
	}
	if b.NeedsApproval != nil {
		t.NeedsApproval = *b.NeedsApproval
	}
	return t
}

type loanTypeResponse struct {
	ID int64 `json:"id"`
	loanTypeBody
	FixedInterest string `json:"fixed_interest"`
	NeedsApproval bool   `json:"needs_approval"`
}

func toLoanType(t store.LoanType) (loanTypeResponse, error) {
	lt, err := books.LoanTypeFrom(t)
	if err != nil {
		return loanTypeResponse{}, err
	}
	opt := func(s *string) *Amount {
		if s == nil {
			return nil
		}
		a := Amount(*s)
		return &a
	}
	return loanTypeResponse{
		ID: t.ID,
		loanTypeBody: loanTypeBody{
			Name: lt.Name, InterestType: lt.InterestType, InterestRate: lt.InterestRate, RatePer: lt.RatePer,
			ReducingMethod: lt.ReducingMethod, FixedRate: lt.FixedRate, Frequency: lt.Frequency, FixedTerm: lt.FixedTerm,
			MinTerm: lt.MinTerm, MaxTerm: lt.MaxTerm, GracePeriods: lt.GracePeriods, AmountBasis: lt.AmountBasis,
			MinAmount: opt(lt.MinAmount), MaxAmount: opt(lt.MaxAmount), SavingsMultiple: opt(lt.SavingsMultiple),
			Guarantors: lt.Guarantors, MinGuarantors: lt.MinGuarantors, MaxGuarantors: lt.MaxGuarantors,
			LateFine: lt.LateFine, BalanceFine: lt.BalanceFine, ProcessingFee: lt.ProcessingFee,
			FineDeferment: lt.FineDeferment, IsHidden: lt.IsHidden,
		},
		FixedInterest: lt.FixedInterest, NeedsApproval: lt.NeedsApproval,
	}, nil
}

// listLoanTypes lists the group's loan types. Members see those on offer;
// officials can add ?include_hidden=true.
func (s *Server) listLoanTypes(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	rows, err := s.q.ListLoanTypes(r.Context(), store.ListLoanTypesParams{
		GroupID: a.Group.ID, IncludeHidden: a.IsOfficial && r.URL.Query().Get("include_hidden") == "true",
	})
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	out := make([]loanTypeResponse, 0, len(rows))
	for _, row := range rows {
		t, err := toLoanType(row)
		if err != nil {
			writeInternal(w, r, err)
			return
		}
		out = append(out, t)
	}
	writeJSON(w, http.StatusOK, map[string]any{"loan_types": out})
}

func (s *Server) saveLoanType(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	var id int64
	if r.Method == http.MethodPatch {
		var ok bool
		if id, ok = pathID(r, "loanTypeID"); !ok {
			writeError(w, http.StatusNotFound, "loan_type_not_found", "Loan type not found.")
			return
		}
		if _, err := s.q.GetLoanType(r.Context(), store.GetLoanTypeParams{GroupID: a.Group.ID, ID: id}); errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "loan_type_not_found", "Loan type not found.")
			return
		} else if err != nil {
			writeInternal(w, r, err)
			return
		}
	}
	var body loanTypeBody
	if !decodeJSON(w, r, &body) {
		return
	}
	userID := claimsFrom(r.Context()).UserID
	err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) (err error) {
		id, err = books.SaveLoanType(r.Context(), s.q.WithTx(tx), a.Group.ID, userID, id, body.toBooks())
		return err
	})
	if writeLoanError(w, r, err) {
		return
	}
	row, err := s.q.GetLoanType(r.Context(), store.GetLoanTypeParams{GroupID: a.Group.ID, ID: id})
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	t, err := toLoanType(row)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	status := http.StatusOK
	if r.Method == http.MethodPost {
		status = http.StatusCreated
	}
	writeJSON(w, status, t)
}

// deleteLoanType hides a loan type for good. Loans made with it keep their
// own copy of its terms.
func (s *Server) deleteLoanType(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	id, ok := pathID(r, "loanTypeID")
	if !ok {
		writeError(w, http.StatusNotFound, "loan_type_not_found", "Loan type not found.")
		return
	}
	n, err := s.q.DeleteLoanType(r.Context(), store.DeleteLoanTypeParams{GroupID: a.Group.ID, ID: id})
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	if n == 0 {
		writeError(w, http.StatusNotFound, "loan_type_not_found", "Loan type not found.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- Loans ----

type loanSummary struct {
	ID           int64      `json:"id"`
	Member       namedRef   `json:"member"`
	LoanType     *namedRef  `json:"loan_type"`
	Amount       string     `json:"amount"`
	DisbursedOn  Date       `json:"disbursed_on"`
	EndsOn       Date       `json:"ends_on"`
	InterestType string     `json:"interest_type"`
	InterestRate string     `json:"interest_rate"`
	RatePer      string     `json:"rate_per"`
	Frequency    string     `json:"frequency"`
	Installments int32      `json:"installments"`
	Payable      string     `json:"payable"`
	Interest     string     `json:"interest"`
	Fines        string     `json:"fines"`
	Paid         string     `json:"paid"`
	Balance      string     `json:"balance"`
	Overdue      string     `json:"overdue"`
	Status       string     `json:"status"`
	Description  string     `json:"description"`
	CreatedAt    time.Time  `json:"created_at"`
	VoidedAt     *time.Time `json:"voided_at"`
}

func summaryStatus(voided bool, badDebt bool, balance, overdue int64) string {
	switch {
	case voided:
		return loans.StatusVoided
	case balance <= 0:
		return loans.StatusPaid
	case badDebt:
		return loans.StatusBadDebt
	case overdue > 0:
		return loans.StatusArrears
	}
	return loans.StatusActive
}

// listLoans pages through loans, newest first. Members see only their own.
// Filters: member_id, loan_type_id, status (open, paid, voided; default
// every live loan).
func (s *Server) listLoans(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	pg := pageParams(r)
	p := store.ListLoansParams{GroupID: a.Group.ID, Today: s.today(), Status: r.URL.Query().Get("status"),
		PageSize: pg.size, PageOffset: pg.offset}
	switch p.Status {
	case "", "open", "paid", "voided":
	default:
		writeError(w, http.StatusBadRequest, "invalid_filter", "status must be open, paid or voided.")
		return
	}
	var ok1, ok2 bool
	p.MembershipID, ok1 = optionalID(r, "member_id")
	p.LoanTypeID, ok2 = optionalID(r, "loan_type_id")
	if !ok1 || !ok2 {
		writeError(w, http.StatusBadRequest, "invalid_filter", "Filters must be ids.")
		return
	}
	if !a.IsOfficial {
		if a.MembershipID == nil {
			writeError(w, http.StatusForbidden, "forbidden", "Only members see loans.")
			return
		}
		p.MembershipID = a.MembershipID
	}
	rows, err := s.q.ListLoans(r.Context(), p)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	items := make([]loanSummary, 0, len(rows))
	var total int64
	for _, l := range rows {
		bal, over := books.Cents(l.Balance), books.Cents(l.Overdue)
		item := loanSummary{
			ID: l.ID, Member: namedRef{ID: l.MembershipID, Name: l.MemberName}, Amount: money(l.Amount),
			DisbursedOn: Date{l.DisbursedOn}, EndsOn: Date{l.EndsOn}, InterestType: string(l.InterestType),
			InterestRate: l.InterestRate, RatePer: string(l.RatePer), Frequency: string(l.Frequency),
			Installments: l.Installments, Payable: money(l.Payable), Interest: money(l.Interest), Fines: money(l.Fines),
			Paid: money(l.Paid), Balance: money(l.Balance), Overdue: money(l.Overdue),
			Status: summaryStatus(l.VoidedAt != nil, l.IsBadDebt, bal, over), Description: l.Description,
			CreatedAt: l.CreatedAt, VoidedAt: l.VoidedAt,
		}
		if l.LoanTypeID != nil {
			item.LoanType = &namedRef{ID: *l.LoanTypeID, Name: l.LoanTypeName}
		}
		items = append(items, item)
		total = l.Total
	}
	totals, err := s.q.LoanTotals(r.Context(), a.Group.ID)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	resp := map[string]any{"loans": newPaged(items, total, pg)}
	if a.IsOfficial {
		resp["totals"] = map[string]any{
			"loans": totals.Loans, "lent": money(totals.Lent), "payable": money(totals.Payable),
			"repaid": money(totals.Repaid), "balance": centsText(books.Cents(totals.Payable) - books.Cents(totals.Repaid)),
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

type chargeResponse struct {
	ID         int64      `json:"id"`
	Kind       string     `json:"kind"`
	Number     *int32     `json:"number"`
	InvoiceOn  Date       `json:"invoice_on"`
	DueOn      Date       `json:"due_on"`
	NextFineOn *Date      `json:"next_fine_on"`
	Principal  string     `json:"principal"`
	Interest   string     `json:"interest"`
	Amount     string     `json:"amount"`
	Paid       string     `json:"paid"`
	Balance    string     `json:"balance"`
	Status     string     `json:"status"`
	ParentID   *int64     `json:"parent_id"`
	VoidedAt   *time.Time `json:"voided_at"`
}

type repaymentResponse struct {
	ID          int64      `json:"id"`
	DepositID   *int64     `json:"deposit_id"`
	Account     *namedRef  `json:"account"`
	Method      *string    `json:"method"`
	Amount      string     `json:"amount"`
	PaidOn      Date       `json:"paid_on"`
	Source      string     `json:"source"`
	Description string     `json:"description"`
	VoidedAt    *time.Time `json:"voided_at"`
}

type statementLine struct {
	Date    Date   `json:"date"`
	Kind    string `json:"kind"`
	Ref     int64  `json:"ref"`
	Debit   string `json:"debit"`
	Credit  string `json:"credit"`
	Balance string `json:"balance"`
	Note    string `json:"note"`
}

type guarantorResponse struct {
	ID      int64    `json:"id"`
	Member  namedRef `json:"member"`
	Amount  *string  `json:"amount"`
	Comment string   `json:"comment"`
}

func loanDetailJSON(d *books.LoanDetail) map[string]any {
	l := d.Loan
	charges := make([]chargeResponse, 0, len(d.Charges))
	for _, c := range d.Charges {
		amt := books.Cents(c.Amount)
		charges = append(charges, chargeResponse{
			ID: c.ID, Kind: string(c.Kind), Number: c.Number, InvoiceOn: Date{c.InvoiceOn}, DueOn: Date{c.DueOn},
			NextFineOn: datePtr(c.NextFineOn), Principal: money(c.Principal), Interest: money(c.Interest),
			Amount: money(c.Amount), Paid: centsText(c.Paid), Balance: centsText(amt - c.Paid), Status: c.Status,
			ParentID: c.ParentID, VoidedAt: c.VoidedAt,
		})
	}
	reps := make([]repaymentResponse, 0, len(d.Repayments))
	for _, r := range d.Repayments {
		rr := repaymentResponse{ID: r.ID, DepositID: r.DepositID, Method: nullEnum(r.Method.Valid, r.Method.PaymentMethod),
			Amount: money(r.Amount), PaidOn: Date{r.PaidOn}, Source: r.Source, Description: r.Description, VoidedAt: r.VoidedAt}
		if r.AccountID != nil {
			rr.Account = &namedRef{ID: *r.AccountID, Name: r.AccountName}
		}
		reps = append(reps, rr)
	}
	lines := make([]statementLine, 0, len(d.Statement))
	for _, s := range d.Statement {
		lines = append(lines, statementLine{Date: Date{s.Date}, Kind: s.Kind, Ref: s.Ref, Debit: centsText(s.Debit),
			Credit: centsText(s.Credit), Balance: centsText(s.Balance), Note: s.Note})
	}
	guars := make([]guarantorResponse, 0, len(d.Guarantors))
	for _, g := range d.Guarantors {
		gr := guarantorResponse{ID: g.ID, Member: namedRef{ID: g.MembershipID, Name: g.MemberName}, Comment: g.Comment}
		if !g.NoAmount {
			m := money(g.Amount)
			gr.Amount = &m
		}
		guars = append(guars, gr)
	}
	lateFine, _ := loans.DecodeFine(l.LateFine)
	balanceFine, _ := loans.DecodeFine(l.BalanceFine)
	fee, _ := loans.DecodeFee(l.ProcessingFee)
	out := map[string]any{
		"id": l.ID, "member": namedRef{ID: l.MembershipID, Name: l.MemberName}, "loan_type": nil,
		"amount": money(books.NumText(l.Amount)), "disbursed_on": Date{l.DisbursedOn}, "ends_on": Date{l.EndsOn},
		"account": nil, "interest_type": l.InterestType, "interest_rate": books.NumText(l.InterestRate),
		"rate_per": l.RatePer, "reducing_method": l.ReducingMethod, "fixed_interest": money(books.NumText(l.FixedInterest)),
		"fixed_interest_rate": books.NumText(l.FixedInterestRate), "frequency": l.Frequency,
		"installments": l.Installments, "grace_periods": l.GracePeriods, "first_due_on": datePtr(l.FirstDueOn),
		"late_fine": lateFine, "balance_fine": balanceFine, "processing_fee": fee, "fine_deferment": l.FineDeferment,
		"next_balance_fine_on": datePtr(l.NextBalanceFineOn), "is_bad_debt": l.IsBadDebt, "description": l.Description,
		"created_at": l.CreatedAt, "voided_at": l.VoidedAt, "status": d.Status,
		"totals": map[string]string{
			"payable": centsText(d.Payable), "principal": centsText(d.Principal), "interest": centsText(d.Interest),
			"fines": centsText(d.Fines), "paid": centsText(d.Paid), "balance": centsText(d.Balance), "overdue": centsText(d.Overdue),
			"overpaid": centsText(d.Overpaid),
		},
		"next_due": nil, "charges": charges, "repayments": reps, "guarantors": guars, "statement": lines,
	}
	if l.LoanTypeID != nil {
		out["loan_type"] = namedRef{ID: *l.LoanTypeID, Name: l.LoanTypeName}
	}
	if l.AccountID != nil {
		out["account"] = namedRef{ID: *l.AccountID, Name: l.AccountName}
	}
	if d.NextDue != nil {
		out["next_due"] = map[string]any{"id": d.NextDue.ID, "due_on": Date{d.NextDue.DueOn},
			"amount": centsText(books.Cents(d.NextDue.Amount) - d.NextDue.Paid)}
	}
	return out
}

func (s *Server) getLoan(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	id, ok := pathID(r, "loanID")
	if !ok {
		writeError(w, http.StatusNotFound, "loan_not_found", "Loan not found.")
		return
	}
	s.writeLoan(w, r, a, id, http.StatusOK)
}

func (s *Server) writeLoan(w http.ResponseWriter, r *http.Request, a groupAccess, id int64, status int) {
	d, err := books.GetLoanDetail(r.Context(), s.q, a.Group.ID, id, s.today())
	if errors.Is(err, books.ErrNotFound) || (err == nil && !a.IsOfficial && (a.MembershipID == nil || *a.MembershipID != d.Loan.MembershipID)) {
		writeError(w, http.StatusNotFound, "loan_not_found", "Loan not found.")
		return
	}
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	writeJSON(w, status, loanDetailJSON(d))
}

type guarantorBody struct {
	MemberID int64   `json:"member_id"`
	Amount   *Amount `json:"amount"`
	Comment  string  `json:"comment"`
}

type loanBody struct {
	MemberID          int64                `json:"member_id"`
	LoanTypeID        *int64               `json:"loan_type_id"`
	AccountID         int64                `json:"account_id"`
	Method            *string              `json:"method"`
	Amount            *Amount              `json:"amount"`
	Date              *Date                `json:"disbursed_on"`
	Installments      int                  `json:"installments"`
	FirstDueOn        *Date                `json:"first_due_on"`
	Description       string               `json:"description"`
	Guarantors        []guarantorBody      `json:"guarantors"`
	InterestType      string               `json:"interest_type"`
	InterestRate      float64              `json:"interest_rate"`
	RatePer           string               `json:"rate_per"`
	ReducingMethod    string               `json:"reducing_method"`
	FixedInterest     *Amount              `json:"fixed_interest"`
	FixedRate         float64              `json:"fixed_interest_rate"`
	Frequency         string               `json:"frequency"`
	GracePeriods      int                  `json:"grace_periods"`
	LateFine          *loans.FineRule      `json:"late_fine"`
	BalanceFine       *loans.FineRule      `json:"balance_fine"`
	ProcessingFee     *loans.ProcessingFee `json:"processing_fee"`
	FineDeferment     bool                 `json:"fine_deferment"`
	SkipProcessingFee bool                 `json:"skip_processing_fee"`
}

func (b loanBody) toBooks() books.NewLoan {
	n := books.NewLoan{
		MemberID: b.MemberID, LoanTypeID: b.LoanTypeID, AccountID: b.AccountID, Method: b.Method,
		Installments: b.Installments, Description: b.Description, InterestType: b.InterestType,
		InterestRate: b.InterestRate, RatePer: b.RatePer, ReducingMethod: b.ReducingMethod, FixedRate: b.FixedRate,
		Frequency: b.Frequency, GracePeriods: b.GracePeriods, LateFine: b.LateFine, BalanceFine: b.BalanceFine,
		ProcessingFee: b.ProcessingFee, FineDeferment: b.FineDeferment, SkipProcessingFee: b.SkipProcessingFee,
	}
	if b.Amount != nil {
		n.Amount = string(*b.Amount)
	}
	if b.Date != nil {
		n.Date = &b.Date.Time
	}
	if b.FirstDueOn != nil {
		n.FirstDueOn = &b.FirstDueOn.Time
	}
	if b.FixedInterest != nil {
		n.FixedInterest = string(*b.FixedInterest)
	}
	for _, g := range b.Guarantors {
		n.Guarantors = append(n.Guarantors, books.Guarantor{MemberID: g.MemberID, Amount: amountStr(g.Amount), Comment: g.Comment})
	}
	return n
}

// createLoan records a loan that has been paid out.
func (s *Server) createLoan(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	var body loanBody
	if !decodeJSON(w, r, &body) {
		return
	}
	userID := claimsFrom(r.Context()).UserID
	var id int64
	err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) (err error) {
		id, err = books.RecordLoan(r.Context(), s.q.WithTx(tx), a.Group.ID, userID, body.toBooks())
		return err
	})
	if writeLoanError(w, r, err) {
		return
	}
	s.writeLoan(w, r, a, id, http.StatusCreated)
}

// previewLoan works out a schedule without recording anything, for the
// loan form and for members weighing up a loan type. It takes the same body
// as createLoan; the account, member and date may be left out.
func (s *Server) previewLoan(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	var body loanBody
	if !decodeJSON(w, r, &body) {
		return
	}
	n := body.toBooks()
	if n.Date == nil {
		t := s.today()
		n.Date = &t
	}
	if n.MemberID == 0 && a.MembershipID != nil {
		n.MemberID = *a.MembershipID
	}
	terms, fee, err := books.PreviewLoan(r.Context(), s.q, a.Group.ID, n)
	if writeLoanError(w, r, err) {
		return
	}
	out, err := scheduleJSON(terms, fee)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_terms", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// scheduleJSON is a loan's schedule and totals as the preview shows them.
func scheduleJSON(terms loans.Terms, fee int64) (map[string]any, error) {
	sched, err := terms.Schedule()
	if err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0, len(sched))
	var total int64
	for _, i := range sched {
		total += i.Amount()
		items = append(items, map[string]any{"number": i.Number, "due_on": Date{i.DueDate}, "principal": centsText(i.Principal),
			"interest": centsText(i.Interest), "amount": centsText(i.Amount())})
	}
	return map[string]any{
		"amount": centsText(terms.Amount), "interest": centsText(loans.Interest(sched)), "total": centsText(total),
		"processing_fee": centsText(fee), "ends_on": Date{sched[len(sched)-1].DueDate}, "installments": items,
	}, nil
}

type loanPatch struct {
	Description *string `json:"description"`
	IsBadDebt   *bool   `json:"is_bad_debt"`
}

// updateLoan changes a loan's description or marks it a bad debt. Its terms
// are fixed once recorded: void and record it again to change them.
func (s *Server) updateLoan(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	id, ok := pathID(r, "loanID")
	if !ok {
		writeError(w, http.StatusNotFound, "loan_not_found", "Loan not found.")
		return
	}
	var body loanPatch
	if !decodeJSON(w, r, &body) {
		return
	}
	err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		l, err := books.LockLoan(r.Context(), q, a.Group.ID, id)
		if err != nil {
			return err
		}
		p := store.UpdateLoanParams{GroupID: a.Group.ID, ID: id, Description: l.Description, IsBadDebt: l.IsBadDebt}
		if body.Description != nil {
			p.Description = *body.Description
		}
		if body.IsBadDebt != nil {
			p.IsBadDebt = *body.IsBadDebt
		}
		return q.UpdateLoan(r.Context(), p)
	})
	if writeLoanError(w, r, err) {
		return
	}
	s.writeLoan(w, r, a, id, http.StatusOK)
}

func (s *Server) voidLoan(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	id, ok := pathID(r, "loanID")
	if !ok {
		writeError(w, http.StatusNotFound, "loan_not_found", "Loan not found.")
		return
	}
	userID := claimsFrom(r.Context()).UserID
	err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		return books.VoidLoan(r.Context(), s.q.WithTx(tx), a.Group.ID, id, userID)
	})
	var bad badRequest
	if errors.As(err, &bad) {
		writeError(w, http.StatusConflict, bad.Code, bad.Msg)
		return
	}
	if writeLoanError(w, r, err) {
		return
	}
	s.writeLoan(w, r, a, id, http.StatusOK)
}

type repaymentBody struct {
	AccountID   int64   `json:"account_id"`
	Method      *string `json:"method"`
	Amount      *Amount `json:"amount"`
	Date        *Date   `json:"paid_on"`
	Description string  `json:"description"`
}

func (s *Server) createRepayment(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	id, ok := pathID(r, "loanID")
	if !ok {
		writeError(w, http.StatusNotFound, "loan_not_found", "Loan not found.")
		return
	}
	var body repaymentBody
	if !decodeJSON(w, r, &body) {
		return
	}
	rp := books.Repayment{AccountID: body.AccountID, Method: body.Method, Description: body.Description}
	if body.Amount != nil {
		rp.Amount = string(*body.Amount)
	}
	if body.Date != nil {
		rp.Date = &body.Date.Time
	}
	userID := claimsFrom(r.Context()).UserID
	err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		_, err := books.RecordRepayment(r.Context(), s.q.WithTx(tx), a.Group.ID, userID, id, rp)
		return err
	})
	if writeLoanError(w, r, err) {
		return
	}
	s.writeLoan(w, r, a, id, http.StatusCreated)
}

func (s *Server) voidRepayment(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	id, ok1 := pathID(r, "loanID")
	rid, ok2 := pathID(r, "repaymentID")
	if !ok1 || !ok2 {
		writeError(w, http.StatusNotFound, "not_found", "Repayment not found.")
		return
	}
	userID := claimsFrom(r.Context()).UserID
	err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		return books.VoidRepayment(r.Context(), s.q.WithTx(tx), a.Group.ID, id, rid, userID)
	})
	var bad badRequest
	if errors.As(err, &bad) {
		writeError(w, http.StatusConflict, bad.Code, bad.Msg)
		return
	}
	if errors.Is(err, books.ErrNotFound) {
		writeError(w, http.StatusNotFound, "repayment_not_found", "Repayment not found.")
		return
	}
	if writeLoanError(w, r, err) {
		return
	}
	s.writeLoan(w, r, a, id, http.StatusOK)
}

func (s *Server) voidLoanFine(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	id, ok1 := pathID(r, "loanID")
	cid, ok2 := pathID(r, "chargeID")
	if !ok1 || !ok2 {
		writeError(w, http.StatusNotFound, "fine_not_found", "Fine not found.")
		return
	}
	err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		return books.VoidLoanFine(r.Context(), s.q.WithTx(tx), a.Group.ID, id, cid)
	})
	if errors.Is(err, books.ErrNotFound) {
		writeError(w, http.StatusNotFound, "fine_not_found", "No live fine with that id on this loan.")
		return
	}
	if writeLoanError(w, r, err) {
		return
	}
	s.writeLoan(w, r, a, id, http.StatusOK)
}

// writeLoanError answers a books error from a loan write.
func writeLoanError(w http.ResponseWriter, r *http.Request, err error) bool {
	var bad badRequest
	switch {
	case err == nil:
		return false
	case errors.Is(err, books.ErrNotFound):
		writeError(w, http.StatusNotFound, "loan_not_found", "Loan not found.")
	case errors.As(err, &bad) && bad.Code == "loan_voided":
		writeError(w, http.StatusConflict, bad.Code, bad.Msg)
	case errors.As(err, &bad):
		writeError(w, http.StatusBadRequest, bad.Code, bad.Msg)
	default:
		writeInternal(w, r, err)
	}
	return true
}
