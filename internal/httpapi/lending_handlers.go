package httpapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/swahome/legion-api/internal/books"
	"github.com/swahome/legion-api/internal/store"
)

// Milestone 4b: bank loans (money the group borrowed) and debtors (people
// outside the group who borrow from it). Loans to debtors are ordinary
// loans with a debtor_id: see loans_handlers.go.

// ---- Bank loans ----

type bankLoanResponse struct {
	ID             int64      `json:"id"`
	Description    string     `json:"description"`
	Amount         string     `json:"amount"`
	TotalPayable   string     `json:"total_payable"`
	OpeningBalance string     `json:"opening_balance"`
	Repaid         string     `json:"repaid"`
	Balance        string     `json:"balance"`
	StartsOn       Date       `json:"starts_on"`
	EndsOn         Date       `json:"ends_on"`
	Account        *namedRef  `json:"account"`
	Status         string     `json:"status"` // active, paid, voided
	CreatedAt      time.Time  `json:"created_at"`
	VoidedAt       *time.Time `json:"voided_at"`
}

func bankLoanJSON(id int64, description, amount, total, opening, repaid, balance string, starts, ends time.Time,
	accountID *int64, accountName string, created time.Time, voided *time.Time) bankLoanResponse {
	out := bankLoanResponse{ID: id, Description: description, Amount: money(amount), TotalPayable: money(total),
		OpeningBalance: money(opening), Repaid: money(repaid), Balance: money(balance), StartsOn: Date{starts},
		EndsOn: Date{ends}, Status: "active", CreatedAt: created, VoidedAt: voided}
	if accountID != nil {
		out.Account = &namedRef{ID: *accountID, Name: accountName}
	}
	switch {
	case voided != nil:
		out.Status = "voided"
	case books.Cents(balance) <= 0:
		out.Status = "paid"
	}
	return out
}

// listBankLoans lists the group's bank loans with their totals. status:
// open (still owing), paid or voided; default every live loan.
func (s *Server) listBankLoans(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	status := r.URL.Query().Get("status")
	switch status {
	case "", "open", "paid", "voided":
	default:
		writeError(w, http.StatusBadRequest, "invalid_filter", "status must be open, paid or voided.")
		return
	}
	rows, err := s.q.ListBankLoans(r.Context(), store.ListBankLoansParams{GroupID: a.Group.ID, Status: status})
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	out := make([]bankLoanResponse, 0, len(rows))
	for _, b := range rows {
		out = append(out, bankLoanJSON(b.ID, b.Description, b.Amount, b.TotalPayable, b.OpeningBalance, b.Repaid,
			b.Balance, b.StartsOn, b.EndsOn, b.AccountID, b.AccountName, b.CreatedAt, b.VoidedAt))
	}
	t, err := s.q.BankLoanTotals(r.Context(), a.Group.ID)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"bank_loans": out, "totals": map[string]any{
		"loans": t.Loans, "received": money(t.Received), "repaid": money(t.Repaid), "balance": money(t.Balance)}})
}

type bankRepaymentResponse struct {
	ID          int64      `json:"id"` // the withdrawal
	Account     *namedRef  `json:"account"`
	Method      *string    `json:"method"`
	Amount      string     `json:"amount"`
	PaidOn      Date       `json:"paid_on"`
	Description string     `json:"description"`
	VoidedAt    *time.Time `json:"voided_at"`
}

func (s *Server) getBankLoan(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	id, ok := pathID(r, "bankLoanID")
	if !ok {
		writeError(w, http.StatusNotFound, "bank_loan_not_found", "Bank loan not found.")
		return
	}
	s.writeBankLoan(w, r, a.Group.ID, id, http.StatusOK)
}

func (s *Server) writeBankLoan(w http.ResponseWriter, r *http.Request, groupID, id int64, status int) {
	b, err := s.q.GetBankLoan(r.Context(), store.GetBankLoanParams{GroupID: groupID, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "bank_loan_not_found", "Bank loan not found.")
		return
	}
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	reps, err := s.q.BankLoanRepayments(r.Context(), store.BankLoanRepaymentsParams{BankLoanID: &id, IncludeVoided: true})
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	list := make([]bankRepaymentResponse, 0, len(reps))
	for _, x := range reps {
		rr := bankRepaymentResponse{ID: x.ID, Method: nullEnum(x.Method.Valid, x.Method.PaymentMethod),
			Amount: money(x.Amount), PaidOn: Date{x.WithdrawalDate}, Description: x.Description, VoidedAt: x.VoidedAt}
		if x.AccountID != nil {
			rr.Account = &namedRef{ID: *x.AccountID, Name: x.AccountName}
		}
		list = append(list, rr)
	}
	writeJSON(w, status, map[string]any{
		"bank_loan": bankLoanJSON(b.ID, b.Description, b.Amount, b.TotalPayable, b.OpeningBalance, b.Repaid,
			b.Balance, b.StartsOn, b.EndsOn, b.AccountID, b.AccountName, b.CreatedAt, b.VoidedAt),
		"repayments": list,
	})
}

type bankLoanBody struct {
	Description  string  `json:"description"`
	Amount       *Amount `json:"amount"`
	TotalPayable *Amount `json:"total_payable"`
	Balance      *Amount `json:"balance"` // what is still owed, for a loan taken before it was entered here
	StartsOn     *Date   `json:"starts_on"`
	EndsOn       *Date   `json:"ends_on"`
	AccountID    int64   `json:"account_id"`
	Method       *string `json:"method"`
}

func amountText(a *Amount) string {
	if a == nil {
		return ""
	}
	return string(*a)
}

func dateOf(d *Date) *time.Time {
	if d == nil {
		return nil
	}
	return &d.Time
}

// createBankLoan records money the group borrowed coming into an account.
func (s *Server) createBankLoan(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	var body bankLoanBody
	if !decodeJSON(w, r, &body) {
		return
	}
	b := books.BankLoan{Description: body.Description, Amount: amountText(body.Amount),
		TotalPayable: amountText(body.TotalPayable), Balance: amountStr(body.Balance), StartsOn: dateOf(body.StartsOn),
		EndsOn: dateOf(body.EndsOn), AccountID: body.AccountID, Method: body.Method}
	if body.TotalPayable == nil {
		b.TotalPayable = b.Amount
	}
	userID := claimsFrom(r.Context()).UserID
	var id int64
	err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) (err error) {
		id, err = books.RecordBankLoan(r.Context(), s.q.WithTx(tx), a.Group.ID, userID, b)
		return err
	})
	if writeBankLoanError(w, r, err) {
		return
	}
	s.writeBankLoan(w, r, a.Group.ID, id, http.StatusCreated)
}

type bankLoanEditBody struct {
	Description  string  `json:"description"`
	TotalPayable *Amount `json:"total_payable"`
	Balance      *Amount `json:"balance"`
	EndsOn       *Date   `json:"ends_on"`
}

// updateBankLoan changes a bank loan's description, total payable, opening
// balance and end date. Every field is required.
func (s *Server) updateBankLoan(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	id, ok := pathID(r, "bankLoanID")
	if !ok {
		writeError(w, http.StatusNotFound, "bank_loan_not_found", "Bank loan not found.")
		return
	}
	var body bankLoanEditBody
	if !decodeJSON(w, r, &body) {
		return
	}
	e := books.BankLoanEdit{Description: body.Description, TotalPayable: amountText(body.TotalPayable),
		Balance: amountText(body.Balance), EndsOn: dateOf(body.EndsOn)}
	err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		return books.EditBankLoan(r.Context(), s.q.WithTx(tx), a.Group.ID, id, e)
	})
	if writeBankLoanError(w, r, err) {
		return
	}
	s.writeBankLoan(w, r, a.Group.ID, id, http.StatusOK)
}

// voidBankLoan voids a bank loan, the money it brought in and its repayments.
func (s *Server) voidBankLoan(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	id, ok := pathID(r, "bankLoanID")
	if !ok {
		writeError(w, http.StatusNotFound, "bank_loan_not_found", "Bank loan not found.")
		return
	}
	userID := claimsFrom(r.Context()).UserID
	err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		return books.VoidBankLoan(r.Context(), s.q.WithTx(tx), a.Group.ID, id, userID)
	})
	if writeBankLoanError(w, r, err) {
		return
	}
	s.writeBankLoan(w, r, a.Group.ID, id, http.StatusOK)
}

// createBankLoanRepayment records money paid back to the lender.
func (s *Server) createBankLoanRepayment(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	id, ok := pathID(r, "bankLoanID")
	if !ok {
		writeError(w, http.StatusNotFound, "bank_loan_not_found", "Bank loan not found.")
		return
	}
	var body repaymentBody
	if !decodeJSON(w, r, &body) {
		return
	}
	rp := books.BankLoanRepayment{AccountID: body.AccountID, Method: body.Method, Amount: amountText(body.Amount),
		Date: dateOf(body.Date), Description: body.Description}
	userID := claimsFrom(r.Context()).UserID
	err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		_, err := books.RecordBankLoanRepayment(r.Context(), s.q.WithTx(tx), a.Group.ID, userID, id, rp)
		return err
	})
	if writeBankLoanError(w, r, err) {
		return
	}
	s.writeBankLoan(w, r, a.Group.ID, id, http.StatusCreated)
}

// voidBankLoanRepayment voids one repayment (its withdrawal).
func (s *Server) voidBankLoanRepayment(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	id, ok1 := pathID(r, "bankLoanID")
	rid, ok2 := pathID(r, "repaymentID")
	if !ok1 || !ok2 {
		writeError(w, http.StatusNotFound, "not_found", "Repayment not found.")
		return
	}
	userID := claimsFrom(r.Context()).UserID
	err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		wd, err := q.GetWithdrawal(r.Context(), store.GetWithdrawalParams{GroupID: a.Group.ID, ID: rid})
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && (!books.Is(wd.BankLoanID, id) || wd.Kind != store.WithdrawalKindBankLoanRepayment)) {
			return books.ErrNotFound
		}
		if err != nil {
			return err
		}
		return books.VoidWithdrawal(r.Context(), q, a.Group.ID, rid, userID)
	})
	if errors.Is(err, books.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "Repayment not found.")
		return
	}
	if writeBankLoanError(w, r, err) {
		return
	}
	s.writeBankLoan(w, r, a.Group.ID, id, http.StatusOK)
}

func writeBankLoanError(w http.ResponseWriter, r *http.Request, err error) bool {
	var bad badRequest
	switch {
	case err == nil:
		return false
	case errors.Is(err, books.ErrNotFound):
		writeError(w, http.StatusNotFound, "bank_loan_not_found", "Bank loan not found.")
	case errors.As(err, &bad) && (bad.Code == "loan_voided" || bad.Code == "already_voided"):
		writeError(w, http.StatusConflict, bad.Code, bad.Msg)
	case errors.As(err, &bad):
		writeError(w, http.StatusBadRequest, bad.Code, bad.Msg)
	default:
		writeInternal(w, r, err)
	}
	return true
}

// ---- Debtors ----

type debtorResponse struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Phone       string    `json:"phone"`
	Email       string    `json:"email"`
	IDNumber    string    `json:"id_number"`
	Description string    `json:"description"`
	IsActive    bool      `json:"is_active"`
	Loans       int64     `json:"loans"`
	Lent        string    `json:"lent"`
	Balance     string    `json:"balance"`
	CreatedAt   time.Time `json:"created_at"`
}

// listDebtors lists the group's debtors with what they owe. Officials only.
// ?include_inactive=true adds suspended debtors.
func (s *Server) listDebtors(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	rows, err := s.q.ListDebtors(r.Context(), store.ListDebtorsParams{GroupID: a.Group.ID,
		IncludeInactive: r.URL.Query().Get("include_inactive") == "true"})
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	out := make([]debtorResponse, 0, len(rows))
	for _, d := range rows {
		out = append(out, debtorResponse{ID: d.ID, Name: d.Name, Phone: d.Phone, Email: d.Email, IDNumber: d.IDNumber,
			Description: d.Description, IsActive: d.IsActive, Loans: d.Loans, Lent: money(d.Lent),
			Balance: money(d.Balance), CreatedAt: d.CreatedAt})
	}
	writeJSON(w, http.StatusOK, map[string]any{"debtors": out})
}

func (s *Server) getDebtor(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	id, ok := pathID(r, "debtorID")
	if !ok {
		writeError(w, http.StatusNotFound, "debtor_not_found", "Debtor not found.")
		return
	}
	s.writeDebtor(w, r, a.Group.ID, id, http.StatusOK)
}

func (s *Server) writeDebtor(w http.ResponseWriter, r *http.Request, groupID, id int64, status int) {
	rows, err := s.q.ListDebtors(r.Context(), store.ListDebtorsParams{GroupID: groupID, IncludeInactive: true})
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	for _, d := range rows {
		if d.ID == id {
			writeJSON(w, status, debtorResponse{ID: d.ID, Name: d.Name, Phone: d.Phone, Email: d.Email,
				IDNumber: d.IDNumber, Description: d.Description, IsActive: d.IsActive, Loans: d.Loans,
				Lent: money(d.Lent), Balance: money(d.Balance), CreatedAt: d.CreatedAt})
			return
		}
	}
	writeError(w, http.StatusNotFound, "debtor_not_found", "Debtor not found.")
}

type debtorBody struct {
	Name        string `json:"name"`
	Phone       string `json:"phone"`
	Email       string `json:"email"`
	IDNumber    string `json:"id_number"`
	Description string `json:"description"`
}

// saveDebtor creates (POST) or updates (PATCH) a debtor.
func (s *Server) saveDebtor(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	var id int64
	status := http.StatusCreated
	if r.Method == http.MethodPatch {
		var ok bool
		if id, ok = pathID(r, "debtorID"); !ok {
			writeError(w, http.StatusNotFound, "debtor_not_found", "Debtor not found.")
			return
		}
		status = http.StatusOK
	}
	var body debtorBody
	if !decodeJSON(w, r, &body) {
		return
	}
	userID := claimsFrom(r.Context()).UserID
	id, err := books.SaveDebtor(r.Context(), s.q, a.Group.ID, userID, id, books.Debtor{Name: body.Name,
		Phone: body.Phone, Email: body.Email, IDNumber: body.IDNumber, Description: body.Description})
	if writeDebtorError(w, r, err) {
		return
	}
	s.writeDebtor(w, r, a.Group.ID, id, status)
}

// setDebtorActive suspends or reactivates a debtor. A suspended debtor
// keeps their loans but cannot be lent to.
func (s *Server) setDebtorActive(active bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a := accessFrom(r.Context())
		id, ok := pathID(r, "debtorID")
		if !ok {
			writeError(w, http.StatusNotFound, "debtor_not_found", "Debtor not found.")
			return
		}
		n, err := s.q.SetDebtorActive(r.Context(), store.SetDebtorActiveParams{GroupID: a.Group.ID, ID: id, IsActive: active})
		if err == nil && n == 0 {
			err = books.ErrNotFound
		}
		if writeDebtorError(w, r, err) {
			return
		}
		s.writeDebtor(w, r, a.Group.ID, id, http.StatusOK)
	}
}

// deleteDebtor removes a debtor who has no live loans.
func (s *Server) deleteDebtor(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	id, ok := pathID(r, "debtorID")
	if !ok {
		writeError(w, http.StatusNotFound, "debtor_not_found", "Debtor not found.")
		return
	}
	if writeDebtorError(w, r, books.DeleteDebtor(r.Context(), s.q, a.Group.ID, id)) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeDebtorError(w http.ResponseWriter, r *http.Request, err error) bool {
	var bad badRequest
	switch {
	case err == nil:
		return false
	case errors.Is(err, books.ErrNotFound):
		writeError(w, http.StatusNotFound, "debtor_not_found", "Debtor not found.")
	case errors.As(err, &bad) && bad.Code == "debtor_has_loans":
		writeError(w, http.StatusConflict, bad.Code, bad.Msg)
	case errors.As(err, &bad):
		writeError(w, http.StatusBadRequest, bad.Code, bad.Msg)
	default:
		writeInternal(w, r, err)
	}
	return true
}
