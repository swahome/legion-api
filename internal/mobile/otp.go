package mobile

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/swahome/legion-api/internal/account"
	"github.com/swahome/legion-api/internal/sms"
	"github.com/swahome/legion-api/internal/store"
)

// OTPSender delivers a sign-in PIN by SMS or email.
type OTPSender interface {
	SendOTP(ctx context.Context, phone, email, pin, appSignature string) error
}

var errNoOTPSender = errors.New("no OTP sender configured")

type noOTPSender struct{}

func (noOTPSender) SendOTP(context.Context, string, string, string, string) error {
	return errNoOTPSender
}

// SMSOTP texts PINs through the SMS outbox. The app signature ends the
// message so Android's SMS Retriever can read the PIN in for the user, as
// the legacy user-activation-pin template did.
type SMSOTP struct {
	Pool *pgxpool.Pool
	Kick func()
}

var errEmailOTP = errors.New("PINs can only be texted to a phone number")

func (o SMSOTP) SendOTP(ctx context.Context, phone, _, pin, appSignature string) error {
	if phone == "" {
		return errEmailOTP
	}
	err := pgx.BeginFunc(ctx, o.Pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		body := "<#> Kindly use the code " + pin + " to activate " + account.AppName(ctx, q) +
			" Mobile Application Account. DO NOT share this code"
		if sig := strings.TrimSpace(appSignature); sig != "" {
			body += "\n" + sig
		}
		var uid *int64
		if u, err := q.GetUserByPhone(ctx, &phone); err == nil {
			uid = &u.ID
		}
		_, err := sms.Queue(ctx, q, sms.Message{To: phone, Body: body, Kind: "app_pin", UserID: uid})
		return err
	})
	if err == nil && o.Kick != nil {
		o.Kick()
	}
	return err
}
