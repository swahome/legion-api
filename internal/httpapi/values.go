package httpapi

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// Money values travel as decimal strings ("4200.00") so no client ever
// rounds through a float.

// money normalises a Postgres numeric rendered as text to two decimals.
func money(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "0.00"
	}
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" {
		whole = "0"
	}
	frac = (frac + "00")[:2]
	out := whole + "." + frac
	if neg && out != "0.00" {
		out = "-" + out
	}
	return out
}

func numericMoney(n pgtype.Numeric) *string {
	if !n.Valid {
		return nil
	}
	v, err := n.Value()
	if err != nil || v == nil {
		return nil
	}
	s := money(fmt.Sprint(v))
	return &s
}

func numericString(n pgtype.Numeric) *string {
	if !n.Valid {
		return nil
	}
	v, err := n.Value()
	if err != nil || v == nil {
		return nil
	}
	s := fmt.Sprint(v)
	return &s
}

var amountPattern = regexp.MustCompile(`^\d{1,12}(\.\d{1,2})?$`)

// Amount accepts a JSON string ("4200.50") or number (4200.5).
type Amount string

func (a *Amount) UnmarshalJSON(b []byte) error {
	var s string
	if len(b) > 0 && b[0] == '"' {
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
	} else {
		var n json.Number
		if err := json.Unmarshal(b, &n); err != nil {
			return err
		}
		s = n.String()
	}
	s = strings.TrimSpace(s)
	if !amountPattern.MatchString(s) {
		return fmt.Errorf("amount must be a non-negative number with at most 2 decimals")
	}
	*a = Amount(s)
	return nil
}

func (a Amount) Numeric() pgtype.Numeric {
	var n pgtype.Numeric
	_ = n.Scan(string(a))
	return n
}

// Date is a calendar date, "2026-11-05" in JSON.
type Date struct{ time.Time }

const dateLayout = "2006-01-02"

func (d Date) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.Format(dateLayout))
}

func (d *Date) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	t, err := time.Parse(dateLayout, s)
	if err != nil {
		return fmt.Errorf("dates must look like 2026-11-05")
	}
	d.Time = t
	return nil
}

func datePtr(t *time.Time) *Date {
	if t == nil {
		return nil
	}
	return &Date{*t}
}

func (d *Date) timePtr() *time.Time {
	if d == nil {
		return nil
	}
	t := d.Time
	return &t
}

// Optional distinguishes a field that was absent from one sent as null.
type Optional[T any] struct {
	Set   bool
	Value *T
}

func (o *Optional[T]) UnmarshalJSON(b []byte) error {
	o.Set = true
	if string(b) == "null" {
		o.Value = nil
		return nil
	}
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	o.Value = &v
	return nil
}

// cents parses a decimal money string ("-4200.5") into whole cents.
// Amounts are numeric(14,2), so they fit an int64 with room to spare.
func cents(s string) int64 {
	s = money(s)
	neg := strings.HasPrefix(s, "-")
	whole, frac, _ := strings.Cut(strings.TrimPrefix(s, "-"), ".")
	var n int64
	for _, c := range whole + frac {
		n = n*10 + int64(c-'0')
	}
	if neg {
		return -n
	}
	return n
}

// formatCents renders cents as a two-decimal money string.
func formatCents(n int64) string {
	sign := ""
	if n < 0 {
		sign, n = "-", -n
	}
	return fmt.Sprintf("%s%d.%02d", sign, n/100, n%100)
}
