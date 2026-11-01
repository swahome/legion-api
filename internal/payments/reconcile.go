package payments

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/swahome/legion-api/internal/books"
	"github.com/swahome/legion-api/internal/store"
)

// Line is one part of what an alert's money was for. Money in becomes
// deposits (kind contribution, fine, misc, income or loan); money out
// becomes withdrawals (expense or contribution_refund).
type Line struct {
	Kind              string `json:"kind"`
	MemberID          *int64 `json:"member_id,omitempty"`
	ContributionID    *int64 `json:"contribution_id,omitempty"`
	FineCategoryID    *int64 `json:"fine_category_id,omitempty"`
	IncomeCategoryID  *int64 `json:"income_category_id,omitempty"`
	DepositorID       *int64 `json:"depositor_id,omitempty"`
	LoanID            *int64 `json:"loan_id,omitempty"`
	ExpenseCategoryID *int64 `json:"expense_category_id,omitempty"`
	Amount            string `json:"amount"`
	Description       string `json:"description,omitempty"`
	// Name is what the line pays, for showing (e.g. "Monthly savings").
	Name string `json:"name,omitempty"`
}

// ErrNotFound: no such alert in the group.
var ErrNotFound = books.ErrNotFound

// lockGroupAlert loads an alert for update and checks it belongs to the
// group and is waiting to be reconciled.
func lockGroupAlert(ctx context.Context, q *store.Queries, groupID, alertID int64) (store.TransactionAlert, error) {
	a, err := q.LockAlert(ctx, alertID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (a.GroupID == nil || *a.GroupID != groupID)) {
		return a, ErrNotFound
	}
	if err != nil {
		return a, err
	}
	if a.Status != store.AlertStatusUnreconciled {
		return a, Error{Code: "already_reconciled", Msg: "This transaction has already been dealt with."}
	}
	if a.AccountID == nil {
		return a, Error{Code: "no_account", Msg: "This transaction is not tied to one of the group's accounts."}
	}
	return a, nil
}

// alertDate is the day an alert's money moved, in Nairobi, never later
// than today (bank clocks drift).
func alertDate(a store.TransactionAlert) time.Time {
	loc := books.Nairobi()
	t := a.TransactionAt.In(loc)
	now := time.Now().In(loc)
	if t.After(now) {
		t = now
	}
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func alertMethod(a store.TransactionAlert) *string {
	switch a.Source {
	case SourceChamasoft, SourceChamasoftDirect, SourceMpesa:
		m := string(store.PaymentMethodMpesa)
		return &m
	}
	return nil
}

func defaultDescription(a store.TransactionAlert) string {
	if alertMethod(a) != nil {
		return "MPesa " + a.ProviderRef
	}
	return "Bank transaction " + a.ProviderRef
}

// Reconcile writes the deposits or withdrawals that account for an alert.
// The lines must add up to the alert's amount exactly. userID is who did it
// (0 for the system).
func Reconcile(ctx context.Context, q *store.Queries, groupID, userID, alertID int64, lines []Line, auto bool, note string) error {
	a, err := lockGroupAlert(ctx, q, groupID, alertID)
	if err != nil {
		return err
	}
	return reconcileLocked(ctx, q, a, userID, lines, auto, note)
}

func reconcileLocked(ctx context.Context, q *store.Queries, a store.TransactionAlert, userID int64, lines []Line, auto bool, note string) error {
	if len(lines) == 0 {
		return Error{Code: "missing_fields", Msg: "Say what the money was for: at least one line."}
	}
	var sum int64
	for i := range lines {
		amt, ok := NormAmount(lines[i].Amount)
		if !ok {
			return Error{Code: "invalid_amount", Msg: "Every line needs an amount more than zero."}
		}
		lines[i].Amount = amt
		sum += books.Cents(amt)
	}
	if want := books.Cents(Text(a.Amount)); sum != want {
		return Error{Code: "amount_mismatch", Msg: "The lines add up to " + books.Format(centsText(sum)) +
			" but the transaction is " + Text(a.Amount) + "."}
	}
	groupID, account := *a.GroupID, *a.AccountID
	date, method := alertDate(a), alertMethod(a)
	for _, l := range lines {
		desc := strings.TrimSpace(l.Description)
		if desc == "" {
			desc = defaultDescription(a)
		}
		if a.Direction == 1 {
			var depositID int64
			switch l.Kind {
			case "loan", "loan_repayment":
				if l.LoanID == nil {
					return Error{Code: "missing_fields", Msg: "loan_id is required for a loan repayment."}
				}
				rid, err := books.RecordRepayment(ctx, q, groupID, userID, *l.LoanID, books.Repayment{
					AccountID: account, Method: method, Amount: l.Amount, Date: &date, Description: desc,
				})
				if errors.Is(err, books.ErrNotFound) {
					return Error{Code: "invalid_loan", Msg: "That loan is not in this group."}
				}
				if err != nil {
					return err
				}
				if depositID, err = q.DepositIDForRepayment(ctx, rid); err != nil {
					return err
				}
			default:
				var err error
				depositID, err = books.RecordDeposit(ctx, q, groupID, userID, books.Deposit{
					Kind: store.DepositKind(l.Kind), AccountID: account, Method: method, Amount: l.Amount, Date: &date,
					Description: desc, MemberID: l.MemberID, ContributionID: l.ContributionID,
					FineCategoryID: l.FineCategoryID, IncomeCategoryID: l.IncomeCategoryID, DepositorID: l.DepositorID,
				})
				if err != nil {
					return err
				}
			}
			if err := q.TagAlertDeposit(ctx, store.TagAlertDepositParams{AlertID: &a.ID, FromAlert: true, ID: depositID}); err != nil {
				return err
			}
			continue
		}
		wid, err := books.RecordWithdrawal(ctx, q, groupID, userID, account, books.Withdrawal{
			Kind: store.WithdrawalKind(l.Kind), Amount: l.Amount, MemberID: l.MemberID,
			ContributionID: l.ContributionID, ExpenseCategoryID: l.ExpenseCategoryID,
		}, method, &date, desc)
		if err != nil {
			return err
		}
		if err := q.TagAlertWithdrawal(ctx, store.TagAlertWithdrawalParams{AlertID: &a.ID, FromAlert: true, ID: wid}); err != nil {
			return err
		}
	}
	return q.SetAlertStatus(ctx, store.SetAlertStatusParams{
		ID: a.ID, Status: store.AlertStatusReconciled, AutoReconciled: auto, ReconciledBy: books.By(userID), Note: note,
	})
}

func centsText(c int64) string {
	neg := c < 0
	if neg {
		c = -c
	}
	s := itoa(c/100) + "." + string(rune('0'+c%100/10)) + string(rune('0'+c%10))
	if neg {
		s = "-" + s
	}
	return s
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// Match ties an alert to deposits (money in) or withdrawals (money out) an
// official already recorded by hand. They must be on the alert's account and
// add up to its amount.
func Match(ctx context.Context, q *store.Queries, groupID, userID, alertID int64, ids []int64, note string) error {
	a, err := lockGroupAlert(ctx, q, groupID, alertID)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return Error{Code: "missing_fields", Msg: "Pick the records this transaction is for."}
	}
	type row struct {
		id      int64
		account *int64
		amount  string
	}
	var rows []row
	if a.Direction == 1 {
		found, err := q.MatchableDeposits(ctx, store.MatchableDepositsParams{GroupID: groupID, Ids: ids})
		if err != nil {
			return err
		}
		for _, f := range found {
			rows = append(rows, row{f.ID, f.AccountID, f.Amount})
		}
	} else {
		found, err := q.MatchableWithdrawals(ctx, store.MatchableWithdrawalsParams{GroupID: groupID, Ids: ids})
		if err != nil {
			return err
		}
		for _, f := range found {
			rows = append(rows, row{f.ID, f.AccountID, f.Amount})
		}
	}
	if len(rows) != len(dedupe(ids)) {
		return Error{Code: "invalid_records", Msg: "Some of those records are voided, already matched or not in this group."}
	}
	var sum int64
	for _, r := range rows {
		if r.account == nil || *r.account != *a.AccountID {
			return Error{Code: "wrong_account", Msg: "Every record must be on the same account as the transaction."}
		}
		sum += books.Cents(r.amount)
	}
	if sum != books.Cents(Text(a.Amount)) {
		return Error{Code: "amount_mismatch", Msg: "Those records add up to " + centsText(sum) + " but the transaction is " + Text(a.Amount) + "."}
	}
	for _, r := range rows {
		if a.Direction == 1 {
			err = q.TagAlertDeposit(ctx, store.TagAlertDepositParams{AlertID: &a.ID, ID: r.id})
		} else {
			err = q.TagAlertWithdrawal(ctx, store.TagAlertWithdrawalParams{AlertID: &a.ID, ID: r.id})
		}
		if err != nil {
			return err
		}
	}
	return q.SetAlertStatus(ctx, store.SetAlertStatusParams{
		ID: a.ID, Status: store.AlertStatusReconciled, ReconciledBy: books.By(userID), Note: note,
	})
}

func dedupe(ids []int64) []int64 {
	seen := map[int64]bool{}
	var out []int64
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// Dismiss closes an alert without writing anything: status reconciled
// ("already recorded", as the legacy mark-as-reconciled) or ignored (not
// the group's money, a duplicate). A note is required.
func Dismiss(ctx context.Context, q *store.Queries, groupID, userID, alertID int64, status store.AlertStatus, note string) error {
	note = strings.TrimSpace(note)
	if note == "" {
		return Error{Code: "missing_fields", Msg: "Add a note saying why."}
	}
	a, err := q.LockAlert(ctx, alertID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (a.GroupID == nil || *a.GroupID != groupID)) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if a.Status != store.AlertStatusUnreconciled {
		return Error{Code: "already_reconciled", Msg: "This transaction has already been dealt with."}
	}
	return q.SetAlertStatus(ctx, store.SetAlertStatusParams{ID: a.ID, Status: status, ReconciledBy: books.By(userID), Note: note})
}

// Undo puts an alert back to unreconciled: records reconciliation wrote are
// voided, records it was matched to are only unlinked.
func Undo(ctx context.Context, q *store.Queries, groupID, userID, alertID int64) error {
	a, err := q.LockAlert(ctx, alertID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (a.GroupID == nil || *a.GroupID != groupID)) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if a.Status == store.AlertStatusUnreconciled {
		return Error{Code: "not_reconciled", Msg: "This transaction has not been reconciled."}
	}
	deps, err := q.AlertDeposits(ctx, &a.ID)
	if err != nil {
		return err
	}
	for _, d := range deps {
		if d.FromAlert && d.VoidedAt == nil {
			if err := books.VoidDeposit(ctx, q, groupID, d.ID, userID); err != nil {
				return err
			}
		}
	}
	wds, err := q.AlertWithdrawals(ctx, &a.ID)
	if err != nil {
		return err
	}
	for _, w := range wds {
		if w.FromAlert && w.VoidedAt == nil {
			if err := books.VoidWithdrawal(ctx, q, groupID, w.ID, userID); err != nil {
				return err
			}
		}
	}
	if err := q.UnlinkAlertRecords(ctx, &a.ID); err != nil {
		return err
	}
	if err := q.UnlinkAlertWithdrawals(ctx, &a.ID); err != nil {
		return err
	}
	return q.SetAlertStatus(ctx, store.SetAlertStatusParams{ID: a.ID, Status: store.AlertStatusUnreconciled})
}

// ---- Automatic reconciliation ----

// AutoReconcile tries to reconcile money in on its own: it finds the paying
// member (by phone, a phone in the narration, or a name only one member
// has) and spreads the amount over what they owe, the legacy way. It
// reports whether it did. Bank feed alerts are only touched when the group
// turned auto-reconciliation on; MPesa payments unless it turned it off.
func AutoReconcile(ctx context.Context, q *store.Queries, alertID int64, defaultCC string) (bool, error) {
	a, err := q.LockAlert(ctx, alertID)
	if err != nil {
		return false, err
	}
	if a.Status != store.AlertStatusUnreconciled || a.Direction != 1 || a.GroupID == nil || a.AccountID == nil {
		return false, nil
	}
	flags, err := q.GroupReconcileFlags(ctx, *a.GroupID)
	if err != nil {
		return false, err
	}
	switch a.Source {
	case SourceEquity, SourceCoop:
		if flags.EnableAutoReconcile != "1" {
			return false, nil
		}
	default:
		if flags.DisableAutoReconcile == "1" {
			return false, nil
		}
	}
	member, err := FindPayer(ctx, q, *a.GroupID, a.Phone, a.Particulars+" "+a.PayerName+" "+a.Reference, defaultCC)
	if err != nil || member == 0 {
		return false, err
	}
	lines, err := Arrears(ctx, q, *a.GroupID, member, Text(a.Amount))
	if err != nil || len(lines) == 0 {
		return false, err
	}
	if err := reconcileLocked(ctx, q, a, 0, lines, true, "Matched to the member automatically"); err != nil {
		var refused Error
		if errors.As(err, &refused) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

var phoneInText = regexp.MustCompile(`(?:\+?254|0)[17]\d{8}`)

// FindPayer returns the active member who paid, or 0 when it cannot be told.
func FindPayer(ctx context.Context, q *store.Queries, groupID int64, phone *string, text, defaultCC string) (int64, error) {
	var phones []string
	if phone != nil {
		phones = append(phones, *phone)
	}
	for _, m := range phoneInText.FindAllString(text, 5) {
		if p, err := normalize(m, defaultCC); err == nil {
			phones = append(phones, p)
		}
	}
	if len(phones) > 0 {
		rows, err := q.MembersByPhone(ctx, store.MembersByPhoneParams{GroupID: groupID, Phones: phones})
		if err != nil {
			return 0, err
		}
		for _, p := range phones { // the feed's own phone first
			for _, r := range rows {
				if r.Phone == p {
					return r.MembershipID, nil
				}
			}
		}
	}
	words := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '\'')
	}) {
		words[w] = true
	}
	if len(words) == 0 {
		return 0, nil
	}
	members, err := q.MemberNames(ctx, groupID)
	if err != nil {
		return 0, err
	}
	var found int64
	for _, m := range members {
		first, last := strings.ToLower(strings.TrimSpace(m.FirstName)), strings.ToLower(strings.TrimSpace(m.LastName))
		if len(first) < 2 || len(last) < 2 || !words[first] || !words[last] {
			continue
		}
		if found != 0 {
			return 0, nil // two members fit: leave it to an official
		}
		found = m.MembershipID
	}
	return found, nil
}

// Arrears splits an amount a member paid over what they owe, in the legacy
// order (Transactions::make_group_arrears_payment): late fines on
// contributions, then contributions, then other fines, and whatever is left
// goes to the savings contribution (or the first open contribution, or
// miscellaneous when there is none).
func Arrears(ctx context.Context, q *store.Queries, groupID, memberID int64, amount string) ([]Line, error) {
	left := books.Cents(amount)
	owed, err := q.MemberLedgerArrears(ctx, store.MemberLedgerArrearsParams{GroupID: groupID, MembershipID: &memberID})
	if err != nil {
		return nil, err
	}
	var lines []Line
	take := func(l Line, balance string) {
		if left <= 0 {
			return
		}
		n := books.Cents(balance)
		if n > left {
			n = left
		}
		if n <= 0 {
			return
		}
		l.MemberID, l.Amount = &memberID, centsText(n)
		lines = append(lines, l)
		left -= n
	}
	for _, pass := range []func(store.MemberLedgerArrearsRow) (Line, bool){
		func(o store.MemberLedgerArrearsRow) (Line, bool) { // late fines on contributions
			return Line{Kind: "fine", ContributionID: o.ContributionID, Name: o.Name + " late fine"},
				o.Bucket == "fine" && o.ContributionID != nil
		},
		func(o store.MemberLedgerArrearsRow) (Line, bool) {
			return Line{Kind: "contribution", ContributionID: o.ContributionID, Name: o.Name},
				o.Bucket == "contribution" && o.ContributionID != nil
		},
		func(o store.MemberLedgerArrearsRow) (Line, bool) {
			return Line{Kind: "fine", FineCategoryID: o.FineCategoryID, Name: o.Name},
				o.Bucket == "fine" && o.ContributionID == nil && o.FineCategoryID != nil
		},
	} {
		for _, o := range owed {
			if l, ok := pass(o); ok {
				take(l, o.Balance)
			}
		}
	}
	if left > 0 {
		cs, err := q.PayableContributions(ctx, groupID)
		if err != nil {
			return nil, err
		}
		rest := Line{Kind: "misc", MemberID: &memberID, Amount: centsText(left), Name: "Miscellaneous"}
		if len(cs) > 0 {
			rest = Line{Kind: "contribution", MemberID: &memberID, ContributionID: &cs[0].ID, Amount: centsText(left), Name: cs[0].Name}
		}
		// Fold into an existing line for the same contribution.
		for i := range lines {
			if lines[i].Kind == rest.Kind && rest.Kind == "contribution" && *lines[i].ContributionID == *rest.ContributionID {
				lines[i].Amount = centsText(books.Cents(lines[i].Amount) + left)
				return lines, nil
			}
		}
		lines = append(lines, rest)
	}
	return lines, nil
}
