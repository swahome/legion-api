package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/swahome/legion-api/internal/books"
	"github.com/swahome/legion-api/internal/store"
)

// Deposits: money into a group account. A member payment (contribution,
// fine, miscellaneous) also credits the member's ledger, so the deposit,
// its cash book entry and its ledger entry are written and voided together.

type depositResponse struct {
	ID             int64      `json:"id"`
	Kind           string     `json:"kind"`
	Method         *string    `json:"method"`
	Amount         string     `json:"amount"`
	Date           Date       `json:"date"`
	Description    string     `json:"description"`
	Account        *namedRef  `json:"account"`
	AccountKind    string     `json:"account_kind"`
	Member         *namedRef  `json:"member"`
	Contribution   *namedRef  `json:"contribution"`
	FineCategory   *namedRef  `json:"fine_category"`
	IncomeCategory *namedRef  `json:"income_category"`
	Depositor      *namedRef  `json:"depositor"`
	CreatedAt      time.Time  `json:"created_at"`
	VoidedAt       *time.Time `json:"voided_at"`
}

func ref(id *int64, name *string) *namedRef {
	if id == nil {
		return nil
	}
	return &namedRef{ID: *id, Name: deref(name)}
}

func toDeposit(d store.ListDepositsRow) depositResponse {
	out := depositResponse{
		ID: d.ID, Kind: string(d.Kind), Method: nullEnum(d.Method.Valid, d.Method.PaymentMethod),
		Amount: money(d.Amount), Date: Date{d.DepositDate}, Description: d.Description,
		Account: ref(d.AccountID, d.AccountName), AccountKind: string(d.AccountKind),
		Contribution: ref(d.ContributionID, d.ContributionName), FineCategory: ref(d.FineCategoryID, d.FineCategoryName),
		IncomeCategory: ref(d.IncomeCategoryID, d.IncomeCategoryName), Depositor: ref(d.DepositorID, d.DepositorName),
		CreatedAt: d.CreatedAt, VoidedAt: d.VoidedAt,
	}
	if d.MembershipID != nil {
		out.Member = &namedRef{ID: *d.MembershipID, Name: d.MemberName}
	}
	return out
}

func optionalID(r *http.Request, key string) (*int64, bool) {
	v := r.URL.Query().Get(key)
	if v == "" {
		return nil, true
	}
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil || id <= 0 {
		return nil, false
	}
	return &id, true
}

func optionalDate(r *http.Request, key string) (*time.Time, bool) {
	v := r.URL.Query().Get(key)
	if v == "" {
		return nil, true
	}
	t, err := time.Parse(dateLayout, v)
	if err != nil {
		return nil, false
	}
	return &t, true
}

// listDeposits pages through deposits, newest first. Filters: kind,
// member_id, account_id, from, to, include_voided=true.
func (s *Server) listDeposits(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	pg := pageParams(r)
	p := store.ListDepositsParams{
		GroupID: a.Group.ID, IncludeVoided: r.URL.Query().Get("include_voided") == "true",
		PageSize: pg.size, PageOffset: pg.offset,
	}
	if k := r.URL.Query().Get("kind"); k != "" {
		if !store.DepositKind(k).Valid() {
			writeError(w, http.StatusBadRequest, "invalid_kind", "Unknown deposit kind.")
			return
		}
		p.Kind = store.NullDepositKind{DepositKind: store.DepositKind(k), Valid: true}
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
	rows, err := s.q.ListDeposits(r.Context(), p)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	totals, err := s.q.DepositTotals(r.Context(), store.DepositTotalsParams{
		GroupID: a.Group.ID, MembershipID: p.MembershipID, AccountID: p.AccountID, FromDate: p.FromDate, ToDate: p.ToDate,
	})
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	items := make([]depositResponse, 0, len(rows))
	var total int64
	for _, d := range rows {
		items = append(items, toDeposit(d))
		total = d.Total
	}
	byKind := map[string]string{}
	for _, t := range totals {
		byKind[string(t.Kind)] = money(t.Amount)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"deposits": newPaged(items, total, pg), "totals_by_kind": byKind,
	})
}

func (s *Server) getDeposit(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	id, ok := pathID(r, "depositID")
	if !ok {
		writeError(w, http.StatusNotFound, "deposit_not_found", "Deposit not found.")
		return
	}
	s.writeDeposit(w, r, a.Group.ID, id, http.StatusOK)
}

func (s *Server) writeDeposit(w http.ResponseWriter, r *http.Request, groupID, id int64, status int) {
	d, err := s.q.GetDepositView(r.Context(), store.GetDepositViewParams{GroupID: groupID, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "deposit_not_found", "Deposit not found.")
		return
	}
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	ledger, err := s.q.LedgerEntriesForDeposit(r.Context(), &id)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	refs := make([]depositLedgerEntry, 0, len(ledger))
	for _, l := range ledger {
		refs = append(refs, depositLedgerEntry{ID: l.ID, Type: string(l.EntryType), Amount: money(l.Amount), VoidedAt: l.VoidedAt})
	}
	writeJSON(w, status, depositDetail{toDeposit(store.ListDepositsRow(d)), refs})
}

// depositLedgerEntry is a member ledger row the deposit created.
type depositLedgerEntry struct {
	ID       int64      `json:"id"`
	Type     string     `json:"type"`
	Amount   string     `json:"amount"`
	VoidedAt *time.Time `json:"voided_at"`
}

type depositDetail struct {
	depositResponse
	LedgerEntries []depositLedgerEntry `json:"ledger_entries"`
}

type depositRequest struct {
	Kind             string  `json:"kind"`
	AccountID        int64   `json:"account_id"`
	Method           *string `json:"method"`
	Amount           *Amount `json:"amount"`
	Date             *Date   `json:"date"`
	Description      string  `json:"description"`
	MemberID         *int64  `json:"member_id"`
	ContributionID   *int64  `json:"contribution_id"`
	FineCategoryID   *int64  `json:"fine_category_id"`
	IncomeCategoryID *int64  `json:"income_category_id"`
	DepositorID      *int64  `json:"depositor_id"`
}

func (s *Server) createDeposit(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	var req depositRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	d := books.Deposit{
		Kind: store.DepositKind(req.Kind), AccountID: req.AccountID, Method: req.Method, Description: req.Description,
		MemberID: req.MemberID, ContributionID: req.ContributionID, FineCategoryID: req.FineCategoryID,
		IncomeCategoryID: req.IncomeCategoryID, DepositorID: req.DepositorID,
	}
	if req.Amount != nil {
		d.Amount = string(*req.Amount)
	}
	if req.Date != nil {
		d.Date = &req.Date.Time
	}
	userID := claimsFrom(r.Context()).UserID
	var id int64
	err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) (err error) {
		id, err = books.RecordDeposit(r.Context(), s.q.WithTx(tx), a.Group.ID, userID, d)
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
	s.writeDeposit(w, r, a.Group.ID, id, http.StatusCreated)
}

// voidDeposit reverses a deposit: it, its cash book entry and its member
// ledger entry stop counting. Voiding twice is a 409.
func (s *Server) voidDeposit(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	id, ok := pathID(r, "depositID")
	if !ok {
		writeError(w, http.StatusNotFound, "deposit_not_found", "Deposit not found.")
		return
	}
	userID := claimsFrom(r.Context()).UserID
	err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		return books.VoidDeposit(r.Context(), s.q.WithTx(tx), a.Group.ID, id, userID)
	})
	var bad badRequest
	switch {
	case errors.Is(err, books.ErrNotFound):
		writeError(w, http.StatusNotFound, "deposit_not_found", "Deposit not found.")
	case errors.As(err, &bad):
		writeError(w, http.StatusConflict, bad.Code, bad.Msg)
	case err != nil:
		writeInternal(w, r, err)
	default:
		s.writeDeposit(w, r, a.Group.ID, id, http.StatusOK)
	}
}

// badRequest is a validation failure: 400 with its code, or 409 when a
// void finds the row already voided.
type badRequest = books.Error

// checkAmountDate validates the fields every movement of money has.
func checkAmountDate(amount *Amount, date *Date, method *string, noun string) error {
	a := ""
	if amount != nil {
		a = string(*amount)
	}
	var d *time.Time
	if date != nil {
		d = &date.Time
	}
	return books.CheckAmountDate(a, d, method, noun)
}

var openAccount = books.OpenAccount

func nairobi() *time.Location { return books.Nairobi() }
