// Package payments handles money that arrives from outside: bank feeds,
// MPesa payments and the payments members start from the app. Each one is
// stored as a transaction alert on a group account, then reconciled: turned
// into the deposits (or withdrawals) that account for it, automatically when
// the payer and what they paid for are known, or by an official.
package payments

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/swahome/legion-api/internal/auth"
	"github.com/swahome/legion-api/internal/books"
	"github.com/swahome/legion-api/internal/store"
)

// Error is a refusal the caller can show, as in books.
type Error = books.Error

// Alert sources.
const (
	SourceEquity          = "equity"
	SourceCoop            = "coop"
	SourceChamasoft       = "chamasoft"        // a payment a member started (STK push)
	SourceChamasoftDirect = "chamasoft_direct" // paid straight to the group's account number
	SourceMpesa           = "mpesa"            // Daraja C2B confirmation
)

// Alert is one movement on an account as a bank or MPesa reports it.
type Alert struct {
	Source        string
	ProviderRef   string
	BankID        *int64
	AccountNumber string
	// AccountID pins the alert to a group account (a payment request knows
	// it); otherwise the account is found by AccountNumber.
	AccountID   *int64
	GroupID     *int64
	Direction   int16 // 1 in, -1 out
	Amount      string
	Currency    string
	At          time.Time
	Reference   string
	Particulars string
	Phone       string // any format; stored as E.164 when it parses
	PayerName   string
	Raw         []byte
}

// Ingest stores an alert once. created is false when the same provider
// transaction was already in (the bank sent it again); id is then the
// existing alert's.
func Ingest(ctx context.Context, q *store.Queries, a Alert, defaultCC string) (id int64, created bool, err error) {
	a.ProviderRef = strings.TrimSpace(a.ProviderRef)
	if a.ProviderRef == "" {
		return 0, false, Error{Code: "missing_fields", Msg: "transaction id is required"}
	}
	amount, ok := NormAmount(a.Amount)
	if !ok {
		return 0, false, Error{Code: "invalid_amount", Msg: "amount must be more than zero"}
	}
	if a.Direction != 1 && a.Direction != -1 {
		return 0, false, Error{Code: "invalid_direction", Msg: "unknown debit or credit"}
	}
	if a.Currency == "" {
		a.Currency = "KES"
	}
	if a.At.IsZero() {
		a.At = time.Now()
	}
	a.AccountNumber = strings.TrimSpace(a.AccountNumber)
	if a.AccountID == nil && a.AccountNumber != "" {
		a.GroupID, a.AccountID, err = findAccount(ctx, q, a.AccountNumber, a.BankID)
		if err != nil {
			return 0, false, err
		}
	}
	var phone *string
	if p, err := auth.NormalizePhone(a.Phone, defaultCC); err == nil && a.Phone != "" {
		phone = &p
	}
	raw := a.Raw
	if len(raw) == 0 || raw[0] != '{' {
		raw = []byte("{}")
	}
	id, err = q.InsertAlert(ctx, store.InsertAlertParams{
		Source: a.Source, ProviderRef: a.ProviderRef, BankID: a.BankID, AccountNumber: a.AccountNumber,
		GroupID: a.GroupID, AccountID: a.AccountID, Direction: a.Direction, Amount: amount, Currency: a.Currency,
		TransactionAt: a.At, Reference: strings.TrimSpace(a.Reference), Particulars: strings.TrimSpace(a.Particulars),
		Phone: phone, PayerName: strings.Join(strings.Fields(a.PayerName), " "), Raw: raw,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		id, err = q.GetAlertByRef(ctx, store.GetAlertByRefParams{Source: a.Source, ProviderRef: a.ProviderRef, Direction: a.Direction})
		return id, false, err
	}
	return id, err == nil, err
}

// findAccount picks the group account an account number belongs to. Two
// groups with the same number at the same bank cannot be told apart: the
// alert is then left for the platform admins to assign.
func findAccount(ctx context.Context, q *store.Queries, number string, bankID *int64) (*int64, *int64, error) {
	rows, err := q.FindAccountsByNumber(ctx, store.FindAccountsByNumberParams{AccountNumber: number, BankID: bankID})
	if err != nil || len(rows) == 0 {
		return nil, nil, err
	}
	if bankID != nil {
		var same []store.FindAccountsByNumberRow
		for _, r := range rows {
			if r.BankID != nil && *r.BankID == *bankID {
				same = append(same, r)
			}
		}
		if len(same) > 0 {
			rows = same
		}
	}
	g := rows[0].GroupID
	for _, r := range rows[1:] {
		if r.GroupID != g {
			return nil, nil, nil
		}
	}
	return &rows[0].GroupID, &rows[0].ID, nil
}

var amountRe = regexp.MustCompile(`^\d{1,12}(\.\d+)?$`)

var currencyPrefix = regexp.MustCompile(`(?i)^(kes|ksh|kshs|ugx|tzs|usd)\.?\s*`)

// NormAmount turns "1,500", "KES 1500.5" or "-1500.00" into "1500.50"-style
// text (the direction comes from the feed's debit/credit flag, not the sign),
// and reports false for anything that is not a non-zero amount.
func NormAmount(s string) (string, bool) {
	s = strings.ReplaceAll(strings.TrimSpace(s), ",", "")
	s = strings.TrimSpace(currencyPrefix.ReplaceAllString(s, ""))
	s = strings.TrimPrefix(s, "-")
	if !amountRe.MatchString(s) {
		return "", false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f <= 0 {
		return "", false
	}
	out := strconv.FormatFloat(f, 'f', 2, 64)
	return out, books.Cents(out) > 0
}

// ParseTime reads the many date formats banks and MPesa send, in Nairobi
// time: unix seconds, 20261101143000 (MPesa), 2026-11-01 14:30:00,
// 2026-11-01T14:30:00, 01-11-2026 14:30:00, 01/11/2026 and plain dates.
func ParseTime(s string, loc *time.Location) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		switch {
		case len(s) == 14:
			if t, err := time.ParseInLocation("20060102150405", s, loc); err == nil {
				return t, true
			}
		case len(s) == 13:
			return time.UnixMilli(n), true
		case len(s) >= 9 && len(s) <= 11:
			return time.Unix(n, 0), true
		}
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05.000", "2006-01-02T15:04:05", "2006-01-02 15:04:05",
		"2006-01-02 15:04", "02-01-2006 15:04:05", "02/01/2006 15:04:05", "02-Jan-2006 15:04:05", "2006-01-02",
		"02-01-2006", "02/01/2006", "02-Jan-2006", "02-Jan-06"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func num(s string) pgtype.Numeric {
	var n pgtype.Numeric
	_ = n.Scan(s)
	return n
}

// Text renders a numeric column as "1500.00".
func Text(n pgtype.Numeric) string {
	v, err := n.Value()
	if err != nil || v == nil {
		return "0.00"
	}
	return books.Format(fmt.Sprint(v))
}
