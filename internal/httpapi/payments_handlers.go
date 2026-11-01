package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/swahome/legion-api/internal/auth"
	"github.com/swahome/legion-api/internal/payments"
	"github.com/swahome/legion-api/internal/store"
)

// Payments members start (MPesa prompts) and the transaction alerts
// officials reconcile.

type alertResponse struct {
	ID             int64      `json:"id"`
	Source         string     `json:"source"`
	ProviderRef    string     `json:"provider_ref"`
	Bank           *namedRef  `json:"bank"`
	AccountNumber  string     `json:"account_number"`
	Group          *namedRef  `json:"group"`
	Account        *namedRef  `json:"account"`
	Direction      string     `json:"direction"` // in, out
	Amount         string     `json:"amount"`
	Currency       string     `json:"currency"`
	TransactionAt  time.Time  `json:"transaction_at"`
	Reference      string     `json:"reference"`
	Particulars    string     `json:"particulars"`
	Phone          *string    `json:"phone"`
	PayerName      string     `json:"payer_name"`
	Status         string     `json:"status"`
	AutoReconciled bool       `json:"auto_reconciled"`
	ReconciledAt   *time.Time `json:"reconciled_at"`
	Note           string     `json:"note"`
	CreatedAt      time.Time  `json:"created_at"`
}

func direction(d int16) string {
	if d < 0 {
		return "out"
	}
	return "in"
}

func named(id *int64, name string) *namedRef {
	if id == nil {
		return nil
	}
	return &namedRef{ID: *id, Name: name}
}

func toAlert(t store.ListAlertsRow) alertResponse {
	return alertResponse{
		ID: t.ID, Source: t.Source, ProviderRef: t.ProviderRef, Bank: named(t.BankID, t.BankName),
		AccountNumber: t.AccountNumber, Group: named(t.GroupID, t.GroupName), Account: named(t.AccountID, t.AccountName),
		Direction: direction(t.Direction), Amount: money(t.Amount), Currency: t.Currency, TransactionAt: t.TransactionAt,
		Reference: t.Reference, Particulars: t.Particulars, Phone: t.Phone, PayerName: t.PayerName,
		Status: string(t.Status), AutoReconciled: t.AutoReconciled, ReconciledAt: t.ReconciledAt, Note: t.Note,
		CreatedAt: t.CreatedAt,
	}
}

// alertFilters reads the list filters shared by the group, admin and bank
// views: status, direction (in|out), account_id, source, from, to.
func alertFilters(w http.ResponseWriter, r *http.Request, p *store.ListAlertsParams) bool {
	qv := r.URL.Query()
	if v := qv.Get("status"); v != "" {
		if !store.AlertStatus(v).Valid() {
			writeError(w, http.StatusBadRequest, "invalid_filter", "status is unreconciled, reconciled or ignored.")
			return false
		}
		p.Status = store.NullAlertStatus{AlertStatus: store.AlertStatus(v), Valid: true}
	}
	switch qv.Get("direction") {
	case "":
	case "in":
		d := int16(1)
		p.Direction = &d
	case "out":
		d := int16(-1)
		p.Direction = &d
	default:
		writeError(w, http.StatusBadRequest, "invalid_filter", "direction is in or out.")
		return false
	}
	p.Source, p.AccountNumber = queryPtr(r, "source"), queryPtr(r, "account_number")
	var ok1, ok2, ok3 bool
	p.AccountID, ok1 = optionalID(r, "account_id")
	p.FromDate, ok2 = optionalDate(r, "from")
	p.ToDate, ok3 = optionalDate(r, "to")
	if !ok1 || !ok2 || !ok3 {
		writeError(w, http.StatusBadRequest, "invalid_filter", "Filters must be ids or dates like 2026-11-05.")
		return false
	}
	return true
}

func (s *Server) writeAlerts(w http.ResponseWriter, r *http.Request, p store.ListAlertsParams) {
	pg := pageParams(r)
	p.PageSize, p.PageOffset = pg.size, pg.offset
	rows, err := s.q.ListAlerts(r.Context(), p)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	items := make([]alertResponse, 0, len(rows))
	var total int64
	for _, t := range rows {
		items = append(items, toAlert(t))
		total = t.Total
	}
	writeJSON(w, http.StatusOK, newPaged(items, total, pg))
}

// listGroupAlerts: GET /groups/{id}/transaction-alerts (officials).
func (s *Server) listGroupAlerts(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	p := store.ListAlertsParams{GroupID: &a.Group.ID}
	if !alertFilters(w, r, &p) {
		return
	}
	s.writeAlerts(w, r, p)
}

// alertSummary: unreconciled counts and totals by direction.
func (s *Server) alertSummary(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	rows, err := s.q.AlertCounts(r.Context(), &a.Group.ID)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	out := map[string]any{
		"in":  map[string]any{"count": 0, "amount": "0.00"},
		"out": map[string]any{"count": 0, "amount": "0.00"},
	}
	for _, c := range rows {
		out[direction(c.Direction)] = map[string]any{"count": c.Alerts, "amount": money(c.Amount)}
	}
	writeJSON(w, http.StatusOK, out)
}

type alertRecord struct {
	ID          int64      `json:"id"`
	Kind        string     `json:"kind"`
	Amount      string     `json:"amount"`
	Date        Date       `json:"date"`
	Description string     `json:"description"`
	MemberID    *int64     `json:"member_id"`
	MemberName  string     `json:"member_name"`
	ItemName    string     `json:"item_name"`
	LoanID      *int64     `json:"loan_id,omitempty"`
	FromAlert   bool       `json:"from_alert"`
	VoidedAt    *time.Time `json:"voided_at"`
}

type alertDetail struct {
	alertResponse
	ReconciledBy string          `json:"reconciled_by"`
	Deposits     []alertRecord   `json:"deposits"`
	Withdrawals  []alertRecord   `json:"withdrawals"`
	Suggestion   *alertSuggested `json:"suggestion"`
}

// alertSuggested is a guess at who paid and how the money splits, for the
// official to accept or change.
type alertSuggested struct {
	MemberID int64           `json:"member_id"`
	Lines    []payments.Line `json:"lines"`
}

func (s *Server) alertDetail(r *http.Request, groupID *int64, id int64, suggest bool) (*alertDetail, error) {
	ctx := r.Context()
	t, err := s.q.GetAlertView(ctx, store.GetAlertViewParams{ID: id, GroupID: groupID})
	if err != nil {
		return nil, err
	}
	out := &alertDetail{
		alertResponse: toAlert(store.ListAlertsRow{
			ID: t.ID, Source: t.Source, ProviderRef: t.ProviderRef, BankID: t.BankID, AccountNumber: t.AccountNumber,
			GroupID: t.GroupID, AccountID: t.AccountID, Direction: t.Direction, Amount: payments.Text(t.Amount),
			Currency: t.Currency, TransactionAt: t.TransactionAt, Reference: t.Reference, Particulars: t.Particulars,
			Phone: t.Phone, PayerName: t.PayerName, Status: t.Status, AutoReconciled: t.AutoReconciled,
			ReconciledAt: t.ReconciledAt, Note: t.Note, CreatedAt: t.CreatedAt, GroupName: t.GroupName,
			AccountName: t.AccountName, BankName: t.BankName,
		}),
		ReconciledBy: t.ReconciledByName, Deposits: []alertRecord{}, Withdrawals: []alertRecord{},
	}
	deps, err := s.q.AlertDeposits(ctx, &id)
	if err != nil {
		return nil, err
	}
	for _, d := range deps {
		out.Deposits = append(out.Deposits, alertRecord{ID: d.ID, Kind: string(d.Kind), Amount: money(d.Amount),
			Date: Date{d.DepositDate}, Description: d.Description, MemberID: d.MembershipID, MemberName: d.MemberName,
			ItemName: d.ItemName, LoanID: d.LoanID, FromAlert: d.FromAlert, VoidedAt: d.VoidedAt})
	}
	wds, err := s.q.AlertWithdrawals(ctx, &id)
	if err != nil {
		return nil, err
	}
	for _, d := range wds {
		out.Withdrawals = append(out.Withdrawals, alertRecord{ID: d.ID, Kind: string(d.Kind), Amount: money(d.Amount),
			Date: Date{d.WithdrawalDate}, Description: d.Description, MemberID: d.MembershipID, MemberName: d.MemberName,
			ItemName: d.ItemName, FromAlert: d.FromAlert, VoidedAt: d.VoidedAt})
	}
	if suggest && t.Status == store.AlertStatusUnreconciled && t.Direction == 1 && t.GroupID != nil {
		member, err := payments.FindPayer(ctx, s.q, *t.GroupID, t.Phone, t.Particulars+" "+t.PayerName+" "+t.Reference, s.defaultCallingCode)
		if err != nil {
			return nil, err
		}
		if member != 0 {
			lines, err := payments.Arrears(ctx, s.q, *t.GroupID, member, payments.Text(t.Amount))
			if err != nil {
				return nil, err
			}
			out.Suggestion = &alertSuggested{MemberID: member, Lines: lines}
		}
	}
	return out, nil
}

func (s *Server) getGroupAlert(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	id, ok := pathID(r, "alertID")
	if !ok {
		writeError(w, http.StatusNotFound, "alert_not_found", "Transaction not found.")
		return
	}
	s.writeAlertDetail(w, r, &a.Group.ID, id, http.StatusOK)
}

func (s *Server) writeAlertDetail(w http.ResponseWriter, r *http.Request, groupID *int64, id int64, status int) {
	d, err := s.alertDetail(r, groupID, id, true)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "alert_not_found", "Transaction not found.")
		return
	}
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	writeJSON(w, status, d)
}

// alertAction runs one reconciliation step in a transaction and answers
// with the alert as it now stands.
func (s *Server) alertAction(w http.ResponseWriter, r *http.Request, do func(q *store.Queries, groupID, userID, alertID int64) error) {
	a := accessFrom(r.Context())
	id, ok := pathID(r, "alertID")
	if !ok {
		writeError(w, http.StatusNotFound, "alert_not_found", "Transaction not found.")
		return
	}
	userID := claimsFrom(r.Context()).UserID
	err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		return do(s.q.WithTx(tx), a.Group.ID, userID, id)
	})
	var bad badRequest
	switch {
	case errors.Is(err, payments.ErrNotFound):
		writeError(w, http.StatusNotFound, "alert_not_found", "Transaction not found.")
	case errors.As(err, &bad):
		status := http.StatusBadRequest
		if bad.Code == "already_reconciled" || bad.Code == "not_reconciled" {
			status = http.StatusConflict
		}
		writeError(w, status, bad.Code, bad.Msg)
	case err != nil:
		writeInternal(w, r, err)
	default:
		s.writeAlertDetail(w, r, &a.Group.ID, id, http.StatusOK)
	}
}

type lineRequest struct {
	Kind              string  `json:"kind"`
	MemberID          *int64  `json:"member_id"`
	ContributionID    *int64  `json:"contribution_id"`
	FineCategoryID    *int64  `json:"fine_category_id"`
	IncomeCategoryID  *int64  `json:"income_category_id"`
	DepositorID       *int64  `json:"depositor_id"`
	LoanID            *int64  `json:"loan_id"`
	ExpenseCategoryID *int64  `json:"expense_category_id"`
	Amount            *Amount `json:"amount"`
	Description       string  `json:"description"`
	Name              string  `json:"name"`
}

func (l lineRequest) line() payments.Line {
	out := payments.Line{Kind: l.Kind, MemberID: l.MemberID, ContributionID: l.ContributionID,
		FineCategoryID: l.FineCategoryID, IncomeCategoryID: l.IncomeCategoryID, DepositorID: l.DepositorID,
		LoanID: l.LoanID, ExpenseCategoryID: l.ExpenseCategoryID, Description: l.Description, Name: l.Name}
	if l.Amount != nil {
		out.Amount = string(*l.Amount)
	}
	return out
}

func toLines(in []lineRequest) []payments.Line {
	out := make([]payments.Line, 0, len(in))
	for _, l := range in {
		out = append(out, l.line())
	}
	return out
}

// reconcileAlert: POST .../transaction-alerts/{id}/reconcile
// {"lines": [{"kind": "contribution", "member_id": 1, "contribution_id": 2, "amount": "500"}, ...]}.
// Money in takes contribution, fine, misc, income and loan lines; money out
// takes expense and contribution_refund lines.
func (s *Server) reconcileAlert(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Lines []lineRequest `json:"lines"`
		Note  string        `json:"note"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	s.alertAction(w, r, func(q *store.Queries, groupID, userID, alertID int64) error {
		return payments.Reconcile(r.Context(), q, groupID, userID, alertID, toLines(req.Lines), false, strings.TrimSpace(req.Note))
	})
}

// matchAlert: POST .../match {"deposit_ids": [...]} or {"withdrawal_ids": [...]}.
func (s *Server) matchAlert(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DepositIDs    []int64 `json:"deposit_ids"`
		WithdrawalIDs []int64 `json:"withdrawal_ids"`
		Note          string  `json:"note"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	s.alertAction(w, r, func(q *store.Queries, groupID, userID, alertID int64) error {
		return payments.Match(r.Context(), q, groupID, userID, alertID, append(req.DepositIDs, req.WithdrawalIDs...), strings.TrimSpace(req.Note))
	})
}

// dismissAlert: POST .../dismiss {"status": "reconciled"|"ignored", "note": "..."}.
func (s *Server) dismissAlert(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Status string `json:"status"`
		Note   string `json:"note"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	st := store.AlertStatus(req.Status)
	if st != store.AlertStatusReconciled && st != store.AlertStatusIgnored {
		writeError(w, http.StatusBadRequest, "invalid_status", "status is reconciled (already recorded) or ignored.")
		return
	}
	s.alertAction(w, r, func(q *store.Queries, groupID, userID, alertID int64) error {
		return payments.Dismiss(r.Context(), q, groupID, userID, alertID, st, req.Note)
	})
}

// undoAlert: POST .../undo voids what reconciliation wrote and reopens it.
func (s *Server) undoAlert(w http.ResponseWriter, r *http.Request) {
	s.alertAction(w, r, func(q *store.Queries, groupID, userID, alertID int64) error {
		return payments.Undo(r.Context(), q, groupID, userID, alertID)
	})
}

type reconcileSettings struct {
	AutoReconcilePayments   bool `json:"auto_reconcile_payments"`
	AutoReconcileBankAlerts bool `json:"auto_reconcile_bank_alerts"`
}

func (s *Server) getReconcileSettings(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	f, err := s.q.GroupReconcileFlags(r.Context(), a.Group.ID)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, reconcileSettings{
		AutoReconcilePayments: f.DisableAutoReconcile != "1", AutoReconcileBankAlerts: f.EnableAutoReconcile == "1",
	})
}

func (s *Server) updateReconcileSettings(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	var req reconcileSettings
	if !decodeJSON(w, r, &req) {
		return
	}
	flag := func(b bool) string {
		if b {
			return "1"
		}
		return "0"
	}
	if err := s.q.SetGroupReconcileFlags(r.Context(), store.SetGroupReconcileFlagsParams{
		ID: a.Group.ID, EnableAutoReconcile: flag(req.AutoReconcileBankAlerts), DisableAutoReconcile: flag(!req.AutoReconcilePayments),
	}); err != nil {
		writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, req)
}

// ---- Member payments ----

type paymentResponse struct {
	ID                 int64           `json:"id"`
	Group              namedRef        `json:"group"`
	MemberID           *int64          `json:"member_id"`
	MemberName         string          `json:"member_name"`
	Phone              string          `json:"phone"`
	Amount             string          `json:"amount"`
	Reference          string          `json:"reference"`
	Lines              []payments.Line `json:"lines"`
	Channel            string          `json:"channel"`
	Status             string          `json:"status"`
	Message            string          `json:"message"`
	Receipt            *string         `json:"receipt"`
	TransactionAlertID *int64          `json:"transaction_alert_id"`
	CreatedAt          time.Time       `json:"created_at"`
	UpdatedAt          time.Time       `json:"updated_at"`
}

func toPayment(p store.ListPaymentRequestsRow) paymentResponse {
	out := paymentResponse{
		ID: p.ID, Group: namedRef{ID: p.GroupID, Name: p.GroupName}, MemberID: p.MembershipID, MemberName: p.MemberName,
		Phone: p.Phone, Amount: money(p.Amount), Reference: p.Reference, Channel: p.Channel, Status: string(p.Status),
		Message: firstOf(p.ResultDescription, p.ResponseDescription), Receipt: p.ProviderRef,
		TransactionAlertID: p.TransactionAlertID, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}
	if out.Status == "sent" {
		out.Message = "Check your phone and enter your MPesa PIN."
	}
	_ = json.Unmarshal(p.Lines, &out.Lines)
	if out.Lines == nil {
		out.Lines = []payments.Line{}
	}
	return out
}

func (s *Server) writePayments(w http.ResponseWriter, r *http.Request, p store.ListPaymentRequestsParams) {
	pg := pageParams(r)
	p.PageSize, p.PageOffset = pg.size, pg.offset
	if v := r.URL.Query().Get("status"); v != "" {
		if !store.PaymentRequestStatus(v).Valid() {
			writeError(w, http.StatusBadRequest, "invalid_filter", "status is pending, sent, failed or paid.")
			return
		}
		p.Status = store.NullPaymentRequestStatus{PaymentRequestStatus: store.PaymentRequestStatus(v), Valid: true}
	}
	rows, err := s.q.ListPaymentRequests(r.Context(), p)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	items := make([]paymentResponse, 0, len(rows))
	var total int64
	for _, row := range rows {
		items = append(items, toPayment(row))
		total = row.Total
	}
	writeJSON(w, http.StatusOK, newPaged(items, total, pg))
}

// listGroupPayments: officials see every member's payments; members only
// their own (or an official with mine=true).
func (s *Server) listGroupPayments(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	p := store.ListPaymentRequestsParams{GroupID: &a.Group.ID}
	if !a.IsOfficial || r.URL.Query().Get("mine") == "true" {
		if a.MembershipID == nil {
			writeJSON(w, http.StatusOK, newPaged([]paymentResponse{}, 0, pageParams(r)))
			return
		}
		p.MembershipID = a.MembershipID
	}
	s.writePayments(w, r, p)
}

func (s *Server) getGroupPayment(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	id, ok := pathID(r, "paymentID")
	if !ok {
		writeError(w, http.StatusNotFound, "payment_not_found", "Payment not found.")
		return
	}
	s.writePayment(w, r, a, id, http.StatusOK)
}

func (s *Server) writePayment(w http.ResponseWriter, r *http.Request, a groupAccess, id int64, status int) {
	p, err := s.q.GetPaymentRequest(r.Context(), store.GetPaymentRequestParams{GroupID: a.Group.ID, ID: id})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !a.IsOfficial && (p.MembershipID == nil || !a.canSeeMember(*p.MembershipID))) {
		writeError(w, http.StatusNotFound, "payment_not_found", "Payment not found.")
		return
	}
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	writeJSON(w, status, toPayment(store.ListPaymentRequestsRow{
		ID: p.ID, GroupID: p.GroupID, MembershipID: p.MembershipID, Phone: p.Phone, Amount: payments.Text(p.Amount),
		Reference: p.Reference, Lines: p.Lines, Channel: p.Channel, Status: p.Status,
		ResponseDescription: p.ResponseDescription, ResultCode: p.ResultCode, ResultDescription: p.ResultDescription,
		ProviderRef: p.ProviderRef, TransactionAlertID: p.TransactionAlertID, CreatedAt: p.CreatedAt,
		UpdatedAt: p.UpdatedAt, MemberName: p.MemberName, GroupName: a.Group.Name,
	}))
}

type payItem struct {
	Kind           string  `json:"kind"`
	ContributionID *int64  `json:"contribution_id,omitempty"`
	FineCategoryID *int64  `json:"fine_category_id,omitempty"`
	LoanID         *int64  `json:"loan_id,omitempty"`
	Name           string  `json:"name"`
	Owed           string  `json:"owed"`
	Since          *string `json:"since,omitempty"`
}

// paymentOptions: what the caller owes in the group and where the money
// goes, for the "Pay" screen.
func (s *Server) paymentOptions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a := accessFrom(ctx)
	out := map[string]any{"enabled": s.payments.Enabled(), "account": nil, "phone": "", "owed": []payItem{},
		"contributions": []namedRef{}, "currency": a.Group.Currency}
	if acct, err := s.q.DefaultPaymentAccount(ctx, a.Group.ID); err == nil {
		out["account"] = map[string]any{"id": acct.ID, "account_number": acct.AccountNumber}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		writeInternal(w, r, err)
		return
	}
	if u, err := s.q.GetUserByID(ctx, claimsFrom(ctx).UserID); err == nil && u.Phone != nil {
		out["phone"] = *u.Phone
	}
	cs, err := s.q.PayableContributions(ctx, a.Group.ID)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	contribs := make([]namedRef, 0, len(cs))
	for _, c := range cs {
		contribs = append(contribs, namedRef{ID: c.ID, Name: c.Name})
	}
	out["contributions"] = contribs
	if a.MembershipID == nil {
		writeJSON(w, http.StatusOK, out)
		return
	}
	owed, err := s.q.MemberLedgerArrears(ctx, store.MemberLedgerArrearsParams{GroupID: a.Group.ID, MembershipID: a.MembershipID})
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	items := []payItem{}
	for _, o := range owed {
		switch {
		case o.Bucket == "contribution" && o.ContributionID != nil:
			items = append(items, payItem{Kind: "contribution", ContributionID: o.ContributionID, Name: o.Name, Owed: money(o.Balance)})
		case o.Bucket == "fine" && o.ContributionID != nil:
			items = append(items, payItem{Kind: "fine", ContributionID: o.ContributionID, Name: o.Name + " late fine", Owed: money(o.Balance)})
		case o.Bucket == "fine" && o.FineCategoryID != nil:
			items = append(items, payItem{Kind: "fine", FineCategoryID: o.FineCategoryID, Name: o.Name, Owed: money(o.Balance)})
		}
	}
	loans, err := s.q.MemberOpenLoans(ctx, store.MemberOpenLoansParams{GroupID: a.Group.ID, MembershipID: a.MembershipID})
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	for _, l := range loans {
		since := l.DisbursedOn.Format(dateLayout)
		items = append(items, payItem{Kind: "loan", LoanID: &l.ID, Name: l.Name, Owed: money(l.Balance), Since: &since})
	}
	out["owed"] = items
	writeJSON(w, http.StatusOK, out)
}

// startPayment: POST /groups/{id}/payments {"phone": "0712...", "lines": [...]}
// or {"amount": "1500"} to spread it over what the member owes. Sends the
// MPesa prompt; poll GET /payments/{id} for the result.
func (s *Server) startPayment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a := accessFrom(ctx)
	if a.MembershipID == nil {
		writeError(w, http.StatusForbidden, "members_only", "Only members of the group can pay into it.")
		return
	}
	var req struct {
		Phone  string        `json:"phone"`
		Lines  []lineRequest `json:"lines"`
		Amount *Amount       `json:"amount"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	phone := strings.TrimSpace(req.Phone)
	if phone == "" {
		if u, err := s.q.GetUserByID(ctx, claimsFrom(ctx).UserID); err == nil && u.Phone != nil {
			phone = *u.Phone
		}
	}
	e164, err := auth.NormalizePhone(phone, s.defaultCallingCode)
	if err != nil || phone == "" {
		writeError(w, http.StatusBadRequest, "invalid_phone", "Enter the MPesa phone number to pay from.")
		return
	}
	in := payments.PaymentInput{GroupID: a.Group.ID, MembershipID: *a.MembershipID, UserID: claimsFrom(ctx).UserID,
		Phone: e164, Lines: toLines(req.Lines), Channel: "web"}
	if req.Amount != nil {
		in.Amount = string(*req.Amount)
	}
	if len(in.Lines) == 0 && in.Amount == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "Say what you are paying for, or an amount.")
		return
	}
	id, err := s.payments.StartPayment(ctx, in)
	var bad badRequest
	switch {
	case errors.Is(err, payments.ErrNotConfigured):
		writeError(w, http.StatusServiceUnavailable, "payments_off", "MPesa payments are not available yet.")
	case errors.As(err, &bad):
		writeError(w, http.StatusBadRequest, bad.Code, bad.Msg)
	case err != nil && id == 0:
		writeInternal(w, r, err)
	default:
		s.writePayment(w, r, a, id, http.StatusCreated)
	}
}
