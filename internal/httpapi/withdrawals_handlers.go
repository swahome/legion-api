package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

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

// Kinds that can be recorded by hand so far. Loan disbursements come with
// loans (milestone 4); asset, stock and money market purchases, bank loan
// repayments and dividends with the investments milestone. Funds transfers
// are account transfers.
var recordableWithdrawals = map[store.WithdrawalKind]bool{
	store.WithdrawalKindExpense:            true,
	store.WithdrawalKindContributionRefund: true,
}

// found turns an existence check into a 400 when the row is missing:
// found(code, msg)(q.SomethingInGroup(ctx, ...)).
func found(code, msg string) func(bool, error) error {
	return func(ok bool, err error) error {
		if err != nil {
			return err
		}
		if !ok {
			return badRequest{code, msg}
		}
		return nil
	}
}

// moneyOut is what a withdrawal pays and where it goes, shared by recorded
// withdrawals and withdrawal requests.
type moneyOut struct {
	kind              store.WithdrawalKind
	amount            Amount
	memberID          *int64
	contributionID    *int64
	expenseCategoryID *int64
}

// checkMoneyOut validates the references a withdrawal makes. It runs
// inside the write transaction.
func checkMoneyOut(ctx context.Context, q *store.Queries, groupID int64, m moneyOut) error {
	switch m.kind {
	case store.WithdrawalKindExpense:
		if m.expenseCategoryID == nil {
			return badRequest{"missing_fields", "expense_category_id is required for an expense."}
		}
		if m.memberID != nil || m.contributionID != nil {
			return badRequest{"invalid_body", "An expense is not linked to a member or contribution."}
		}
		return found("invalid_expense_category", "That expense category is not in this group.")(
			q.ExpenseCategoryInGroup(ctx, store.ExpenseCategoryInGroupParams{GroupID: groupID, ID: *m.expenseCategoryID}))
	case store.WithdrawalKindContributionRefund:
		if m.memberID == nil || m.contributionID == nil {
			return badRequest{"missing_fields", "member_id and contribution_id are required for a refund."}
		}
		if m.expenseCategoryID != nil {
			return badRequest{"invalid_body", "A refund has no expense category."}
		}
		if err := found("invalid_member", "That member is not in this group.")(
			q.ActiveMembershipInGroup(ctx, store.ActiveMembershipInGroupParams{GroupID: groupID, ID: *m.memberID})); err != nil {
			return err
		}
		if err := found("invalid_contribution", "That contribution is not in this group.")(
			q.ContributionInGroup(ctx, store.ContributionInGroupParams{GroupID: groupID, ID: *m.contributionID})); err != nil {
			return err
		}
		paid, err := q.ContributionPaidByMember(ctx, store.ContributionPaidByMemberParams{
			GroupID: groupID, MembershipID: m.memberID, ContributionID: m.contributionID,
		})
		if err != nil {
			return err
		}
		if cents(string(m.amount)) > cents(paid) {
			return badRequest{"refund_too_large", "The member has only paid " + money(paid) + " into this contribution."}
		}
		return nil
	}
	return badRequest{"invalid_kind", "kind must be expense or contribution_refund."}
}

// checkBalance refuses to take an account below zero.
func checkBalance(ctx context.Context, q *store.Queries, acct store.GroupAccount, amount Amount) error {
	bal, err := q.AccountBalance(ctx, store.AccountBalanceParams{GroupID: acct.GroupID, ID: acct.ID})
	if err != nil {
		return err
	}
	if cents(string(amount)) > cents(bal) {
		return badRequest{"insufficient_balance", acct.Name + " has only " + money(bal) + "."}
	}
	return nil
}

// recordWithdrawal writes a withdrawal, its cash book entry and, for a
// refund, the member's ledger entry. It runs inside the caller's transaction.
func recordWithdrawal(ctx context.Context, q *store.Queries, acct store.GroupAccount, m moneyOut,
	method *string, date time.Time, description string, userID int64) (int64, error) {
	p := store.CreateWithdrawalParams{
		GroupID: acct.GroupID, AccountID: &acct.ID, AccountKind: acct.Kind, Kind: m.kind,
		MembershipID: m.memberID, ContributionID: m.contributionID, ExpenseCategoryID: m.expenseCategoryID,
		Amount: m.amount.Numeric(), WithdrawalDate: date, Description: description, CreatedBy: &userID,
	}
	if method != nil {
		p.Method = store.NullPaymentMethod{PaymentMethod: store.PaymentMethod(*method), Valid: true}
	}
	id, err := q.CreateWithdrawal(ctx, p)
	if err != nil {
		return 0, err
	}
	if err := q.CreateAccountEntry(ctx, store.CreateAccountEntryParams{
		GroupID: acct.GroupID, AccountID: &acct.ID, AccountKind: acct.Kind, Direction: -1, Amount: p.Amount,
		EntryDate: date, Description: description, WithdrawalID: &id, MembershipID: m.memberID,
	}); err != nil {
		return 0, err
	}
	if m.kind == store.WithdrawalKindContributionRefund {
		if err := q.CreateRefundLedgerEntry(ctx, store.CreateRefundLedgerEntryParams{
			GroupID: acct.GroupID, MembershipID: m.memberID, ContributionID: m.contributionID,
			Amount: p.Amount, EntryDate: date, Description: description, WithdrawalID: &id,
		}); err != nil {
			return 0, err
		}
	}
	return id, nil
}

func (s *Server) createWithdrawal(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	var req withdrawalRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Description = strings.TrimSpace(req.Description)
	userID := claimsFrom(r.Context()).UserID
	var id int64
	err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		ctx, q := r.Context(), s.q.WithTx(tx)
		kind := store.WithdrawalKind(req.Kind)
		if !recordableWithdrawals[kind] {
			return badRequest{"invalid_kind", "kind must be expense or contribution_refund."}
		}
		if err := checkAmountDate(req.Amount, req.Date, req.Method, "withdrawal"); err != nil {
			return err
		}
		acct, err := openAccount(ctx, q, a.Group.ID, req.AccountID)
		if err != nil {
			return err
		}
		m := moneyOut{kind: kind, amount: *req.Amount, memberID: req.MemberID,
			contributionID: req.ContributionID, expenseCategoryID: req.ExpenseCategoryID}
		if err := checkMoneyOut(ctx, q, a.Group.ID, m); err != nil {
			return err
		}
		id, err = recordWithdrawal(ctx, q, acct, m, req.Method, req.Date.Time, req.Description, userID)
		return err
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
	var exists bool
	err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		if _, err := q.GetWithdrawal(r.Context(), store.GetWithdrawalParams{GroupID: a.Group.ID, ID: id}); err != nil {
			return err
		}
		exists = true
		n, err := q.VoidWithdrawal(r.Context(), store.VoidWithdrawalParams{GroupID: a.Group.ID, ID: id, VoidedBy: &userID})
		if err != nil {
			return err
		}
		if n == 0 {
			return badRequest{"already_voided", "This withdrawal is already voided."}
		}
		if err := q.VoidWithdrawalEntries(r.Context(), &id); err != nil {
			return err
		}
		return q.VoidWithdrawalLedger(r.Context(), &id)
	})
	var bad badRequest
	switch {
	case !exists && errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "withdrawal_not_found", "Withdrawal not found.")
	case errors.As(err, &bad):
		writeError(w, http.StatusConflict, bad.code, bad.msg)
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

// recordTransfer writes a transfer and its two cash book entries. It runs
// inside the caller's transaction.
func recordTransfer(ctx context.Context, q *store.Queries, from, to store.GroupAccount, amount Amount,
	date time.Time, description string, userID int64) (int64, error) {
	n := amount.Numeric()
	id, err := q.CreateAccountTransfer(ctx, store.CreateAccountTransferParams{
		GroupID: from.GroupID, FromAccountID: &from.ID, ToAccountID: &to.ID, Amount: n,
		TransferDate: date, Description: description, CreatedBy: &userID,
	})
	if err != nil {
		return 0, err
	}
	for _, leg := range []struct {
		acct store.GroupAccount
		dir  int16
	}{{from, -1}, {to, 1}} {
		if err := q.CreateAccountEntry(ctx, store.CreateAccountEntryParams{
			GroupID: from.GroupID, AccountID: &leg.acct.ID, AccountKind: leg.acct.Kind, Direction: leg.dir,
			Amount: n, EntryDate: date, Description: description, AccountTransferID: &id,
		}); err != nil {
			return 0, err
		}
	}
	return id, nil
}

// transferAccounts checks both sides of a transfer.
func transferAccounts(ctx context.Context, q *store.Queries, groupID, fromID, toID int64) (from, to store.GroupAccount, err error) {
	if fromID == toID {
		return from, to, badRequest{"same_account", "Pick two different accounts."}
	}
	if from, err = openAccount(ctx, q, groupID, fromID); err != nil {
		return from, to, err
	}
	to, err = openAccount(ctx, q, groupID, toID)
	return from, to, err
}

func (s *Server) createTransfer(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	var req transferRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Description = strings.TrimSpace(req.Description)
	userID := claimsFrom(r.Context()).UserID
	var id int64
	err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		ctx, q := r.Context(), s.q.WithTx(tx)
		if err := checkAmountDate(req.Amount, req.Date, nil, "transfer"); err != nil {
			return err
		}
		from, to, err := transferAccounts(ctx, q, a.Group.ID, req.FromAccountID, req.ToAccountID)
		if err != nil {
			return err
		}
		id, err = recordTransfer(ctx, q, from, to, *req.Amount, req.Date.Time, req.Description, userID)
		return err
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
	var exists bool
	err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		ok, err := q.AccountTransferExists(r.Context(), store.AccountTransferExistsParams{GroupID: a.Group.ID, ID: id})
		if err != nil || !ok {
			return err
		}
		exists = true
		n, err := q.VoidAccountTransfer(r.Context(), store.VoidAccountTransferParams{GroupID: a.Group.ID, ID: id, VoidedBy: &userID})
		if err != nil {
			return err
		}
		if n == 0 {
			return badRequest{"already_voided", "This transfer is already voided."}
		}
		return q.VoidAccountTransferEntries(r.Context(), &id)
	})
	var bad badRequest
	switch {
	case errors.As(err, &bad):
		writeError(w, http.StatusConflict, bad.code, bad.msg)
	case err != nil:
		writeInternal(w, r, err)
	case !exists:
		writeError(w, http.StatusNotFound, "transfer_not_found", "Transfer not found.")
	default:
		s.writeTransfer(w, r, a.Group.ID, id, http.StatusOK)
	}
}
