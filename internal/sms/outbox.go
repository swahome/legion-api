package sms

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/swahome/legion-api/internal/store"
)

// Message is one text to queue.
type Message struct {
	To      string // E.164
	Body    string
	Kind    string // what it is for, e.g. "invite"; shown in the admin outbox
	UserID  *int64
	GroupID *int64
}

// Queue writes a message to the outbox. Call it inside the transaction of
// the change that causes it: the message is sent only if that commits.
func Queue(ctx context.Context, q *store.Queries, m Message) (int64, error) {
	return q.QueueSMS(ctx, store.QueueSMSParams{ToPhone: m.To, Body: m.Body, Kind: m.Kind, UserID: m.UserID, GroupID: m.GroupID})
}

// backoff is the wait before each retry; after the last one a message fails.
var backoff = []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute, time.Hour, 6 * time.Hour}

// Dispatcher sends what is in the outbox.
type Dispatcher struct {
	pool   *pgxpool.Pool
	sender Sender
	log    *slog.Logger
	kick   chan struct{}
	// Poll is how often the outbox is checked without a Kick.
	Poll time.Duration
}

func NewDispatcher(pool *pgxpool.Pool, sender Sender, log *slog.Logger) *Dispatcher {
	return &Dispatcher{pool: pool, sender: sender, log: log, kick: make(chan struct{}, 1), Poll: 5 * time.Second}
}

// Kick asks the dispatcher to look at the outbox now, after a commit that
// queued a message. It never blocks. A nil dispatcher ignores it.
func (d *Dispatcher) Kick() {
	if d == nil {
		return
	}
	select {
	case d.kick <- struct{}{}:
	default:
	}
}

// Run sends queued messages until ctx ends.
func (d *Dispatcher) Run(ctx context.Context) {
	t := time.NewTicker(d.Poll)
	defer t.Stop()
	for {
		for {
			n, err := d.SendDue(ctx)
			if err != nil && ctx.Err() == nil {
				d.log.Error("sms: dispatch", "err", err)
			}
			if n == 0 || err != nil {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-d.kick:
		}
	}
}

// SendDue sends one batch of due messages and says how many it took.
func (d *Dispatcher) SendDue(ctx context.Context) (int, error) {
	q := store.New(d.pool)
	rows, err := q.ClaimDueSMS(ctx, 20)
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	disabled := d.disabled(ctx, q)
	for _, m := range rows {
		var res Result
		if disabled {
			res = Result{Status: "Disabled", Err: ErrDisabled}
		} else if m.GroupID != nil && d.groupOff(ctx, q, *m.GroupID) {
			res = Result{Status: "GroupDisabled", Err: ErrGroupDisabled}
		} else {
			sendCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			res = d.sender.Send(sendCtx, m.ToPhone, m.Body)
			cancel()
		}
		if err := d.record(ctx, q, m.ID, int(m.Attempts), res); err != nil {
			return len(rows), err
		}
	}
	return len(rows), nil
}

func (d *Dispatcher) record(ctx context.Context, q *store.Queries, id int64, attempts int, res Result) error {
	status := opt(res.Status)
	if res.Err == nil {
		return q.MarkSMSSent(ctx, store.MarkSMSSentParams{ID: id, ProviderMessageID: opt(res.MessageID), ProviderStatus: status, Cost: opt(res.Cost)})
	}
	msg := res.Err.Error()
	if res.Retry && attempts <= len(backoff) {
		d.log.Warn("sms: will retry", "id", id, "attempt", attempts, "err", msg)
		return q.MarkSMSRetry(ctx, store.MarkSMSRetryParams{ID: id, NextAttemptAt: time.Now().Add(backoff[attempts-1]), ProviderStatus: status, Error: &msg})
	}
	d.log.Warn("sms: failed", "id", id, "attempts", attempts, "err", msg)
	return q.MarkSMSFailed(ctx, store.MarkSMSFailedParams{ID: id, ProviderStatus: status, Error: &msg})
}

// groupOff says whether a platform admin switched off a group's
// notifications (the group setting disable_notifications).
func (d *Dispatcher) groupOff(ctx context.Context, q *store.Queries, groupID int64) bool {
	off, err := q.GroupNotificationsOff(ctx, groupID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			d.log.Error("sms: read group notifications switch", "err", err, "group_id", groupID)
		}
		return false
	}
	return off
}

// disabled reads the legacy platform switch disable_smses.
func (d *Dispatcher) disabled(ctx context.Context, q *store.Queries) bool {
	v, err := q.GetPlatformSetting(ctx, "disable_smses")
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			d.log.Error("sms: read disable_smses", "err", err)
		}
		return false
	}
	var x any
	if json.Unmarshal(v, &x) != nil {
		return false
	}
	switch t := x.(type) {
	case bool:
		return t
	case float64:
		return t != 0
	case string:
		return t == "1" || t == "true"
	}
	return false
}

func opt(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
