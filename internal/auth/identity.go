package auth

import (
	"errors"
	"strings"
)

var ErrInvalidIdentity = errors.New("invalid phone number or email")

// Identity is what a user types into the login box: a phone number or an email.
type Identity struct {
	Phone string // E.164, set when the identity is a phone number
	Email string // lower-cased, set when the identity is an email
}

// Key is the normalised form used for throttling.
func (i Identity) Key() string {
	if i.Phone != "" {
		return i.Phone
	}
	return i.Email
}

// ParseIdentity accepts an email address or a phone number in any of the
// forms the legacy app stored: "+254712345678", "254712345678",
// "0712345678", "712345678", with or without spaces and dashes.
// defaultCC is the calling code without "+" (e.g. "254").
func ParseIdentity(raw, defaultCC string) (Identity, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Identity{}, ErrInvalidIdentity
	}
	if strings.Contains(raw, "@") {
		at := strings.LastIndex(raw, "@")
		if at == 0 || at == len(raw)-1 || strings.ContainsAny(raw, " \t") {
			return Identity{}, ErrInvalidIdentity
		}
		return Identity{Email: strings.ToLower(raw)}, nil
	}
	phone, err := NormalizePhone(raw, defaultCC)
	if err != nil {
		return Identity{}, err
	}
	return Identity{Phone: phone}, nil
}

// NormalizePhone converts a phone number to E.164.
func NormalizePhone(raw, defaultCC string) (string, error) {
	plus := strings.HasPrefix(strings.TrimSpace(raw), "+")
	var b strings.Builder
	for _, r := range raw {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '+' || r == ' ' || r == '-' || r == '(' || r == ')' || r == '.':
		default:
			return "", ErrInvalidIdentity
		}
	}
	digits := b.String()
	switch {
	case plus:
		// already international
	case strings.HasPrefix(digits, "00"):
		digits = digits[2:]
	case strings.HasPrefix(digits, defaultCC) && len(digits) > len(defaultCC)+8:
		// e.g. 254712345678
	case strings.HasPrefix(digits, "0"):
		digits = defaultCC + digits[1:]
	default:
		digits = defaultCC + digits
	}
	if len(digits) < 7 || len(digits) > 15 || digits[0] == '0' {
		return "", ErrInvalidIdentity
	}
	return "+" + digits, nil
}
