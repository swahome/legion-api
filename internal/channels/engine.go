// Package channels runs the USSD and WhatsApp menus (milestone 7).
//
// Both channels share one menu engine. A conversation is a row in
// channel_sessions holding the caller's place in the menus (State). Each
// request locks that row, applies only the answers it has not seen, saves
// the new place and logs the exchange, all in one transaction. So a telco
// retry or a duplicate webhook gets the same reply instead of moving the
// caller twice, and a failure leaves the session where it was.
//
// The legacy controllers (Ussd.php, Whatsapp.php) kept a numeric "position"
// per session and a long if-chain to move between them; WhatsApp used the
// Twilio account id as the session id, so every caller shared one session.
// Here each screen is a named function, and every money action goes through
// the same rules as the web and the app (internal/books, internal/payments).
package channels

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/swahome/legion-api/internal/auth"
	"github.com/swahome/legion-api/internal/books"
	"github.com/swahome/legion-api/internal/payments"
	"github.com/swahome/legion-api/internal/store"
)

// Format is how replies are laid out.
type Format string

const (
	USSD     Format = "ussd"     // short screens, "CON"/"END", 182 characters
	WhatsApp Format = "whatsapp" // chat messages
)

// Engine answers USSD and WhatsApp requests.
type Engine struct {
	Pool      *pgxpool.Pool
	Payments  *payments.Service // nil or not enabled: no MPesa payments
	DefaultCC string            // calling code for local numbers, e.g. 254
	Brand     string            // the product name callers see
	WebURL    string            // where unknown callers can sign up
	Log       *slog.Logger
	Now       func() time.Time
	// Later runs work after the reply has gone out. The MPesa prompt is
	// sent this way: a phone cannot show it while the USSD screen is open.
	// Nil runs it in a goroutine.
	Later func(delay time.Duration, fn func())
}

// Request is one message from a caller.
type Request struct {
	Channel     store.ChannelKind
	Format      Format
	SessionKey  string // USSD sessionId; the phone number for WhatsApp
	Phone       string // the caller's number as the provider sent it
	ServiceCode string
	// Text is the whole USSD answer string ("1*2*500"): the engine works
	// out which answers are new. For message channels it is one message.
	Text       string
	Cumulative bool
	// MessageID is the provider's id for a message, so a re-delivered
	// message gets the same reply.
	MessageID string
}

// Reply is what the caller sees. End closes the conversation.
type Reply struct {
	Text string
	End  bool
}

// Session lengths: telcos close a USSD session after about three minutes;
// a WhatsApp conversation starts again after half an hour of quiet.
const (
	ussdTTL     = 5 * time.Minute
	whatsappTTL = 30 * time.Minute
)

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Engine) log() *slog.Logger {
	if e.Log != nil {
		return e.Log
	}
	return slog.Default()
}

func (e *Engine) brand() string {
	if e.Brand != "" {
		return e.Brand
	}
	return "Legion"
}

func (e *Engine) later(delay time.Duration, fn func()) {
	if e.Later != nil {
		e.Later(delay, fn)
		return
	}
	go func() {
		time.Sleep(delay)
		fn()
	}()
}

func ttl(f Format) time.Duration {
	if f == USSD {
		return ussdTTL
	}
	return whatsappTTL
}

const sorry = "Sorry, something went wrong. Please try again in a few minutes."

// Handle answers one request. It never returns an error to the provider:
// a failure becomes a polite final message and is logged with the session.
func (e *Engine) Handle(ctx context.Context, req Request) Reply {
	phone, err := auth.NormalizePhone(strings.TrimPrefix(strings.TrimSpace(req.Phone), "whatsapp:"), e.DefaultCC)
	if err != nil || req.SessionKey == "" {
		return Reply{Text: endText(req.Format, "Sorry, we could not read your phone number."), End: true}
	}
	var (
		reply     Reply
		after     []func()
		sessionID int64
	)
	err = pgx.BeginFunc(ctx, e.Pool, func(tx pgx.Tx) error {
		var err error
		reply, sessionID, after, err = e.handle(ctx, tx, req, phone)
		return err
	})
	if err != nil {
		e.log().Error("channel request", "channel", req.Channel, "session", req.SessionKey, "err", err)
		reply = Reply{Text: endText(req.Format, sorry), End: true}
		if sessionID != 0 {
			e.recordFailure(ctx, sessionID, req.Text, reply.Text, err)
		}
		return reply
	}
	for _, fn := range after {
		fn()
	}
	return reply
}

// recordFailure ends a session whose request failed, in its own
// transaction because the request's one was rolled back.
func (e *Engine) recordFailure(ctx context.Context, sessionID int64, input, response string, cause error) {
	q := store.New(e.Pool)
	if err := q.EndChannelSession(ctx, sessionID); err != nil {
		e.log().Error("channel end session", "err", err)
	}
	msg := cause.Error()
	if len(msg) > 500 {
		msg = msg[:500]
	}
	if err := q.AddChannelMessage(ctx, store.AddChannelMessageParams{SessionID: sessionID, Input: input, Response: response, Error: msg}); err != nil {
		e.log().Error("channel log message", "err", err)
	}
}

// splitAnswers splits a USSD text into its answers.
func splitAnswers(text string) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	return strings.Split(text, "*")
}

func (e *Engine) handle(ctx context.Context, tx pgx.Tx, req Request, phone string) (Reply, int64, []func(), error) {
	q := store.New(tx)
	now := e.now()
	sess, err := q.LatestChannelSession(ctx, store.LatestChannelSessionParams{Channel: req.Channel, SessionKey: req.SessionKey})
	found := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Reply{}, 0, nil, err
	}
	dedupeKey := req.MessageID
	if req.Cumulative {
		dedupeKey = "text:" + req.Text
	}
	if found && sess.LastRequest != "" && dedupeKey != "" && sess.LastRequest == dedupeKey {
		// The provider is asking again for an answer it already got.
		return Reply{Text: sess.LastResponse, End: sess.EndedAt != nil}, sess.ID, nil, nil
	}
	if found && sess.EndedAt == nil && sess.ExpiresAt.Before(now) {
		if err := q.EndChannelSession(ctx, sess.ID); err != nil {
			return Reply{}, sess.ID, nil, err
		}
		sess.EndedAt = &now
		if req.Cumulative {
			return Reply{Text: endText(req.Format, "Your session timed out. Please dial again."), End: true}, sess.ID, nil, nil
		}
	}
	if found && sess.EndedAt != nil && req.Cumulative {
		// A USSD session id is used once; this one is over.
		return Reply{Text: endText(req.Format, "This session has ended. Please dial again."), End: true}, sess.ID, nil, nil
	}

	user, err := q.GetUserByPhone(ctx, &phone)
	var userID *int64
	switch {
	case err == nil && user.IsActive:
		userID = &user.ID
	case err == nil, errors.Is(err, pgx.ErrNoRows):
	default:
		return Reply{}, 0, nil, err
	}

	fresh := !found || sess.EndedAt != nil
	if fresh {
		sess, err = q.CreateChannelSession(ctx, store.CreateChannelSessionParams{
			Channel: req.Channel, SessionKey: req.SessionKey, Phone: phone, UserID: userID,
			ServiceCode: req.ServiceCode, ExpiresAt: now.Add(ttl(req.Format)),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			// Another request opened it a moment ago; take that one.
			if sess, err = q.LatestChannelSession(ctx, store.LatestChannelSessionParams{Channel: req.Channel, SessionKey: req.SessionKey}); err != nil {
				return Reply{}, 0, nil, err
			}
			fresh = false
		} else if err != nil {
			return Reply{}, 0, nil, err
		}
	}

	var st State
	if len(sess.State) > 0 {
		_ = json.Unmarshal(sess.State, &st)
	}
	c := &convo{ctx: ctx, e: e, tx: tx, q: q, format: req.Format, phone: phone, st: st, today: now}
	if userID != nil {
		c.user = &user
	}

	// Which answers are new?
	var inputs []string
	steps := sess.Steps
	if req.Cumulative {
		parts := splitAnswers(req.Text)
		switch {
		case len(parts) > int(steps):
			inputs = parts[steps:]
		case len(parts) > 0:
			inputs = parts[len(parts)-1:] // a gateway that sends only the last answer
		}
		steps = int32(len(parts))
	} else if !fresh {
		inputs = []string{req.Text}
	}
	if !req.Cumulative && fresh {
		inputs = nil // "Hi" opens the menu; it is not an answer
	} else if !req.Cumulative && isKeyword(req.Text, "menu", "hi", "hello", "start", "home") {
		c.home()
		inputs = nil
	} else if !req.Cumulative && isKeyword(req.Text, "stop", "exit", "bye", "quit") {
		c.finish("Goodbye. Send Hi any time to start again.")
		inputs = nil
	}

	text, err := c.run(inputs)
	if err != nil {
		return Reply{}, sess.ID, nil, err
	}
	stateJSON, _ := json.Marshal(c.st)
	var groupID *int64
	if c.st.GroupID != 0 {
		groupID = &c.st.GroupID
	}
	if err := q.SaveChannelSession(ctx, store.SaveChannelSessionParams{
		ID: sess.ID, State: stateJSON, GroupID: groupID, Steps: steps, LastRequest: dedupeKey, LastResponse: text,
		ExpiresAt: now.Add(ttl(req.Format)), Added: int32(len(inputs)), Ended: c.ended,
	}); err != nil {
		return Reply{}, sess.ID, nil, err
	}
	if err := q.AddChannelMessage(ctx, store.AddChannelMessageParams{SessionID: sess.ID, Input: req.Text, Response: text, Error: c.fault}); err != nil {
		return Reply{}, sess.ID, nil, err
	}
	return Reply{Text: text, End: c.ended}, sess.ID, c.after, nil
}

func isKeyword(s string, words ...string) bool {
	s = strings.ToLower(strings.Trim(strings.TrimSpace(s), ".!?"))
	for _, w := range words {
		if s == w {
			return true
		}
	}
	return false
}

// State is a caller's place in the menus, saved between requests.
type State struct {
	Screen  string            `json:"screen"`
	Vars    map[string]string `json:"vars,omitempty"`
	Page    int               `json:"page,omitempty"`
	Back    []Frame           `json:"back,omitempty"`
	GroupID int64             `json:"group_id,omitempty"`
}

// Frame is a screen to go back to.
type Frame struct {
	Screen string            `json:"screen"`
	Vars   map[string]string `json:"vars,omitempty"`
	Page   int               `json:"page,omitempty"`
}

// maxBack keeps the back stack small: a USSD session cannot go deeper.
const maxBack = 20

// convo is one request's work on a conversation.
type convo struct {
	ctx    context.Context
	e      *Engine
	tx     pgx.Tx
	q      *store.Queries
	format Format
	phone  string
	user   *store.User
	st     State
	today  time.Time

	g      *member // the chosen group, loaded on demand
	notice string  // shown above the screen once, e.g. why an answer was refused
	ended  bool
	final  string
	fault  string // an unexpected error, for the transcript
	after  []func()
}

func (c *convo) v(key string) string { return c.st.Vars[key] }

func (c *convo) vInt(key string) int64 {
	n, _ := strconv.ParseInt(c.st.Vars[key], 10, 64)
	return n
}

func copyVars(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// open goes to a screen, remembering this one for "back". kv sets
// variables for the new screen; earlier ones carry on.
func (c *convo) open(screen string, kv ...string) error {
	c.st.Back = append(c.st.Back, Frame{Screen: c.st.Screen, Vars: copyVars(c.st.Vars), Page: c.st.Page})
	if len(c.st.Back) > maxBack {
		c.st.Back = c.st.Back[len(c.st.Back)-maxBack:]
	}
	return c.replace(screen, kv...)
}

// replace goes to a screen without remembering this one.
func (c *convo) replace(screen string, kv ...string) error {
	vars := copyVars(c.st.Vars)
	for i := 0; i+1 < len(kv); i += 2 {
		vars[kv[i]] = kv[i+1]
	}
	c.st.Screen, c.st.Vars, c.st.Page = screen, vars, 0
	return nil
}

// home goes to the group's main menu, or the start.
func (c *convo) home() {
	c.st.Back, c.st.Vars, c.st.Page = nil, nil, 0
	c.st.Screen = "main"
	if c.st.GroupID == 0 {
		c.st.Screen = "start"
	}
}

func (c *convo) back() {
	if len(c.st.Back) == 0 {
		c.finish("Thank you for using " + c.e.brand() + ".")
		return
	}
	f := c.st.Back[len(c.st.Back)-1]
	c.st.Back = c.st.Back[:len(c.st.Back)-1]
	c.st.Screen, c.st.Vars, c.st.Page = f.Screen, f.Vars, f.Page
}

// finish ends the conversation with a last message.
func (c *convo) finish(text string) {
	c.ended, c.final = true, text
}

// say is an answer the caller must change: shown, and the screen stays.
type say string

func (s say) Error() string { return string(s) }

// errRedirect: the screen moved the caller elsewhere while drawing itself.
var errRedirect = errors.New("redirect")

// write runs fn in a savepoint, so a refused action leaves nothing behind.
func (c *convo) write(fn func(q *store.Queries) error) error {
	sp, err := c.tx.Begin(c.ctx)
	if err != nil {
		return err
	}
	if err := fn(c.q.WithTx(sp)); err != nil {
		_ = sp.Rollback(c.ctx)
		return err
	}
	return sp.Commit(c.ctx)
}

// userError turns an error the caller can act on into its message.
func userError(err error) (string, bool) {
	var s say
	var be books.Error
	var bc books.Conflict
	switch {
	case errors.As(err, &s):
		return string(s), true
	case errors.As(err, &be):
		return be.Msg, true
	case errors.As(err, &bc):
		return bc.Msg, true
	case errors.Is(err, books.ErrNotFound):
		return "That is no longer available.", true
	case errors.Is(err, payments.ErrNotConfigured):
		return "MPesa payments are not available right now.", true
	}
	return "", false
}

// view is a drawn screen: some text, then numbered options or a question.
type view struct {
	text    string
	options []option
	input   func(c *convo, answer string) error
}

type option struct {
	label string
	do    func(c *convo) error
}

func opt(label string, do func(c *convo) error) option { return option{label: label, do: do} }

// to is an option that opens a screen.
func to(label, screen string, kv ...string) option {
	return opt(label, func(c *convo) error { return c.open(screen, kv...) })
}

// draw builds the current screen, following redirects.
func (c *convo) draw() (view, error) {
	for range 6 {
		fn, ok := screens[c.st.Screen]
		if !ok {
			c.home()
			fn = screens[c.st.Screen]
		}
		v, err := fn(c)
		if errors.Is(err, errRedirect) {
			continue
		}
		if msg, ok := userError(err); ok {
			// The screen cannot be shown (e.g. a closed request): say why
			// and go back.
			c.notice = msg
			c.back()
			if c.ended {
				c.final = msg
				return view{}, nil
			}
			continue
		}
		return v, err
	}
	return view{}, errors.New("channels: too many redirects from " + c.st.Screen)
}

// run applies the answers in turn and draws the screen the caller ends on.
func (c *convo) run(inputs []string) (string, error) {
	if c.st.Screen == "" {
		c.st.Screen = "start"
	}
	for _, in := range inputs {
		if c.ended {
			break
		}
		if err := c.answer(strings.TrimSpace(in)); err != nil {
			return "", err
		}
	}
	if !c.ended {
		v, err := c.draw()
		if err != nil {
			return "", err
		}
		if !c.ended {
			return c.layout(v), nil
		}
	}
	return c.layoutEnd(c.final), nil
}

// answer applies one answer to the current screen.
func (c *convo) answer(in string) error {
	notice := c.notice
	c.notice = ""
	switch in {
	case "00":
		c.home()
		return nil
	case "0":
		c.back()
		return nil
	}
	v, err := c.draw()
	if err != nil || c.ended {
		return err
	}
	if c.notice != "" { // the screen we were on could not be shown
		return nil
	}
	switch {
	case len(v.options) > 0:
		_, more := c.page(v)
		if in == "98" && more {
			c.st.Page++
			return nil
		}
		n, err := strconv.Atoi(in)
		if err != nil || n < 1 || n > len(v.options) {
			c.notice = "Please reply with one of the numbers shown."
			return nil
		}
		err = v.options[n-1].do(c)
		return c.settle(err)
	case v.input != nil:
		if in == "" {
			c.notice = notice
			return nil
		}
		return c.settle(v.input(c, in))
	}
	c.notice = "Reply 0 to go back or 00 for the main menu."
	return nil
}

// settle shows a refused answer to the caller; other errors fail the request.
func (c *convo) settle(err error) error {
	if err == nil {
		return nil
	}
	if msg, ok := userError(err); ok {
		c.notice = msg
		return nil
	}
	return err
}

// ---- layout ----

// ussdLimit is the longest USSD screen most Kenyan networks show.
const ussdLimit = 182

const (
	waPageSize = 9
	moreKey    = "98"
)

func (c *convo) atRoot() bool { return len(c.st.Back) == 0 }

func (c *convo) footer() string {
	if c.format == WhatsApp {
		if c.atRoot() {
			return "Reply 0 to exit."
		}
		return "Reply 0 to go back, 00 for the main menu."
	}
	if c.atRoot() {
		return "0 Exit"
	}
	return "0 Back 00 Home"
}

func runeLen(s string) int { return utf8.RuneCountInString(s) }

// page returns the options on the current page and whether more follow.
// USSD pages fill the screen; WhatsApp pages hold nine options.
func (c *convo) page(v view) (lines []string, more bool) {
	all := make([]string, len(v.options))
	for i, o := range v.options {
		all[i] = fmt.Sprintf("%d. %s", i+1, o.label)
	}
	start := 0
	for p := 0; ; p++ {
		end := c.pageEnd(v, all, start)
		if p == c.st.Page || end >= len(all) {
			if p != c.st.Page {
				c.st.Page = p
			}
			return all[start:end], end < len(all)
		}
		start = end
	}
}

func (c *convo) pageEnd(v view, all []string, start int) int {
	if c.format == WhatsApp {
		return min(start+waPageSize, len(all))
	}
	budget := ussdLimit - runeLen("CON ") - runeLen(c.header(v)) - runeLen(c.footer()) - 2
	end := start
	used := 0
	for end < len(all) {
		need := runeLen(all[end]) + 1
		reserve := 0
		if end+1 < len(all) {
			reserve = runeLen(moreKey+" More") + 1
		}
		if end > start && used+need+reserve > budget {
			break
		}
		used += need
		end++
	}
	return end
}

func (c *convo) header(v view) string {
	parts := []string{}
	if c.notice != "" {
		parts = append(parts, c.notice)
	}
	if v.text != "" {
		parts = append(parts, v.text)
	}
	return strings.Join(parts, "\n")
}

func (c *convo) layout(v view) string {
	head := c.header(v)
	lines, more := c.page(v)
	var tail strings.Builder
	if len(lines) > 0 {
		tail.WriteString(strings.Join(lines, "\n"))
		if more {
			tail.WriteString("\n" + moreKey + ". More")
		}
	}
	if c.format == WhatsApp {
		gap := ""
		if head != "" && tail.Len() > 0 {
			gap = "\n\n"
		}
		return head + gap + tail.String() + "\n\n_" + c.footer() + "_"
	}
	if tail.Len() > 0 {
		tail.WriteString("\n")
	}
	tail.WriteString(c.footer())
	// Keep the options and the way out; shorten the text if it must give.
	room := ussdLimit - runeLen("CON ") - runeLen(tail.String()) - 1
	if runeLen(head) > room {
		head = clip(head, max(room, 1))
	}
	if head == "" {
		return "CON " + tail.String()
	}
	return "CON " + head + "\n" + tail.String()
}

func (c *convo) layoutEnd(text string) string { return endText(c.format, text) }

// endText lays out a last message.
func endText(f Format, text string) string {
	if f == USSD {
		return clip("END "+text, ussdLimit)
	}
	return text
}

func clip(s string, n int) string {
	if runeLen(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

// ---- money text ----

// amount is "KES 1,500.00".
func (c *convo) amount(v string) string {
	cur := "KES"
	if c.g != nil && c.g.Currency != "" {
		cur = c.g.Currency
	}
	return cur + " " + commas(v)
}

// commas is legacy number_to_currency: "1,500.00".
func commas(v string) string {
	s := books.Format(v)
	neg := strings.HasPrefix(s, "-")
	whole, frac, _ := strings.Cut(strings.TrimPrefix(s, "-"), ".")
	var b strings.Builder
	for i, ch := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(ch)
	}
	out := b.String()
	if frac != "" {
		out += "." + frac
	}
	if neg {
		out = "-" + out
	}
	return out
}

// RunCleanup closes conversations that went quiet and, once a day, deletes
// those older than keep. It runs until ctx is done.
func (e *Engine) RunCleanup(ctx context.Context, keep time.Duration) {
	tick := time.NewTicker(10 * time.Minute)
	defer tick.Stop()
	var lastPurge time.Time
	for {
		q := store.New(e.Pool)
		if _, err := q.EndExpiredChannelSessions(ctx); err != nil && ctx.Err() == nil {
			e.log().Error("channel sessions: end expired", "err", err)
		}
		if time.Since(lastPurge) > 24*time.Hour {
			if n, err := q.DeleteOldChannelSessions(ctx, e.now().Add(-keep)); err != nil && ctx.Err() == nil {
				e.log().Error("channel sessions: purge", "err", err)
			} else if n > 0 {
				e.log().Info("channel sessions purged", "count", n)
			}
			lastPurge = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
