package sms

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/swahome/legion-api/internal/store"
	"github.com/swahome/legion-api/internal/testdb"
)

func TestAfricasTalking(t *testing.T) {
	var got url.Values
	var key string
	reply := `{"SMSMessageData":{"Message":"Sent to 1/1 Total Cost: KES 0.8000","Recipients":[{"statusCode":101,"number":"+254711000001","status":"Success","cost":"KES 0.8000","messageId":"ATXid_1"}]}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got, _ = url.ParseQuery(string(b))
		key = r.Header.Get("apiKey")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, reply)
	}))
	defer srv.Close()
	at := &AfricasTalking{Username: "sandbox", APIKey: "k", SenderID: "LEGION", Endpoint: srv.URL}

	res := at.Send(context.Background(), "+254711000001", "hello")
	if res.Err != nil || res.MessageID != "ATXid_1" || res.Cost != "KES 0.8000" || res.Status != "Success" {
		t.Fatalf("%+v", res)
	}
	if got.Get("username") != "sandbox" || got.Get("to") != "+254711000001" || got.Get("message") != "hello" ||
		got.Get("from") != "LEGION" || key != "k" {
		t.Fatalf("sent %v key %q", got, key)
	}

	reply = `{"SMSMessageData":{"Message":"Sent to 0/1","Recipients":[{"statusCode":403,"number":"+254711000001","status":"InvalidPhoneNumber","cost":"0","messageId":"None"}]}}`
	if res := at.Send(context.Background(), "+254711000001", "x"); res.Err == nil || res.Retry {
		t.Fatalf("invalid number: %+v", res)
	}
	reply = `{"SMSMessageData":{"Message":"Sent to 0/1","Recipients":[{"statusCode":405,"number":"+254711000001","status":"InsufficientBalance","cost":"0","messageId":"None"}]}}`
	if res := at.Send(context.Background(), "+254711000001", "x"); res.Err == nil || !res.Retry {
		t.Fatalf("low balance: %+v", res)
	}
	srv.Close()
	if res := at.Send(context.Background(), "+254711000001", "x"); res.Err == nil || !res.Retry {
		t.Fatalf("gateway down: %+v", res)
	}
	if (&AfricasTalking{Username: "sandbox"}).endpoint() != sandboxEndpoint || (&AfricasTalking{Username: "acme"}).endpoint() != liveEndpoint {
		t.Fatal("endpoint")
	}
}

type fakeSender struct{ results map[string]Result }

func (f fakeSender) Send(_ context.Context, to, _ string) Result { return f.results[to] }

func TestDispatcherRetriesAndFails(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	q := store.New(pool)
	for _, to := range []string{"+254700000001", "+254700000002", "+254700000003"} {
		if _, err := Queue(ctx, q, Message{To: to, Body: "hi", Kind: "test"}); err != nil {
			t.Fatal(err)
		}
	}
	f := fakeSender{results: map[string]Result{
		"+254700000001": {MessageID: "m1", Status: "Success", Cost: "KES 0.80"},
		"+254700000002": {Status: "InternalServerError", Retry: true, Err: errors.New("down")},
		"+254700000003": {Status: "InvalidPhoneNumber", Err: errors.New("bad number")},
	}}
	d := NewDispatcher(pool, f, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if n, err := d.SendDue(ctx); err != nil || n != 3 {
		t.Fatalf("sent %d, %v", n, err)
	}
	status := func(to string) (s string, attempts int) {
		_ = pool.QueryRow(ctx, `SELECT status, attempts FROM sms_messages WHERE to_phone = $1`, to).Scan(&s, &attempts)
		return
	}
	if s, _ := status("+254700000001"); s != "sent" {
		t.Fatal(s)
	}
	if s, a := status("+254700000002"); s != "queued" || a != 1 {
		t.Fatal(s, a)
	}
	if s, _ := status("+254700000003"); s != "failed" {
		t.Fatal(s)
	}
	// Not due again until the backoff passes.
	if n, _ := d.SendDue(ctx); n != 0 {
		t.Fatalf("resent %d too early", n)
	}
	// After every retry it gives up.
	for i := 0; i < len(backoff); i++ {
		if _, err := pool.Exec(ctx, `UPDATE sms_messages SET next_attempt_at = now() WHERE status = 'queued'`); err != nil {
			t.Fatal(err)
		}
		if _, err := d.SendDue(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if s, a := status("+254700000002"); s != "failed" || a != len(backoff)+1 {
		t.Fatal(s, a)
	}

	// The legacy disable_smses switch drops messages.
	if _, err := pool.Exec(ctx, `INSERT INTO platform_settings (key, value) VALUES ('disable_smses', '1')`); err != nil {
		t.Fatal(err)
	}
	if _, err := Queue(ctx, q, Message{To: "+254700000004", Body: "hi", Kind: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SendDue(ctx); err != nil {
		t.Fatal(err)
	}
	if s, _ := status("+254700000004"); s != "failed" {
		t.Fatal(s)
	}
}
