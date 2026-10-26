package mail

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"time"
)

// SMTPSender sends over SMTP with net/smtp and crypto/tls, the way the
// operator's other production app does. Port 465 is implicit TLS; any other
// port (587 by default) is a plain connection upgraded with STARTTLS.
//
// With Username set, STARTTLS and AUTH are both mandatory: a server that does
// not offer them gets an error, never the message. Without a Username the
// plaintext fallback exists for local catch-all servers only. Password is
// used for AUTH and appears in no error or log line.
type SMTPSender struct {
	Host     string
	Port     string
	Username string
	Password string
	From     string
}

var _ Mailer = (*SMTPSender)(nil)

// Send implements Mailer.
func (s *SMTPSender) Send(ctx context.Context, m Message) error {
	if s.Host == "" {
		return errors.New("smtp: host not configured")
	}
	for name, v := range map[string]string{"from": s.From, "to": m.To, "subject": m.Subject} {
		if strings.ContainsAny(v, "\r\n") {
			return fmt.Errorf("smtp: %s header contains a line break", name)
		}
	}
	if m.To == "" {
		return errors.New("smtp: recipient is empty")
	}
	port := s.Port
	if port == "" {
		port = "587"
	}
	addr := net.JoinHostPort(s.Host, port)
	msg := buildMessage(s.From, m)

	var auth smtp.Auth
	if s.Username != "" {
		auth = smtp.PlainAuth("", s.Username, s.Password, s.Host)
	}
	tlsConf := &tls.Config{ServerName: s.Host, MinVersion: tls.VersionTLS12}
	dialer := &net.Dialer{Timeout: 15 * time.Second}

	var conn net.Conn
	var err error
	if port == "465" {
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: tlsConf}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("smtp: dial %s: %w", addr, err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	client, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("smtp: session with %s: %w", addr, err)
	}
	defer func() { _ = client.Close() }()

	if port != "465" {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(tlsConf); err != nil {
				return fmt.Errorf("smtp: STARTTLS with %s: %w", addr, err)
			}
		} else if auth != nil {
			return fmt.Errorf("smtp: %s does not offer STARTTLS but credentials are configured; refusing to send unencrypted", addr)
		}
	}
	return s.deliver(client, auth, m.To, msg)
}

// deliver runs AUTH (mandatory when credentials are set) and the
// MAIL/RCPT/DATA exchange on a connected client.
func (s *SMTPSender) deliver(client *smtp.Client, auth smtp.Auth, to string, msg []byte) error {
	if auth != nil {
		if ok, _ := client.Extension("AUTH"); !ok {
			return fmt.Errorf("smtp: %s does not offer AUTH but credentials are configured; refusing to send unauthenticated", s.Host)
		}
		if err := client.Auth(auth); err != nil {
			return errors.New("smtp: authentication failed")
		}
	}
	if err := client.Mail(envelopeAddress(s.From)); err != nil {
		return fmt.Errorf("smtp: MAIL FROM: %w", err)
	}
	if err := client.Rcpt(to); err != nil {
		return fmt.Errorf("smtp: RCPT TO: %w", err)
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("smtp: DATA: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		_ = w.Close()
		return fmt.Errorf("smtp: write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp: finish: %w", err)
	}
	return client.Quit()
}

// envelopeAddress strips a display name: "SanHolo <a@b.c>" -> "a@b.c".
func envelopeAddress(from string) string {
	if a, err := mail.ParseAddress(strings.TrimSpace(from)); err == nil {
		return a.Address
	}
	return from
}

func buildMessage(from string, m Message) []byte {
	domain := "localhost"
	if addr := envelopeAddress(from); strings.Contains(addr, "@") {
		domain = addr[strings.LastIndex(addr, "@")+1:]
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", m.To)
	fmt.Fprintf(&b, "Subject: %s\r\n", m.Subject)
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().UTC().Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-ID: <%s@%s>\r\n", rand.Text(), domain)
	b.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=\"utf-8\"\r\n\r\n")
	b.WriteString(strings.ReplaceAll(strings.ReplaceAll(m.Body, "\r\n", "\n"), "\n", "\r\n"))
	return b.Bytes()
}

// LogSender never sends. It is the sender when no SMTP host is configured
// (development and tests). It logs that a message was dropped, without the
// address or the body, which may hold a sign-in link.
type LogSender struct{}

var _ Mailer = LogSender{}

// Send implements Mailer.
func (LogSender) Send(_ context.Context, m Message) error {
	log.Printf("mail: log-only sender, message %q not sent", m.Subject)
	return nil
}
