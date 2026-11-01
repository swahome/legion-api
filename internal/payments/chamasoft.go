package payments

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/swahome/legion-api/internal/auth"
)

func normalize(phone, defaultCC string) (string, error) { return auth.NormalizePhone(phone, defaultCC) }

// Order is one MPesa prompt to send.
type Order struct {
	Amount        string
	AccountNumber string // the group's account the money goes to
	Reference     string
	AccountSecret string // the account's password, if the bank set one
	Phone         string // E.164
	CallbackURL   string
}

// Answer is the gateway's reply to an order: accepted means the prompt is on
// its way to the phone and the result comes later on the callback.
type Answer struct {
	Accepted    bool
	Code        string
	Description string
}

// Gateway sends MPesa payment prompts.
type Gateway interface {
	MakePayment(ctx context.Context, o Order) (Answer, error)
}

// Chamasoft is the Chamasoft payment API the legacy app used
// (Transactions::make_online_group_payment): a bearer token from
// /access_tokens/get_access_token, then POST /transactions/make_payment. The
// result comes back on the order's callback URL.
type Chamasoft struct {
	BaseURL   string // e.g. https://api.chamasoft.com:443/api
	BasicAuth string // "user:secret" for the token call
	// PublicKey encrypts the account password into security_pass, as the
	// legacy openssl_key_encrypt did with encryptioncer.crt. Nil sends it empty.
	PublicKey *rsa.PublicKey
	HTTP      *http.Client

	mu      sync.Mutex
	token   string
	tokenAt time.Time
}

// LoadPublicKey reads a PEM certificate or public key file.
func LoadPublicKey(path string) (*rsa.PublicKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, errors.New("no PEM data in " + path)
	}
	if cert, err := x509.ParseCertificate(block.Bytes); err == nil {
		if k, ok := cert.PublicKey.(*rsa.PublicKey); ok {
			return k, nil
		}
		return nil, errors.New("the certificate's key is not RSA")
	}
	k, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		if k1, err1 := x509.ParsePKCS1PublicKey(block.Bytes); err1 == nil {
			return k1, nil
		}
		return nil, err
	}
	rk, ok := k.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("the public key is not RSA")
	}
	return rk, nil
}

func (c *Chamasoft) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (c *Chamasoft) bearer(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Since(c.tokenAt) < 5*time.Minute {
		return c.token, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.BaseURL, "/")+"/access_tokens/get_access_token", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(c.BasicAuth)))
	req.Header.Set("Content-Type", "application/json")
	res, err := c.client().Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	var out struct {
		Type  string `json:"access_token_type"`
		Token string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&out); err != nil || out.Token == "" {
		return "", fmt.Errorf("chamasoft token: HTTP %d, no token", res.StatusCode)
	}
	c.token, c.tokenAt = strings.TrimSpace(out.Type+" "+out.Token), time.Now()
	return c.token, nil
}

func (c *Chamasoft) securityPass(secret string) (string, error) {
	if secret == "" || c.PublicKey == nil {
		return "", nil
	}
	b, err := rsa.EncryptPKCS1v15(rand.Reader, c.PublicKey, []byte(secret))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// MakePayment asks Chamasoft to send the MPesa prompt.
func (c *Chamasoft) MakePayment(ctx context.Context, o Order) (Answer, error) {
	token, err := c.bearer(ctx)
	if err != nil {
		return Answer{}, err
	}
	pass, err := c.securityPass(o.AccountSecret)
	if err != nil {
		return Answer{}, err
	}
	amount, _ := strconv.ParseFloat(o.Amount, 64)
	channel := 1 // Safaricom MPesa
	if strings.HasPrefix(o.Phone, "+256") {
		channel = 3 // MTN Uganda
	}
	body, _ := json.Marshal(map[string]any{
		"request_id": time.Now().Unix(),
		"data": map[string]any{
			"transaction": map[string]any{
				"amount": amount, "account_number": o.AccountNumber, "reference_number": o.Reference,
				"security_pass": pass, "channel": channel,
			},
			"user":         map[string]any{"phone_number": strings.TrimPrefix(o.Phone, "+")},
			"callback_url": o.CallbackURL,
		},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+"/transactions/make_payment", bytes.NewReader(body))
	if err != nil {
		return Answer{}, err
	}
	req.Header.Set("Authorization", token)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.client().Do(req)
	if err != nil {
		return Answer{}, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	var out struct {
		Code        any    `json:"code"`
		Description string `json:"description"`
		Data        *struct {
			ResponseCode        any    `json:"response_code"`
			ResponseDescription string `json:"response_description"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		if res.StatusCode == http.StatusUnauthorized {
			c.mu.Lock()
			c.token = ""
			c.mu.Unlock()
		}
		return Answer{}, fmt.Errorf("chamasoft make_payment: HTTP %d: %.200s", res.StatusCode, raw)
	}
	code := fmt.Sprint(out.Code)
	if out.Data == nil {
		return Answer{Code: code, Description: out.Description}, nil
	}
	return Answer{Accepted: code == "200", Code: fmt.Sprint(out.Data.ResponseCode), Description: out.Data.ResponseDescription}, nil
}

// FakeGateway accepts every order and reports it paid a few seconds later,
// for trying the payment screens without MPesa (LEGION_CHAMASOFT_FAKE=true).
// Never use it in production.
type FakeGateway struct {
	Delay  time.Duration
	Settle func(reference, receipt string)
}

func (f *FakeGateway) MakePayment(_ context.Context, o Order) (Answer, error) {
	go func() {
		time.Sleep(f.Delay)
		if f.Settle != nil {
			f.Settle(o.Reference, "FAKE"+o.Reference[len(o.Reference)-6:])
		}
	}()
	return Answer{Accepted: true, Code: "0", Description: "Success. Request accepted for processing (test mode)"}, nil
}
