package mobile

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/swahome/legion-api/internal/books"
	"github.com/swahome/legion-api/internal/legacy"
	"github.com/swahome/legion-api/internal/loans"
	"github.com/swahome/legion-api/internal/store"
)

// Loans (legacy loans/Mobile.php) and loan repayments from the app. The
// writes go through internal/books like the web's.

func (g *member) epoch(d time.Time) int64 {
	return time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, g.loc).Unix()
}

// fullReportTime is timestamp_to_report_time: "5th February, 2024 ".
func fullReportTime(t time.Time) string { return ordinal(t.Day()) + t.Format(" January, 2006 ") }

func pickerDate(t time.Time) string { return t.Format("02-01-2006") }

func loanInterestText(rate string, ratePer store.InterestRatePer, it store.InterestType) string {
	return books.Format(rate) + "% " + ratePerNames[ratePerCodes[string(ratePer)]] + " " + interestTypeNames[interestTypeCodes[string(it)]]
}

func (s *Server) listLoans(ctx context.Context, g *member, membershipID *int64, status string, limit, offset int32) ([]store.ListLoansRow, error) {
	return s.q.ListLoans(ctx, store.ListLoansParams{GroupID: g.group.ID, Today: s.today(g), Status: status,
		MembershipID: membershipID, PageSize: limit, PageOffset: offset})
}

func (s *Server) today(g *member) time.Time {
	n := s.now().In(g.loc)
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC)
}

func loanTypeName(r store.ListLoansRow) string {
	if r.LoanTypeName != "" {
		return r.LoanTypeName
	}
	return "Normal Loan"
}

// groupLoansList is get_group_loans_list. With is_member_loans it lists one
// member's loans: the caller's, or for an official the member_id given
// (unless the group keeps member information private).
func (s *Server) groupLoansList(ctx context.Context, c *call, g *member) (any, error) {
	lower, upper := c.req.Int("lower_limit"), c.req.Int("upper_limit")
	if upper == 0 {
		upper = 100
	}
	size := upper - lower
	if lower > upper || size <= 0 {
		size = 100
	}
	var who *int64
	if c.req.Str("is_member_loans") != "" && c.req.Str("is_member_loans") != "0" && c.req.Str("is_member_loans") != "false" {
		id := g.m.ID
		if g.official() && !g.flag("enable_member_information_privacy") {
			if m := appID(c.req.Str("member_id")); m != nil {
				id = *m
			}
		}
		who = &id
	}
	rows, err := s.listLoans(ctx, g, who, "", int32(min(size, 500)), int32(lower))
	if err != nil {
		return nil, err
	}
	_, accts, err := s.groupAccounts(ctx, g)
	if err != nil {
		return nil, err
	}
	out := []any{}
	for _, r := range rows {
		from := ""
		if r.AccountID != nil {
			from = accountLabel(accts[*r.AccountID])
		}
		out = append(out, map[string]any{
			"id": fmt.Sprint(r.ID), "name": r.MemberName, "loan_type": loanTypeName(r),
			"description": "Loan disbursed to " + r.MemberName + " on " + mobileShortTime(r.DisbursedOn) + " at " +
				loanInterestText(r.InterestRate, r.RatePer, r.InterestType) + " payable in " + strconv.Itoa(int(r.Installments)) +
				" months. Disbursed from " + from,
			"member_id": fmt.Sprint(deref64(r.MembershipID)), "amount": books.Format(r.Amount),
			"disbursement_date": mobileShortTime(r.DisbursedOn), "end_date": mobileShortTime(r.EndsOn),
			"is_fully_paid": boolInt(books.Cents(r.Balance) <= 0),
		})
	}
	return map[string]any{"status": 1, "message": "success", "loans": out, "time": s.now().Unix()}, nil
}

func accountRef(a store.ListGroupAccountsRow) string {
	return accountPrefix[a.Kind] + "-" + fmt.Sprint(a.ID)
}

// groupLoan is get_group_loan: the legacy loans row.
func (s *Server) groupLoan(ctx context.Context, c *call, g *member) (any, error) {
	id := appID(c.req.Str("id"))
	if id == nil {
		return timed(status(0, "Bad request: Missing loan id"), s.now()), nil
	}
	d, err := books.GetLoanDetail(ctx, s.q, g.group.ID, *id, s.today(g))
	if errors.Is(err, books.ErrNotFound) || (err == nil && d.Loan.VoidedAt != nil) {
		return timed(status(0, "Group loan not available"), s.now()), nil
	}
	if err != nil {
		return nil, err
	}
	l := d.Loan
	_, accts, err := s.groupAccounts(ctx, g)
	if err != nil {
		return nil, err
	}
	acct, acctName := "", ""
	if l.AccountID != nil {
		acct, acctName = accountRef(accts[*l.AccountID]), accountLabel(accts[*l.AccountID])
	}
	late, _ := loans.DecodeFine(l.LateFine)
	bal, _ := loans.DecodeFine(l.BalanceFine)
	fee, _ := loans.DecodeFee(l.ProcessingFee)
	typeName := l.LoanTypeName
	if typeName == "" {
		typeName = "Normal Loan"
	}
	row := merge(lateFineFields(late), balanceFineFields(bal), feeFields(fee), map[string]any{
		"id": fmt.Sprint(l.ID), "member_id": fmt.Sprint(deref64(l.MembershipID)), "group_id": fmt.Sprint(l.GroupID),
		"loan_type_id": idString(l.LoanTypeID), "loan_type": "1", "disbursement_date": fmt.Sprint(g.epoch(l.DisbursedOn)),
		"loan_end_date": fmt.Sprint(g.epoch(l.EndsOn)), "loan_amount": books.Format(books.NumText(l.Amount)),
		"account_id": acct, "repayment_period": fmt.Sprint(l.Installments), "interest_rate": books.Format(books.NumText(l.InterestRate)),
		"interest_type": interestTypeCodes[string(l.InterestType)], "loan_interest_rate_per": ratePerCodes[string(l.RatePer)],
		"grace_period": fmt.Sprint(l.GracePeriods), "enable_loan_fine_deferment": boolInt(l.FineDeferment),
		"enable_loan_guarantors": boolInt(len(d.Guarantors) > 0), "active": boolInt(l.VoidedAt == nil),
		"is_fully_paid": boolInt(d.Balance <= 0), "is_a_bad_loan": boolInt(l.IsBadDebt),
		"is_a_back_dating_record": boolInt(l.IsBackdated), "created_on": fmt.Sprint(l.CreatedAt.Unix()),
		"account_name": acctName, "loan_type_name": typeName,
	})
	if l.FirstDueOn != nil {
		row["grace_period"], row["grace_period_date"] = "date", fmt.Sprint(g.epoch(*l.FirstDueOn))
	}
	if l.NextBalanceFineOn != nil {
		row["outstanding_loan_balance_fine_date"] = fmt.Sprint(g.epoch(*l.NextBalanceFineOn))
	}
	return timed(map[string]any{"status": 1, "message": "success", "loan": row}, s.now()), nil
}

// loanOptions is get_member_loan_options (one member's unpaid loans) and
// get_members_loans_options (everyone's).
func (s *Server) loanOptions(everyone bool) groupFn {
	return func(ctx context.Context, c *call, g *member) (any, error) {
		var who *int64
		if !everyone {
			id := g.m.ID
			if m := appID(c.req.Str("member_id")); m != nil {
				id = *m
			}
			who = &id
		}
		rows, err := s.listLoans(ctx, g, who, "open", 5000, 0)
		if err != nil {
			return nil, err
		}
		out := []any{}
		for _, r := range rows {
			o := map[string]any{"id": fmt.Sprint(r.ID), "name": loanTypeName(r), "amount": books.Format(r.Amount),
				"description": "Disbursed on " + fullReportTime(r.DisbursedOn), "balance": amount(r.Balance)}
			if everyone {
				o["member_id"], o["is_selected"] = fmt.Sprint(deref64(r.MembershipID)), boolInt(books.Is(r.MembershipID, g.m.ID))
			}
			out = append(out, o)
		}
		return timed(map[string]any{"status": 1, "message": "Member loans", "loans": out}, s.now()), nil
	}
}

// loanStatement is loans/statement.
func (s *Server) loanStatement(ctx context.Context, c *call, g *member) (any, error) {
	id := appID(c.req.Str("id"))
	var d *books.LoanDetail
	var err error
	if id != nil {
		d, err = books.GetLoanDetail(ctx, s.q, g.group.ID, *id, s.today(g))
	}
	if id == nil || errors.Is(err, books.ErrNotFound) || (err == nil && (d.Loan.VoidedAt != nil || d.Loan.MembershipID == nil)) {
		return timed(status(0, "Could not find group member loan details"), s.now()), nil
	}
	if err != nil {
		return nil, err
	}
	if !books.Is(d.Loan.MembershipID, g.m.ID) && !g.official() && g.flag("enable_member_information_privacy") {
		return timed(status(0, "Could not find group member loan details"), s.now()), nil
	}
	ms, err := s.q.MobileGroupMembers(ctx, store.MobileGroupMembersParams{GroupID: g.group.ID, MembershipID: d.Loan.MembershipID, Lim: 1})
	if err != nil {
		return nil, err
	}
	user := map[string]any{"name": d.Loan.MemberName, "email": "", "phone": "", "avatar": nil, "role": "Member"}
	if len(ms) == 1 {
		m := ms[0]
		user = map[string]any{"name": m.FirstName + " " + m.LastName, "email": deref(m.Email), "phone": legacyPhone(m.Phone),
			"avatar": nullable(m.Avatar), "role": roleName(m, "Member")}
	}
	var transfers int64
	for _, ch := range d.Charges {
		if ch.Kind == store.LoanChargeKindTransferOut {
			transfers += books.Cents(ch.Amount)
		}
	}
	body := []any{map[string]any{"description": "Total Amount Payable", "date": "", "amount_paid": 0,
		"balance": float64(d.Payable-transfers) / 100}}
	var paid int64
	for _, r := range d.Repayments {
		paid += books.Cents(r.Amount)
		body = append(body, map[string]any{"description": "Payment", "date": pickerDate(r.PaidOn),
			"amount_paid": amount(r.Amount), "balance": float64(d.Payable-paid) / 100})
	}
	l := d.Loan
	return timed(map[string]any{"status": 1, "message": "Successful", "data": map[string]any{
		"is_fully_paid": boolInt(d.Balance <= 0), "lump_sum": float64(d.Balance) / 100,
		"description":    "Loan disbursed on " + mobileTime(l.DisbursedOn) + " at " + loanInterestText(books.NumText(l.InterestRate), l.RatePer, l.InterestType),
		"user_data":      user,
		"statement_body": body,
		"statement_footer": map[string]any{"type": "Total", "paid": float64(d.Paid-transfers) / 100,
			"balance": float64(d.Payable-d.Paid) / 100},
	}}, s.now()), nil
}

// loanCalculator is loans/get_member_loan_calculator and
// loan_applications/get_group_loan_calculator (formatted is the latter's
// number_format and rounded totals).
func (s *Server) loanCalculator(formatted bool) groupFn {
	return func(ctx context.Context, c *call, g *member) (any, error) {
		if g.m.Status != store.MembershipStatusActive {
			return timed(status(0, "Could not proceed. Account is suspended"), s.now()), nil
		}
		typeID := appID(c.req.Str("loan_type_id"))
		var lt *books.LoanType
		if typeID != nil {
			if r, err := s.q.GetLoanType(ctx, store.GetLoanTypeParams{GroupID: g.group.ID, ID: *typeID}); err == nil {
				t, err := books.LoanTypeFrom(r)
				if err != nil {
					return nil, err
				}
				lt = &t
			}
		}
		if lt == nil {
			return timed(status(0, "Could not proceed. Loan type details missing"), s.now()), nil
		}
		amt := appAmount(c.req.Str("loan_amount"))
		if amt == "" || books.Cents(amt) <= 0 {
			return timed(status(0, "Loan amount is required"), s.now()), nil
		}
		period := int(c.req.Int("repayment_period"))
		if period <= 0 && lt.FixedTerm != nil {
			period = int(*lt.FixedTerm)
		}
		if period <= 0 {
			return timed(status(0, "Loan repayment period is required"), s.now()), nil
		}
		today := s.today(g)
		terms := loans.Terms{Amount: books.Cents(amt), InterestType: loans.InterestType(lt.InterestType), Rate: lt.InterestRate,
			RatePer: loans.RatePer(lt.RatePer), Frequency: loans.Frequency(lt.Frequency), Installments: period,
			GracePeriods: int(lt.GracePeriods), Reducing: loans.ReducingMethod(lt.ReducingMethod),
			FixedInterest: books.Cents(lt.FixedInterest), FixedPercent: lt.FixedRate, DisbursementOn: today}
		sched, err := terms.Schedule()
		if err != nil {
			return timed(status(0, sentenceOf(err)), s.now()), nil
		}
		var total, principal, interest int64
		for _, i := range sched {
			total, principal, interest = total+i.Amount(), principal+i.Principal, interest+i.Interest
		}
		money := func(c int64) any {
			if formatted {
				return commaMoney(loans.Money(c))
			}
			return float64(c) / 100
		}
		breakdown := []any{}
		left := total
		for _, i := range sched {
			left -= i.Amount()
			breakdown = append(breakdown, map[string]any{"due_date": pickerDate(i.DueDate), "amount_payable": money(i.Amount()),
				"principle_payable": money(i.Principal), "interest_payable": money(i.Interest),
				"total_intrest_payable": money(i.Interest), "balance": money(left)})
		}
		totals := map[string]any{"total_payable": float64(total) / 100, "total_principle": float64(principal) / 100,
			"total_interest": float64(interest) / 100}
		if formatted {
			for k, v := range totals {
				totals[k] = int64(v.(float64) + 0.5)
			}
		}
		return timed(map[string]any{"status": 1, "amortization_totals": totals, "breakdown": breakdown}, s.now()), nil
	}
}

func sentenceOf(err error) string {
	s := err.Error()
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// appGuarantors reads the guarantors array ({member_id, guaranteed_amount
// or amount, comment}).
func appGuarantors(c *call) []books.Guarantor {
	var out []books.Guarantor
	for _, it := range items(c.req) {
		id := appID(it.Str("member_id"))
		if id == nil {
			continue
		}
		g := books.Guarantor{MemberID: *id, Comment: it.Str("comment")}
		amt := appAmount(it.Str("guaranteed_amount"))
		if amt == "" {
			amt = appAmount(it.Str("amount"))
		}
		if amt != "" {
			g.Amount = &amt
		}
		out = append(out, g)
	}
	return out
}

// appLoan reads record_member_loan / edit_member_loan.
func appLoan(c *call, g *member) (books.NewLoan, map[string]string) {
	errs := map[string]string{}
	n := books.NewLoan{Amount: appAmount(c.req.Str("loan_amount")), Date: appDate(c.req.Str("disbursement_date"), g.loc),
		AccountID: appAccount(c.req.Str("account_id")), Installments: int(c.req.Int("repayment_period")),
		Guarantors: appGuarantors(c), SkipProcessingFee: c.req.Str("disable_automatic_loan_processing_income") == "1"}
	if m := appID(c.req.Str("member_id")); m != nil {
		n.MemberID = *m
	} else {
		errs["member_id"] = "The Member field is required."
	}
	if n.Amount == "" {
		errs["loan_amount"] = "The Loan Amount field is required."
	}
	if n.Date == nil {
		errs["disbursement_date"] = "The Disbursement Date field is required."
	}
	if n.AccountID == 0 {
		errs["account_id"] = "The Account field is required."
	}
	switch gp := c.req.Str("grace_period"); {
	case gp == "date":
		n.FirstDueOn = appDate(c.req.Str("grace_period_date"), g.loc)
		if n.FirstDueOn == nil {
			errs["grace_period_date"] = "The Grace Period Date field is required."
		}
	case gp != "":
		n.GracePeriods = int(c.req.Int("grace_period"))
	}
	if t := appID(c.req.Str("loan_type_id")); t != nil {
		n.LoanTypeID = t
		return n, errs
	}
	n.InterestType = map[string]string{"1": "fixed_balance", "2": "reducing_balance", "4": "fixed_amount"}[c.req.Str("interest_type")]
	if n.InterestType == "" {
		errs["interest_type"] = "The Interest Type field is required."
	}
	n.InterestRate, _ = strconv.ParseFloat(c.req.Str("interest_rate"), 64)
	n.RatePer = map[string]string{"1": "day", "2": "week", "3": "month", "4": "year", "5": "term"}[c.req.Str("loan_interest_rate_per")]
	n.Frequency = map[string]string{"1": "daily", "2": "weekly", "3": "monthly"}[c.req.Str("loan_repayment_frequency")]
	if c.req.Str("fixed_interest_type") == "1" {
		n.FixedInterest = appAmount(c.req.Str("fixed_interest_amount"))
	} else if v, err := strconv.ParseFloat(c.req.Str("fixed_percentage_interest"), 64); err == nil {
		n.FixedRate = v
	}
	n.FineDeferment = c.req.Str("enable_loan_fine_deferment") == "1"
	row := reqRow(c)
	var ok bool
	if n.LateFine, ok = legacy.LegacyLateFine(row); !ok {
		errs["loan_fine_type"] = "Enter the late loan payment fine settings in full."
	}
	if n.BalanceFine, ok = legacy.LegacyBalanceFine(row); !ok {
		errs["outstanding_loan_balance_fine_type"] = "Enter the outstanding loan balance fine settings in full."
	}
	if n.ProcessingFee, ok = legacy.LegacyProcessingFee(row); !ok {
		errs["loan_processing_fee_type"] = "Enter the loan processing fee settings in full."
	}
	return n, errs
}

var loanErrorFields = map[string]string{
	"invalid_member": "member_id", "invalid_account": "account_id", "account_closed": "account_id",
	"invalid_amount": "loan_amount", "amount_over_limit": "loan_amount", "amount_under_limit": "loan_amount",
	"invalid_installments": "repayment_period", "invalid_loan_type": "loan_type_id", "invalid_guarantor": "guarantors",
	"guarantors_required": "guarantors", "too_many_guarantors": "guarantors", "invalid_date": "disbursement_date",
}

func (s *Server) loanFailed(be books.Error) map[string]any {
	field := loanErrorFields[be.Code]
	if field == "" {
		field = "form"
	}
	return s.formFailed(map[string]string{field: be.Msg})
}

func (s *Server) recordMemberLoan(ctx context.Context, c *call, g *member) (any, error) {
	n, errs := appLoan(c, g)
	if len(errs) > 0 {
		return s.formFailed(errs), nil
	}
	err := s.inTx(ctx, func(q *store.Queries) error {
		_, err := books.RecordLoan(ctx, q, g.group.ID, g.user.ID, n)
		return err
	})
	if be, ok := asBooksError(err); ok {
		return s.loanFailed(be), nil
	}
	if err != nil {
		return nil, err
	}
	return timed(map[string]any{"status": 1, "success": "Successfully recorded member loan"}, s.now()), nil
}

// editMemberLoan records the loan again with the new terms. Legacy rewrote
// the loan in place; here the old loan is voided and a new one recorded,
// which is only allowed while nothing has been repaid on it.
func (s *Server) editMemberLoan(ctx context.Context, c *call, g *member) (any, error) {
	id := appID(c.req.Str("id"))
	if id == nil {
		return timed(status(0, "Loan details are missing"), s.now()), nil
	}
	n, errs := appLoan(c, g)
	if len(errs) > 0 {
		return s.formFailed(errs), nil
	}
	var reply map[string]any
	err := s.inTx(ctx, func(q *store.Queries) error {
		d, err := books.GetLoanDetail(ctx, q, g.group.ID, *id, s.today(g))
		if errors.Is(err, books.ErrNotFound) || (err == nil && d.Loan.VoidedAt != nil) {
			reply = timed(status(0, "Loan details not found"), s.now())
			return nil
		}
		if err != nil {
			return err
		}
		if d.Paid > 0 {
			reply = timed(status(0, "This loan has repayments. Void them first, or void the loan and record it again."), s.now())
			return nil
		}
		if err := books.VoidLoan(ctx, q, g.group.ID, *id, g.user.ID); err != nil {
			return err
		}
		_, err = books.RecordLoan(ctx, q, g.group.ID, g.user.ID, n)
		return err
	})
	if be, ok := asBooksError(err); ok {
		return s.loanFailed(be), nil
	}
	if err != nil || reply != nil {
		return reply, err
	}
	return timed(map[string]any{"status": 1, "success": "Successfully updated member loan", "message": "Successfully updated member loan"}, s.now()), nil
}

func (s *Server) voidMemberLoan(ctx context.Context, c *call, g *member) (any, error) {
	id := appID(c.req.Str("id"))
	if id == nil {
		return timed(status(0, "Loan details are missing"), s.now()), nil
	}
	err := s.inTx(ctx, func(q *store.Queries) error { return books.VoidLoan(ctx, q, g.group.ID, *id, g.user.ID) })
	if errors.Is(err, books.ErrNotFound) {
		return timed(status(0, "Not details not found"), s.now()), nil
	}
	if _, ok := asBooksError(err); ok {
		return timed(status(0, "Not details not found"), s.now()), nil
	}
	if err != nil {
		return nil, err
	}
	return timed(map[string]any{"status": 1}, s.now()), nil
}

// recordLoanRepayments is deposits/record_loan_repayments: an array of
// {member_id, loan_id, account_id, deposit_date, amount, deposit_method,
// description}, all recorded or none.
func (s *Server) recordLoanRepayments(ctx context.Context, c *call, g *member) (any, error) {
	list := items(c.req)
	if len(list) == 0 {
		return timed(status(0, "Error loan repayments array empty."), s.now()), nil
	}
	type rep struct {
		loan, member int64
		r            books.Repayment
	}
	var reps []rep
	for _, it := range list {
		r := books.Repayment{AccountID: appAccount(it.Str("account_id")), Amount: appAmount(it.Str("amount")),
			Date: appDate(it.Str("deposit_date"), g.loc), Method: appMethod(depositMethodCodes, it.Str("deposit_method")),
			Description: it.Str("description")}
		loanID, memberID := appID(it.Str("loan_id")), appID(it.Str("member_id"))
		if r.AccountID == 0 || r.Amount == "" || r.Date == nil || it.Str("deposit_method") == "" || loanID == nil || memberID == nil {
			return timed(status(0, "Error data validation errors, some values are missing."), s.now()), nil
		}
		reps = append(reps, rep{*loanID, *memberID, r})
	}
	err := s.inTx(ctx, func(q *store.Queries) error {
		for _, x := range reps {
			l, err := books.LockLoan(ctx, q, g.group.ID, x.loan)
			if err != nil {
				return err
			}
			if !books.Is(l.MembershipID, x.member) {
				return books.Error{Code: "invalid_loan", Msg: "That loan is not the member's."}
			}
			if _, err := books.RecordRepayment(ctx, q, g.group.ID, g.user.ID, x.loan, x.r); err != nil {
				return err
			}
		}
		return nil
	})
	if errors.Is(err, books.ErrNotFound) {
		return timed(status(0, "Error adding transactions. Loan not found."), s.now()), nil
	}
	if be, ok := asBooksError(err); ok {
		return timed(status(0, "Error adding transactions. "+be.Msg), s.now()), nil
	}
	if err != nil {
		return nil, err
	}
	return timed(status(1, "Successfully added transactions"), s.now()), nil
}

// loanBook is every live loan of the group with its totals, for the
// dashboards and loan reports.
type loanBook struct {
	rows                  []store.ListLoansRow
	lent, repaid, balance int64
	byMember              map[int64]int64 // balance owed per member
}

func (s *Server) loanBook(ctx context.Context, g *member, who *int64) (*loanBook, error) {
	rows, err := s.listLoans(ctx, g, who, "", 100000, 0)
	if err != nil {
		return nil, err
	}
	b := &loanBook{rows: rows, byMember: map[int64]int64{}}
	for _, r := range rows {
		b.lent += books.Cents(r.Amount)
		b.repaid += books.Cents(r.Paid)
		if bal := books.Cents(r.Balance); bal > 0 {
			b.balance += bal
			b.byMember[deref64(r.MembershipID)] += bal
		}
	}
	return b, nil
}

// loansSummary is reports/loans_summary: one line per loan. Profit is the
// interest and fines share of what has been repaid.
func (s *Server) loansSummary(ctx context.Context, c *call, g *member) (any, error) {
	var who *int64
	if !g.official() {
		who = &g.m.ID
	}
	b, err := s.loanBook(ctx, g, who)
	if err != nil {
		return nil, err
	}
	var tLoan, tInterest, tPaid, tBalance, tProfit, tProjected int64
	body := []any{}
	for i := len(b.rows) - 1; i >= 0; i-- {
		r := b.rows[i]
		amt, payable, paid := books.Cents(r.Amount), books.Cents(r.Payable), books.Cents(r.Paid)
		interest, fines := books.Cents(r.Interest), books.Cents(r.Fines)
		projected := interest + fines
		var profit int64
		if payable > 0 {
			profit = min(paid, payable) * projected / payable
		}
		body = append(body, map[string]any{
			"position": len(body) + 1, "member": r.MemberName,
			"disbursement_date": pickerDate(r.DisbursedOn) + " - " + pickerDate(r.EndsOn),
			"amount":            float64(amt) / 100, "interest": float64(interest) / 100, "amount_paid": float64(paid) / 100,
			"balance": float64(payable-paid) / 100, "projected_profit": float64(profit) / 100,
			"outstanding_profit": float64(projected-profit) / 100, "projected_profits": float64(projected) / 100,
		})
		tLoan, tInterest, tPaid, tBalance = tLoan+amt, tInterest+interest, tPaid+paid, tBalance+payable-paid
		tProfit, tProjected = tProfit+profit, tProjected+projected
	}
	return timed(map[string]any{"status": 1, "message": "successful", "data": map[string]any{
		"statement_body": body,
		"statement_footer": map[string]any{"total_loan": float64(tLoan) / 100, "total_interest": float64(tInterest) / 100,
			"total_paid": float64(tPaid) / 100, "total_balance": float64(tBalance) / 100, "total_profits": float64(tProfit) / 100,
			"total_projected_profit": float64(tProjected) / 100, "total_outstanding_profit": float64(tProjected-tProfit) / 100},
	}}, s.now()), nil
}

// memberLoansSummary is get_group_member_loans_summary: the caller's open
// loans and their next installment.
func (s *Server) memberLoansSummary(ctx context.Context, c *call, g *member) (any, error) {
	if g.m.Status != store.MembershipStatusActive {
		return suspendedReply(s.now()), nil
	}
	group, err := s.loanBook(ctx, g, nil)
	if err != nil {
		return nil, err
	}
	today := s.today(g)
	balances := []any{}
	var lent, nextAmount int64
	var nextDate *time.Time
	for _, r := range group.rows {
		if !books.Is(r.MembershipID, g.m.ID) || books.Cents(r.Balance) <= 0 {
			continue
		}
		lent += books.Cents(r.Amount)
		balances = append(balances, map[string]any{"id": fmt.Sprint(r.ID), "name": loanTypeName(r),
			"amount": amount(r.Amount), "balance": amount(r.Balance), "disbursement_date": mobileShortTime(r.DisbursedOn)})
		d, err := books.GetLoanDetail(ctx, s.q, g.group.ID, r.ID, today)
		if err != nil {
			return nil, err
		}
		if n := d.NextDue; n != nil {
			due := n.DueOn
			switch {
			case nextDate == nil || due.Before(*nextDate):
				nextDate, nextAmount = &due, books.Cents(n.Amount)-n.Paid
			case due.Equal(*nextDate):
				nextAmount += books.Cents(n.Amount) - n.Paid
			}
		}
	}
	summary := map[string]any{"loan_balances": balances, "total_loan_amount": float64(lent) / 100,
		"next_instalment_amount": float64(nextAmount) / 100, "next_instalment_date": "", "days_to_next_instalment": 0}
	if nextDate != nil {
		summary["next_instalment_date"] = mobileShortTime(*nextDate)
		summary["days_to_next_instalment"] = int(nextDate.Sub(today).Hours() / 24)
	}
	return timed(map[string]any{"status": 1, "total_group_loan_balance": float64(group.lent) / 100,
		"member_loans_summary": summary}, s.now()), nil
}
