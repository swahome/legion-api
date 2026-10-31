package mobile

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/swahome/legion-api/internal/books"
	"github.com/swahome/legion-api/internal/store"
)

// Recording money from the app: deposits, expenses, refunds, transfers,
// fines and invoices, and voiding them. The writes go through
// internal/books, the same code the web API uses; this file only reads the
// app's request shapes and answers in the legacy wording.

type groupFn = func(ctx context.Context, c *call, g *member) (any, error)

// officialsOnly is the legacy "group_role_id || is_admin" check.
func (s *Server) officialsOnly(fn groupFn) groupFn {
	return func(ctx context.Context, c *call, g *member) (any, error) {
		if !g.official() {
			return timed(status(0, "You are not allowed to perform this request. For group admins only"), s.now()), nil
		}
		return fn(ctx, c, g)
	}
}

// inTx runs fn with queries bound to one transaction.
func (s *Server) inTx(ctx context.Context, fn func(q *store.Queries) error) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error { return fn(s.q.WithTx(tx)) })
}

// asBooksError reports whether err is a validation failure from books.
func asBooksError(err error) (books.Error, bool) {
	var be books.Error
	ok := errors.As(err, &be)
	return be, ok
}

// formFailed is the legacy "Form validation failed" answer.
func (s *Server) formFailed(errs map[string]string) map[string]any {
	m := timed(status(0, "Form validation failed"), s.now())
	m["validation_errors"] = errs
	return m
}

// errorFields says which request field a books error code is about.
var errorFields = map[string]string{
	"invalid_account": "account_id", "account_closed": "account_id", "insufficient_balance": "amount",
	"invalid_amount": "amount", "refund_too_large": "amount", "invalid_method": "method",
	"invalid_member": "member_id", "invalid_contribution": "contribution_id",
	"invalid_fine_category": "fine_category_id", "invalid_income_category": "income_category_id",
	"invalid_depositor": "depositor_id", "invalid_expense_category": "expense_category_id",
	"same_account": "to_account_id",
}

// booksFailed turns a books validation error into "Form validation failed"
// on the field it concerns. dateField and methodField name the action's
// own date and method fields.
func (s *Server) booksFailed(be books.Error, dateField, methodField string) map[string]any {
	field := errorFields[be.Code]
	switch {
	case be.Code == "invalid_date" || (be.Code == "missing_fields" && strings.HasPrefix(be.Msg, "date")):
		field = dateField
	case field == "method":
		field = methodField
	case field == "":
		field = "form"
	}
	return s.formFailed(map[string]string{field: be.Msg})
}

// required checks the fields a legacy form required, in order, and names
// the first one missing as CodeIgniter did.
func (s *Server) required(c *call, fields ...[2]string) map[string]any {
	errs := map[string]string{}
	for _, f := range fields {
		if c.req.Str(f[0]) == "" {
			errs[f[0]] = "The " + f[1] + " field is required."
		}
	}
	if len(errs) > 0 {
		return s.formFailed(errs)
	}
	return nil
}

// ---- Request values ----

// appDateLayouts are the date shapes the app has sent, read the way PHP's
// strtotime reads them: day-month-year with dashes, month/day/year with
// slashes.
var appDateLayouts = []string{
	"2006-01-02", "2-1-2006", "02-01-2006", "1/2/2006", "01/02/2006", "2006/01/02",
	"2 Jan 2006", "02 Jan 2006", "2 January 2006", "Jan 2, 2006", "January 2, 2006", "2 Jan, 2006",
	"Mon Jan 2 2006", "Mon, 2 Jan 2006", "2006-01-02 15:04:05", "2006-01-02 15:04", "02-01-2006 15:04:05",
	"2006-01-02T15:04:05Z07:00", "2006-01-02T15:04:05",
}

// appDate reads a date the app sent: a unix timestamp (seconds or
// milliseconds) or a date string. It returns the calendar date in the
// group's time zone, or nil.
func appDate(v string, loc *time.Location) *time.Time {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	day := func(t time.Time) *time.Time {
		d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
		return &d
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n <= 0 {
			return nil
		}
		if n > 1e11 {
			n /= 1000
		}
		return day(time.Unix(n, 0).In(loc))
	}
	for _, l := range appDateLayouts {
		if t, err := time.ParseInLocation(l, v, loc); err == nil {
			return day(t.In(loc))
		}
	}
	return nil
}

// appAmount reads an amount the way the legacy currency() did (commas
// dropped) and returns it with at most two decimals, or "" if it isn't one.
func appAmount(v string) string {
	v = strings.ReplaceAll(strings.TrimSpace(v), ",", "")
	if books.ValidAmount(v) {
		return v
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 || math.IsInf(f, 0) || math.IsNaN(f) || f >= 1e12 {
		return ""
	}
	return strconv.FormatFloat(f, 'f', 2, 64)
}

// appAccount reads an account id: "bank-12" as the app names accounts, or
// a plain id.
func appAccount(v string) int64 {
	if _, id, ok := parseAccountID(v); ok {
		return id
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	return n
}

func appID(v string) *int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil || n <= 0 {
		return nil
	}
	return &n
}

// appFineRef reads a fine the app names as "contribution-5" (a
// contribution's late fine), "fine_category-3" or a plain category id.
func appFineRef(v string) (contributionID, fineCategoryID *int64) {
	v = strings.TrimSpace(v)
	if rest, ok := strings.CutPrefix(v, "contribution-"); ok {
		return appID(rest), nil
	}
	return nil, appID(strings.TrimPrefix(v, "fine_category-"))
}

// Legacy payment method numbers.
var (
	depositMethodCodes = map[string]store.PaymentMethod{
		"1": "mpesa", "2": "cash", "3": "cheque", "4": "funds_transfer", "5": "equitel", "6": "standing_order",
	}
	withdrawalMethodCodes = map[string]store.PaymentMethod{
		"1": "cash", "2": "mpesa", "3": "cheque", "4": "funds_transfer", "5": "equitel",
	}
)

// appMethod maps a method number; unknown numbers record no method, as the
// legacy code stored whatever number it was given.
func appMethod(codes map[string]store.PaymentMethod, v string) *string {
	if m, ok := codes[strings.TrimSpace(v)]; ok {
		s := string(m)
		return &s
	}
	return nil
}

// items collects the objects of every array in the request: the legacy
// recording actions took "any array of payments", whatever its key.
func items(r request) []request {
	var out []request
	for _, v := range r {
		list, ok := v.([]any)
		if !ok {
			continue
		}
		for _, x := range list {
			if m, ok := x.(map[string]any); ok {
				out = append(out, request(m))
			}
		}
	}
	return out
}

// memberAmount is one member's share of a payment.
type memberAmount struct {
	id     int64
	amount string
}

// individualPayments reads {"<member id>": "<amount>"} (or a list of
// {member_id, amount}). ok is false when an entry is incomplete.
func individualPayments(r request) (out []memberAmount, msg string) {
	add := func(id, amt string) bool {
		mid, a := appID(id), appAmount(amt)
		if mid == nil || a == "" {
			return false
		}
		out = append(out, memberAmount{*mid, a})
		return true
	}
	switch v := r["individual_payments"].(type) {
	case map[string]any:
		for id, amt := range v {
			if !add(id, request{"v": amt}.Str("v")) {
				return nil, "Individual payments fields must all be filled"
			}
		}
	case []any:
		for _, x := range v {
			m, _ := x.(map[string]any)
			if !add(request(m).Str("member_id"), request(m).Str("amount")) {
				return nil, "Individual payments fields must all be filled"
			}
		}
	}
	if len(out) == 0 {
		return nil, "Select atleast 1 member to record individual payment"
	}
	return out, ""
}

// ---- Deposits ----

// depositItem reads one payment of the legacy array actions. ok is false
// when a value the legacy validation required is missing.
func depositItem(kind store.DepositKind, it request, loc *time.Location) (books.Deposit, bool) {
	d := books.Deposit{
		Kind: kind, AccountID: appAccount(it.Str("account_id")), Amount: appAmount(it.Str("amount")),
		Date: appDate(it.Str("deposit_date"), loc), Method: appMethod(depositMethodCodes, it.Str("deposit_method")),
		Description: it.Str("description"),
	}
	ok := d.AccountID > 0 && d.Amount != "" && d.Date != nil && it.Str("deposit_method") != ""
	switch kind {
	case store.DepositKindContribution:
		d.MemberID, d.ContributionID = appID(it.Str("member_id")), appID(it.Str("contribution_id"))
		ok = ok && d.MemberID != nil && d.ContributionID != nil
	case store.DepositKindFine:
		d.MemberID = appID(it.Str("member_id"))
		d.ContributionID, d.FineCategoryID = appFineRef(it.Str("fine_category_id"))
		ok = ok && d.MemberID != nil && (d.ContributionID != nil || d.FineCategoryID != nil)
	case store.DepositKindMisc:
		d.MemberID = appID(it.Str("member_id"))
		ok = ok && d.MemberID != nil
	case store.DepositKindIncome:
		d.IncomeCategoryID, d.DepositorID = appID(it.Str("income_category_id")), appID(it.Str("depositor_id"))
		ok = ok && d.IncomeCategoryID != nil
	}
	return d, ok
}

// recordDeposits is record_contribution_payments, record_fine_payments,
// record_miscellaneous_payments and record_income: an array of payments,
// all recorded or none.
func (s *Server) recordDeposits(kind store.DepositKind, emptyMsg string) groupFn {
	return s.officialsOnly(func(ctx context.Context, c *call, g *member) (any, error) {
		list := items(c.req)
		if len(list) == 0 {
			return timed(status(0, emptyMsg), s.now()), nil
		}
		deposits := make([]books.Deposit, 0, len(list))
		for _, it := range list {
			d, ok := depositItem(kind, it, g.loc)
			if !ok {
				return timed(status(0, "Error data validation errors, some values are missing."), s.now()), nil
			}
			deposits = append(deposits, d)
		}
		err := s.inTx(ctx, func(q *store.Queries) error {
			for _, d := range deposits {
				if _, err := books.RecordDeposit(ctx, q, g.group.ID, g.user.ID, d); err != nil {
					return err
				}
			}
			return nil
		})
		if be, ok := asBooksError(err); ok {
			return timed(status(0, "Error adding transactions. "+be.Msg), s.now()), nil
		}
		if err != nil {
			return nil, err
		}
		return timed(status(1, "Successfully added transactions"), s.now()), nil
	})
}

// payers is who a new_record_* payment is for: every active member at the
// same amount, or the individual_payments.
func (s *Server) payers(ctx context.Context, c *call, g *member, all bool) ([]memberAmount, map[string]any, error) {
	if !all {
		list, msg := individualPayments(c.req)
		if msg != "" {
			return nil, s.formFailed(map[string]string{"member_type_id": msg}), nil
		}
		return list, nil, nil
	}
	amt := appAmount(c.req.Str("amount"))
	if amt == "" {
		return nil, s.formFailed(map[string]string{"member_type_id": "Amount is required and must be currency"}), nil
	}
	ids, err := s.q.ActiveMembershipIDs(ctx, g.group.ID)
	if err != nil {
		return nil, nil, err
	}
	out := make([]memberAmount, len(ids))
	for i, id := range ids {
		out[i] = memberAmount{id, amt}
	}
	return out, nil, nil
}

// newMemberDeposits is new_record_contribution_payments and
// new_record_fine_payments: one payment, for chosen members or everyone.
func (s *Server) newMemberDeposits(kind store.DepositKind) groupFn {
	return s.officialsOnly(func(ctx context.Context, c *call, g *member) (any, error) {
		refField, refLabel := "contribution_id", "Contribution"
		if kind == store.DepositKindFine {
			refField, refLabel = "fine_category_id", "Fine Category"
		}
		if reply := s.required(c, [2]string{"deposit_date", "Deposit Date"}, [2]string{"deposit_method", "Deposit Method"},
			[2]string{refField, refLabel}, [2]string{"account_id", "Account Id"}); reply != nil {
			return reply, nil
		}
		// Contributions: 2 is everyone. Fines: 1 is chosen members. As legacy.
		all := c.req.Str("member_type_id") == "2"
		if kind == store.DepositKindFine {
			all = c.req.Str("member_type_id") != "1"
		}
		members, reply, err := s.payers(ctx, c, g, all)
		if err != nil || reply != nil {
			return reply, err
		}
		base := books.Deposit{
			Kind: kind, AccountID: appAccount(c.req.Str("account_id")), Date: appDate(c.req.Str("deposit_date"), g.loc),
			Method: appMethod(depositMethodCodes, c.req.Str("deposit_method")), Description: c.req.Str("description"),
		}
		if base.Date == nil {
			return s.formFailed(map[string]string{"deposit_date": "The Deposit Date field must contain a valid date."}), nil
		}
		if kind == store.DepositKindFine {
			base.ContributionID, base.FineCategoryID = appFineRef(c.req.Str("fine_category_id"))
		} else {
			base.ContributionID = appID(c.req.Str("contribution_id"))
		}
		err = s.inTx(ctx, func(q *store.Queries) error {
			for _, m := range members {
				d := base
				d.MemberID, d.Amount = &m.id, m.amount
				if _, err := books.RecordDeposit(ctx, q, g.group.ID, g.user.ID, d); err != nil {
					return err
				}
			}
			return nil
		})
		if be, ok := asBooksError(err); ok {
			return s.booksFailed(be, "deposit_date", "deposit_method"), nil
		}
		if err != nil {
			return nil, err
		}
		if kind == store.DepositKindFine {
			return status(1, "Successfully added transactions"), nil
		}
		return status(1, "Contributions recorded successfully."), nil
	})
}

// newDeposit is new_record_miscellaneous_payments and
// new_record_income_payment: one payment.
func (s *Server) newDeposit(kind store.DepositKind) groupFn {
	return s.officialsOnly(func(ctx context.Context, c *call, g *member) (any, error) {
		fields := [][2]string{{"deposit_date", "Deposit Date"}, {"deposit_method", "Deposit Method"}}
		if kind == store.DepositKindIncome {
			fields = append(fields, [2]string{"depositor_id", "Depositor"}, [2]string{"income_category_id", "Income Category"})
		} else {
			fields = append(fields, [2]string{"member_id", "Member"})
		}
		fields = append(fields, [2]string{"account_id", "Account Id"}, [2]string{"amount", "Deposit Amount"})
		if reply := s.required(c, fields...); reply != nil {
			return reply, nil
		}
		d := books.Deposit{
			Kind: kind, AccountID: appAccount(c.req.Str("account_id")), Amount: appAmount(c.req.Str("amount")),
			Date: appDate(c.req.Str("deposit_date"), g.loc), Method: appMethod(depositMethodCodes, c.req.Str("deposit_method")),
			Description: c.req.Str("description"),
		}
		if d.Date == nil {
			return s.formFailed(map[string]string{"deposit_date": "The Deposit Date field must contain a valid date."}), nil
		}
		if kind == store.DepositKindIncome {
			d.DepositorID, d.IncomeCategoryID = appID(c.req.Str("depositor_id")), appID(c.req.Str("income_category_id"))
		} else {
			d.MemberID = appID(c.req.Str("member_id"))
		}
		err := s.inTx(ctx, func(q *store.Queries) error {
			_, err := books.RecordDeposit(ctx, q, g.group.ID, g.user.ID, d)
			return err
		})
		if be, ok := asBooksError(err); ok {
			return s.booksFailed(be, "deposit_date", "deposit_method"), nil
		}
		if err != nil {
			return nil, err
		}
		if kind == store.DepositKindIncome {
			return status(1, "Income payment successfully recorded"), nil
		}
		return status(1, "Miscellaneous payment successfully recorded"), nil
	})
}

// voidDeposit is deposits/void.
func (s *Server) voidDeposit(ctx context.Context, c *call, g *member) (any, error) {
	id := appID(c.req.Str("id"))
	if id == nil {
		return status(0, "Invalid deposit tag"), nil
	}
	err := s.inTx(ctx, func(q *store.Queries) error { return books.VoidDeposit(ctx, q, g.group.ID, *id, g.user.ID) })
	if errors.Is(err, books.ErrNotFound) {
		return status(0, "Error occured while voiding"), nil
	}
	if be, ok := asBooksError(err); ok {
		return timed(status(0, be.Msg), s.now()), nil
	}
	if err != nil {
		return nil, err
	}
	return status(1, "success"), nil
}

// ---- Withdrawals ----

func withdrawalSuccess(now time.Time, msg string) map[string]any {
	return map[string]any{"status": 1, "time": now.Unix(), "success": msg}
}

// recordExpenses is withdrawals/record_expenses: an array of expenses.
func (s *Server) recordExpenses(ctx context.Context, c *call, g *member) (any, error) {
	list := items(c.req)
	if len(list) == 0 {
		return timed(status(0, "Error expenses array empty."), s.now()), nil
	}
	type expense struct {
		account int64
		w       books.Withdrawal
		date    *time.Time
		method  *string
		desc    string
	}
	expenses := make([]expense, 0, len(list))
	for _, it := range list {
		e := expense{
			account: appAccount(it.Str("account_id")), date: appDate(it.Str("expense_date"), g.loc),
			method: appMethod(withdrawalMethodCodes, it.Str("withdrawal_method")), desc: it.Str("description"),
			w: books.Withdrawal{Kind: store.WithdrawalKindExpense, Amount: appAmount(it.Str("amount")),
				ExpenseCategoryID: appID(it.Str("expense_category_id"))},
		}
		if e.account <= 0 || e.date == nil || e.w.Amount == "" || e.w.ExpenseCategoryID == nil || it.Str("withdrawal_method") == "" {
			return timed(status(0, "Error data validation errors, some values are missing."), s.now()), nil
		}
		expenses = append(expenses, e)
	}
	err := s.inTx(ctx, func(q *store.Queries) error {
		for _, e := range expenses {
			if _, err := books.RecordWithdrawal(ctx, q, g.group.ID, g.user.ID, e.account, e.w, e.method, e.date, e.desc); err != nil {
				return err
			}
		}
		return nil
	})
	if be, ok := asBooksError(err); ok {
		return timed(status(0, "Error adding transactions. "+be.Msg), s.now()), nil
	}
	if err != nil {
		return nil, err
	}
	return withdrawalSuccess(s.now(), "Successfully added transactions"), nil
}

// newRecordExpense is withdrawals/new_record_expenses: one expense.
func (s *Server) newRecordExpense(ctx context.Context, c *call, g *member) (any, error) {
	if reply := s.required(c, [2]string{"expense_date", "Expense Date"}, [2]string{"expense_category_id", "Expense Category"},
		[2]string{"account_id", "Account"}, [2]string{"withdrawal_method", "Withdrawal Method"},
		[2]string{"amount", "Amount"}); reply != nil {
		return reply, nil
	}
	date := appDate(c.req.Str("expense_date"), g.loc)
	if date == nil {
		return s.formFailed(map[string]string{"expense_date": "The Expense Date field must contain a valid date."}), nil
	}
	w := books.Withdrawal{Kind: store.WithdrawalKindExpense, Amount: appAmount(c.req.Str("amount")),
		ExpenseCategoryID: appID(c.req.Str("expense_category_id"))}
	err := s.inTx(ctx, func(q *store.Queries) error {
		_, err := books.RecordWithdrawal(ctx, q, g.group.ID, g.user.ID, appAccount(c.req.Str("account_id")), w,
			appMethod(withdrawalMethodCodes, c.req.Str("withdrawal_method")), date, c.req.Str("description"))
		return err
	})
	if be, ok := asBooksError(err); ok {
		return s.booksFailed(be, "expense_date", "withdrawal_method"), nil
	}
	if err != nil {
		return nil, err
	}
	return status(1, "Successfully recorded expense payment"), nil
}

// recordContributionRefund is withdrawals/record_contribution_refund.
func (s *Server) recordContributionRefund(ctx context.Context, c *call, g *member) (any, error) {
	if reply := s.required(c, [2]string{"member_id", "Member"}, [2]string{"refund_date", "Refund Date"},
		[2]string{"contribution_id", "Contribution"}, [2]string{"account_id", "Account"},
		[2]string{"refund_method", "Refund Method"}, [2]string{"amount", "Amount"}); reply != nil {
		return reply, nil
	}
	date := appDate(c.req.Str("refund_date"), g.loc)
	if date == nil {
		return s.formFailed(map[string]string{"refund_date": "The Refund Date field must contain a valid date."}), nil
	}
	w := books.Withdrawal{Kind: store.WithdrawalKindContributionRefund, Amount: appAmount(c.req.Str("amount")),
		MemberID: appID(c.req.Str("member_id")), ContributionID: appID(c.req.Str("contribution_id"))}
	err := s.inTx(ctx, func(q *store.Queries) error {
		_, err := books.RecordWithdrawal(ctx, q, g.group.ID, g.user.ID, appAccount(c.req.Str("account_id")), w,
			appMethod(withdrawalMethodCodes, c.req.Str("refund_method")), date, c.req.Str("description"))
		return err
	})
	if be, ok := asBooksError(err); ok {
		return s.booksFailed(be, "refund_date", "refund_method"), nil
	}
	if err != nil {
		return nil, err
	}
	return withdrawalSuccess(s.now(), "Successfully recorded a transaction"), nil
}

// recordFundsTransfer is withdrawals/record_funds_transfer: between two of
// the group's accounts. As before, the source account must hold the amount.
func (s *Server) recordFundsTransfer(ctx context.Context, c *call, g *member) (any, error) {
	if reply := s.required(c, [2]string{"transfer_date", "Transfer Date"}, [2]string{"from_account_id", "From Account"},
		[2]string{"to_account_id", "To Account"}, [2]string{"amount", "Amount"}); reply != nil {
		return reply, nil
	}
	date := appDate(c.req.Str("transfer_date"), g.loc)
	if date == nil {
		return s.formFailed(map[string]string{"transfer_date": "The Transfer Date field must contain a valid date."}), nil
	}
	from, to, amt := appAccount(c.req.Str("from_account_id")), appAccount(c.req.Str("to_account_id")), appAmount(c.req.Str("amount"))
	err := s.inTx(ctx, func(q *store.Queries) error {
		if acct, err := books.OpenAccount(ctx, q, g.group.ID, from); err != nil {
			return err
		} else if err := books.CheckBalance(ctx, q, acct, amt); amt != "" && err != nil {
			return err
		}
		_, err := books.RecordTransfer(ctx, q, g.group.ID, g.user.ID, from, to, amt, date, c.req.Str("description"))
		return err
	})
	if be, ok := asBooksError(err); ok {
		if be.Code == "invalid_account" || be.Code == "account_closed" {
			return s.formFailed(map[string]string{"from_account_id": be.Msg}), nil
		}
		return s.booksFailed(be, "transfer_date", "method"), nil
	}
	if err != nil {
		return nil, err
	}
	return withdrawalSuccess(s.now(), "Successfully recorded a transaction"), nil
}

// voidWithdrawal is withdrawals/void.
func (s *Server) voidWithdrawal(ctx context.Context, c *call, g *member) (any, error) {
	id := appID(c.req.Str("withdrawal_id"))
	if id == nil {
		return timed(status(0, "Invoice details are missing"), s.now()), nil
	}
	err := s.inTx(ctx, func(q *store.Queries) error { return books.VoidWithdrawal(ctx, q, g.group.ID, *id, g.user.ID) })
	if errors.Is(err, books.ErrNotFound) {
		return timed(status(0, "Withdrawal request trying to void is not available. Refresh page and try again"), s.now()), nil
	}
	if be, ok := asBooksError(err); ok {
		return timed(status(0, be.Msg), s.now()), nil
	}
	if err != nil {
		return nil, err
	}
	return timed(status(1, "success"), s.now()), nil
}

// ---- Fines and invoices ----

// fineMembersBy raises a fine (a fine category or a contribution's late
// fine) on each member, all or none, and returns how many were fined.
func (s *Server) fineMembersBy(ctx context.Context, g *member, memberIDs []int64, ref, amount string, date *time.Time,
	description string) (int, error) {
	contributionID, categoryID := appFineRef(ref)
	if contributionID == nil {
		if categoryID == nil {
			return 0, books.Error{Code: "invalid_fine_category", Msg: "Pick one of the group's fine categories."}
		}
		var ids []int64
		err := s.inTx(ctx, func(q *store.Queries) (err error) {
			ids, err = books.FineMembers(ctx, q, g.group.ID, g.user.ID, memberIDs, *categoryID, amount, date, description)
			return err
		})
		return len(ids), err
	}
	return s.raiseInvoices(ctx, g, store.LedgerEntryTypeContributionFineInvoice, memberIDs, contributionID, nil, amount,
		date, date, description)
}

// raiseInvoices charges each member (duplicates ignored), all or none.
func (s *Server) raiseInvoices(ctx context.Context, g *member, typ store.LedgerEntryType, memberIDs []int64,
	contributionID, fineCategoryID *int64, amount string, date, due *time.Time, description string) (int, error) {
	if err := books.CheckAmountDate(amount, date, nil, "invoice"); err != nil {
		return 0, err
	}
	if due == nil || due.Before(*date) {
		return 0, books.Error{Code: "invalid_due_date", Msg: "The due date can't be before the invoice date."}
	}
	if len(memberIDs) == 0 || len(memberIDs) > 2000 {
		return 0, books.Error{Code: "missing_fields", Msg: "Member list error"}
	}
	n := 0
	err := s.inTx(ctx, func(q *store.Queries) error {
		if contributionID != nil {
			if err := books.Found("invalid_contribution", "Contribution selected does not exist in this group")(
				q.ContributionInGroup(ctx, store.ContributionInGroupParams{GroupID: g.group.ID, ID: *contributionID})); err != nil {
				return err
			}
		}
		if fineCategoryID != nil {
			if err := books.Found("invalid_fine_category", "Fine Category selected does not exist in this group")(
				q.FineCategoryInGroup(ctx, store.FineCategoryInGroupParams{GroupID: g.group.ID, ID: *fineCategoryID})); err != nil {
				return err
			}
		}
		seen := map[int64]bool{}
		for _, mid := range memberIDs {
			if seen[mid] {
				continue
			}
			seen[mid] = true
			if err := books.Found("invalid_member", "One of those members is not in this group.")(
				q.ActiveMembershipInGroup(ctx, store.ActiveMembershipInGroupParams{GroupID: g.group.ID, ID: mid})); err != nil {
				return err
			}
			m := mid
			if _, err := q.CreateInvoice(ctx, store.CreateInvoiceParams{
				GroupID: g.group.ID, MembershipID: &m, ContributionID: contributionID, FineCategoryID: fineCategoryID,
				EntryType: typ, Amount: amount, InvoiceDate: *date, DueDate: due, Description: strings.TrimSpace(description),
				CreatedBy: &g.user.ID,
			}); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	return n, err
}

// memberIDsOf reads member ids from a list of ids or of {member_id, ...}.
func memberIDsOf(r request, keys ...string) []int64 {
	var out []int64
	for _, k := range keys {
		list, _ := r[k].([]any)
		for _, x := range list {
			v := request{"v": x}.Str("v")
			if m, ok := x.(map[string]any); ok {
				v = request(m).Str("member_id")
			}
			if id := appID(v); id != nil {
				out = append(out, *id)
			}
		}
	}
	return out
}

// fineMembers is fines/fine_members: members [{member_id, member_name}].
func (s *Server) fineMembers(ctx context.Context, c *call, g *member) (any, error) {
	if reply := s.required(c, [2]string{"fine_date", "Fine Date"}, [2]string{"fine_category_id", "Fine Category"},
		[2]string{"amount", "Amount"}); reply != nil {
		return reply, nil
	}
	ids := memberIDsOf(c.req, "members")
	if len(ids) == 0 {
		return s.formFailed(map[string]string{"members": "Select at least one member to fine"}), nil
	}
	n, err := s.fineMembersBy(ctx, g, ids, c.req.Str("fine_category_id"), appAmount(c.req.Str("amount")),
		appDate(c.req.Str("fine_date"), g.loc), c.req.Str("description"))
	if be, ok := asBooksError(err); ok {
		return s.booksFailed(be, "fine_date", "method"), nil
	}
	if err != nil {
		return nil, err
	}
	return timed(map[string]any{"status": "1", "message": fmt.Sprintf("%d members fined", n)}, s.now()), nil
}

// newFineMembers is fines/new_fine_members: member_type_id 2 fines every
// active member, 1 the listed members.
func (s *Server) newFineMembers(ctx context.Context, c *call, g *member) (any, error) {
	if reply := s.required(c, [2]string{"fine_date", "Fine Date"}, [2]string{"fine_category_id", "Fine Category"},
		[2]string{"amount", "Amount"}); reply != nil {
		return reply, nil
	}
	var ids []int64
	switch c.req.Str("member_type_id") {
	case "2":
		var err error
		if ids, err = s.q.ActiveMembershipIDs(ctx, g.group.ID); err != nil {
			return nil, err
		}
	case "1":
		ids = memberIDsOf(c.req, "members")
	default:
		return status(0, "Member type is invalid"), nil
	}
	if len(ids) == 0 {
		return s.formFailed(map[string]string{"members": "Select at least one member to fine"}), nil
	}
	n, err := s.fineMembersBy(ctx, g, ids, c.req.Str("fine_category_id"), appAmount(c.req.Str("amount")),
		appDate(c.req.Str("fine_date"), g.loc), c.req.Str("description"))
	if be, ok := asBooksError(err); ok {
		return s.booksFailed(be, "fine_date", "method"), nil
	}
	if err != nil {
		return nil, err
	}
	return status(1, fmt.Sprintf("Successfully fined %d members", n)), nil
}

// invoiceTypeCodes are the legacy invoice types.
var invoiceTypeCodes = map[string]store.LedgerEntryType{
	"1": store.LedgerEntryTypeContributionInvoice, "2": store.LedgerEntryTypeContributionFineInvoice,
	"3": store.LedgerEntryTypeFineInvoice, "4": store.LedgerEntryTypeMiscInvoice,
}

// createInvoices is invoices/create: send_to 1 invoices every active member,
// otherwise member_ids (or members).
func (s *Server) createInvoices(ctx context.Context, c *call, g *member) (any, error) {
	if reply := s.required(c, [2]string{"type", "Invoice Type"}, [2]string{"send_to", "Send To"},
		[2]string{"amount_payable", "Amount Payable"}, [2]string{"invoice_date", "Invoice Date"},
		[2]string{"due_date", "Due Date"}); reply != nil {
		return reply, nil
	}
	typ, ok := invoiceTypeCodes[c.req.Str("type")]
	if !ok {
		return s.formFailed(map[string]string{"type": "Invoice type is invalid"}), nil
	}
	var contributionID, fineCategoryID *int64
	switch typ {
	case store.LedgerEntryTypeContributionInvoice, store.LedgerEntryTypeContributionFineInvoice:
		if contributionID = appID(c.req.Str("contribution_id")); contributionID == nil {
			return s.formFailed(map[string]string{"contribution_id": "The Contribution field is required."}), nil
		}
	case store.LedgerEntryTypeFineInvoice:
		_, fineCategoryID = appFineRef(c.req.Str("fine_category_id"))
		if fineCategoryID == nil {
			return s.formFailed(map[string]string{"fine_category_id": "The Fine Category field is required."}), nil
		}
	}
	var ids []int64
	if c.req.Str("send_to") == "1" {
		var err error
		if ids, err = s.q.ActiveMembershipIDs(ctx, g.group.ID); err != nil {
			return nil, err
		}
	} else {
		ids = memberIDsOf(c.req, "member_ids", "members")
	}
	if len(ids) == 0 {
		return status(0, "Member list error"), nil
	}
	date := appDate(c.req.Str("invoice_date"), g.loc)
	due := appDate(c.req.Str("due_date"), g.loc)
	if typ == store.LedgerEntryTypeFineInvoice {
		due = date
	}
	_, err := s.raiseInvoices(ctx, g, typ, ids, contributionID, fineCategoryID, appAmount(c.req.Str("amount_payable")),
		date, due, c.req.Str("description"))
	if be, ok := asBooksError(err); ok {
		if be.Code == "invalid_amount" {
			return s.formFailed(map[string]string{"amount_payable": be.Msg}), nil
		}
		if be.Code == "invalid_due_date" {
			return s.formFailed(map[string]string{"due_date": be.Msg}), nil
		}
		return s.booksFailed(be, "invoice_date", "method"), nil
	}
	if err != nil {
		return nil, err
	}
	return status(1, "Invoice successfully added"), nil
}

// voidInvoice is invoices/void, for any of the four invoice types.
func (s *Server) voidInvoice(ctx context.Context, c *call, g *member) (any, error) {
	id := appID(c.req.Str("id"))
	if id == nil {
		return status(0, "Invoice not available"), nil
	}
	n, err := s.q.VoidInvoice(ctx, store.VoidInvoiceParams{GroupID: g.group.ID, ID: *id, VoidedBy: &g.user.ID})
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return status(0, "Invoice not available"), nil
	}
	return status(1, "success"), nil
}
