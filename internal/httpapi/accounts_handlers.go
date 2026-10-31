package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/swahome/legion-api/internal/store"
)

// Group accounts (bank, sacco, mobile money, petty cash) and the cash book.

type namedRef struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type accountResponse struct {
	ID             int64     `json:"id"`
	Kind           string    `json:"kind"`
	Name           string    `json:"name"`
	AccountNumber  string    `json:"account_number"`
	Institution    *namedRef `json:"institution"`
	Branch         *namedRef `json:"branch"`
	InitialBalance string    `json:"initial_balance"`
	Balance        string    `json:"balance"`
	IsDefault      bool      `json:"is_default"`
	IsVerified     bool      `json:"is_verified"`
	IsClosed       bool      `json:"is_closed"`
}

type unassignedResponse struct {
	Kind    string `json:"kind"`
	Balance string `json:"balance"`
	Entries int64  `json:"entries"`
}

func firstID(ids ...*int64) *int64 {
	for _, id := range ids {
		if id != nil {
			return id
		}
	}
	return nil
}

func (s *Server) listAccounts(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	rows, err := s.q.ListGroupAccounts(r.Context(), a.Group.ID)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	var total int64
	out := make([]accountResponse, 0, len(rows))
	for _, ac := range rows {
		resp := accountResponse{
			ID: ac.ID, Kind: string(ac.Kind), Name: ac.Name, AccountNumber: ac.AccountNumber,
			InitialBalance: money(ac.InitialBalance), Balance: money(ac.Balance),
			IsDefault: ac.IsDefault, IsVerified: ac.IsVerified, IsClosed: ac.IsClosed,
		}
		if id := firstID(ac.BankID, ac.SaccoID, ac.ProviderID); id != nil {
			resp.Institution = &namedRef{ID: *id, Name: ac.InstitutionName}
		}
		if id := firstID(ac.BankBranchID, ac.SaccoBranchID); id != nil {
			resp.Branch = &namedRef{ID: *id, Name: ac.BranchName}
		}
		total += cents(ac.Balance)
		out = append(out, resp)
	}
	un, err := s.q.UnassignedBalances(r.Context(), a.Group.ID)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	unassigned := make([]unassignedResponse, 0, len(un))
	for _, u := range un {
		total += cents(u.Balance)
		unassigned = append(unassigned, unassignedResponse{Kind: string(u.AccountKind), Balance: money(u.Balance), Entries: u.Entries})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts": out, "unassigned": unassigned, "total_balance": formatCents(total),
	})
}

type accountRequest struct {
	Kind           *string         `json:"kind"`
	Name           *string         `json:"name"`
	AccountNumber  *string         `json:"account_number"`
	InstitutionID  *int64          `json:"institution_id"`
	BranchID       Optional[int64] `json:"branch_id"`
	InitialBalance *Amount         `json:"initial_balance"`
	IsDefault      *bool           `json:"is_default"`
	IsClosed       *bool           `json:"is_closed"`
}

// checkBranch verifies a branch belongs to the account's bank or sacco.
func (s *Server) checkBranch(r *http.Request, kind store.AccountKind, institution, branch *int64) (bool, error) {
	if branch == nil {
		return true, nil
	}
	if institution == nil {
		return false, nil
	}
	switch kind {
	case store.AccountKindBank:
		return s.q.BankBranchBelongs(r.Context(), store.BankBranchBelongsParams{ID: *branch, BankID: *institution})
	case store.AccountKindSacco:
		return s.q.SaccoBranchBelongs(r.Context(), store.SaccoBranchBelongsParams{ID: *branch, SaccoID: *institution})
	}
	return false, nil
}

func (s *Server) createAccount(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	var req accountRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Kind == nil || !store.AccountKind(*req.Kind).Valid() {
		writeError(w, http.StatusBadRequest, "invalid_kind", "kind must be bank, sacco, mobile_money or petty_cash.")
		return
	}
	kind := store.AccountKind(*req.Kind)
	name := strings.TrimSpace(deref(req.Name))
	if name == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "Name is required.")
		return
	}
	if req.IsClosed != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "A new account cannot be closed.")
		return
	}
	p := store.CreateGroupAccountParams{
		GroupID: a.Group.ID, Kind: kind, Name: name, AccountNumber: strings.TrimSpace(deref(req.AccountNumber)),
		IsDefault: req.IsDefault != nil && *req.IsDefault, InitialBalance: Amount("0").Numeric(),
	}
	if req.InitialBalance != nil {
		p.InitialBalance = req.InitialBalance.Numeric()
	}
	if kind == store.AccountKindPettyCash {
		if req.InstitutionID != nil || req.BranchID.Value != nil {
			writeError(w, http.StatusBadRequest, "invalid_institution", "Petty cash accounts have no bank, sacco or provider.")
			return
		}
	} else {
		if req.InstitutionID == nil {
			writeError(w, http.StatusBadRequest, "missing_fields", "institution_id (the bank, sacco or mobile money provider) is required.")
			return
		}
		found, err := s.q.InstitutionExists(r.Context(), store.InstitutionExistsParams{Kind: kind, InstitutionID: *req.InstitutionID})
		if err != nil {
			writeInternal(w, r, err)
			return
		}
		if found == nil || !*found {
			writeError(w, http.StatusBadRequest, "invalid_institution", "Unknown bank, sacco or provider.")
			return
		}
		ok, err := s.checkBranch(r, kind, req.InstitutionID, req.BranchID.Value)
		if err != nil {
			writeInternal(w, r, err)
			return
		}
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid_branch", "That branch does not belong to the bank or sacco.")
			return
		}
		switch kind {
		case store.AccountKindBank:
			p.BankID, p.BankBranchID = req.InstitutionID, req.BranchID.Value
		case store.AccountKindSacco:
			p.SaccoID, p.SaccoBranchID = req.InstitutionID, req.BranchID.Value
		case store.AccountKindMobileMoney:
			if req.BranchID.Value != nil {
				writeError(w, http.StatusBadRequest, "invalid_branch", "Mobile money accounts have no branch.")
				return
			}
			p.ProviderID = req.InstitutionID
		}
	}
	var id int64
	err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		var err error
		if id, err = q.CreateGroupAccount(r.Context(), p); err != nil {
			return err
		}
		if p.IsDefault {
			return q.ClearDefaultAccounts(r.Context(), store.ClearDefaultAccountsParams{GroupID: a.Group.ID, Kind: kind, ID: id})
		}
		return nil
	})
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	s.writeAccount(w, r, a.Group.ID, id, http.StatusCreated)
}

func (s *Server) writeAccount(w http.ResponseWriter, r *http.Request, groupID, id int64, status int) {
	rows, err := s.q.ListGroupAccounts(r.Context(), groupID)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	for _, ac := range rows {
		if ac.ID != id {
			continue
		}
		resp := accountResponse{
			ID: ac.ID, Kind: string(ac.Kind), Name: ac.Name, AccountNumber: ac.AccountNumber,
			InitialBalance: money(ac.InitialBalance), Balance: money(ac.Balance),
			IsDefault: ac.IsDefault, IsVerified: ac.IsVerified, IsClosed: ac.IsClosed,
		}
		if iid := firstID(ac.BankID, ac.SaccoID, ac.ProviderID); iid != nil {
			resp.Institution = &namedRef{ID: *iid, Name: ac.InstitutionName}
		}
		if bid := firstID(ac.BankBranchID, ac.SaccoBranchID); bid != nil {
			resp.Branch = &namedRef{ID: *bid, Name: ac.BranchName}
		}
		writeJSON(w, status, resp)
		return
	}
	writeError(w, http.StatusNotFound, "account_not_found", "Account not found.")
}

func (s *Server) updateAccount(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	id, ok := pathID(r, "accountID")
	if !ok {
		writeError(w, http.StatusNotFound, "account_not_found", "Account not found.")
		return
	}
	var req accountRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Kind != nil || req.InstitutionID != nil {
		writeError(w, http.StatusBadRequest, "immutable", "An account's kind and institution cannot be changed; open a new account instead.")
		return
	}
	acct, err := s.q.GetGroupAccount(r.Context(), store.GetGroupAccountParams{GroupID: a.Group.ID, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "account_not_found", "Account not found.")
		return
	}
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	if req.Name != nil {
		n := strings.TrimSpace(*req.Name)
		if n == "" {
			writeError(w, http.StatusBadRequest, "invalid_name", "Name cannot be blank.")
			return
		}
		req.Name = &n
	}
	p := store.UpdateGroupAccountParams{
		GroupID: a.Group.ID, ID: id, Name: req.Name, AccountNumber: trimmedPtr(req.AccountNumber),
		IsDefault: req.IsDefault, IsClosed: req.IsClosed,
	}
	if req.InitialBalance != nil {
		p.InitialBalance = req.InitialBalance.Numeric()
	}
	if req.BranchID.Set {
		ok, err := s.checkBranch(r, acct.Kind, firstID(acct.BankID, acct.SaccoID), req.BranchID.Value)
		if err != nil {
			writeInternal(w, r, err)
			return
		}
		if !ok || (req.BranchID.Value != nil && acct.Kind != store.AccountKindBank && acct.Kind != store.AccountKindSacco) {
			writeError(w, http.StatusBadRequest, "invalid_branch", "That branch does not belong to the account's bank or sacco.")
			return
		}
		p.SetBranch, p.BranchID = true, req.BranchID.Value
	}
	if req.IsClosed != nil && *req.IsClosed {
		p.IsDefault = ptr(false)
	}
	err = pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		if _, err := q.UpdateGroupAccount(r.Context(), p); err != nil {
			return err
		}
		if p.IsDefault != nil && *p.IsDefault {
			return q.ClearDefaultAccounts(r.Context(), store.ClearDefaultAccountsParams{GroupID: a.Group.ID, Kind: acct.Kind, ID: id})
		}
		return nil
	})
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	s.writeAccount(w, r, a.Group.ID, id, http.StatusOK)
}

// deleteAccount removes an account with no transactions, as the legacy
// app does; one with history must be closed instead.
func (s *Server) deleteAccount(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	id, ok := pathID(r, "accountID")
	if !ok {
		writeError(w, http.StatusNotFound, "account_not_found", "Account not found.")
		return
	}
	if _, err := s.q.GetGroupAccount(r.Context(), store.GetGroupAccountParams{GroupID: a.Group.ID, ID: id}); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "account_not_found", "Account not found.")
		return
	} else if err != nil {
		writeInternal(w, r, err)
		return
	}
	n, err := s.q.CountAccountEntries(r.Context(), &id)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	if n > 0 {
		writeError(w, http.StatusConflict, "account_has_transactions", "This account has transactions. Close it instead of deleting it.")
		return
	}
	if err := s.q.DeleteGroupAccount(r.Context(), store.DeleteGroupAccountParams{GroupID: a.Group.ID, ID: id}); err != nil {
		writeInternal(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func trimmedPtr(p *string) *string {
	if p == nil {
		return nil
	}
	v := strings.TrimSpace(*p)
	return &v
}

type cashBookEntry struct {
	ID           int64     `json:"id"`
	Date         Date      `json:"date"`
	Type         string    `json:"type"`
	Description  string    `json:"description"`
	Account      *namedRef `json:"account"`
	AccountKind  string    `json:"account_kind"`
	MoneyIn      *string   `json:"money_in"`
	MoneyOut     *string   `json:"money_out"`
	Balance      string    `json:"balance"`
	DepositID    *int64    `json:"deposit_id,omitempty"`
	WithdrawalID *int64    `json:"withdrawal_id,omitempty"`
	TransferID   *int64    `json:"account_transfer_id,omitempty"`
	Member       *namedRef `json:"member"`
}

const cashBookLimit = 5000

// dateRange reads ?from and ?to (YYYY-MM-DD). Defaults: the last 12 months.
func dateRange(r *http.Request) (from, to time.Time, ok bool) {
	now := time.Now().UTC()
	to = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	from = to.AddDate(-1, 0, 0)
	for key, dst := range map[string]*time.Time{"from": &from, "to": &to} {
		if v := r.URL.Query().Get(key); v != "" {
			t, err := time.Parse(dateLayout, v)
			if err != nil {
				return from, to, false
			}
			*dst = t
		}
	}
	return from, to, !from.After(to)
}

// cashBook lists money in and out of the group's accounts with a running
// balance. ?account_id narrows it to one account's statement.
func (s *Server) cashBook(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	from, to, ok := dateRange(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_dates", "from and to must look like 2026-11-05, with from not after to.")
		return
	}
	var accountID *int64
	if v := r.URL.Query().Get("account_id"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			writeError(w, http.StatusBadRequest, "invalid_account", "account_id must be an id.")
			return
		}
		if _, err := s.q.GetGroupAccount(r.Context(), store.GetGroupAccountParams{GroupID: a.Group.ID, ID: id}); errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "account_not_found", "Account not found.")
			return
		} else if err != nil {
			writeInternal(w, r, err)
			return
		}
		accountID = &id
	}
	initial, err := s.q.InitialBalances(r.Context(), store.InitialBalancesParams{GroupID: a.Group.ID, AccountID: accountID})
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	before, err := s.q.CashBookOpening(r.Context(), store.CashBookOpeningParams{GroupID: a.Group.ID, AccountID: accountID, Before: from})
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	rows, err := s.q.CashBook(r.Context(), store.CashBookParams{GroupID: a.Group.ID, AccountID: accountID, FromDate: from, ToDate: to})
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	truncated := len(rows) > cashBookLimit
	if truncated {
		rows = rows[:cashBookLimit]
	}
	opening := cents(initial) + cents(before)
	bal := opening
	var in, out int64
	entries := make([]cashBookEntry, 0, len(rows))
	for _, e := range rows {
		amt := cents(e.Amount)
		ce := cashBookEntry{
			ID: e.ID, Date: Date{e.EntryDate}, Type: entryTypeName(e), Description: e.Description,
			AccountKind: string(e.AccountKind), DepositID: e.DepositID, WithdrawalID: e.WithdrawalID,
			TransferID: e.AccountTransferID,
		}
		if e.AccountID != nil {
			ce.Account = &namedRef{ID: *e.AccountID, Name: deref(e.AccountName)}
		}
		if e.MembershipID != nil {
			ce.Member = &namedRef{ID: *e.MembershipID, Name: e.MemberName}
		}
		v := formatCents(amt)
		if e.Direction > 0 {
			bal, in = bal+amt, in+amt
			ce.MoneyIn = &v
		} else {
			bal, out = bal-amt, out+amt
			ce.MoneyOut = &v
		}
		ce.Balance = formatCents(bal)
		entries = append(entries, ce)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"from": Date{from}, "to": Date{to}, "account_id": accountID,
		"opening_balance": formatCents(opening), "closing_balance": formatCents(bal),
		"total_in": formatCents(in), "total_out": formatCents(out),
		"entries": entries, "truncated": truncated,
	})
}

// entryTypeName says what a cash book row is, as a stable machine name.
func entryTypeName(e store.CashBookRow) string {
	switch {
	case e.DepositKind.Valid:
		return "deposit." + string(e.DepositKind.DepositKind)
	case e.WithdrawalKind.Valid:
		return "withdrawal." + string(e.WithdrawalKind.WithdrawalKind)
	case e.AccountTransferID != nil && e.Direction > 0:
		return "transfer.in"
	case e.AccountTransferID != nil:
		return "transfer.out"
	case e.Direction > 0:
		return "deposit"
	default:
		return "withdrawal"
	}
}
