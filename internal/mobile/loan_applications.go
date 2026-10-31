package mobile

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/swahome/legion-api/internal/auth"
	"github.com/swahome/legion-api/internal/books"
	"github.com/swahome/legion-api/internal/store"
)

// Loan applications from the app (legacy loan_applications/Mobile.php). The
// flow is books.Apply's: guarantors, then signatories, then an official
// pays it out (on the web, or loans/record_member_loan).

// dateAndTime is timestamp_to_date_and_time: "05-02-2024, 3:04pm".
func dateAndTime(t time.Time) string { return t.Format("02-01-2006, 3:04pm") }

// appResult turns a books error into the legacy {status:0,message}.
func (s *Server) appResult(err error, ok string) (any, error) {
	var cf books.Conflict
	switch {
	case err == nil:
		return timed(status(1, ok), s.now()), nil
	case errors.Is(err, books.ErrNotFound):
		return timed(status(0, "Loan Application Does Not Exist"), s.now()), nil
	case errors.As(err, &cf):
		return timed(status(0, cf.Msg), s.now()), nil
	}
	if be, isBE := asBooksError(err); isBE {
		return timed(status(0, be.Msg), s.now()), nil
	}
	return nil, err
}

func (s *Server) activeOnly(fn groupFn) groupFn {
	return func(ctx context.Context, c *call, g *member) (any, error) {
		if g.m.Status != store.MembershipStatusActive {
			return timed(status(0, "Could not proceed. Account is suspended"), s.now()), nil
		}
		return fn(ctx, c, g)
	}
}

func (g *member) officialsSign() bool { return g.flag("restrict_wallet_approval_to_group_admins") }

// createLoanApplication is create_group_loan_application: loan_product_id,
// loan_amount, repayment_period and the parallel guarantors, amounts and
// comments arrays.
func (s *Server) createLoanApplication(ctx context.Context, c *call, g *member) (any, error) {
	typeID := appID(c.req.Str("loan_product_id"))
	if typeID == nil {
		typeID = appID(c.req.Str("loan_type_id"))
	}
	if typeID == nil {
		return timed(status(0, "Loan Type is required"), s.now()), nil
	}
	amt := appAmount(c.req.Str("loan_amount"))
	if amt == "" {
		return timed(status(0, "Loan Application Amount is required"), s.now()), nil
	}
	app := books.NewApplication{MemberID: g.m.ID, LoanTypeID: *typeID, Amount: amt, Installments: int(c.req.Int("repayment_period")),
		Description: c.req.Str("description"), OfficialsOnly: g.officialsSign()}
	ids, amounts, comments := c.req.Strs("guarantors"), c.req.Strs("amounts"), c.req.Strs("comments")
	for i, v := range ids {
		id := appID(v)
		if id == nil {
			continue
		}
		gu := books.Guarantor{MemberID: *id}
		if i < len(amounts) {
			if a := appAmount(amounts[i]); a != "" {
				gu.Amount = &a
			}
		}
		if i < len(comments) {
			gu.Comment = comments[i]
		}
		app.Guarantors = append(app.Guarantors, gu)
	}
	if app.Installments == 0 {
		if r, err := s.q.GetLoanType(ctx, store.GetLoanTypeParams{GroupID: g.group.ID, ID: *typeID}); err == nil && r.FixedTerm != nil {
			app.Installments = int(*r.FixedTerm)
		}
	}
	if app.Installments == 0 {
		return timed(status(0, "Repayment Period is required"), s.now()), nil
	}
	var id int64
	err := s.inTx(ctx, func(q *store.Queries) (err error) {
		id, err = books.Apply(ctx, q, g.group.ID, g.user.ID, app, s.today(g))
		return err
	})
	if err != nil {
		return s.appResult(err, "")
	}
	msg := "Loan application submitted. An approval request has been sent to the group signatories."
	if len(app.Guarantors) > 0 {
		msg = "Loan Application Processed. Approval sent to all guarantors."
	}
	out := timed(status(1, msg), s.now())
	out["loan_application_id"] = id
	return out, nil
}

func (s *Server) applicationRow(x store.ListLoanApplicationsRow, loc *time.Location) map[string]any {
	st := x.Status
	return map[string]any{
		"id": fmt.Sprint(x.ID), "member_id": fmt.Sprint(x.MembershipID), "loan_type_id": fmt.Sprint(x.LoanTypeID),
		"loan_amount": books.Format(x.Amount), "repayment_period": fmt.Sprint(x.Installments), "description": x.Description,
		"status": string(st), "is_approved": boolInt(st == store.LoanApplicationStatusApproved || st == store.LoanApplicationStatusDisbursed),
		"is_declined":       boolInt(st == store.LoanApplicationStatusDeclined),
		"is_loan_disbursed": boolInt(st == store.LoanApplicationStatusDisbursed), "active": boolInt(st != store.LoanApplicationStatusCancelled),
		"decline_message": x.DeclineReason, "created_on": dateAndTime(x.CreatedAt.In(loc)), "member_name": x.MemberName,
		"ngo_portal_loan_application_name": x.LoanTypeName, "name": x.LoanTypeName, "loan_id": idString(x.LoanID),
		"awaiting_my_guarantee": boolInt(x.AwaitingMyGuarantee), "awaiting_my_approval": boolInt(x.AwaitingMyApproval),
	}
}

func (s *Server) applications(ctx context.Context, g *member, who *int64, awaitingMe bool) ([]store.ListLoanApplicationsRow, error) {
	return s.q.ListLoanApplications(ctx, store.ListLoanApplicationsParams{Viewer: g.m.ID, GroupID: g.group.ID,
		MembershipID: who, AllVisible: g.official(), AwaitingMe: awaitingMe, Lim: 1000})
}

// loanApplicationsList is get_group_loan_applications (mine=false) and
// get_member_loan_applications.
func (s *Server) loanApplicationsList(mine bool) groupFn {
	return s.activeOnly(func(ctx context.Context, c *call, g *member) (any, error) {
		var who *int64
		if mine {
			who = &g.m.ID
		}
		rows, err := s.applications(ctx, g, who, false)
		if err != nil {
			return nil, err
		}
		data := []any{}
		for _, x := range rows {
			data = append(data, s.applicationRow(x, g.loc))
		}
		return timed(map[string]any{"status": 1, "message": "Fetched Loan Applications Successfully", "count": len(data), "data": data}, s.now()), nil
	})
}

func guarantorRequestRow(a store.GetLoanApplicationRow, x store.ApplicationGuarantorsRow, groupID int64) map[string]any {
	first, last, _ := strings.Cut(x.MemberName, " ")
	amt := ""
	if !x.NoAmount {
		amt = books.Format(x.Amount)
	}
	return map[string]any{
		"id": fmt.Sprint(x.ID), "loan_application_id": fmt.Sprint(a.ID), "loan_type_id": fmt.Sprint(a.LoanTypeID),
		"group_id": fmt.Sprint(groupID), "guarantor_member_id": fmt.Sprint(x.MembershipID), "loan_request_applicant_member_id": fmt.Sprint(a.MembershipID),
		"loan_applicant_member_id": fmt.Sprint(a.MembershipID), "loan_request_member_id": fmt.Sprint(a.MembershipID),
		"applicant_name": a.MemberName, "loan_type_name": a.LoanTypeName, "loan_amount": books.Format(a.Amount),
		"amount": amt, "comment": x.Comment, "is_approved": boolInt(x.Decision == store.ApprovalDecisionApproved),
		"is_declined": boolInt(x.Decision == store.ApprovalDecisionDeclined), "decline_reason": x.Reason, "decline_comment": x.Reason,
		"loan_request_progress_status": map[store.ApprovalDecision]string{store.ApprovalDecisionPending: "1", store.ApprovalDecisionApproved: "2", store.ApprovalDecisionDeclined: "3"}[x.Decision],
		"guarantor_first_name":         first, "guarantor_last_name": last, "active": "1",
	}
}

func approvalRequestRow(a store.GetLoanApplicationRow, x store.ApplicationApprovalsRow, groupID int64) map[string]any {
	return map[string]any{
		"id": fmt.Sprint(x.ID), "loan_application_id": fmt.Sprint(a.ID), "loan_type_id": fmt.Sprint(a.LoanTypeID),
		"group_id": fmt.Sprint(groupID), "signatory_member_id": fmt.Sprint(x.MembershipID), "signatory_name": x.MemberName,
		"group_role": deref(x.RoleName), "loan_request_member_id": fmt.Sprint(a.MembershipID), "loan_request_member_name": a.MemberName,
		"loan_amount": books.Format(a.Amount), "loan_type_name": a.LoanTypeName,
		"is_approved": boolInt(x.Decision == store.ApprovalDecisionApproved), "is_declined": boolInt(x.Decision == store.ApprovalDecisionDeclined),
		"comments": x.Comment, "decline_comment": x.Comment, "active": "1", "metadata": []any{},
		"loan_signatory_progress_status": map[store.ApprovalDecision]string{store.ApprovalDecisionPending: "1", store.ApprovalDecisionApproved: "2", store.ApprovalDecisionDeclined: "3"}[x.Decision],
	}
}

// visibleApplication loads an application the caller may see.
func (s *Server) visibleApplication(ctx context.Context, c *call, g *member) (*store.GetLoanApplicationRow, error) {
	id := appID(c.req.Str("loan_application_id"))
	if id == nil {
		return nil, nil
	}
	a, err := s.q.GetLoanApplication(ctx, store.GetLoanApplicationParams{GroupID: g.group.ID, ID: *id})
	if err != nil {
		return nil, nil
	}
	return &a, nil
}

// applicationRequests is get_loan_application_guarantorship_requests and
// get_loan_application_approval_requests: one application's guarantors or
// signatories.
func (s *Server) applicationRequests(signatories bool) groupFn {
	return s.activeOnly(func(ctx context.Context, c *call, g *member) (any, error) {
		a, err := s.visibleApplication(ctx, c, g)
		if err != nil || a == nil {
			if err == nil {
				return timed(status(0, "Loan Application ID is required"), s.now()), nil
			}
			return nil, err
		}
		data := []any{}
		msg := "Fetched Loan Application Guarantorship Requests Successfully"
		if signatories {
			msg = "Fetched Loan Application Approval Requests Successfully"
			rows, err := s.q.ApplicationApprovals(ctx, a.ID)
			if err != nil {
				return nil, err
			}
			for _, x := range rows {
				data = append(data, approvalRequestRow(*a, x, g.group.ID))
			}
		} else {
			rows, err := s.q.ApplicationGuarantors(ctx, a.ID)
			if err != nil {
				return nil, err
			}
			for _, x := range rows {
				data = append(data, guarantorRequestRow(*a, x, g.group.ID))
			}
		}
		return timed(map[string]any{"status": 1, "message": msg, "count": len(data), "data": data}, s.now()), nil
	})
}

// myRequests is get_member_action_guarantorship_requests and
// get_member_action_approval_requests: what waits for the caller's answer.
func (s *Server) myRequests(signatories bool) groupFn {
	return s.activeOnly(func(ctx context.Context, c *call, g *member) (any, error) {
		rows, err := s.applications(ctx, g, nil, true)
		if err != nil {
			return nil, err
		}
		data := []any{}
		for _, x := range rows {
			if (signatories && !x.AwaitingMyApproval) || (!signatories && !x.AwaitingMyGuarantee) {
				continue
			}
			a := store.GetLoanApplicationRow{ID: x.ID, MembershipID: x.MembershipID, MemberName: x.MemberName, LoanTypeID: x.LoanTypeID,
				LoanTypeName: x.LoanTypeName, Amount: x.Amount, Installments: x.Installments, Status: x.Status, CreatedAt: x.CreatedAt}
			if signatories {
				ap, err := s.q.ApplicationApprovals(ctx, x.ID)
				if err != nil {
					return nil, err
				}
				for _, r := range ap {
					if r.MembershipID == g.m.ID {
						data = append(data, approvalRequestRow(a, r, g.group.ID))
					}
				}
				continue
			}
			gs, err := s.q.ApplicationGuarantors(ctx, x.ID)
			if err != nil {
				return nil, err
			}
			for _, r := range gs {
				if r.MembershipID == g.m.ID {
					data = append(data, guarantorRequestRow(a, r, g.group.ID))
				}
			}
		}
		msg := "Fetched Loan Application Guarantorship Requests Successfully"
		if signatories {
			msg = "Fetched Loan Application Requests Successfully"
		}
		return timed(map[string]any{"status": 1, "message": msg, "count": len(data), "data": data}, s.now()), nil
	})
}

// answerGuarantee is approve_loan_guarantorship_request and
// decline_loan_guarantorship_request.
func (s *Server) answerGuarantee(approve bool) groupFn {
	return s.activeOnly(func(ctx context.Context, c *call, g *member) (any, error) {
		id := appID(c.req.Str("loan_application_id"))
		if id == nil {
			return timed(status(0, "Loan Application ID is required"), s.now()), nil
		}
		reason := strings.TrimSpace(c.req.Str("decline_message"))
		if !approve && reason == "" {
			return timed(status(0, "Decline reason is required"), s.now()), nil
		}
		err := s.inTx(ctx, func(q *store.Queries) error {
			return books.AnswerGuarantee(ctx, q, g.group.ID, *id, g.m.ID, g.user.ID, approve, reason, g.officialsSign())
		})
		var cf books.Conflict
		if errors.As(err, &cf) && cf.Code == "not_a_guarantor" {
			return timed(status(0, "No pending guarantorship request found for you on this loan application."), s.now()), nil
		}
		if approve {
			return s.appResult(err, "Guarantorship request approved successfully.")
		}
		return s.appResult(err, "Guarantorship request declined successfully.")
	})
}

// approveLoanApplication is approve_loan_application: action 1 approves,
// 2 declines (with decline_message). A password, when the app sends one,
// is checked as for withdrawal requests.
func (s *Server) approveLoanApplication(ctx context.Context, c *call, g *member) (any, error) {
	id := appID(c.req.Str("loan_application_id"))
	if id == nil {
		return timed(status(0, "Loan Application ID is required"), s.now()), nil
	}
	var approve bool
	switch strings.ToLower(c.req.Str("action")) {
	case "1", "approve", "approved":
		approve = true
	case "2", "0", "decline", "declined":
	default:
		return timed(status(0, "Unsupported Action."), s.now()), nil
	}
	reason := strings.TrimSpace(c.req.Str("decline_message"))
	if !approve && reason == "" {
		return timed(status(0, "Decline Message is required while declining"), s.now()), nil
	}
	if pw := c.req.Str("password"); pw != "" {
		var locked *auth.LockedOutError
		if err := s.auth.ConfirmPassword(ctx, g.user.ID, pw, clientInfo(c.r)); errors.Is(err, auth.ErrInvalidCredentials) {
			return timed(status(0, "You entered the wrong password."), s.now()), nil
		} else if errors.As(err, &locked) {
			return timed(status(0, "Too many wrong passwords. Try again later."), s.now()), nil
		} else if err != nil {
			return nil, err
		}
	}
	err := s.inTx(ctx, func(q *store.Queries) error {
		return books.DecideApplication(ctx, q, g.group.ID, *id, g.m.ID, g.user.ID, approve, reason)
	})
	var cf books.Conflict
	if errors.As(err, &cf) && cf.Code == "not_a_signatory" {
		return timed(status(0, "No Loan Application Request Found For Your Approval"), s.now()), nil
	}
	if approve {
		return s.appResult(err, "Loan Application Approval Processed Successfully")
	}
	return s.appResult(err, "Loan Application Decline Processed Successfully")
}

// voidLoanApplication is void_group_loan_application: the applicant
// withdraws an open application, or an official declines it.
func (s *Server) voidLoanApplication(ctx context.Context, c *call, g *member) (any, error) {
	id := appID(c.req.Str("id"))
	if id == nil {
		id = appID(c.req.Str("loan_application_id"))
	}
	if id == nil {
		return timed(status(0, "Loan Application does not exist"), s.now()), nil
	}
	err := s.inTx(ctx, func(q *store.Queries) error {
		a, err := books.LockApplication(ctx, q, g.group.ID, *id)
		if err != nil {
			return err
		}
		own := a.MembershipID == g.m.ID
		if !own && !g.official() {
			return books.ErrNotFound
		}
		reason := c.req.Str("reason")
		if !own && reason == "" {
			reason = "Voided by an official."
		}
		return books.CloseApplication(ctx, q, g.group.ID, *id, !own, reason)
	})
	var cf books.Conflict
	if errors.As(err, &cf) {
		return timed(status(0, "Loan Application cannot be voided at this stage"), s.now()), nil
	}
	if errors.Is(err, books.ErrNotFound) {
		return timed(status(0, "Loan Application does not exist"), s.now()), nil
	}
	return s.appResult(err, "Loan application voided successfully")
}

func (s *Server) notAvailable(ctx context.Context, c *call, g *member) (any, error) {
	return timed(status(0, "This service is not available."), s.now()), nil
}
