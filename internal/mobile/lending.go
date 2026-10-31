package mobile

import (
	"context"
	"errors"
	"fmt"

	"github.com/swahome/legion-api/internal/books"
	"github.com/swahome/legion-api/internal/store"
)

// Milestone 4b: bank loans (money the group borrowed) and debtors.

// bankLoanOptions is bank_loans/get_group_bank_loan_options: the loans
// still being repaid, for the repayment form.
func (s *Server) bankLoanOptions(ctx context.Context, c *call, g *member) (any, error) {
	rows, err := s.q.ListBankLoans(ctx, store.ListBankLoansParams{GroupID: g.group.ID, Status: "open"})
	if err != nil {
		return nil, err
	}
	out := []any{}
	for _, b := range rows {
		out = append(out, map[string]any{"id": fmt.Sprint(b.ID), "amount": books.Format(b.Amount),
			"description": b.Description, "balance": books.Format(b.Balance)})
	}
	return timed(map[string]any{"status": 1, "message": "Group bank loans", "loans": out}, s.now()), nil
}

// recordBankLoan is deposits/record_bank_loan: a loan the group received.
func (s *Server) recordBankLoan(ctx context.Context, c *call, g *member) (any, error) {
	if reply := s.required(c, [2]string{"description", "Bank loan description"},
		[2]string{"amount_loaned", "Bank loan amount received"},
		[2]string{"total_loan_amount_payable", "Bank loan total amount payable"},
		[2]string{"loan_balance", "Bank loan balance"}, [2]string{"loan_start_date", "Bank loan start date"},
		[2]string{"loan_end_date", "Bank loan end date"}, [2]string{"account_id", "Group account loan deposited to"}); reply != nil {
		return reply, nil
	}
	starts, ends := appDate(c.req.Str("loan_start_date"), g.loc), appDate(c.req.Str("loan_end_date"), g.loc)
	if starts == nil || ends == nil {
		return s.formFailed(map[string]string{"loan_start_date": "Enter valid loan start and end dates."}), nil
	}
	bal := appAmount(c.req.Str("loan_balance"))
	b := books.BankLoan{Description: c.req.Str("description"), Amount: appAmount(c.req.Str("amount_loaned")),
		TotalPayable: appAmount(c.req.Str("total_loan_amount_payable")), Balance: &bal, StartsOn: starts, EndsOn: ends,
		AccountID: appAccount(c.req.Str("account_id"))}
	err := s.inTx(ctx, func(q *store.Queries) error {
		_, err := books.RecordBankLoan(ctx, q, g.group.ID, g.user.ID, b)
		return err
	})
	if be, ok := asBooksError(err); ok {
		field := map[string]string{"invalid_amount": "amount_loaned", "invalid_total_payable": "total_loan_amount_payable",
			"invalid_balance": "loan_balance", "invalid_date": "loan_end_date", "invalid_account": "account_id",
			"account_closed": "account_id", "missing_fields": "description"}[be.Code]
		if be.Code == "invalid_date" && starts.After(s.today(g)) {
			field = "loan_start_date"
		}
		if field == "" {
			field = "form"
		}
		return s.formFailed(map[string]string{field: be.Msg}), nil
	}
	if err != nil {
		return nil, err
	}
	return timed(status(1, "Successfully added transaction"), s.now()), nil
}

// voidBankLoan is deposits/void_bank_loan.
func (s *Server) voidBankLoan(ctx context.Context, c *call, g *member) (any, error) {
	id := appID(c.req.Str("bank_loan_id"))
	if id == nil {
		return timed(status(0, "Loan details are missing"), s.now()), nil
	}
	err := s.inTx(ctx, func(q *store.Queries) error { return books.VoidBankLoan(ctx, q, g.group.ID, *id, g.user.ID) })
	if errors.Is(err, books.ErrNotFound) {
		return timed(status(0, "Bank loan details unavailable"), s.now()), nil
	}
	if be, ok := asBooksError(err); ok {
		return timed(status(0, be.Msg), s.now()), nil
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"status": 1, "time": s.now().Unix()}, nil
}

// recordBankLoanRepayment is withdrawals/record_bank_loan_repayment.
func (s *Server) recordBankLoanRepayment(ctx context.Context, c *call, g *member) (any, error) {
	if reply := s.required(c, [2]string{"bank_loan_id", "Bank Loan"}, [2]string{"repayment_date", "Repayment Date"},
		[2]string{"account_id", "Group Account"}, [2]string{"repayment_method", "Repayment Method"},
		[2]string{"description", "Repayment Descriptions"}, [2]string{"amount", "Amount"}); reply != nil {
		return reply, nil
	}
	date := appDate(c.req.Str("repayment_date"), g.loc)
	if date == nil {
		return s.formFailed(map[string]string{"repayment_date": "The Repayment Date field must contain a valid date."}), nil
	}
	id := appID(c.req.Str("bank_loan_id"))
	if id == nil {
		return s.formFailed(map[string]string{"bank_loan_id": "The Bank Loan field must contain a number."}), nil
	}
	rp := books.BankLoanRepayment{AccountID: appAccount(c.req.Str("account_id")), Amount: appAmount(c.req.Str("amount")),
		Method: appMethod(withdrawalMethodCodes, c.req.Str("repayment_method")), Date: date,
		Description: c.req.Str("description")}
	err := s.inTx(ctx, func(q *store.Queries) error {
		_, err := books.RecordBankLoanRepayment(ctx, q, g.group.ID, g.user.ID, *id, rp)
		return err
	})
	if errors.Is(err, books.ErrNotFound) {
		return status(0, "Could not find bank loan"), nil
	}
	if be, ok := asBooksError(err); ok {
		if be.Code == "loan_voided" {
			return status(0, "Could not find bank loan"), nil
		}
		return s.booksFailed(be, "repayment_date", "repayment_method"), nil
	}
	if err != nil {
		return nil, err
	}
	return withdrawalSuccess(s.now(), "Successfully recorded a transaction"), nil
}

// bankLoansSummary is reports/bank_loans_summary.
func (s *Server) bankLoansSummary(ctx context.Context, c *call, g *member) (any, error) {
	rows, err := s.q.ListBankLoans(ctx, store.ListBankLoansParams{GroupID: g.group.ID})
	if err != nil {
		return nil, err
	}
	_, accts, err := s.groupAccounts(ctx, g)
	if err != nil {
		return nil, err
	}
	body := []any{}
	var received, repaid, arrears int64
	today := s.today(g)
	for _, b := range rows {
		account := ""
		if b.AccountID != nil {
			if a, ok := accts[*b.AccountID]; ok {
				account = optionLabel(a)
			}
		}
		bal := books.Cents(b.Balance)
		received += books.Cents(b.Amount)
		repaid += books.Cents(b.Repaid)
		if b.EndsOn.Before(today) {
			arrears += bal
		}
		body = append(body, map[string]any{"description": b.Description, "start_date": reportTime(b.StartsOn),
			"end_date": reportTime(b.EndsOn), "account": account, "is_active": 1, "is_fully_paid": boolInt(bal <= 0),
			"loaned_amount": books.Format(b.Amount), "amount_payble": books.Format(b.TotalPayable),
			"loan_balance": books.Format(b.Balance)})
	}
	return map[string]any{"status": 1, "message": "successful", "data": map[string]any{
		"statement_body": body,
		"statement_footer": map[string]any{"total_received": centsAmount(received), "total_repaid": centsAmount(repaid),
			"total_arrears": centsAmount(arrears)},
	}}, nil
}

func centsAmount(c int64) string { return books.Format(fmt.Sprintf("%d.%02d", c/100, c%100)) }

// debtorsList is debtors/get_debtors_list.
func (s *Server) debtorsList(ctx context.Context, c *call, g *member) (any, error) {
	rows, err := s.q.ListDebtors(ctx, store.ListDebtorsParams{GroupID: g.group.ID})
	if err != nil {
		return nil, err
	}
	out := []any{}
	for _, d := range rows {
		out = append(out, map[string]any{"id": fmt.Sprint(d.ID), "name": d.Name, "email": d.Email, "phone": d.Phone,
			"description": d.Description, "group_id": fmt.Sprint(g.group.ID), "active": "1", "id_number": d.IDNumber,
			"created_on": fmt.Sprint(d.CreatedAt.Unix())})
	}
	return map[string]any{"status": 1, "message": "success", "debtors": out}, nil
}
