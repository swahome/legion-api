package httpapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/swahome/legion-api/internal/books"
	"github.com/swahome/legion-api/internal/store"
)

// Withdrawals: money out of a group account, recorded by an official after
// it has left (legacy "record expenses", "record contribution refund"). A
// refund also comes off the member's contribution balance, so the
// withdrawal, its cash book entry and its ledger entry are written and
// voided together. Transfers between the group's own accounts are
// account_transfers, with an entry out of one account and into the other.

type withdrawalResponse struct {
	ID              int64      `json:"id"`
	Kind            string     `json:"kind"`
	Method          *string    `json:"method"`
	Amount          string     `json:"amount"`
	Date            Date       `json:"date"`
	Description     string     `json:"description"`
	Account         *namedRef  `json:"account"`
	AccountKind     string     `json:"account_kind"`
	Member          *namedRef  `json:"member"`
	Contribution    *namedRef  `json:"contribution"`
	ExpenseCategory *namedRef  `json:"expense_category"`
	CreatedAt       time.Time  `json:"created_at"`
	VoidedAt        *time.Time `json:"voided_at"`
}

func toWithdrawal(w store.ListWithdrawalsRow) withdrawalResponse {
	out := withdrawalResponse{
		ID: w.ID, Kind: string(w.Kind), Method: nullEnum(w.Method.Valid, w.Method.PaymentMethod),
		Amount: money(w.Amount), Date: Date{w.WithdrawalDate}, Description: w.Description,
		Account: ref(w.AccountID, w.AccountName), AccountKind: string(w.AccountKind),
		Contribution: ref(w.ContributionID, w.ContributionName), ExpenseCategory: ref(w.ExpenseCategoryID, w.ExpenseCategoryName),
		CreatedAt: w.CreatedAt, VoidedAt: w.VoidedAt,
	}
	if w.MembershipID != nil {
		out.Member = &namedRef{ID: *w.MembershipID, Name: w.MemberName}
	}
	return out
}

// listWithdrawals pages through withdrawals, newest first. Filters: kind,
// member_id, account_id, from, to, include_voided=true.
func (s *Server) listWithdrawals(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	pg := pageParams(r)
	p := store.ListWithdrawalsParams{
		GroupID: a.Group.ID, IncludeVoided: r.URL.Query().Get("include_voided") == "true",
		PageSize: pg.size, PageOffset: pg.offset,
	}
	if k := r.URL.Query().Get("kind"); k != "" {
		if !store.WithdrawalKind(k).Valid() {
			writeError(w, http.StatusBadRequest, "invalid_kind", "Unknown withdrawal kind.")
			return
		}
		p.Kind = store.NullWithdrawalKind{WithdrawalKind: store.WithdrawalKind(k), Valid: true}
	}
	var ok1, ok2, ok3, ok4 bool
	p.MembershipID, ok1 = optionalID(r, "member_id")
	p.AccountID, ok2 = optionalID(r, "account_id")
	p.FromDate, ok3 = optionalDate(r, "from")
	p.ToDate, ok4 = optionalDate(r, "to")
	if !ok1 || !ok2 || !ok3 || !ok4 {
		writeError(w, http.StatusBadRequest, "invalid_filter", "Filters must be ids or dates like 2026-11-05.")
		return
	}
	rows, err := s.q.ListWithdrawals(r.Context(), p)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	totals, err := s.q.WithdrawalTotals(r.Context(), store.WithdrawalTotalsParams{
		GroupID: a.Group.ID, MembershipID: p.MembershipID, AccountID: p.AccountID, FromDate: p.FromDate, ToDate: p.ToDate,
	})
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	items := make([]withdrawalResponse, 0, len(rows))
	var total int64
	for _, row := range rows {
		items = append(items, toWithdrawal(row))
		total = row.Total
	}
	byKind := map[string]string{}
	for _, t := range totals {
		byKind[string(t.Kind)] = money(t.Amount)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"withdrawals": newPaged(items, total, pg), "totals_by_kind": byKind,
	})
}

func (s *Server) getWithdrawal(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "withdrawalID")
	if !ok {
		writeError(w, http.StatusNotFound, "withdrawal_not_found", "Withdrawal not found.")
		return
	}
	s.writeWithdrawal(w, r, accessFrom(r.Context()).Group.ID, id, http.StatusOK)
}

func (s *Server) writeWithdrawal(w http.ResponseWriter, r *http.Request, groupID, id int64, status int) {
	row, err := s.q.GetWithdrawalView(r.Context(), store.GetWithdrawalViewParams{GroupID: groupID, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "withdrawal_not_found", "Withdrawal not found.")
		return
	}
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	writeJSON(w, status, toWithdrawal(store.ListWithdrawalsRow(row)))
}

type withdrawalRequest struct {
	Kind              string  `json:"kind"`
	AccountID         int64   `json:"account_id"`
	Method            *string `json:"method"`
	Amount            *Amount `json:"amount"`
	Date              *Date   `json:"date"`
	Description       string  `json:"description"`
	MemberID          *int64  `json:"member_id"`
	ContributionID    *int64  `json:"contribution_id"`
	ExpenseCategoryID *int64  `json:"expense_category_id"`
}

func (s *Server) createWithdrawal(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	var req withdrawalRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	m := books.Withdrawal{Kind: store.WithdrawalKind(req.Kind), MemberID: req.MemberID,
		ContributionID: req.ContributionID, ExpenseCategoryID: req.ExpenseCategoryID}
	if req.Amount != nil {
		m.Amount = string(*req.Amount)
	}
	var date *time.Time
	if req.Date != nil {
		date = &req.Date.Time
	}
	userID := claimsFrom(r.Context()).UserID
	var id int64
	err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) (err error) {
		id, err = books.RecordWithdrawal(r.Context(), s.q.WithTx(tx), a.Group.ID, userID, req.AccountID, m, req.Method, date, req.Description)
		return err
	})
	var bad badRequest
	if errors.As(err, &bad) {
		writeError(w, http.StatusBadRequest, bad.Code, bad.Msg)
		return
	}
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	s.writeWithdrawal(w, r, a.Group.ID, id, http.StatusCreated)
}

// voidWithdrawal reverses a withdrawal with its cash book and ledger
// entries. Voiding twice is a 409.
func (s *Server) voidWithdrawal(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	id, ok := pathID(r, "withdrawalID")
	if !ok {
		writeError(w, http.StatusNotFound, "withdrawal_not_found", "Withdrawal not found.")
		return
	}
	userID := claimsFrom(r.Context()).UserID
	err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		return books.VoidWithdrawal(r.Context(), s.q.WithTx(tx), a.Group.ID, id, userID)
	})
	var bad badRequest
	switch {
	case errors.Is(err, books.ErrNotFound):
		writeError(w, http.StatusNotFound, "withdrawal_not_found", "Withdrawal not found.")
	case errors.As(err, &bad):
		writeError(w, http.StatusConflict, bad.Code, bad.Msg)
	case err != nil:
		writeInternal(w, r, err)
	default:
		s.writeWithdrawal(w, r, a.Group.ID, id, http.StatusOK)
	}
}

// Transfers between the group's accounts.

type transferResponse struct {
	ID          int64      `json:"id"`
	Amount      string     `json:"amount"`
	Date        Date       `json:"date"`
	Description string     `json:"description"`
	FromAccount *namedRef  `json:"from_account"`
	ToAccount   *namedRef  `json:"to_account"`
	CreatedAt   time.Time  `json:"created_at"`
	VoidedAt    *time.Time `json:"voided_at"`
}

func toTransfer(t store.ListAccountTransfersRow) transferResponse {
	return transferResponse{
		ID: t.ID, Amount: money(t.Amount), Date: Date{t.TransferDate}, Description: t.Description,
		FromAccount: ref(t.FromAccountID, t.FromAccountName), ToAccount: ref(t.ToAccountID, t.ToAccountName),
		CreatedAt: t.CreatedAt, VoidedAt: t.VoidedAt,
	}
}

// listTransfers pages through transfers, newest first. Filters: account_id
// (either side), from, to, include_voided=true.
func (s *Server) listTransfers(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	pg := pageParams(r)
	p := store.ListAccountTransfersParams{
		GroupID: a.Group.ID, IncludeVoided: r.URL.Query().Get("include_voided") == "true",
		PageSize: pg.size, PageOffset: pg.offset,
	}
	var ok1, ok2, ok3 bool
	p.AccountID, ok1 = optionalID(r, "account_id")
	p.FromDate, ok2 = optionalDate(r, "from")
	p.ToDate, ok3 = optionalDate(r, "to")
	if !ok1 || !ok2 || !ok3 {
		writeError(w, http.StatusBadRequest, "invalid_filter", "Filters must be ids or dates like 2026-11-05.")
		return
	}
	rows, err := s.q.ListAccountTransfers(r.Context(), p)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	items := make([]transferResponse, 0, len(rows))
	var total int64
	for _, t := range rows {
		items = append(items, toTransfer(t))
		total = t.Total
	}
	writeJSON(w, http.StatusOK, map[string]any{"transfers": newPaged(items, total, pg)})
}

func (s *Server) writeTransfer(w http.ResponseWriter, r *http.Request, groupID, id int64, status int) {
	t, err := s.q.GetAccountTransferView(r.Context(), store.GetAccountTransferViewParams{GroupID: groupID, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "transfer_not_found", "Transfer not found.")
		return
	}
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	writeJSON(w, status, toTransfer(store.ListAccountTransfersRow(t)))
}

type transferRequest struct {
	FromAccountID int64   `json:"from_account_id"`
	ToAccountID   int64   `json:"to_account_id"`
	Amount        *Amount `json:"amount"`
	Date          *Date   `json:"date"`
	Description   string  `json:"description"`
}

func (s *Server) createTransfer(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	var req transferRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	amount := ""
	if req.Amount != nil {
		amount = string(*req.Amount)
	}
	var date *time.Time
	if req.Date != nil {
		date = &req.Date.Time
	}
	userID := claimsFrom(r.Context()).UserID
	var id int64
	err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) (err error) {
		id, err = books.RecordTransfer(r.Context(), s.q.WithTx(tx), a.Group.ID, userID, req.FromAccountID, req.ToAccountID, amount, date, req.Description)
		return err
	})
	var bad badRequest
	if errors.As(err, &bad) {
		writeError(w, http.StatusBadRequest, bad.Code, bad.Msg)
		return
	}
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	s.writeTransfer(w, r, a.Group.ID, id, http.StatusCreated)
}

func (s *Server) voidTransfer(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	id, ok := pathID(r, "transferID")
	if !ok {
		writeError(w, http.StatusNotFound, "transfer_not_found", "Transfer not found.")
		return
	}
	userID := claimsFrom(r.Context()).UserID
	err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		return books.VoidTransfer(r.Context(), s.q.WithTx(tx), a.Group.ID, id, userID)
	})
	var bad badRequest
	switch {
	case errors.Is(err, books.ErrNotFound):
		writeError(w, http.StatusNotFound, "transfer_not_found", "Transfer not found.")
	case errors.As(err, &bad):
		writeError(w, http.StatusConflict, bad.Code, bad.Msg)
	case err != nil:
		writeInternal(w, r, err)
	default:
		s.writeTransfer(w, r, a.Group.ID, id, http.StatusOK)
	}
}
