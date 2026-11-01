package httpapi

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/swahome/legion-api/internal/channels"
	"github.com/swahome/legion-api/internal/payments"
	"github.com/swahome/legion-api/internal/store"
)

// USSD and WhatsApp (milestone 7). Africa's Talking posts each USSD step to
// /v1/hooks/ussd; Twilio posts each WhatsApp message to /v1/hooks/whatsapp.
// Both URLs carry a key (`legion-api hook-urls` prints them); Twilio's
// signature is checked too when LEGION_TWILIO_AUTH_TOKEN is set. Platform
// admins can switch each channel off (settings channels.ussd.enabled and
// channels.whatsapp.enabled) and read every conversation.

// ChannelConfig holds what the channel webhooks check.
type ChannelConfig struct {
	HookKey     []byte
	USSDIPs     []string // Africa's Talking source IPs, if you want to pin them
	TwilioToken string   // Twilio auth token: checks X-Twilio-Signature
	PublicURL   string   // the address Twilio calls, for the signature
}

// WithChannels serves USSD and WhatsApp.
func (s *Server) WithChannels(e *channels.Engine, cfg ChannelConfig) *Server {
	s.channels, s.channelCfg = e, cfg
	return s
}

// ChannelHookKey is the key in a channel's webhook URL.
func ChannelHookKey(hookKey []byte, channel string) string {
	return (&payments.Service{HookKey: hookKey}).HookSecret(channel)
}

func (s *Server) channelKeyOK(r *http.Request, channel string) bool {
	want := ChannelHookKey(s.channelCfg.HookKey, channel)
	return subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("key")), []byte(want)) == 1
}

// channelOn reads the admin switch; a channel is on unless switched off.
func (s *Server) channelOn(r *http.Request, channel string) bool {
	v, err := s.q.GetPlatformSetting(r.Context(), "channels."+channel+".enabled")
	if err != nil {
		return true
	}
	var on bool
	if json.Unmarshal(v, &on) != nil {
		return true
	}
	return on
}

const maxChannelBody = 16 << 10

// ussdHook answers Africa's Talking: form fields sessionId, serviceCode,
// phoneNumber and text; the reply is plain text starting CON or END.
func (s *Server) ussdHook(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if s.channels == nil || !s.channelKeyOK(r, "ussd") {
		http.Error(w, "END Not allowed", http.StatusForbidden)
		return
	}
	if ips := s.channelCfg.USSDIPs; len(ips) > 0 && !(HookAuth{IPs: ips}).allows(remoteIP(r), "", "") {
		http.Error(w, "END Not allowed", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxChannelBody)
	if err := r.ParseForm(); err != nil {
		_, _ = w.Write([]byte("END Invalid request"))
		return
	}
	if !s.channelOn(r, "ussd") {
		_, _ = w.Write([]byte("END This service is not available right now. Please try again later."))
		return
	}
	session, phone := r.PostForm.Get("sessionId"), r.PostForm.Get("phoneNumber")
	if session == "" || phone == "" {
		_, _ = w.Write([]byte("END Invalid request"))
		return
	}
	reply := s.channels.Handle(r.Context(), channels.Request{
		Channel: store.ChannelKindUssd, Format: channels.USSD, SessionKey: session, Phone: phone,
		ServiceCode: r.PostForm.Get("serviceCode"), Text: r.PostForm.Get("text"), Cumulative: true,
	})
	// The engine lays USSD replies out with CON/END already.
	_, _ = w.Write([]byte(reply.Text))
}

// whatsappHook answers Twilio's WhatsApp webhook (fields WaId or From,
// Body, MessageSid) with TwiML.
func (s *Server) whatsappHook(w http.ResponseWriter, r *http.Request) {
	if s.channels == nil || !s.channelKeyOK(r, "whatsapp") {
		http.Error(w, "Not allowed", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxChannelBody)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}
	if tok := s.channelCfg.TwilioToken; tok != "" {
		full := strings.TrimRight(s.channelCfg.PublicURL, "/") + r.URL.RequestURI()
		if !channels.ValidTwilioSignature(tok, full, r.PostForm, r.Header.Get("X-Twilio-Signature")) {
			http.Error(w, "Bad signature", http.StatusForbidden)
			return
		}
	}
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	if !s.channelOn(r, "whatsapp") {
		_, _ = w.Write(channels.TwiML("This service is not available right now. Please try again later."))
		return
	}
	phone := r.PostForm.Get("WaId")
	if phone == "" {
		phone = strings.TrimPrefix(r.PostForm.Get("From"), "whatsapp:")
	}
	if phone == "" {
		_, _ = w.Write(channels.TwiML("Sorry, we could not read your phone number."))
		return
	}
	if !strings.HasPrefix(phone, "+") && !strings.HasPrefix(phone, "0") {
		phone = "+" + phone // WaId is the number without the plus
	}
	reply := s.channels.Handle(r.Context(), channels.Request{
		Channel: store.ChannelKindWhatsapp, Format: channels.WhatsApp, SessionKey: phone, Phone: phone,
		Text: r.PostForm.Get("Body"), MessageID: r.PostForm.Get("MessageSid"),
	})
	_, _ = w.Write(channels.TwiML(reply.Text))
}

// ---- admin ----

type channelSessionResponse struct {
	ID        int64      `json:"id"`
	Channel   string     `json:"channel"`
	Phone     string     `json:"phone"`
	User      *namedRef  `json:"user"`
	Group     *namedRef  `json:"group"`
	Screen    string     `json:"screen"`
	Inputs    int32      `json:"inputs"`
	Errors    int64      `json:"errors"`
	EndedAt   *time.Time `json:"ended_at"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// adminListChannelSessions: GET /admin/channel-sessions?channel=&phone=&errors=true
func (s *Server) adminListChannelSessions(w http.ResponseWriter, r *http.Request) {
	pg := pageParams(r)
	p := store.ListChannelSessionsParams{Phone: queryPtr(r, "phone"), ErrorsOnly: r.URL.Query().Get("errors") == "true",
		Lim: pg.size, Off: pg.offset}
	if ch := r.URL.Query().Get("channel"); ch != "" {
		k := store.ChannelKind(ch)
		if !k.Valid() {
			writeError(w, http.StatusBadRequest, "invalid_channel", "channel is ussd, whatsapp or test.")
			return
		}
		p.Channel = store.NullChannelKind{ChannelKind: k, Valid: true}
	}
	rows, err := s.q.ListChannelSessions(r.Context(), p)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	items := make([]channelSessionResponse, 0, len(rows))
	var total int64
	for _, x := range rows {
		items = append(items, channelSessionResponse{
			ID: x.ID, Channel: string(x.Channel), Phone: x.Phone, User: named(x.UserID, x.UserName),
			Group: named(x.GroupID, x.GroupName), Screen: x.Screen, Inputs: x.Inputs, Errors: x.Errors,
			EndedAt: x.EndedAt, CreatedAt: x.CreatedAt, UpdatedAt: x.UpdatedAt,
		})
		total = x.Total
	}
	writeJSON(w, http.StatusOK, newPaged(items, total, pg))
}

// adminGetChannelSession: one conversation with every exchange.
func (s *Server) adminGetChannelSession(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "sessionID")
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "Session not found.")
		return
	}
	x, err := s.q.GetChannelSession(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "Session not found.")
		return
	}
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	msgs, err := s.q.ListChannelMessages(r.Context(), id)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	type message struct {
		ID        int64     `json:"id"`
		Input     string    `json:"input"`
		Response  string    `json:"response"`
		Error     string    `json:"error"`
		CreatedAt time.Time `json:"created_at"`
	}
	out := make([]message, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, message{m.ID, m.Input, m.Response, m.Error, m.CreatedAt})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": x.ID, "channel": x.Channel, "phone": x.Phone, "user": named(x.UserID, x.UserName),
		"group": named(x.GroupID, x.GroupName), "service_code": x.ServiceCode, "inputs": x.Inputs,
		"ended_at": x.EndedAt, "expires_at": x.ExpiresAt, "created_at": x.CreatedAt, "updated_at": x.UpdatedAt,
		"messages": out,
	})
}

// adminChannels: GET /admin/channels, the switches and the last week's use.
func (s *Server) adminChannels(w http.ResponseWriter, r *http.Request) {
	st, err := s.q.ChannelStats(r.Context(), time.Now().Add(-7*24*time.Hour))
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"configured":       s.channels != nil,
		"ussd_enabled":     s.channelOn(r, "ussd"),
		"whatsapp_enabled": s.channelOn(r, "whatsapp"),
		"whatsapp_signed":  s.channelCfg.TwilioToken != "",
		"last_7_days": map[string]int64{
			"ussd_sessions": st.UssdSessions, "whatsapp_sessions": st.WhatsappSessions, "callers": st.Callers,
		},
	})
}

// adminTryChannel: POST /admin/channels/try {"format": "ussd", "session": "", "input": "1"}.
// A test phone for admins: it runs the real menus as the admin's own phone
// number, so what it does (payments, requests) is real. An empty session
// starts a new conversation; the reply carries the session to send back.
func (s *Server) adminTryChannel(w http.ResponseWriter, r *http.Request) {
	if s.channels == nil {
		writeError(w, http.StatusServiceUnavailable, "channels_off", "USSD and WhatsApp are not set up on this server.")
		return
	}
	var req struct {
		Format  string `json:"format"`
		Session string `json:"session"`
		Input   string `json:"input"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	format := channels.Format(req.Format)
	if format != channels.USSD && format != channels.WhatsApp {
		writeError(w, http.StatusBadRequest, "invalid_format", "format is ussd or whatsapp.")
		return
	}
	u, err := s.q.GetUserByID(r.Context(), claimsFrom(r.Context()).UserID)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	if u.Phone == nil || *u.Phone == "" {
		writeError(w, http.StatusConflict, "no_phone", "Add a phone number to your account to use the test phone.")
		return
	}
	session := req.Session
	if session == "" || !strings.HasPrefix(session, "try-") || len(session) > 64 {
		b := make([]byte, 8)
		_, _ = rand.Read(b)
		session = "try-" + hex.EncodeToString(b)
	}
	// Each answer is sent on its own, like a WhatsApp message; the first
	// call (empty input) opens the menu.
	reply := s.channels.Handle(r.Context(), channels.Request{
		Channel: store.ChannelKindTest, Format: format, SessionKey: session, Phone: *u.Phone,
		Text: req.Input, MessageID: "try:" + session + ":" + hex.EncodeToString(randomBytes()),
	})
	writeJSON(w, http.StatusOK, map[string]any{"session": session, "text": reply.Text, "end": reply.End})
}

func randomBytes() []byte {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return b
}
