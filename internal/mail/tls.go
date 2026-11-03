package mail

import (
	"context"
	"crypto/tls"
	"net"
	"net/smtp"
)

func sendTLS(ctx context.Context, addr, host string, auth smtp.Auth, from, to string, msg []byte) error {
	d := tls.Dialer{NetDialer: &net.Dialer{}, Config: &tls.Config{ServerName: host}}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if auth != nil {
		if err := c.Auth(auth); err != nil {
			return err
		}
	}
	if err := c.Mail(from); err != nil {
		return err
	}
	if err := c.Rcpt(to); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}
