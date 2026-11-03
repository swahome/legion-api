package payments

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ChargeQuoter is a gateway that can say what MPesa charges a member to
// pay an amount into the group's account (the app's "convenience charge").
type ChargeQuoter interface {
	DepositCharge(ctx context.Context, accountNumber, accountSecret string, amount float64) (charge float64, problem string, err error)
}

// ErrNoPaymentAccount is DepositCharge's answer for a group without an
// account to pay into.
var ErrNoPaymentAccount = errors.New("no payment account")

// DepositCharge quotes the charge on paying amount into the group's
// payment account. problem is the gateway's reason when it gave no figure.
func (s *Service) DepositCharge(ctx context.Context, groupID int64, amount float64) (float64, string, error) {
	cq, ok := s.gatewayQuoter()
	if !ok {
		return 0, "", ErrNotConfigured
	}
	acct, err := s.q().DefaultPaymentAccount(ctx, groupID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", ErrNoPaymentAccount
	}
	if err != nil {
		return 0, "", err
	}
	return cq.DepositCharge(ctx, acct.AccountNumber, acct.AccountPassword, amount)
}

func (s *Service) gatewayQuoter() (ChargeQuoter, bool) {
	if !s.Enabled() {
		return nil, false
	}
	cq, ok := s.Gateway.(ChargeQuoter)
	return cq, ok
}

// DepositCharge is POST /transactions/calculate_deposit_charge, as the
// legacy Transactions::calculate_convenience_charge called it.
func (c *Chamasoft) DepositCharge(ctx context.Context, accountNumber, secret string, amount float64) (float64, string, error) {
	token, err := c.bearer(ctx)
	if err != nil {
		return 0, "", err
	}
	pass, err := c.securityPass(secret)
	if err != nil {
		return 0, "", err
	}
	body, _ := json.Marshal(map[string]any{"request_id": time.Now().Unix(), "data": map[string]any{
		"transaction": map[string]any{"amount": amount, "account_number": accountNumber, "security_pass": pass}}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+"/transactions/calculate_deposit_charge", bytes.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Authorization", token)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.client().Do(req)
	if err != nil {
		return 0, "", err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	var out struct {
		Description string `json:"description"`
		Data        *struct {
			Charge any `json:"charge_amount"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return 0, "", fmt.Errorf("chamasoft calculate_deposit_charge: HTTP %d: %.200s", res.StatusCode, raw)
	}
	if out.Data == nil {
		return 0, out.Description, nil
	}
	v, err := strconv.ParseFloat(fmt.Sprint(out.Data.Charge), 64)
	if err != nil {
		return 0, fmt.Sprint(out.Data.Charge), nil
	}
	return v, "", nil
}

// DepositCharge is free in test mode.
func (f *FakeGateway) DepositCharge(context.Context, string, string, float64) (float64, string, error) {
	return 0, "", nil
}
