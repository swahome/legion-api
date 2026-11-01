// Package sms sends text messages through Africa's Talking. Messages are
// written to an outbox table in the same transaction as the change that
// causes them (Queue) and a dispatcher sends them, retrying while the
// gateway is down, so a crash never loses or half-sends a message.
package sms

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Result is what the gateway said about one message.
type Result struct {
	MessageID string
	Status    string // the gateway's word for it, e.g. Success, InvalidPhoneNumber
	Cost      string
	// Retry is true when sending again later may work (gateway trouble,
	// low balance); false when the message can never be delivered.
	Retry bool
	Err   error // nil when the gateway accepted the message
}

// Sender delivers one message.
type Sender interface {
	Send(ctx context.Context, to, body string) Result
}

// AfricasTalking sends through the Africa's Talking bulk SMS API
// (POST /version1/messaging), as the legacy app did.
type AfricasTalking struct {
	Username string
	APIKey   string
	SenderID string // optional alphanumeric sender id or short code
	Endpoint string // defaults to the live API, or the sandbox for username "sandbox"
	Client   *http.Client
}

const (
	liveEndpoint    = "https://api.africastalking.com/version1/messaging"
	sandboxEndpoint = "https://api.sandbox.africastalking.com/version1/messaging"
)

// Status codes from the Africa's Talking messaging API. 100-102 mean the
// message was accepted.
var permanentCodes = map[int]bool{
	402: true, // InvalidSenderId
	403: true, // InvalidPhoneNumber
	404: true, // UnsupportedNumberType
	406: true, // UserInBlacklist
	407: true, // CouldNotRoute
}

type atResponse struct {
	SMSMessageData struct {
		Message    string `json:"Message"`
		Recipients []struct {
			StatusCode int    `json:"statusCode"`
			Number     string `json:"number"`
			Status     string `json:"status"`
			Cost       string `json:"cost"`
			MessageID  string `json:"messageId"`
		} `json:"Recipients"`
	} `json:"SMSMessageData"`
}

func (a *AfricasTalking) endpoint() string {
	switch {
	case a.Endpoint != "":
		return a.Endpoint
	case a.Username == "sandbox":
		return sandboxEndpoint
	}
	return liveEndpoint
}

func (a *AfricasTalking) Send(ctx context.Context, to, body string) Result {
	form := url.Values{"username": {a.Username}, "to": {to}, "message": {body}}
	if a.SenderID != "" {
		form.Set("from", a.SenderID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint(), strings.NewReader(form.Encode()))
	if err != nil {
		return Result{Err: err}
	}
	req.Header.Set("apiKey", a.APIKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := a.Client
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return Result{Retry: true, Err: err}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 300 {
		// 401 is a wrong key, which an operator can fix; keep the message.
		return Result{Retry: true, Status: "HTTP " + strconv.Itoa(resp.StatusCode),
			Err: fmt.Errorf("africa's talking: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))}
	}
	var out atResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return Result{Retry: true, Err: fmt.Errorf("africa's talking: bad response: %w", err)}
	}
	if len(out.SMSMessageData.Recipients) == 0 {
		// The gateway refused the whole request, e.g. "InvalidPhoneNumber".
		msg := out.SMSMessageData.Message
		return Result{Status: msg, Err: fmt.Errorf("africa's talking: %s", msg)}
	}
	r := out.SMSMessageData.Recipients[0]
	res := Result{MessageID: r.MessageID, Status: r.Status, Cost: r.Cost}
	if r.StatusCode < 100 || r.StatusCode > 102 {
		res.Retry = !permanentCodes[r.StatusCode]
		res.Err = fmt.Errorf("africa's talking: %s (%d)", r.Status, r.StatusCode)
	}
	return res
}

// LogSender writes messages to the log instead of sending them. It is the
// sender when no Africa's Talking account is configured, so a developer
// can read sign-up and reset codes from the server log.
type LogSender struct{ Log *slog.Logger }

func (l LogSender) Send(ctx context.Context, to, body string) Result {
	log := l.Log
	if log == nil {
		log = slog.Default()
	}
	log.InfoContext(ctx, "sms: not sent, no gateway configured", "to", to, "body", body)
	return Result{Status: "Logged"}
}

// ErrDisabled marks messages dropped because SMS is switched off.
var ErrDisabled = errors.New("SMS is switched off in the platform settings")
