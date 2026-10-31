package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/swahome/legion-api/internal/books"
	"github.com/swahome/legion-api/internal/store"
)

// Loan applications (legacy loan_applications). A member applies for a loan
// of a loan type and names guarantors; the guarantors answer, then the
// signatories (as for withdrawal requests, with their password), then an
// official pays it out, which records the loan. See books.Apply.

type applicationSummary struct {
	ID                  int64      `json:"id"`
	Member              namedRef   `json:"member"`
	LoanType            namedRef   `json:"loan_type"`
	Amount              string     `json:"amount"`
	Installments        int32      `json:"installments"`
	Description         string     `json:"description"`
	Status              string     `json:"status"`
	DeclineReason       string     `json:"decline_reason"`
	DecidedAt           *time.Time `json:"decided_at"`
	LoanID              *int64     `json:"loan_id"`
	CreatedAt           time.Time  `json:"created_at"`
	AwaitingMyGuarantee bool       `json:"awaiting_my_guarantee"`
	AwaitingMyApproval  bool       `json:"awaiting_my_approval"`
}

type applicationGuarantor struct {
	MemberID  int64      `json:"member_id"`
	Name      string     `json:"name"`
	Amount    *string    `json:"amount"`
	Comment   string     `json:"comment"`
	Decision  string     `json:"decision"`
	DecidedAt *time.Time `json:"decided_at"`
	Reason    string     `json:"reason"`
}

func (s *Server) listLoanApplications(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	pg := pageParams(r)
	qs := r.URL.Query()
	p := store.ListLoanApplicationsParams{GroupID: a.Group.ID, Status: qs.Get("status"), AllVisible: a.IsOfficial,
		AwaitingMe: qs.Get("awaiting") == "me", Lim: int32(pg.size), Off: int32(pg.offset)}
	switch p.Status {
	case "", "open", "pending_guarantors", "pending_approval", "approved", "disbursed", "declined", "cancelled":
	default:
		writeError(w, http.StatusBadRequest, "invalid_filter",
			"status must be open, pending_guarantors, pending_approval, approved, disbursed, declined or cancelled.")
		return
	}
	var ok bool
	if p.MembershipID, ok = optionalID(r, "member_id"); !ok {
		writeError(w, http.StatusBadRequest, "invalid_filter", "member_id must be an id.")
		return
	}
	if a.MembershipID != nil {
		p.Viewer = *a.MembershipID
	} else if !a.IsOfficial {
		writeError(w, http.StatusForbidden, "forbidden", "Only members see loan applications.")
		return
	}
	rows, err := s.q.ListLoanApplications(r.Context(), p)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	items := make([]applicationSummary, 0, len(rows))
	var total int64
	for _, x := range rows {
		items = append(items, applicationSummary{
			ID: x.ID, Member: namedRef{ID: x.MembershipID, Name: x.MemberName}, LoanType: namedRef{ID: x.LoanTypeID, Name: x.LoanTypeName},
			Amount: money(x.Amount), Installments: x.Installments, Description: x.Description, Status: string(x.Status),
			DeclineReason: x.DeclineReason, DecidedAt: x.DecidedAt, LoanID: x.LoanID, CreatedAt: x.CreatedAt,
			AwaitingMyGuarantee: x.AwaitingMyGuarantee, AwaitingMyApproval: x.AwaitingMyApproval,
		})
		total = x.Total
	}
	writeJSON(w, http.StatusOK, map[string]any{"loan_applications": newPaged(items, total, pg)})
}

func (s *Server) getLoanApplication(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	id, _ := pathID(r, "applicationID")
	s.writeLoanApplication(w, r, a, id, http.StatusOK)
}

// writeLoanApplication answers with an application, its guarantors and
// signatories, what the caller may do with it, and the schedule it would
// have if paid out today. Members see their own and the ones they are asked
// to guarantee or approve.
func (s *Server) writeLoanApplication(w http.ResponseWriter, r *http.Request, a groupAccess, id int64, status int) {
	ctx := r.Context()
	x, err := s.q.GetLoanApplication(ctx, store.GetLoanApplicationParams{GroupID: a.Group.ID, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "application_not_found", "Loan application not found.")
		return
	}
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	gs, err := s.q.ApplicationGuarantors(ctx, id)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	approvals, err := s.q.ApplicationApprovals(ctx, id)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	me := int64(-1)
	if a.MembershipID != nil {
		me = *a.MembershipID
	}
	myGuarantee, myApproval := "", ""
	guarantors := make([]applicationGuarantor, 0, len(gs))
	for _, g := range gs {
		out := applicationGuarantor{MemberID: g.MembershipID, Name: g.MemberName, Comment: g.Comment,
			Decision: string(g.Decision), DecidedAt: g.DecidedAt, Reason: g.Reason}
		if !g.NoAmount {
			m := money(g.Amount)
			out.Amount = &m
		}
		if g.MembershipID == me {
			myGuarantee = string(g.Decision)
		}
		guarantors = append(guarantors, out)
	}
	approvalList := make([]approvalResponse, 0, len(approvals))
	for _, x := range approvals {
		approvalList = append(approvalList, approvalResponse{MemberID: x.MembershipID, Name: x.MemberName, Role: x.RoleName,
			Decision: string(x.Decision), DecidedAt: x.DecidedAt, Comment: x.Comment})
		if x.MembershipID == me {
			myApproval = string(x.Decision)
		}
	}
	if !a.IsOfficial && x.MembershipID != me && myGuarantee == "" && myApproval == "" {
		writeError(w, http.StatusNotFound, "application_not_found", "Loan application not found.")
		return
	}
	resp := map[string]any{
		"id": x.ID, "member": namedRef{ID: x.MembershipID, Name: x.MemberName},
		"loan_type": namedRef{ID: x.LoanTypeID, Name: x.LoanTypeName}, "amount": money(x.Amount),
		"installments": x.Installments, "description": x.Description, "status": string(x.Status),
		"decline_reason": x.DeclineReason, "decided_at": x.DecidedAt, "loan_id": x.LoanID, "created_at": x.CreatedAt,
		"guarantors": guarantors, "approval_list": approvalList,
		"my_guarantee": nullIfEmpty(myGuarantee), "my_decision": nullIfEmpty(myApproval),
		"can_cancel":   x.MembershipID == me && applicationOpen(x.Status),
		"can_disburse": a.IsOfficial && x.Status == store.LoanApplicationStatusApproved,
	}
	if applicationOpen(x.Status) {
		typeID := x.LoanTypeID
		today := s.today()
		terms, fee, err := books.PreviewLoan(ctx, s.q, a.Group.ID, books.NewLoan{MemberID: x.MembershipID, LoanTypeID: &typeID,
			Amount: money(x.Amount), Installments: int(x.Installments), Date: &today, Guarantors: placeholderGuarantors(len(gs))})
		if err == nil {
			resp["schedule"], _ = scheduleJSON(terms, fee)
		} else {
			resp["schedule_problem"] = err.Error()
		}
	}
	writeJSON(w, status, resp)
}

// placeholderGuarantors stands in for an application's guarantors when its
// schedule is previewed, so the guarantor count check passes.
func placeholderGuarantors(n int) []books.Guarantor { return make([]books.Guarantor, n) }

func applicationOpen(s store.LoanApplicationStatus) bool {
	return s == store.LoanApplicationStatusPendingGuarantors || s == store.LoanApplicationStatusPendingApproval ||
		s == store.LoanApplicationStatusApproved
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

type applicationBody struct {
	MemberID     *int64          `json:"member_id"`
	LoanTypeID   int64           `json:"loan_type_id"`
	Amount       *Amount         `json:"amount"`
	Installments int             `json:"installments"`
	Description  string          `json:"description"`
	Guarantors   []guarantorBody `json:"guarantors"`
}

// createLoanApplication files an application. Members apply for
// themselves; an official may apply for another member with member_id.
func (s *Server) createLoanApplication(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	var body applicationBody
	if !decodeJSON(w, r, &body) {
		return
	}
	app := books.NewApplication{LoanTypeID: body.LoanTypeID, Installments: body.Installments, Description: body.Description,
		ByOfficial: a.IsOfficial, OfficialsOnly: a.setting("restrict_wallet_approval_to_group_admins") == "1"}
	switch {
	case body.MemberID != nil && (a.MembershipID == nil || *body.MemberID != *a.MembershipID):
		if !a.IsOfficial {
			writeError(w, http.StatusForbidden, "forbidden", "Members apply for their own loans.")
			return
		}
		app.MemberID = *body.MemberID
	case a.MembershipID != nil:
		app.MemberID = *a.MembershipID
	default:
		writeError(w, http.StatusBadRequest, "missing_fields", "member_id is required.")
		return
	}
	if body.Amount != nil {
		app.Amount = string(*body.Amount)
	}
	for _, g := range body.Guarantors {
		app.Guarantors = append(app.Guarantors, books.Guarantor{MemberID: g.MemberID, Amount: amountStr(g.Amount), Comment: g.Comment})
	}
	userID := claimsFrom(r.Context()).UserID
	var id int64
	err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) (err error) {
		id, err = books.Apply(r.Context(), s.q.WithTx(tx), a.Group.ID, userID, app, s.today())
		return err
	})
	if writeApplicationError(w, r, err) {
		return
	}
	s.writeLoanApplication(w, r, a, id, http.StatusCreated)
}

// guaranteeLoanApplication records the caller's answer as a guarantor:
// {"approve": true} or {"approve": false, "reason": "..."}.
func (s *Server) guaranteeLoanApplication(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	var body struct {
		Approve *bool  `json:"approve"`
		Reason  string `json:"reason"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	id, _ := pathID(r, "applicationID")
	err := func() error {
		if body.Approve == nil {
			return badRequest{Code: "missing_fields", Msg: "approve is required: true to guarantee, false to decline."}
		}
		if a.MembershipID == nil {
			return conflict{Code: "not_a_guarantor", Msg: "You are not a guarantor on this application."}
		}
		return pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
			return books.AnswerGuarantee(r.Context(), s.q.WithTx(tx), a.Group.ID, id, *a.MembershipID, claimsFrom(r.Context()).UserID,
				*body.Approve, body.Reason, a.setting("restrict_wallet_approval_to_group_admins") == "1")
		})
	}()
	if writeApplicationError(w, r, err) {
		return
	}
	s.writeLoanApplication(w, r, a, id, http.StatusOK)
}

func (s *Server) approveLoanApplication(w http.ResponseWriter, r *http.Request) {
	s.decideLoanApplication(w, r, true)
}

func (s *Server) declineLoanApplication(w http.ResponseWriter, r *http.Request) {
	s.decideLoanApplication(w, r, false)
}

// decideLoanApplication records a signatory's answer, with their password.
func (s *Server) decideLoanApplication(w http.ResponseWriter, r *http.Request, approve bool) {
	a := accessFrom(r.Context())
	var body decisionBody
	if !decodeJSON(w, r, &body) {
		return
	}
	userID := claimsFrom(r.Context()).UserID
	id, _ := pathID(r, "applicationID")
	err := func() error {
		if body.Password == "" {
			return badRequest{Code: "missing_fields", Msg: "Enter your password to answer a loan application."}
		}
		if !approve && strings.TrimSpace(body.Reason) == "" {
			return badRequest{Code: "missing_fields", Msg: "Say why you are declining."}
		}
		if a.MembershipID == nil {
			return conflict{Code: "not_a_signatory", Msg: "You are not a signatory on this application."}
		}
		if err := s.auth.ConfirmPassword(r.Context(), userID, body.Password, clientInfo(r)); err != nil {
			return err
		}
		return pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
			return books.DecideApplication(r.Context(), s.q.WithTx(tx), a.Group.ID, id, *a.MembershipID, userID, approve, body.Reason)
		})
	}()
	if writeApplicationError(w, r, err) {
		return
	}
	s.writeLoanApplication(w, r, a, id, http.StatusOK)
}

// cancelLoanApplication: the applicant withdraws an open application, or an
// official declines it (a reason is required).
func (s *Server) cancelLoanApplication(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	var body struct {
		Reason string `json:"reason"`
	}
	if r.ContentLength != 0 && !decodeJSON(w, r, &body) {
		return
	}
	id, _ := pathID(r, "applicationID")
	err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		ctx, q := r.Context(), s.q.WithTx(tx)
		x, err := books.LockApplication(ctx, q, a.Group.ID, id)
		if err != nil {
			return err
		}
		own := a.MembershipID != nil && x.MembershipID == *a.MembershipID
		if !own && !a.IsOfficial {
			return books.ErrNotFound
		}
		return books.CloseApplication(ctx, q, a.Group.ID, id, !own, body.Reason)
	})
	if writeApplicationError(w, r, err) {
		return
	}
	s.writeLoanApplication(w, r, a, id, http.StatusOK)
}

type disburseBody struct {
	AccountID         int64   `json:"account_id"`
	Method            *string `json:"method"`
	Date              *Date   `json:"disbursed_on"`
	FirstDueOn        *Date   `json:"first_due_on"`
	Description       string  `json:"description"`
	SkipProcessingFee bool    `json:"skip_processing_fee"`
}

// disburseLoanApplication pays out an approved application: it records the
// loan (and its disbursement withdrawal) and links it.
func (s *Server) disburseLoanApplication(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	var body disburseBody
	if !decodeJSON(w, r, &body) {
		return
	}
	d := books.Disbursement{AccountID: body.AccountID, Method: body.Method, Description: body.Description,
		SkipProcessingFee: body.SkipProcessingFee}
	if body.Date != nil {
		d.Date = &body.Date.Time
	}
	if body.FirstDueOn != nil {
		d.FirstDueOn = &body.FirstDueOn.Time
	}
	id, _ := pathID(r, "applicationID")
	userID := claimsFrom(r.Context()).UserID
	err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		_, err := books.DisburseApplication(r.Context(), s.q.WithTx(tx), a.Group.ID, userID, id, d)
		return err
	})
	if writeApplicationError(w, r, err) {
		return
	}
	s.writeLoanApplication(w, r, a, id, http.StatusOK)
}

func writeApplicationError(w http.ResponseWriter, r *http.Request, err error) bool {
	if errors.Is(err, books.ErrNotFound) {
		writeError(w, http.StatusNotFound, "application_not_found", "Loan application not found.")
		return true
	}
	return writeMoneyError(w, r, err)
}
