package mobile

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/swahome/legion-api/internal/auth"
	"github.com/swahome/legion-api/internal/books"
	"github.com/swahome/legion-api/internal/payments"
	"github.com/swahome/legion-api/internal/store"
)

// Subscription billing, linking a bank account to its bank's transaction
// alerts, the MPesa convenience charge, and the old names the app still
// sends for some actions.

// ---- Billing ----
//
// Every group is on the free plan for now: the imported invoices and
// receipts are shown as history, nothing is payable and no new charges are
// made. Paying a subscription from the app answers that nothing is due.

var billingCycles = map[int16]string{1: "Monthly", 2: "Quarterly", 3: "Annual"}

var billingMethods = map[int16]string{1: "Cash", 2: "Mpesa", 3: "Equitel", 4: "Cheque", 5: "Paypal", 6: "Coupon", 7: "MTN"}

func (s *Server) getGroupBillingInvoices(ctx context.Context, c *call, g *member) (any, error) {
	rows, err := s.q.ListBillingInvoices(ctx, g.group.ID)
	if err != nil {
		return nil, err
	}
	posts := []any{}
	var subs, tax, prorated, total, balance float64
	for _, r := range rows {
		amt, t, p, paid := amount(books.NumText(r.Amount)), amount(books.NumText(r.Tax)), amount(books.NumText(r.ProratedAmount)),
			amount(books.NumText(r.AmountPaid))
		st := "Unpaid"
		if r.IsPaid {
			st = "Paid"
		}
		due := ""
		if r.DueDate != nil {
			due = mobileShortTime(*r.DueDate)
		}
		title := ""
		if r.BillingCycle != nil {
			title = billingCycles[*r.BillingCycle]
		}
		posts = append(posts, map[string]any{"id": r.ID, "due_date": due, "subscription": amt - t - p, "title": title,
			"tax": t, "prorated_amount": p, "amount": amt, "balance": amt - paid, "status": st})
		subs, tax, prorated, total, balance = subs+amt-t-p, tax+t, prorated+p, total+amt, balance+amt-paid
	}
	var summary any = []any{}
	if len(rows) > 0 {
		summary = map[string]any{"total_subscription": subs, "total_tax": tax, "total_prorated_amount": prorated,
			"total_amount": total, "total_balance": balance}
	}
	return map[string]any{"status": 1, "message": "Group billing invoices", "invoices": posts, "summary": summary}, nil
}

func (s *Server) getGroupBillingPayments(ctx context.Context, c *call, g *member) (any, error) {
	rows, err := s.q.ListBillingPayments(ctx, g.group.ID)
	if err != nil {
		return nil, err
	}
	posts := []any{}
	var total, tax float64
	for _, r := range rows {
		date, method := "", ""
		if r.ReceiptDate != nil {
			date = mobileShortTime(*r.ReceiptDate)
		}
		if r.PaymentMethod != nil {
			method = billingMethods[*r.PaymentMethod]
		}
		// The legacy summary held the last receipt's figures, not a sum.
		total, tax = amount(books.NumText(r.Amount)), amount(books.NumText(r.Tax))
		posts = append(posts, map[string]any{"id": r.ID, "date": date, "receipt_number": r.ReceiptNumber, "tax": tax,
			"amount": total, "payment_method": method, "transaction_code": r.TransactionCode})
	}
	var summary any = []any{}
	if len(rows) > 0 {
		summary = map[string]any{"total_amount": total, "total_tax": tax}
	}
	return map[string]any{"status": 1, "message": "Group payment receipts", "payments": posts, "summary": summary}, nil
}

func (s *Server) getGroupAmountPayable(ctx context.Context, c *call, g *member) (any, error) {
	return map[string]any{"status": 1, "message": "Success", "payments": []any{}, "time": s.now().Unix()}, nil
}

const freePlan = "Your group is on the free plan. No payment is needed."

func (s *Server) noBillingPayment(ctx context.Context, c *call, g *member) (any, error) {
	return timed(status(0, freePlan), s.now()), nil
}

func (s *Server) confirmFreeBilling(ctx context.Context, c *call, g *member) (any, error) {
	return timed(status(1, "Successfully subscribed to the free plan"), s.now()), nil
}

// calculateConversion converted a subscription amount to the default
// currency; with one currency it is the amount, to two places.
func (s *Server) calculateConversion(ctx context.Context, c *call) (any, error) {
	v, _ := strconv.ParseFloat(strings.ReplaceAll(c.req.Str("amount"), ",", ""), 64)
	return map[string]any{"status": 1, "message": "amount", "amount": float64(int64(v*100+0.5)) / 100}, nil
}

// ---- Bank account linking ----

// BankLinker asks a partner bank to confirm that a phone number is a
// signatory of an account, with a code the bank texts to it.
type BankLinker interface {
	RequestCode(ctx context.Context, bank store.Bank, accountNumber, phone string) (sent bool, err error)
	VerifyCode(ctx context.Context, bank store.Bank, accountNumber, phone, code string) (ok bool, err error)
}

// FakeBankLinker confirms every account and accepts any code, for trying
// the screens without a bank (LEGION_CHAMASOFT_FAKE). Never in production.
type FakeBankLinker struct{}

func (FakeBankLinker) RequestCode(context.Context, store.Bank, string, string) (bool, error) {
	return true, nil
}
func (FakeBankLinker) VerifyCode(_ context.Context, _ store.Bank, _, _, code string) (bool, error) {
	return strings.TrimSpace(code) != "", nil
}

// WithBankLinker sets how bank accounts are linked. Without one, linking
// answers that the service is down, as the legacy app did when the bank
// did not answer.
func (s *Server) WithBankLinker(b BankLinker) *Server {
	s.banks = b
	return s
}

// linkTarget runs the legacy checks shared by connect and verify.
func (s *Server) linkTarget(ctx context.Context, c *call, g *member) (store.Bank, store.GroupAccount, string, map[string]any, error) {
	fail := func(msg string) (store.Bank, store.GroupAccount, string, map[string]any, error) {
		return store.Bank{}, store.GroupAccount{}, "", timed(status(0, msg), s.now()), nil
	}
	bank, err := s.q.GetBank(ctx, c.req.Int("bank_id"))
	if errors.Is(err, pgx.ErrNoRows) {
		return fail("Could not find bank. Try again later")
	}
	if err != nil {
		return store.Bank{}, store.GroupAccount{}, "", nil, err
	}
	if !bank.IsPartner {
		return fail("Bank selected is not a partner bank. Kindly use a different bank")
	}
	acct, err := s.q.AccountForNumber(ctx, store.AccountForNumberParams{GroupID: g.group.ID, ID: c.req.Int("id")})
	if errors.Is(err, pgx.ErrNoRows) {
		return fail("Could not group bank account. Select a different account")
	}
	if err != nil {
		return store.Bank{}, store.GroupAccount{}, "", nil, err
	}
	phone, ok := s.phoneOf(c.req.Str("signatory_phone"))
	if !ok {
		return fail("Signatory phone number is invalid. Try a different one")
	}
	return bank, acct, phone, nil, nil
}

func (s *Server) phoneOf(v string) (string, bool) {
	p, err := auth.NormalizePhone(v, s.auth.DefaultCallingCode())
	if err != nil {
		return "", false
	}
	return strings.TrimPrefix(p, "+"), true
}

const bankDown = "Service is temporarily down. Kindly try again later. "

func (s *Server) connectBankAccount(ctx context.Context, c *call, g *member) (any, error) {
	bank, acct, phone, reply, err := s.linkTarget(ctx, c, g)
	if err != nil || reply != nil {
		return reply, err
	}
	if acct.IsVerified {
		return timed(status(0, "Account already verified. Disconnect to start verification again"), s.now()), nil
	}
	if s.banks == nil {
		return timed(status(0, bankDown), s.now()), nil
	}
	sent, err := s.banks.RequestCode(ctx, bank, acct.AccountNumber, phone)
	if err != nil {
		s.logger().Error("bank linking: request code", "bank", bank.Slug, "err", err)
		return timed(status(0, "Service is temporarily down. No response received. Kindly try again later."), s.now()), nil
	}
	if !sent {
		return timed(status(0, "The Account Number did not match the signatory number. Kindly try another signatory phone number"), s.now()), nil
	}
	return timed(status(1, "The verification code was sent to "+phone), s.now()), nil
}

func (s *Server) verifyBankAccount(ctx context.Context, c *call, g *member) (any, error) {
	bank, acct, phone, reply, err := s.linkTarget(ctx, c, g)
	if err != nil || reply != nil {
		return reply, err
	}
	if s.banks == nil {
		return timed(status(0, bankDown), s.now()), nil
	}
	ok, err := s.banks.VerifyCode(ctx, bank, acct.AccountNumber, phone, strings.ToUpper(c.req.Str("verification_code")))
	if err != nil {
		s.logger().Error("bank linking: verify code", "bank", bank.Slug, "err", err)
		return timed(status(0, "The verification  request failed. Kindly request another verification code"), s.now()), nil
	}
	if !ok {
		return timed(status(0, "Verification code failed. Ensure you enter correct verification code or ensure the verification code has not expired."), s.now()), nil
	}
	if err := s.q.SetAccountVerified(ctx, store.SetAccountVerifiedParams{GroupID: g.group.ID, ID: acct.ID, IsVerified: true}); err != nil {
		return nil, err
	}
	return timed(status(1, "The verification request was successful. Your account can now receive transaction alerts. "), s.now()), nil
}

func (s *Server) disconnectBankAccount(ctx context.Context, c *call, g *member) (any, error) {
	acct, err := s.q.AccountForNumber(ctx, store.AccountForNumberParams{GroupID: g.group.ID, ID: c.req.Int("id")})
	if errors.Is(err, pgx.ErrNoRows) {
		return timed(status(0, "Could not group bank account. Select a different account"), s.now()), nil
	}
	if err != nil {
		return nil, err
	}
	if err := s.q.SetAccountVerified(ctx, store.SetAccountVerifiedParams{GroupID: g.group.ID, ID: acct.ID}); err != nil {
		return nil, err
	}
	return timed(status(1, "successful"), s.now()), nil
}

// activateWallet opened a Chamasoft e-wallet through the bank's
// open_online_account call, which isn't available to Legion; the terms
// check still answers as before.
func (s *Server) activateWallet(ctx context.Context, c *call, g *member) (any, error) {
	if v := c.req.Str("accept_terms_and_conditions"); v == "" || v == "0" || v == "false" {
		return timed(status(0, "You must accept terms and conditions to open a group Wallet"), s.now()), nil
	}
	return timed(status(0, bankDown), s.now()), nil
}

// ---- Payments ----

func (s *Server) calculateConvenienceCharge(ctx context.Context, c *call, g *member) (any, error) {
	if g.m.Status != store.MembershipStatusActive {
		return suspendedReply(s.now()), nil
	}
	if s.pay == nil {
		return timed(status(0, "Group does not have an active bank account. Contact Admin for support"), s.now()), nil
	}
	v, err := strconv.ParseFloat(strings.ReplaceAll(c.req.Str("amount"), ",", ""), 64)
	if err != nil || v <= 0 {
		return map[string]any{"status": 0, "message": "The total amount passed is not valid currency"}, nil
	}
	charge, problem, err := s.pay.DepositCharge(ctx, g.group.ID, v)
	switch {
	case errors.Is(err, payments.ErrNoPaymentAccount):
		return timed(status(0, "Group does not have an active bank account. Contact Admin for support"), s.now()), nil
	case errors.Is(err, payments.ErrNotConfigured):
		return map[string]any{"status": 0, "message": "Server error: payments are not set up"}, nil
	case err != nil:
		s.logger().Error("convenience charge", "err", err)
		return map[string]any{"status": 0, "message": "Server error: Error occured while processing request. Try again later"}, nil
	case problem != "":
		return map[string]any{"status": 0, "message": "Server error: " + problem}, nil
	}
	return map[string]any{"status": 1, "message": "Transaction charge", "charge": charge}, nil
}

// pushNotSent answers the app's push test actions; push notifications go
// out through FCM, which this server does not send to yet.
func (s *Server) pushNotSent(ctx context.Context, c *call) (any, error) {
	return timed(status(0, "Push notifications are not available"), s.now()), nil
}
