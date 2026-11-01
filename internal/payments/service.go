package payments

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/swahome/legion-api/internal/books"
	"github.com/swahome/legion-api/internal/sms"
	"github.com/swahome/legion-api/internal/store"
)

// Service runs payments: it starts MPesa prompts, takes the gateway's
// callbacks and bank feeds in, and reconciles what it can.
type Service struct {
	Pool      *pgxpool.Pool
	Gateway   Gateway // nil: members cannot start payments
	PublicURL string  // where the gateway can reach this API, e.g. https://legion.com
	HookKey   []byte  // signs callback URLs
	DefaultCC string
	Location  *time.Location
	Log       *slog.Logger
	SMSKick   func()
}

// ErrNotConfigured: no payment gateway is set up.
var ErrNotConfigured = errors.New("payments: gateway not configured")

func (s *Service) q() *store.Queries { return store.New(s.Pool) }

func (s *Service) loc() *time.Location {
	if s.Location != nil {
		return s.Location
	}
	return books.Nairobi()
}

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func (s *Service) kick() {
	if s.SMSKick != nil {
		s.SMSKick()
	}
}

// Enabled reports whether members can start payments.
func (s *Service) Enabled() bool { return s != nil && s.Gateway != nil }

// CallbackKey signs a payment reference so a callback cannot be forged
// for a request the caller did not get the URL of.
func (s *Service) CallbackKey(reference string) string {
	m := hmac.New(sha256.New, s.HookKey)
	m.Write([]byte("payment:" + reference))
	return hex.EncodeToString(m.Sum(nil))[:32]
}

// ValidCallbackKey checks a key from a callback URL.
func (s *Service) ValidCallbackKey(reference, key string) bool {
	return hmac.Equal([]byte(s.CallbackKey(reference)), []byte(key))
}

// HookSecret is the shared secret for one inbound hook, derived from the
// hook key, for the URLs given to Chamasoft and Safaricom.
func (s *Service) HookSecret(hook string) string {
	m := hmac.New(sha256.New, s.HookKey)
	m.Write([]byte("hook:" + hook))
	return hex.EncodeToString(m.Sum(nil))[:32]
}

// PaymentInput is a payment a member starts.
type PaymentInput struct {
	GroupID      int64
	MembershipID int64
	UserID       int64
	Phone        string // E.164
	Lines        []Line // what it pays; empty with Amount set means "pay my arrears"
	Amount       string
	Channel      string // web, app, ussd
}

// CheckLines validates what a member wants to pay for and fills in names.
func CheckLines(ctx context.Context, q *store.Queries, groupID, memberID int64, lines []Line) (string, error) {
	if len(lines) == 0 {
		return "", Error{Code: "missing_fields", Msg: "Pick at least one thing to pay for."}
	}
	var total int64
	for i := range lines {
		l := &lines[i]
		amt, ok := NormAmount(l.Amount)
		if !ok {
			return "", Error{Code: "invalid_amount", Msg: "Every amount must be more than zero."}
		}
		l.Amount, l.MemberID = amt, &memberID
		total += books.Cents(amt)
		l.IncomeCategoryID, l.DepositorID, l.ExpenseCategoryID = nil, nil, nil
		switch l.Kind {
		case "contribution":
			l.FineCategoryID, l.LoanID = nil, nil
			if l.ContributionID == nil {
				return "", Error{Code: "missing_fields", Msg: "Pick the contribution."}
			}
			if err := books.Found("invalid_contribution", "That contribution is not in this group.")(
				q.ContributionInGroup(ctx, store.ContributionInGroupParams{GroupID: groupID, ID: *l.ContributionID})); err != nil {
				return "", err
			}
		case "fine":
			l.LoanID = nil
			if (l.ContributionID == nil) == (l.FineCategoryID == nil) {
				return "", Error{Code: "missing_fields", Msg: "A fine payment names its contribution (late fine) or fine category."}
			}
			if l.ContributionID != nil {
				if err := books.Found("invalid_contribution", "That contribution is not in this group.")(
					q.ContributionInGroup(ctx, store.ContributionInGroupParams{GroupID: groupID, ID: *l.ContributionID})); err != nil {
					return "", err
				}
			} else if err := books.Found("invalid_fine_category", "That fine category is not in this group.")(
				q.FineCategoryInGroup(ctx, store.FineCategoryInGroupParams{GroupID: groupID, ID: *l.FineCategoryID})); err != nil {
				return "", err
			}
		case "loan":
			l.ContributionID, l.FineCategoryID = nil, nil
			if l.LoanID == nil {
				return "", Error{Code: "missing_fields", Msg: "Pick the loan."}
			}
			if err := books.Found("invalid_loan", "That is not one of your loans in this group.")(
				q.LoanOfMember(ctx, store.LoanOfMemberParams{GroupID: groupID, ID: *l.LoanID, MembershipID: &memberID})); err != nil {
				return "", err
			}
		case "misc":
			l.ContributionID, l.FineCategoryID, l.LoanID = nil, nil, nil
		default:
			return "", Error{Code: "invalid_kind", Msg: "Each line is a contribution, fine, loan or misc payment."}
		}
	}
	return centsText(total), nil
}

// newReference is the payment's reference with the gateway: digits only,
// as the legacy numeric references.
func newReference() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(1000))
	return fmt.Sprintf("%d%03d", time.Now().Unix(), n.Int64())
}

// StartPayment records a payment request and sends the MPesa prompt. The
// request is returned whether or not the gateway took it; its status says.
func (s *Service) StartPayment(ctx context.Context, in PaymentInput) (int64, error) {
	if !s.Enabled() {
		return 0, ErrNotConfigured
	}
	q := s.q()
	acct, err := q.DefaultPaymentAccount(ctx, in.GroupID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, Error{Code: "no_payment_account", Msg: "The group has no bank account to pay into yet. Ask an official to add one."}
	}
	if err != nil {
		return 0, err
	}
	lines := in.Lines
	if len(lines) == 0 {
		amt, ok := NormAmount(in.Amount)
		if !ok {
			return 0, Error{Code: "invalid_amount", Msg: "amount must be more than zero."}
		}
		if lines, err = Arrears(ctx, q, in.GroupID, in.MembershipID, amt); err != nil {
			return 0, err
		}
	}
	total, err := CheckLines(ctx, q, in.GroupID, in.MembershipID, lines)
	if err != nil {
		return 0, err
	}
	linesJSON, _ := json.Marshal(lines)
	ref := newReference()
	channel := in.Channel
	if channel == "" {
		channel = "web"
	}
	id, err := q.CreatePaymentRequest(ctx, store.CreatePaymentRequestParams{
		GroupID: in.GroupID, MembershipID: &in.MembershipID, UserID: books.By(in.UserID), AccountID: &acct.ID,
		AccountNumber: acct.AccountNumber, Phone: in.Phone, Amount: total, Reference: ref, Lines: linesJSON,
		Channel: channel, Provider: "chamasoft",
	})
	if err != nil {
		return 0, err
	}
	callback := strings.TrimRight(s.PublicURL, "/") + "/v1/hooks/chamasoft/payments/" + ref + "?key=" + s.CallbackKey(ref)
	ans, gerr := s.Gateway.MakePayment(ctx, Order{
		Amount: total, AccountNumber: acct.AccountNumber, Reference: ref, AccountSecret: acct.AccountPassword,
		Phone: in.Phone, CallbackURL: callback,
	})
	p := store.SetPaymentRequestSentParams{ID: id, Status: store.PaymentRequestStatusSent, ResponseCode: ans.Code,
		ResponseDescription: ans.Description}
	switch {
	case gerr != nil:
		s.log().Error("payment gateway", "err", gerr, "reference", ref)
		p.Status, p.ResultDescription = store.PaymentRequestStatusFailed, "Could not reach MPesa. Try again in a few minutes."
	case !ans.Accepted:
		p.Status, p.ResultDescription = store.PaymentRequestStatusFailed, firstNonEmpty(ans.Description, "MPesa did not accept the request.")
	}
	if err := q.SetPaymentRequestSent(ctx, p); err != nil {
		return id, err
	}
	return id, nil
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// PaymentResult is the gateway's callback for one payment request.
type PaymentResult struct {
	Success     bool
	Code        string
	Description string
	Receipt     string // MPesa receipt (transaction id)
	At          time.Time
	Phone       string
	Raw         []byte
}

// Settle records a payment request's outcome. A paid request becomes a
// transaction alert and, unless the group turned auto-reconciliation off,
// the deposits its lines describe. Calling it again for a settled request
// changes nothing and returns duplicate=true.
func (s *Service) Settle(ctx context.Context, reference string, res PaymentResult) (alertID int64, duplicate bool, err error) {
	var texts []sms.Message
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.q().WithTx(tx)
		pr, err := q.LockPaymentRequestByReference(ctx, reference)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if pr.Status == store.PaymentRequestStatusPaid || (pr.Status == store.PaymentRequestStatusFailed && !res.Success) {
			duplicate = true
			if pr.TransactionAlertID != nil {
				alertID = *pr.TransactionAlertID
			}
			return nil
		}
		group, gerr := q.GetGroup(ctx, pr.GroupID)
		if gerr != nil {
			return gerr
		}
		amount := Text(pr.Amount)
		if !res.Success || res.Receipt == "" {
			if err := q.SetPaymentRequestResult(ctx, store.SetPaymentRequestResultParams{
				ID: pr.ID, Status: store.PaymentRequestStatusFailed, ResultCode: res.Code,
				ResultDescription: firstNonEmpty(res.Description, "The payment did not go through."),
			}); err != nil {
				return err
			}
			texts = append(texts, sms.Message{To: pr.Phone, Kind: "payment_failed", UserID: pr.UserID, GroupID: &pr.GroupID,
				Body: fmt.Sprintf("Your payment of %s %s to %s did not go through: %s", group.Currency, amount, group.Name,
					firstNonEmpty(res.Description, "no reason given"))})
			return nil
		}
		at := res.At
		if at.IsZero() {
			at = time.Now()
		}
		var lines []Line
		_ = json.Unmarshal(pr.Lines, &lines)
		var names []string
		for _, l := range lines {
			if l.Name != "" {
				names = append(names, l.Name)
			}
		}
		id, _, err := Ingest(ctx, q, Alert{
			Source: SourceChamasoft, ProviderRef: res.Receipt, AccountNumber: pr.AccountNumber, AccountID: pr.AccountID,
			GroupID: &pr.GroupID, Direction: 1, Amount: amount, At: at, Reference: pr.Reference,
			Particulars: "Payment for " + strings.Join(names, ", "), Phone: firstNonEmpty(res.Phone, pr.Phone), Raw: res.Raw,
		}, s.DefaultCC)
		if err != nil {
			return err
		}
		alertID = id
		if err := q.SetPaymentRequestResult(ctx, store.SetPaymentRequestResultParams{
			ID: pr.ID, Status: store.PaymentRequestStatusPaid, ResultCode: res.Code,
			ResultDescription: firstNonEmpty(res.Description, "Paid"), ProviderRef: &res.Receipt, TransactionAlertID: &id,
		}); err != nil {
			return err
		}
		texts = append(texts, sms.Message{To: pr.Phone, Kind: "payment_received", UserID: pr.UserID, GroupID: &pr.GroupID,
			Body: fmt.Sprintf("%s confirmed. %s received %s %s from you. Thank you.", res.Receipt, group.Name, group.Currency, amount)})
		flags, err := q.GroupReconcileFlags(ctx, pr.GroupID)
		if err != nil {
			return err
		}
		if flags.DisableAutoReconcile == "1" || pr.AccountID == nil {
			return nil
		}
		// Reconcile in a savepoint: if a line no longer fits (a loan was
		// voided meanwhile), the alert stays for an official to sort out.
		sp, err := tx.Begin(ctx)
		if err != nil {
			return err
		}
		if err := Reconcile(ctx, q.WithTx(sp), pr.GroupID, ptrOr(pr.UserID), id, lines, true, "Paid from the app"); err != nil {
			_ = sp.Rollback(ctx)
			var refused Error
			if !errors.As(err, &refused) {
				return err
			}
			s.log().Warn("payment not auto-reconciled", "reference", reference, "reason", refused.Msg)
			return nil
		}
		return sp.Commit(ctx)
	})
	if err == nil && len(texts) > 0 {
		q := s.q()
		for _, t := range texts {
			if !strings.HasPrefix(t.To, "+") {
				continue
			}
			if _, qerr := sms.Queue(ctx, q, t); qerr != nil {
				s.log().Error("queue payment sms", "err", qerr)
			}
		}
		s.kick()
	}
	return alertID, duplicate, err
}

func ptrOr(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// Receive stores an alert from a feed and tries to reconcile it on its
// own. It returns the alert id, whether it was new and whether it was
// reconciled.
func (s *Service) Receive(ctx context.Context, a Alert) (id int64, created, auto bool, err error) {
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.q().WithTx(tx)
		var err error
		if id, created, err = Ingest(ctx, q, a, s.DefaultCC); err != nil || !created {
			return err
		}
		auto, err = AutoReconcile(ctx, q, id, s.DefaultCC)
		return err
	})
	return id, created, auto, err
}

// AccountKnown reports whether an account number belongs to a group, for
// MPesa's validation call.
func (s *Service) AccountKnown(ctx context.Context, number string) (bool, error) {
	rows, err := s.q().FindAccountsByNumber(ctx, store.FindAccountsByNumberParams{AccountNumber: strings.TrimSpace(number)})
	return len(rows) > 0, err
}

// ExpireStale fails payment requests that never got an answer.
func (s *Service) ExpireStale(ctx context.Context, olderThan time.Duration) (int, error) {
	q := s.q()
	ids, err := q.StalePaymentRequests(ctx, time.Now().Add(-olderThan))
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if err := q.ExpirePaymentRequest(ctx, id); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

// RunExpiry fails stale payment requests every few minutes until ctx ends.
func (s *Service) RunExpiry(ctx context.Context) {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := s.ExpireStale(ctx, 30*time.Minute); err != nil {
				s.log().Error("expire payment requests", "err", err)
			} else if n > 0 {
				s.log().Info("expired payment requests", "count", n)
			}
		}
	}
}
