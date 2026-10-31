package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/swahome/legion-api/internal/auth"
	"github.com/swahome/legion-api/internal/books"
	"github.com/swahome/legion-api/internal/store"
)

// Withdrawal requests (legacy e-wallet "withdraw money"). An official asks
// to pay money out of an account. Every signatory, an active member who
// holds a group role (only officials among them when the group sets
// restrict_wallet_approval_to_group_admins), gets an approval to answer
// with their password. The requester's own approval counts at once. One
// decline declines the request; when everyone has approved it is approved.
// Paying an approved request records the withdrawal or transfer. Paying is
// an official's step for now; the payment API will take it over.

type withdrawalRequestResponse struct {
	ID              int64           `json:"id"`
	ReferenceNumber string          `json:"reference_number"`
	Kind            string          `json:"kind"`
	Status          string          `json:"status"`
	Amount          string          `json:"amount"`
	Description     string          `json:"description"`
	Account         *namedRef       `json:"account"`
	ToAccount       *namedRef       `json:"to_account"`
	ExpenseCategory *namedRef       `json:"expense_category"`
	Member          *namedRef       `json:"member"`
	Contribution    *namedRef       `json:"contribution"`
	RequestedBy     *namedRef       `json:"requested_by"`
	Recipient       json.RawMessage `json:"recipient"`
	Approvals       int64           `json:"approvals"`
	Signatories     int64           `json:"signatories"`
	DecidedAt       *time.Time      `json:"decided_at"`
	DeclineReason   string          `json:"decline_reason"`
	WithdrawalID    *int64          `json:"withdrawal_id"`
	TransferID      *int64          `json:"transfer_id"`
	CreatedAt       time.Time       `json:"created_at"`
}

func toWithdrawalRequest(x store.ListWithdrawalRequestsRow) withdrawalRequestResponse {
	out := withdrawalRequestResponse{
		ID: x.ID, ReferenceNumber: x.ReferenceNumber, Kind: string(x.Kind), Status: string(x.Status),
		Amount: money(x.Amount), Description: x.Description,
		Account: ref(x.AccountID, x.AccountName), ToAccount: ref(x.ToAccountID, x.ToAccountName),
		ExpenseCategory: ref(x.ExpenseCategoryID, x.ExpenseCategoryName), Contribution: ref(x.ContributionID, x.ContributionName),
		Recipient: x.Recipient, Approvals: x.Approvals, Signatories: x.Signatories,
		DecidedAt: x.DecidedAt, DeclineReason: x.DeclineReason, WithdrawalID: x.WithdrawalID, TransferID: x.AccountTransferID,
		CreatedAt: x.CreatedAt,
	}
	if len(out.Recipient) == 0 {
		out.Recipient = json.RawMessage("{}")
	}
	if x.MembershipID != nil {
		out.Member = &namedRef{ID: *x.MembershipID, Name: x.MemberName}
	}
	if x.RequestedBy != nil {
		out.RequestedBy = &namedRef{ID: *x.RequestedBy, Name: x.RequestedByName}
	}
	return out
}

type approvalResponse struct {
	MemberID  int64      `json:"member_id"`
	Name      string     `json:"name"`
	Role      *string    `json:"role"`
	Decision  string     `json:"decision"`
	DecidedAt *time.Time `json:"decided_at"`
	Comment   string     `json:"comment"`
}

// canSeeRequests: officials and signatories.
func (s *Server) canSeeRequests(ctx context.Context, a groupAccess) (bool, error) {
	if a.IsOfficial {
		return true, nil
	}
	if a.MembershipID == nil {
		return false, nil
	}
	return s.q.IsSignatory(ctx, store.IsSignatoryParams{GroupID: a.Group.ID, ID: *a.MembershipID})
}

func (s *Server) requestsAllowed(w http.ResponseWriter, r *http.Request) bool {
	ok, err := s.canSeeRequests(r.Context(), accessFrom(r.Context()))
	if err != nil {
		writeInternal(w, r, err)
		return false
	}
	if !ok {
		writeError(w, http.StatusForbidden, "signatories_only", "Only officials and signatories can see withdrawal requests.")
	}
	return ok
}

// listWithdrawalRequests pages through requests, newest first. Filters:
// status, awaiting=me (requests waiting on the caller's answer).
func (s *Server) listWithdrawalRequests(w http.ResponseWriter, r *http.Request) {
	if !s.requestsAllowed(w, r) {
		return
	}
	a := accessFrom(r.Context())
	pg := pageParams(r)
	p := store.ListWithdrawalRequestsParams{GroupID: a.Group.ID, PageSize: pg.size, PageOffset: pg.offset}
	if st := r.URL.Query().Get("status"); st != "" {
		if !store.WithdrawalRequestStatus(st).Valid() {
			writeError(w, http.StatusBadRequest, "invalid_status", "Unknown request status.")
			return
		}
		p.Status = store.NullWithdrawalRequestStatus{WithdrawalRequestStatus: store.WithdrawalRequestStatus(st), Valid: true}
	}
	if aw := r.URL.Query().Get("awaiting"); aw != "" {
		if aw != "me" {
			writeError(w, http.StatusBadRequest, "invalid_filter", "awaiting can only be me.")
			return
		}
		none := int64(0)
		p.Awaiting = &none
		if a.MembershipID != nil {
			p.Awaiting = a.MembershipID
		}
	}
	rows, err := s.q.ListWithdrawalRequests(r.Context(), p)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	items := make([]withdrawalRequestResponse, 0, len(rows))
	var total int64
	for _, x := range rows {
		items = append(items, toWithdrawalRequest(x))
		total = x.Total
	}
	writeJSON(w, http.StatusOK, map[string]any{"requests": newPaged(items, total, pg)})
}

func (s *Server) getWithdrawalRequest(w http.ResponseWriter, r *http.Request) {
	if !s.requestsAllowed(w, r) {
		return
	}
	id, ok := pathID(r, "requestID")
	if !ok {
		writeError(w, http.StatusNotFound, "request_not_found", "Withdrawal request not found.")
		return
	}
	s.writeWithdrawalRequest(w, r, accessFrom(r.Context()).Group.ID, id, http.StatusOK)
}

func (s *Server) writeWithdrawalRequest(w http.ResponseWriter, r *http.Request, groupID, id int64, status int) {
	x, err := s.q.GetWithdrawalRequestView(r.Context(), store.GetWithdrawalRequestViewParams{GroupID: groupID, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "request_not_found", "Withdrawal request not found.")
		return
	}
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	approvals, err := s.q.ListWithdrawalApprovals(r.Context(), id)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	out := struct {
		withdrawalRequestResponse
		ApprovalList []approvalResponse `json:"approval_list"`
		MyDecision   *string            `json:"my_decision"`
	}{withdrawalRequestResponse: toWithdrawalRequest(store.ListWithdrawalRequestsRow(x)), ApprovalList: []approvalResponse{}}
	me := accessFrom(r.Context()).MembershipID
	for _, ap := range approvals {
		out.ApprovalList = append(out.ApprovalList, approvalResponse{
			MemberID: ap.MembershipID, Name: ap.MemberName, Role: ap.RoleName,
			Decision: string(ap.Decision), DecidedAt: ap.DecidedAt, Comment: ap.Comment,
		})
		if me != nil && *me == ap.MembershipID {
			d := string(ap.Decision)
			out.MyDecision = &d
		}
	}
	writeJSON(w, status, out)
}

// recipient says where the money is paid: a phone number, a paybill or a
// bank account. It is kept for the payment API and shown to signatories.
type recipient struct {
	Type          string `json:"type"`
	Phone         string `json:"phone,omitempty"`
	Paybill       string `json:"paybill,omitempty"`
	AccountNumber string `json:"account_number,omitempty"`
	AccountName   string `json:"account_name,omitempty"`
	BankID        int64  `json:"bank_id,omitempty"`
}

func (rc *recipient) check() error {
	rc.Phone, rc.Paybill = strings.TrimSpace(rc.Phone), strings.TrimSpace(rc.Paybill)
	rc.AccountNumber, rc.AccountName = strings.TrimSpace(rc.AccountNumber), strings.TrimSpace(rc.AccountName)
	switch rc.Type {
	case "mobile":
		if rc.Phone == "" {
			return badRequest{Code: "invalid_recipient", Msg: "A mobile money recipient needs a phone number."}
		}
	case "paybill":
		if rc.Paybill == "" {
			return badRequest{Code: "invalid_recipient", Msg: "A paybill recipient needs the paybill number."}
		}
	case "bank":
		if rc.BankID == 0 || rc.AccountNumber == "" || rc.AccountName == "" {
			return badRequest{Code: "invalid_recipient", Msg: "A bank recipient needs the bank, account number and account name."}
		}
	default:
		return badRequest{Code: "invalid_recipient", Msg: "recipient.type must be mobile, paybill or bank."}
	}
	return nil
}

type createWithdrawalRequestBody struct {
	Kind              string     `json:"kind"`
	AccountID         int64      `json:"account_id"`
	ToAccountID       *int64     `json:"to_account_id"`
	Amount            *Amount    `json:"amount"`
	Description       string     `json:"description"`
	ExpenseCategoryID *int64     `json:"expense_category_id"`
	MemberID          *int64     `json:"member_id"`
	ContributionID    *int64     `json:"contribution_id"`
	Recipient         *recipient `json:"recipient"`
}

const refAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

func referenceNumber() string {
	b := make([]byte, 10)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = refAlphabet[int(b[i])%len(refAlphabet)]
	}
	return string(b)
}

func (s *Server) createWithdrawalRequest(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	var req createWithdrawalRequestBody
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Description = strings.TrimSpace(req.Description)
	userID := claimsFrom(r.Context()).UserID
	var id int64
	err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		ctx, q := r.Context(), s.q.WithTx(tx)
		if req.Amount == nil || cents(string(*req.Amount)) <= 0 {
			return badRequest{Code: "invalid_amount", Msg: "amount must be more than zero."}
		}
		kind := store.WithdrawalKind(req.Kind)
		acct, err := openAccount(ctx, q, a.Group.ID, req.AccountID)
		if err != nil {
			return err
		}
		switch {
		case kind == store.WithdrawalKindFundsTransfer:
			if req.ToAccountID == nil {
				return badRequest{Code: "missing_fields", Msg: "to_account_id is required for a transfer."}
			}
			if req.ExpenseCategoryID != nil || req.MemberID != nil || req.ContributionID != nil || req.Recipient != nil {
				return badRequest{Code: "invalid_body", Msg: "A transfer only names the two accounts."}
			}
			if _, _, err := books.TransferAccounts(ctx, q, a.Group.ID, acct.ID, *req.ToAccountID); err != nil {
				return err
			}
		case books.RecordableWithdrawals[kind]:
			if req.ToAccountID != nil {
				return badRequest{Code: "invalid_body", Msg: "to_account_id is only for transfers."}
			}
			if err := books.CheckWithdrawal(ctx, q, a.Group.ID, books.Withdrawal{Kind: kind, Amount: string(*req.Amount), MemberID: req.MemberID,
				ContributionID: req.ContributionID, ExpenseCategoryID: req.ExpenseCategoryID}); err != nil {
				return err
			}
			if req.Recipient == nil {
				return badRequest{Code: "missing_fields", Msg: "recipient is required: who is being paid."}
			}
			if err := req.Recipient.check(); err != nil {
				return err
			}
		default:
			return badRequest{Code: "invalid_kind", Msg: "kind must be expense, contribution_refund or funds_transfer."}
		}
		if err := books.CheckBalance(ctx, q, acct, string(*req.Amount)); err != nil {
			return err
		}
		signatories, err := q.GroupSignatories(ctx, store.GroupSignatoriesParams{
			GroupID: a.Group.ID, OfficialsOnly: a.setting("restrict_wallet_approval_to_group_admins") == "1",
		})
		if err != nil {
			return err
		}
		if len(signatories) < 2 {
			return conflict{"not_enough_signatories", "The group needs at least two signatories (members with a group role) to approve withdrawals."}
		}
		rcp := []byte("{}")
		if req.Recipient != nil {
			rcp, _ = json.Marshal(req.Recipient)
		}
		id, err = q.CreateWithdrawalRequest(ctx, store.CreateWithdrawalRequestParams{
			GroupID: a.Group.ID, ReferenceNumber: referenceNumber(), Kind: kind, RequestedBy: a.MembershipID,
			AccountID: &acct.ID, ToAccountID: req.ToAccountID, Amount: req.Amount.Numeric(), Description: req.Description,
			ExpenseCategoryID: req.ExpenseCategoryID, MembershipID: req.MemberID, ContributionID: req.ContributionID,
			Recipient: rcp, CreatedBy: &userID,
		})
		if err != nil {
			return err
		}
		for _, m := range signatories {
			p := store.CreateWithdrawalApprovalParams{
				GroupID: a.Group.ID, WithdrawalRequestID: id, MembershipID: m, Decision: store.ApprovalDecisionPending,
			}
			if a.MembershipID != nil && *a.MembershipID == m {
				p.Decision, p.DecidedBy = store.ApprovalDecisionApproved, &userID
			}
			if err := q.CreateWithdrawalApproval(ctx, p); err != nil {
				return err
			}
		}
		return nil
	})
	if writeMoneyError(w, r, err) {
		return
	}
	s.writeWithdrawalRequest(w, r, a.Group.ID, id, http.StatusCreated)
}

// conflict is a 409 raised inside a transaction.
type conflict struct{ code, msg string }

func (e conflict) Error() string { return e.msg }

// writeMoneyError writes the response for an error from a money write and
// reports whether there was one.
func writeMoneyError(w http.ResponseWriter, r *http.Request, err error) bool {
	var bad badRequest
	var cf conflict
	var locked *auth.LockedOutError
	switch {
	case err == nil:
		return false
	case errors.As(err, &bad):
		writeError(w, http.StatusBadRequest, bad.Code, bad.Msg)
	case errors.As(err, &cf):
		writeError(w, http.StatusConflict, cf.code, cf.msg)
	case errors.Is(err, errNotFound):
		writeError(w, http.StatusNotFound, "request_not_found", "Withdrawal request not found.")
	case errors.Is(err, auth.ErrInvalidCredentials):
		writeError(w, http.StatusUnauthorized, "wrong_password", "That password is not right.")
	case errors.As(err, &locked):
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(locked.RetryAfter.Seconds()))))
		writeError(w, http.StatusTooManyRequests, "too_many_attempts", "Too many wrong passwords. Try again later.")
	default:
		writeInternal(w, r, err)
	}
	return true
}

var errNotFound = errors.New("not found")

// lockRequest loads a request for update, or errNotFound.
func lockRequest(ctx context.Context, q *store.Queries, groupID int64, r *http.Request) (store.WithdrawalRequest, error) {
	id, ok := pathID(r, "requestID")
	if !ok {
		return store.WithdrawalRequest{}, errNotFound
	}
	x, err := q.GetWithdrawalRequestForUpdate(ctx, store.GetWithdrawalRequestForUpdateParams{GroupID: groupID, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return x, errNotFound
	}
	return x, err
}

type decisionBody struct {
	Password string `json:"password"`
	Reason   string `json:"reason"`
}

func (s *Server) approveWithdrawalRequest(w http.ResponseWriter, r *http.Request) {
	s.decideWithdrawalRequest(w, r, store.ApprovalDecisionApproved)
}

func (s *Server) declineWithdrawalRequest(w http.ResponseWriter, r *http.Request) {
	s.decideWithdrawalRequest(w, r, store.ApprovalDecisionDeclined)
}

// decideWithdrawalRequest records a signatory's answer after checking
// their password, then settles the request if that answer decides it.
func (s *Server) decideWithdrawalRequest(w http.ResponseWriter, r *http.Request, decision store.ApprovalDecision) {
	a := accessFrom(r.Context())
	var body decisionBody
	if !decodeJSON(w, r, &body) {
		return
	}
	body.Reason = strings.TrimSpace(body.Reason)
	userID := claimsFrom(r.Context()).UserID
	var id int64
	err := func() error {
		if body.Password == "" {
			return badRequest{Code: "missing_fields", Msg: "Enter your password to answer a withdrawal request."}
		}
		if decision == store.ApprovalDecisionDeclined && body.Reason == "" {
			return badRequest{Code: "missing_fields", Msg: "Say why you are declining."}
		}
		if a.MembershipID == nil {
			return conflict{"not_a_signatory", "You are not a signatory on this request."}
		}
		if err := s.auth.ConfirmPassword(r.Context(), userID, body.Password, clientInfo(r)); err != nil {
			return err
		}
		return pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
			ctx, q := r.Context(), s.q.WithTx(tx)
			x, err := lockRequest(ctx, q, a.Group.ID, r)
			if err != nil {
				return err
			}
			id = x.ID
			if x.Status != store.WithdrawalRequestStatusPending {
				return conflict{"request_closed", "This request is already " + string(x.Status) + "."}
			}
			n, err := q.DecideWithdrawalApproval(ctx, store.DecideWithdrawalApprovalParams{
				Decision: decision, Comment: body.Reason, DecidedBy: &userID,
				WithdrawalRequestID: x.ID, MembershipID: *a.MembershipID,
			})
			if err != nil {
				return err
			}
			if n == 0 {
				return conflict{"not_a_signatory", "You are not a signatory on this request, or you have already answered."}
			}
			t, err := q.ApprovalTally(ctx, x.ID)
			if err != nil {
				return err
			}
			switch {
			case t.Declined > 0:
				return q.SetWithdrawalRequestStatus(ctx, store.SetWithdrawalRequestStatusParams{
					GroupID: a.Group.ID, ID: x.ID, Status: store.WithdrawalRequestStatusDeclined, DeclineReason: &body.Reason,
				})
			case t.Approved == t.Total:
				return q.SetWithdrawalRequestStatus(ctx, store.SetWithdrawalRequestStatusParams{
					GroupID: a.Group.ID, ID: x.ID, Status: store.WithdrawalRequestStatusApproved,
				})
			}
			return nil
		})
	}()
	if writeMoneyError(w, r, err) {
		return
	}
	s.writeWithdrawalRequest(w, r, a.Group.ID, id, http.StatusOK)
}

// cancelWithdrawalRequest withdraws a request that has not been paid.
func (s *Server) cancelWithdrawalRequest(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	var body struct {
		Reason string `json:"reason"`
	}
	if r.ContentLength != 0 && !decodeJSON(w, r, &body) {
		return
	}
	reason := strings.TrimSpace(body.Reason)
	if reason == "" {
		reason = "Cancelled"
	}
	var id int64
	err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		ctx, q := r.Context(), s.q.WithTx(tx)
		x, err := lockRequest(ctx, q, a.Group.ID, r)
		if err != nil {
			return err
		}
		id = x.ID
		if x.Status != store.WithdrawalRequestStatusPending && x.Status != store.WithdrawalRequestStatusApproved {
			return conflict{"request_closed", "This request is already " + string(x.Status) + "."}
		}
		return q.SetWithdrawalRequestStatus(ctx, store.SetWithdrawalRequestStatusParams{
			GroupID: a.Group.ID, ID: x.ID, Status: store.WithdrawalRequestStatusCancelled, DeclineReason: &reason,
		})
	})
	if writeMoneyError(w, r, err) {
		return
	}
	s.writeWithdrawalRequest(w, r, a.Group.ID, id, http.StatusOK)
}

type payBody struct {
	Date   *Date   `json:"date"`
	Method *string `json:"method"`
}

// payWithdrawalRequest records the money leaving for an approved request:
// a withdrawal, or a transfer for a funds transfer request.
func (s *Server) payWithdrawalRequest(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	var body payBody
	if !decodeJSON(w, r, &body) {
		return
	}
	userID := claimsFrom(r.Context()).UserID
	var id int64
	err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		ctx, q := r.Context(), s.q.WithTx(tx)
		x, err := lockRequest(ctx, q, a.Group.ID, r)
		if err != nil {
			return err
		}
		id = x.ID
		if x.Status != store.WithdrawalRequestStatusApproved {
			return conflict{"not_approved", "Only an approved request can be paid; this one is " + string(x.Status) + "."}
		}
		amount := Amount(money(deref(numericString(x.Amount))))
		if err := checkAmountDate(&amount, body.Date, body.Method, "payment"); err != nil {
			return err
		}
		if x.AccountID == nil {
			return badRequest{Code: "invalid_account", Msg: "The request has no account to pay from."}
		}
		acct, err := openAccount(ctx, q, a.Group.ID, *x.AccountID)
		if err != nil {
			return err
		}
		if err := books.CheckBalance(ctx, q, acct, string(amount)); err != nil {
			return err
		}
		desc := x.Description
		if desc == "" {
			desc = "Withdrawal request " + x.ReferenceNumber
		}
		var wid, tid *int64
		if x.Kind == store.WithdrawalKindFundsTransfer {
			if x.ToAccountID == nil {
				return badRequest{Code: "invalid_account", Msg: "The request has no account to transfer to."}
			}
			from, to, err := books.TransferAccounts(ctx, q, a.Group.ID, acct.ID, *x.ToAccountID)
			if err != nil {
				return err
			}
			t, err := books.WriteTransfer(ctx, q, from, to, string(amount), body.Date.Time, desc, userID)
			if err != nil {
				return err
			}
			tid = &t
		} else {
			m := books.Withdrawal{Kind: x.Kind, Amount: string(amount), MemberID: x.MembershipID,
				ContributionID: x.ContributionID, ExpenseCategoryID: x.ExpenseCategoryID}
			if err := books.CheckWithdrawal(ctx, q, a.Group.ID, m); err != nil {
				return err
			}
			wd, err := books.WriteWithdrawal(ctx, q, acct, m, body.Method, body.Date.Time, desc, userID)
			if err != nil {
				return err
			}
			wid = &wd
		}
		return q.MarkWithdrawalRequestPaid(ctx, store.MarkWithdrawalRequestPaidParams{
			GroupID: a.Group.ID, ID: x.ID, WithdrawalID: wid, AccountTransferID: tid,
		})
	})
	if writeMoneyError(w, r, err) {
		return
	}
	s.writeWithdrawalRequest(w, r, a.Group.ID, id, http.StatusOK)
}
