package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/swahome/legion-api/internal/payments"
	"github.com/swahome/legion-api/internal/store"
)

// Inbound webhooks: bank feeds, MPesa and Chamasoft callbacks. They are
// public routes, so each checks its caller (shared credentials, an IP
// allowlist or a signed URL), stores the raw call in webhook_events first,
// and answers in the shape the caller already expects from the legacy app.

// HookAuth is how one feed proves who it is: the username and password it
// puts in its JSON body (as Equity and Co-op Bank do), or the IP it calls
// from. A feed with neither set is switched off.
type HookAuth struct {
	Username, Password string
	IPs                []string
}

func (h HookAuth) enabled() bool { return h.Username != "" || len(h.IPs) > 0 }

func (h HookAuth) allows(ip, user, pass string) bool {
	if h.Username != "" && subtle.ConstantTimeCompare([]byte(user), []byte(h.Username)) == 1 &&
		subtle.ConstantTimeCompare([]byte(pass), []byte(h.Password)) == 1 {
		return true
	}
	for _, allowed := range h.IPs {
		if allowed == ip {
			return true
		}
	}
	return false
}

// HookConfig holds the feeds' credentials.
type HookConfig struct {
	Equity, Coop, ChamasoftDirect HookAuth
}

// WithPayments sets up MPesa payments, the inbound feeds and reconciliation.
func (s *Server) WithPayments(p *payments.Service, hooks HookConfig) *Server {
	s.payments, s.hooks = p, hooks
	return s
}

func (s *Server) hookRoutes(r chi.Router) {
	r.Post("/equity", s.equityHook)
	r.Post("/coop", s.coopHook)
	r.Post("/chamasoft/payments/{reference}", s.chamasoftPaymentHook)
	r.Post("/chamasoft/direct", s.chamasoftDirectHook)
	r.Post("/mpesa/validation", s.mpesaValidationHook)
	r.Post("/mpesa/confirmation", s.mpesaConfirmationHook)
	r.Post("/ussd", s.ussdHook)
	r.Post("/whatsapp", s.whatsappHook)
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// hookCall is one inbound webhook call, logged as it arrives.
type hookCall struct {
	s    *Server
	r    *http.Request
	id   int64
	body []byte
}

func (s *Server) openHook(w http.ResponseWriter, r *http.Request, source string) (*hookCall, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"message": "Request too large"})
		return nil, false
	}
	id, err := s.q.LogWebhook(r.Context(), store.LogWebhookParams{Source: source, RemoteIp: remoteIP(r), Body: redactBody(string(body))})
	if err != nil {
		slog.ErrorContext(r.Context(), "log webhook", "err", err, "source", source)
	}
	return &hookCall{s: s, r: r, id: id, body: body}, true
}

func (h *hookCall) finish(outcome, detail string, alertID int64) {
	if h.id == 0 {
		return
	}
	var a *int64
	if alertID > 0 {
		a = &alertID
	}
	// Log even if the caller hung up.
	ctx := context.WithoutCancel(h.r.Context())
	if err := h.s.q.FinishWebhook(ctx, store.FinishWebhookParams{ID: h.id, Outcome: outcome, Detail: detail, AlertID: a}); err != nil {
		slog.ErrorContext(ctx, "finish webhook", "err", err)
	}
}

func (s *Server) paymentsReady(w http.ResponseWriter) bool {
	if s.payments == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"message": "Payments are not set up"})
		return false
	}
	return true
}

// str reads a JSON value that may come as a string or a number.
func str(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(t)
	case json.Number:
		return t.String()
	case float64:
		return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%.2f", t), "0"), ".")
	default:
		return strings.TrimSpace(fmt.Sprint(t))
	}
}

func decodeLoose(b []byte) (map[string]any, bool) {
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil || m == nil {
		return nil, false
	}
	return m, true
}

func (s *Server) bankByCode(ctx context.Context, slug string) *int64 {
	id, err := s.q.BankIDBySlug(ctx, slug)
	if err != nil {
		return nil
	}
	return &id
}

// equityHook takes Equity Bank's transaction feed (legacy Endpoint::index).
// Body: {username, password, tranid, tranCurrency, tranDate, tranAmount,
// trandrcr C|D, accid, refNo, tranType, tranParticular, tranRemarks}.
func (s *Server) equityHook(w http.ResponseWriter, r *http.Request) {
	if !s.paymentsReady(w) {
		return
	}
	h, ok := s.openHook(w, r, payments.SourceEquity)
	if !ok {
		return
	}
	reply := func(status, msg, ack string, code int) {
		writeJSON(w, http.StatusOK, map[string]any{"status": status, "input": "post", "message": msg, "ACK": ack, "responseCode": code})
	}
	m, ok := decodeLoose(h.body)
	if !s.hooks.Equity.enabled() || !s.hooks.Equity.allows(remoteIP(r), str(m["username"]), str(m["password"])) {
		h.finish("rejected", "access denied", 0)
		reply("error", "Access Denied", "NO", 2)
		return
	}
	if !ok {
		h.finish("rejected", "blank request", 0)
		reply("success", "Blank Request", "NO", 2)
		return
	}
	dir := int16(1)
	if strings.EqualFold(str(m["trandrcr"]), "D") {
		dir = -1
	}
	at, _ := payments.ParseTime(str(m["tranDate"]), s.location)
	particulars := strings.TrimSpace(strings.Trim(str(m["tranParticular"]), "0") + " " + strings.Trim(str(m["tranRemarks"]), "0"))
	id, created, auto, err := s.payments.Receive(r.Context(), payments.Alert{
		Source: payments.SourceEquity, ProviderRef: str(m["tranid"]), BankID: s.bankByCode(r.Context(), "equity-bank"),
		AccountNumber: str(m["accid"]), Direction: dir, Amount: str(m["tranAmount"]), Currency: str(m["tranCurrency"]),
		At: at, Reference: str(m["refNo"]), Particulars: particulars, Raw: redact(h.body),
	})
	var refused payments.Error
	switch {
	case errors.As(err, &refused):
		h.finish("rejected", refused.Msg, 0)
		reply("success", "Could not insert transaction alert: "+refused.Msg, "NO", 2)
	case err != nil:
		slog.ErrorContext(r.Context(), "equity hook", "err", err)
		h.finish("error", err.Error(), 0)
		reply("success", "Could not insert transaction alert", "NO", 2)
	case !created:
		h.finish("duplicate", "", id)
		reply("success", "Duplicate Request", "OK", 1)
	default:
		h.finish(outcome(auto), "", id)
		reply("success", "Request OK", "OK", 0)
	}
}

func outcome(auto bool) string {
	if auto {
		return "reconciled"
	}
	return "accepted"
}

// coopHook takes Co-op Bank's IPN (legacy Coop_bank::ipn). Body:
// {username, password, TransactionId, AcctNo, Amount, Currency, EventType
// CREDIT|DEBIT, PaymentRef, Narration, PostingDate, TransactionDate}.
func (s *Server) coopHook(w http.ResponseWriter, r *http.Request) {
	if !s.paymentsReady(w) {
		return
	}
	h, ok := s.openHook(w, r, payments.SourceCoop)
	if !ok {
		return
	}
	m, ok := decodeLoose(h.body)
	// Co-op sends HTTP basic auth; credentials in the body work too.
	user, pass, basic := r.BasicAuth()
	if !basic {
		user, pass = str(m["username"]), str(m["password"])
	}
	if !s.hooks.Coop.enabled() || !s.hooks.Coop.allows(remoteIP(r), user, pass) {
		h.finish("rejected", "access denied", 0)
		writeJSON(w, http.StatusOK, map[string]any{"status": "error", "input": "post", "message": "Access Denied", "ACK": "NO", "responseCode": 2})
		return
	}
	// The legacy endpoint answered the same way whatever happened next.
	ack := map[string]any{"Message": "Successfully received data", "MessageCode": 200}
	if !ok {
		h.finish("rejected", "blank request", 0)
		writeJSON(w, http.StatusOK, map[string]any{"status": "success", "input": "post", "message": "Blank Request", "ACK": "NO", "responseCode": 2})
		return
	}
	dir := int16(1)
	if strings.EqualFold(str(m["EventType"]), "DEBIT") {
		dir = -1
	}
	at, ok := payments.ParseTime(str(m["TransactionDate"]), s.location)
	if !ok {
		at, _ = payments.ParseTime(str(m["PostingDate"]), s.location)
	}
	id, created, auto, err := s.payments.Receive(r.Context(), payments.Alert{
		Source: payments.SourceCoop, ProviderRef: str(m["TransactionId"]),
		BankID: s.bankByCode(r.Context(), "co-operative-bank-of-kenya"), AccountNumber: str(m["AcctNo"]), Direction: dir,
		Amount: str(m["Amount"]), Currency: str(m["Currency"]), At: at, Reference: str(m["PaymentRef"]),
		Particulars: str(m["Narration"]), Raw: redact(h.body),
	})
	var refused payments.Error
	switch {
	case errors.As(err, &refused):
		h.finish("rejected", refused.Msg, 0)
	case err != nil:
		slog.ErrorContext(r.Context(), "coop hook", "err", err)
		h.finish("error", err.Error(), 0)
	case !created:
		h.finish("duplicate", "", id)
	default:
		h.finish(outcome(auto), "", id)
	}
	writeJSON(w, http.StatusOK, ack)
}

// redact drops credentials from a feed body before it is kept on the alert.
func redact(body []byte) []byte {
	m, ok := decodeLoose(body)
	if !ok {
		return nil
	}
	delete(m, "password")
	delete(m, "username")
	b, _ := json.Marshal(m)
	return b
}

// chamasoftResult reads the callback body Chamasoft sends for a payment:
// {code, description, data: {reference_number, transaction_id,
// transaction_date, phone_number, account_number, amount, customer_name,
// payment_description, organization_balance}}.
type chamasoftResult struct {
	code, description                                string
	reference, receipt, date, phone, account, amount string
	customer, paymentDescription                     string
}

func readChamasoft(b []byte) (chamasoftResult, bool) {
	m, ok := decodeLoose(b)
	if !ok {
		return chamasoftResult{}, false
	}
	d, _ := m["data"].(map[string]any)
	return chamasoftResult{
		code: str(m["code"]), description: str(m["description"]),
		reference: str(d["reference_number"]), receipt: str(d["transaction_id"]), date: str(d["transaction_date"]),
		phone: str(d["phone_number"]), account: str(d["account_number"]), amount: str(d["amount"]),
		customer: str(d["customer_name"]), paymentDescription: str(d["payment_description"]),
	}, true
}

// chamasoftPaymentHook is the result of a payment a member started (legacy
// transaction_alerts/online_banking_payment). The URL carries the request's
// reference and a key only this API can make.
func (s *Server) chamasoftPaymentHook(w http.ResponseWriter, r *http.Request) {
	if !s.paymentsReady(w) {
		return
	}
	h, ok := s.openHook(w, r, payments.SourceChamasoft)
	if !ok {
		return
	}
	ref := chi.URLParam(r, "reference")
	reply := func(code, msg string) { writeJSON(w, http.StatusOK, map[string]string{"code": code, "response": msg}) }
	if !s.payments.ValidCallbackKey(ref, r.URL.Query().Get("key")) {
		h.finish("rejected", "bad key", 0)
		reply("1", "Unknown request")
		return
	}
	res, ok := readChamasoft(h.body)
	if !ok || res.code == "" {
		h.finish("rejected", "file error", 0)
		reply("1", "File error")
		return
	}
	if res.reference != "" && res.reference != ref {
		h.finish("rejected", "reference mismatch", 0)
		reply("1", "Reference does not match")
		return
	}
	at, _ := payments.ParseTime(res.date, s.location)
	id, dup, err := s.payments.Settle(r.Context(), ref, payments.PaymentResult{
		Success: res.code == "200" && res.receipt != "" && res.receipt != "0", Code: res.code,
		Description: firstOf(res.paymentDescription, res.description), Receipt: res.receipt, At: at, Phone: res.phone,
		Raw: h.body,
	})
	switch {
	case errors.Is(err, payments.ErrNotFound):
		h.finish("rejected", "deposit not found", 0)
		reply("1", "Deposit not found")
	case err != nil:
		slog.ErrorContext(r.Context(), "chamasoft payment hook", "err", err)
		h.finish("error", err.Error(), 0)
		reply("1", "Could not record the payment")
	case dup:
		h.finish("duplicate", "", id)
		reply("1", "Duplicate deposit")
	default:
		h.finish("accepted", res.code, id)
		reply("0", "Updated")
	}
}

func firstOf(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// chamasoftDirectHook is money paid straight to a group's account number
// through Chamasoft's paybill (legacy
// transaction_alerts/reconcile_direct_online_banking_payment). It is matched
// to the member by phone and spread over what they owe.
func (s *Server) chamasoftDirectHook(w http.ResponseWriter, r *http.Request) {
	if !s.paymentsReady(w) {
		return
	}
	h, ok := s.openHook(w, r, payments.SourceChamasoftDirect)
	if !ok {
		return
	}
	reply := func(code, msg string) { writeJSON(w, http.StatusOK, map[string]string{"code": code, "response": msg}) }
	keyOK := subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("key")), []byte(s.payments.HookSecret("chamasoft_direct"))) == 1
	if !keyOK && !s.hooks.ChamasoftDirect.allows(remoteIP(r), "", "") {
		h.finish("rejected", "unknown host", 0)
		reply("1", "Unknown host")
		return
	}
	res, ok := readChamasoft(h.body)
	if !ok {
		h.finish("rejected", "file error", 0)
		reply("1", "File error")
		return
	}
	if res.code == "" || res.account == "" || res.amount == "" || res.receipt == "" || res.receipt == "0" {
		h.finish("rejected", "missing fields", 0)
		reply("1", "missing files")
		return
	}
	if res.code != "200" {
		h.finish("rejected", "code "+res.code, 0)
		reply("1", "Code number "+res.code)
		return
	}
	at, _ := payments.ParseTime(res.date, s.location)
	id, created, auto, err := s.payments.Receive(r.Context(), payments.Alert{
		Source: payments.SourceChamasoftDirect, ProviderRef: res.receipt, AccountNumber: res.account, Direction: 1,
		Amount: res.amount, At: at, Reference: res.reference, Particulars: firstOf(res.description, res.paymentDescription),
		Phone: res.phone, PayerName: res.customer, Raw: h.body,
	})
	s.finishReceive(w, r, h, id, created, auto, err, func(ok bool, msg string) {
		if ok {
			reply("0", msg)
		} else {
			reply("1", msg)
		}
	})
}

func (s *Server) finishReceive(w http.ResponseWriter, r *http.Request, h *hookCall, id int64, created, auto bool, err error,
	reply func(ok bool, msg string)) {
	var refused payments.Error
	switch {
	case errors.As(err, &refused):
		h.finish("rejected", refused.Msg, 0)
		reply(false, refused.Msg)
	case err != nil:
		slog.ErrorContext(r.Context(), "payment hook", "err", err, "source", h.r.URL.Path)
		h.finish("error", err.Error(), 0)
		reply(false, "Could not record the payment")
	case !created:
		h.finish("duplicate", "", id)
		reply(false, "Duplicate")
	default:
		h.finish(outcome(auto), "", id)
		reply(true, "Updated")
	}
}

// mpesaHookKey guards the Daraja C2B URLs, registered with Safaricom as
// .../v1/hooks/mpesa/{validation,confirmation}?key=<secret>.
func (s *Server) mpesaHookKey(r *http.Request) bool {
	return subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("key")), []byte(s.payments.HookSecret("mpesa"))) == 1
}

// mpesaValidationHook accepts a paybill payment only when the account
// number (BillRefNumber) is one of the groups' accounts.
func (s *Server) mpesaValidationHook(w http.ResponseWriter, r *http.Request) {
	if !s.paymentsReady(w) {
		return
	}
	h, ok := s.openHook(w, r, "mpesa_validation")
	if !ok {
		return
	}
	reject := func(code, why string) {
		h.finish("rejected", why, 0)
		writeJSON(w, http.StatusOK, map[string]string{"ResultCode": code, "ResultDesc": "Rejected"})
	}
	if !s.mpesaHookKey(r) {
		reject("C2B00016", "bad key")
		return
	}
	m, ok := decodeLoose(h.body)
	if !ok {
		reject("C2B00016", "file error")
		return
	}
	account := firstOf(str(m["BillRefNumber"]), str(m["InvoiceNumber"]))
	known, err := s.payments.AccountKnown(r.Context(), account)
	if err != nil {
		slog.ErrorContext(r.Context(), "mpesa validation", "err", err)
		reject("C2B00016", err.Error())
		return
	}
	if !known {
		reject("C2B00012", "account not recognized")
		return
	}
	h.finish("accepted", account, 0)
	writeJSON(w, http.StatusOK, map[string]any{"ResultCode": 0, "ResultDesc": "Accepted"})
}

// mpesaConfirmationHook records a paybill payment Safaricom has completed.
func (s *Server) mpesaConfirmationHook(w http.ResponseWriter, r *http.Request) {
	if !s.paymentsReady(w) {
		return
	}
	h, ok := s.openHook(w, r, payments.SourceMpesa)
	if !ok {
		return
	}
	// Safaricom only needs to hear that the call arrived.
	accepted := map[string]any{"ResultCode": 0, "ResultDesc": "Accepted"}
	if !s.mpesaHookKey(r) {
		h.finish("rejected", "bad key", 0)
		writeJSON(w, http.StatusOK, map[string]any{"ResultCode": 1, "ResultDesc": "Rejected"})
		return
	}
	m, ok := decodeLoose(h.body)
	if !ok {
		h.finish("rejected", "file error", 0)
		writeJSON(w, http.StatusOK, accepted)
		return
	}
	at, _ := payments.ParseTime(str(m["TransTime"]), s.location)
	name := strings.Join(strings.Fields(str(m["FirstName"])+" "+str(m["MiddleName"])+" "+str(m["LastName"])), " ")
	id, created, auto, err := s.payments.Receive(r.Context(), payments.Alert{
		Source: payments.SourceMpesa, ProviderRef: str(m["TransID"]),
		AccountNumber: firstOf(str(m["BillRefNumber"]), str(m["InvoiceNumber"])), Direction: 1,
		Amount: str(m["TransAmount"]), At: at, Reference: str(m["BusinessShortCode"]),
		Particulars: "MPesa " + str(m["TransactionType"]), Phone: str(m["MSISDN"]), PayerName: name, Raw: h.body,
	})
	s.finishReceive(w, r, h, id, created, auto, err, func(bool, string) { writeJSON(w, http.StatusOK, accepted) })
}
