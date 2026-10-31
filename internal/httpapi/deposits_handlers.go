package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

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

// Kinds that can be recorded by hand so far. Loan repayments arrive with
// loans (milestone 4); stock, asset and money market sales with milestone 8.
var recordableDeposits = map[store.DepositKind]store.LedgerEntryType{
	store.DepositKindContribution: store.LedgerEntryTypeContributionPayment,
	store.DepositKindFine:         store.LedgerEntryTypeFinePayment,
	store.DepositKindMisc:         store.LedgerEntryTypeMiscPayment,
	store.DepositKindIncome:       "",
}

type badRequest struct{ code, msg string }

func (e badRequest) Error() string { return e.msg }

// checkDeposit validates the references a deposit makes. It runs inside
// the write transaction.
func (s *Server) checkDeposit(r *http.Request, q *store.Queries, groupID int64, req *depositRequest) (store.GroupAccount, error) {
	ctx := r.Context()
	kind := store.DepositKind(req.Kind)
	if _, ok := recordableDeposits[kind]; !ok {
		return store.GroupAccount{}, badRequest{"invalid_kind", "kind must be contribution, fine, misc or income."}
	}
	if err := checkAmountDate(req.Amount, req.Date, req.Method, "deposit"); err != nil {
		return store.GroupAccount{}, err
	}
	acct, err := openAccount(ctx, q, groupID, req.AccountID)
	if err != nil {
		return acct, err
	}
	exists := func(f func() (bool, error), code, msg string) error {
		ok, err := f()
		if err != nil {
			return err
		}
		if !ok {
			return badRequest{code, msg}
		}
		return nil
	}
	member := kind != store.DepositKindIncome
	switch {
	case member && req.MemberID == nil:
		return acct, badRequest{"missing_fields", "member_id is required for a member payment."}
	case !member && req.MemberID != nil:
		return acct, badRequest{"invalid_body", "Income is not linked to a member; use a depositor."}
	case kind == store.DepositKindContribution && req.ContributionID == nil:
		return acct, badRequest{"missing_fields", "contribution_id is required."}
	case kind == store.DepositKindFine && (req.FineCategoryID == nil) == (req.ContributionID == nil):
		return acct, badRequest{"missing_fields", "A fine payment needs either fine_category_id or contribution_id (for a contribution's late fine)."}
	case kind == store.DepositKindIncome && req.IncomeCategoryID == nil:
		return acct, badRequest{"missing_fields", "income_category_id is required."}
	case kind == store.DepositKindMisc && (req.ContributionID != nil || req.FineCategoryID != nil):
		return acct, badRequest{"invalid_body", "A miscellaneous payment has no contribution or fine."}
	case kind != store.DepositKindFine && kind != store.DepositKindContribution && req.ContributionID != nil,
		kind != store.DepositKindFine && req.FineCategoryID != nil,
		kind != store.DepositKindIncome && (req.IncomeCategoryID != nil || req.DepositorID != nil):
		return acct, badRequest{"invalid_body", "That field does not apply to this kind of deposit."}
	}
	if req.MemberID != nil {
		if err := exists(func() (bool, error) {
			return q.ActiveMembershipInGroup(ctx, store.ActiveMembershipInGroupParams{GroupID: groupID, ID: *req.MemberID})
		}, "invalid_member", "That member is not in this group."); err != nil {
			return acct, err
		}
	}
	if req.ContributionID != nil {
		if err := exists(func() (bool, error) {
			return q.ContributionInGroup(ctx, store.ContributionInGroupParams{GroupID: groupID, ID: *req.ContributionID})
		}, "invalid_contribution", "That contribution is not in this group."); err != nil {
			return acct, err
		}
	}
	if req.FineCategoryID != nil {
		if err := exists(func() (bool, error) {
			return q.FineCategoryInGroup(ctx, store.FineCategoryInGroupParams{GroupID: groupID, ID: *req.FineCategoryID})
		}, "invalid_fine_category", "That fine category is not in this group."); err != nil {
			return acct, err
		}
	}
	if req.IncomeCategoryID != nil {
		if err := exists(func() (bool, error) {
			return q.IncomeCategoryInGroup(ctx, store.IncomeCategoryInGroupParams{GroupID: groupID, ID: *req.IncomeCategoryID})
		}, "invalid_income_category", "That income category is not in this group."); err != nil {
			return acct, err
		}
	}
	if req.DepositorID != nil {
		if err := exists(func() (bool, error) {
			return q.DepositorInGroup(ctx, store.DepositorInGroupParams{GroupID: groupID, ID: *req.DepositorID})
		}, "invalid_depositor", "That depositor is not in this group."); err != nil {
			return acct, err
		}
	}
	return acct, nil
}

// checkAmountDate validates the fields every movement of money has.
func checkAmountDate(amount *Amount, date *Date, method *string, noun string) error {
	if amount == nil || cents(string(*amount)) <= 0 {
		return badRequest{"invalid_amount", "amount must be more than zero."}
	}
	if date == nil {
		return badRequest{"missing_fields", "date is required."}
	}
	y, m, d := time.Now().In(nairobi()).Date()
	if date.After(time.Date(y, m, d, 0, 0, 0, 0, time.UTC)) {
		return badRequest{"invalid_date", "A " + noun + " cannot be dated in the future."}
	}
	if method != nil && !store.PaymentMethod(*method).Valid() {
		return badRequest{"invalid_method", "Unknown payment method."}
	}
	return nil
}

// openAccount loads one of the group's accounts that can take new entries.
func openAccount(ctx context.Context, q *store.Queries, groupID, id int64) (store.GroupAccount, error) {
	acct, err := q.GetGroupAccount(ctx, store.GetGroupAccountParams{GroupID: groupID, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return acct, badRequest{"invalid_account", "Pick one of the group's accounts."}
	}
	if err != nil {
		return acct, err
	}
	if acct.IsClosed {
		return acct, badRequest{"account_closed", "That account is closed."}
	}
	return acct, nil
}

func nairobi() *time.Location {
	loc, err := time.LoadLocation("Africa/Nairobi")
	if err != nil {
		return time.FixedZone("EAT", 3*3600)
	}
	return loc
}

func (s *Server) createDeposit(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	var req depositRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Description = strings.TrimSpace(req.Description)
	userID := claimsFrom(r.Context()).UserID
	var id int64
	err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		acct, err := s.checkDeposit(r, q, a.Group.ID, &req)
		if err != nil {
			return err
		}
		kind := store.DepositKind(req.Kind)
		p := store.CreateDepositParams{
			GroupID: a.Group.ID, AccountID: &acct.ID, AccountKind: acct.Kind, Kind: kind,
			MembershipID: req.MemberID, ContributionID: req.ContributionID, FineCategoryID: req.FineCategoryID,
			IncomeCategoryID: req.IncomeCategoryID, DepositorID: req.DepositorID, Amount: req.Amount.Numeric(),
			DepositDate: req.Date.Time, Description: req.Description, CreatedBy: &userID,
		}
		if req.Method != nil {
			p.Method = store.NullPaymentMethod{PaymentMethod: store.PaymentMethod(*req.Method), Valid: true}
		}
		if id, err = q.CreateDeposit(r.Context(), p); err != nil {
			return err
		}
		if err := q.CreateAccountEntry(r.Context(), store.CreateAccountEntryParams{
			GroupID: a.Group.ID, AccountID: &acct.ID, AccountKind: acct.Kind, Direction: 1, Amount: p.Amount,
			EntryDate: p.DepositDate, Description: p.Description, DepositID: &id, MembershipID: req.MemberID,
		}); err != nil {
			return err
		}
		if entry := recordableDeposits[kind]; entry != "" {
			return q.CreatePaymentLedgerEntry(r.Context(), store.CreatePaymentLedgerEntryParams{
				GroupID: a.Group.ID, MembershipID: req.MemberID, ContributionID: req.ContributionID,
				FineCategoryID: req.FineCategoryID, EntryType: entry, Amount: p.Amount, EntryDate: p.DepositDate,
				Description: p.Description, DepositID: &id,
			})
		}
		return nil
	})
	var bad badRequest
	if errors.As(err, &bad) {
		writeError(w, http.StatusBadRequest, bad.code, bad.msg)
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
	var found bool
	err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		if _, err := q.GetDeposit(r.Context(), store.GetDepositParams{GroupID: a.Group.ID, ID: id}); err != nil {
			return err
		}
		found = true
		n, err := q.VoidDeposit(r.Context(), store.VoidDepositParams{GroupID: a.Group.ID, ID: id, VoidedBy: &userID})
		if err != nil {
			return err
		}
		if n == 0 {
			return badRequest{"already_voided", "This deposit is already voided."}
		}
		if err := q.VoidDepositEntries(r.Context(), &id); err != nil {
			return err
		}
		return q.VoidDepositLedger(r.Context(), &id)
	})
	var bad badRequest
	switch {
	case !found && errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "deposit_not_found", "Deposit not found.")
	case errors.As(err, &bad):
		writeError(w, http.StatusConflict, bad.code, bad.msg)
	case err != nil:
		writeInternal(w, r, err)
	default:
		s.writeDeposit(w, r, a.Group.ID, id, http.StatusOK)
	}
}
