package mobile

import (
	"context"
	"errors"
)

// OTPSender delivers a sign-in PIN by SMS or email. Africa's Talking SMS
// arrives with milestone 5; until then no sender is configured and the app
// is told the PIN couldn't be sent, as the legacy app said on failure.
type OTPSender interface {
	SendOTP(ctx context.Context, phone, email, pin, appSignature string) error
}

var errNoOTPSender = errors.New("no OTP sender configured")

type noOTPSender struct{}

func (noOTPSender) SendOTP(context.Context, string, string, string, string) error {
	return errNoOTPSender
}
