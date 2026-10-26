package mail

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"testing"
	"time"
)

type fakeResult struct {
	from string
	to   []string
	data string
}

// startFakeSMTP serves one plaintext SMTP session on loopback. caps are
// extra EHLO capabilities; with none it offers neither STARTTLS nor AUTH.
func startFakeSMTP(t *testing.T, caps ...string) (host, port string, results chan fakeResult) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	results = make(chan fakeResult, 16)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveFake(conn, caps, results)
		}
	}()
	host, port, _ = net.SplitHostPort(ln.Addr().String())
	return host, port, results
}

func serveFake(conn net.Conn, caps []string, results chan fakeResult) {
	{
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		r := bufio.NewReader(conn)
		fmt.Fprintf(conn, "220 fake ESMTP\r\n")
		var res fakeResult
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			up := strings.ToUpper(line)
			switch {
			case strings.HasPrefix(up, "EHLO"):
				fmt.Fprintf(conn, "250-fake\r\n")
				for _, c := range caps {
					fmt.Fprintf(conn, "250-%s\r\n", c)
				}
				fmt.Fprintf(conn, "250 OK\r\n")
			case strings.HasPrefix(up, "MAIL FROM:"):
				res.from = line[len("MAIL FROM:"):]
				fmt.Fprintf(conn, "250 OK\r\n")
			case strings.HasPrefix(up, "RCPT TO:"):
				res.to = append(res.to, line[len("RCPT TO:"):])
				fmt.Fprintf(conn, "250 OK\r\n")
			case up == "DATA":
				fmt.Fprintf(conn, "354 go\r\n")
				var sb strings.Builder
				for {
					l, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if l == ".\r\n" {
						break
					}
					sb.WriteString(l)
				}
				res.data = sb.String()
				fmt.Fprintf(conn, "250 OK\r\n")
			case up == "QUIT":
				fmt.Fprintf(conn, "221 Bye\r\n")
				results <- res
				return
			default:
				fmt.Fprintf(conn, "500 what\r\n")
			}
		}
	}
}

func TestSMTPSendsPlainWithoutCredentials(t *testing.T) {
	host, port, results := startFakeSMTP(t)
	s := &SMTPSender{Host: host, Port: port, From: "SanHolo <no-reply@sanholo.space>"}
	if err := s.Send(context.Background(), Message{To: "a@example.org", Subject: "Hello", Body: "Line one\nLine two\n"}); err != nil {
		t.Fatal(err)
	}
	got := <-results
	if !strings.Contains(got.from, "<no-reply@sanholo.space>") || strings.Contains(got.from, "SanHolo") {
		t.Errorf("envelope from = %q, want the bare address", got.from)
	}
	for _, want := range []string{"From: SanHolo <no-reply@sanholo.space>\r\n", "To: a@example.org\r\n", "Subject: Hello\r\n", "Message-ID: <", "Line one\r\nLine two\r\n"} {
		if !strings.Contains(got.data, want) {
			t.Errorf("message lacks %q:\n%s", want, got.data)
		}
	}
}

// With credentials, a server that does not offer STARTTLS gets nothing.
func TestSMTPRefusesPlaintextWithCredentials(t *testing.T) {
	host, port, results := startFakeSMTP(t)
	s := &SMTPSender{Host: host, Port: port, Username: "u", Password: "pw-secret", From: "a@sanholo.space"}
	err := s.Send(context.Background(), Message{To: "a@example.org", Subject: "s", Body: "b"})
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("err = %v, want a STARTTLS refusal", err)
	}
	if strings.Contains(err.Error(), "pw-secret") {
		t.Fatal("error leaks the password")
	}
	select {
	case got := <-results:
		t.Fatalf("message delivered despite refusal: %+v", got)
	case <-time.After(200 * time.Millisecond):
	}
}

// An encrypted session whose server withholds AUTH is refused. deliver is
// called directly: a TLS fake would need a certificate the sender must trust.
func TestSMTPDeliverRefusesUnadvertisedAuth(t *testing.T) {
	host, port, results := startFakeSMTP(t)
	conn, err := net.Dial("tcp", net.JoinHostPort(host, port))
	if err != nil {
		t.Fatal(err)
	}
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	s := &SMTPSender{Host: host, Username: "u", Password: "pw-secret", From: "a@sanholo.space"}
	err = s.deliver(client, smtp.PlainAuth("", "u", "pw-secret", host), "a@example.org", []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "AUTH") {
		t.Fatalf("err = %v, want an AUTH refusal", err)
	}
	select {
	case got := <-results:
		t.Fatalf("delivered: %+v", got)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestSMTPRejectsLineBreaksInHeaders(t *testing.T) {
	host, port, results := startFakeSMTP(t)
	cases := []struct {
		name string
		s    SMTPSender
		m    Message
	}{
		{"to", SMTPSender{From: "a@sanholo.space"}, Message{To: "a@example.org\r\nBcc: x@y.z", Subject: "s"}},
		{"to lf", SMTPSender{From: "a@sanholo.space"}, Message{To: "a@example.org\nBcc: x@y.z", Subject: "s"}},
		{"subject", SMTPSender{From: "a@sanholo.space"}, Message{To: "a@example.org", Subject: "s\r\nBcc: x@y.z"}},
		{"subject lf only", SMTPSender{From: "a@sanholo.space"}, Message{To: "a@example.org", Subject: "s\nBcc: x@y.z"}},
		{"subject cr only", SMTPSender{From: "a@sanholo.space"}, Message{To: "a@example.org", Subject: "s\rBcc: x@y.z"}},
		{"from lf only", SMTPSender{From: "a@sanholo.space\nBcc: x@y.z"}, Message{To: "a@example.org", Subject: "s"}},
		{"from", SMTPSender{From: "a@sanholo.space\r\nBcc: x@y.z"}, Message{To: "a@example.org", Subject: "s"}},
		{"empty to", SMTPSender{From: "a@sanholo.space"}, Message{Subject: "s"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.s
			s.Host, s.Port = host, port
			if err := s.Send(context.Background(), tc.m); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	select {
	case got := <-results:
		t.Fatalf("something reached the server: %+v", got)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestSMTPNoHost(t *testing.T) {
	if err := (&SMTPSender{}).Send(context.Background(), Message{To: "a@example.org"}); err == nil {
		t.Fatal("sent without a host")
	}
}

func TestLogSenderSendsNothing(t *testing.T) {
	if err := (LogSender{}).Send(context.Background(), Message{To: "a@example.org", Subject: "s", Body: "https://x/?token=abc"}); err != nil {
		t.Fatal(err)
	}
}
