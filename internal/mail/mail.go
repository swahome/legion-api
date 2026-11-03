// Package mail sends email with one optional attachment over SMTP, for
// the statements members ask to have emailed.
package mail

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"mime"
	"net"
	netmail "net/mail"
	"net/smtp"
	"strings"
	"time"
)

// Message is one email.
type Message struct {
	To, Subject, Body string
	// Attachment is optional.
	FileName string
	File     []byte
}

// Sender sends email.
type Sender interface {
	Send(ctx context.Context, m Message) error
}

// SMTP sends through a mail server. Port 465 uses TLS from the start;
// other ports upgrade with STARTTLS when the server offers it.
type SMTP struct {
	Host     string
	Port     int
	User     string
	Password string
	From     string
}

func (s SMTP) Send(ctx context.Context, m Message) error {
	addr := net.JoinHostPort(s.Host, fmt.Sprint(s.Port))
	var auth smtp.Auth
	if s.User != "" {
		auth = smtp.PlainAuth("", s.User, s.Password, s.Host)
	}
	msg := Build(s.From, m, time.Now())
	from := s.From
	if a, err := netmail.ParseAddress(s.From); err == nil {
		from = a.Address
	}
	if s.Port == 465 {
		return sendTLS(ctx, addr, s.Host, auth, from, m.To, msg)
	}
	return smtp.SendMail(addr, auth, from, []string{m.To}, msg)
}

// Build is the MIME message.
func Build(from string, m Message, now time.Time) []byte {
	var b bytes.Buffer
	boundary := fmt.Sprintf("legion-%d", now.UnixNano())
	fmt.Fprintf(&b, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMIME-Version: 1.0\r\n",
		from, m.To, mime.QEncoding.Encode("utf-8", m.Subject), now.Format(time.RFC1123Z))
	if m.FileName == "" {
		fmt.Fprintf(&b, "Content-Type: text/plain; charset=utf-8\r\n\r\n%s\r\n", m.Body)
		return b.Bytes()
	}
	fmt.Fprintf(&b, "Content-Type: multipart/mixed; boundary=%q\r\n\r\n", boundary)
	fmt.Fprintf(&b, "--%s\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s\r\n", boundary, m.Body)
	fmt.Fprintf(&b, "--%s\r\nContent-Type: application/pdf\r\nContent-Transfer-Encoding: base64\r\nContent-Disposition: attachment; filename=%q\r\n\r\n",
		boundary, m.FileName)
	enc := base64.StdEncoding.EncodeToString(m.File)
	for len(enc) > 76 {
		b.WriteString(enc[:76] + "\r\n")
		enc = enc[76:]
	}
	b.WriteString(enc + "\r\n")
	fmt.Fprintf(&b, "--%s--\r\n", boundary)
	return b.Bytes()
}

// Valid is a light check that s looks like an email address.
func Valid(s string) bool {
	at := strings.LastIndex(s, "@")
	return at > 0 && at < len(s)-3 && strings.Contains(s[at:], ".") && !strings.ContainsAny(s, " \r\n,;<>")
}
