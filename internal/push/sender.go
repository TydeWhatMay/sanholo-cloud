package push

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// Outcomes of one delivery attempt, as the push service answered.
var (
	// ErrGone: the subscription is dead (404 or 410). Delete it.
	ErrGone = errors.New("push: subscription gone")
	// ErrPermanent: the push service refused this message for good.
	ErrPermanent = errors.New("push: refused")
)

// RetryError asks for a later retry (429, 5xx, network failure).
type RetryError struct {
	After time.Duration // the push service's Retry-After, 0 if none
}

func (e *RetryError) Error() string { return "push: try again later" }

// Sender delivers one encrypted message to one subscription.
type Sender interface {
	Send(ctx context.Context, sub Subscription, payload []byte) error
}

// VAPID is the application server identity.
type VAPID struct {
	Private string // base64url P-256 scalar; a secret
	Public  string // base64url uncompressed point
	Subject string // mailto:... or https://...
}

// WebPush sends RFC 8291 (aes128gcm) messages signed with RFC 8292 VAPID, via
// github.com/SherClockHolmes/webpush-go.
type WebPush struct {
	vapid  VAPID
	client webpush.HTTPClient
}

// NewWebPush returns a sender whose HTTP client refuses non-public addresses
// at dial time, uses no proxy and follows no redirect.
func NewWebPush(v VAPID) *WebPush {
	return &WebPush{vapid: v, client: SafeHTTPClient()}
}

// NewWebPushWithClient is for tests that need to reach a local fake.
func NewWebPushWithClient(v VAPID, c webpush.HTTPClient) *WebPush {
	return &WebPush{vapid: v, client: c}
}

// SafeHTTPClient is the client used to reach push services.
func SafeHTTPClient() *http.Client {
	d := &net.Dialer{Timeout: 5 * time.Second, Control: SafeDialControl}
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			Proxy: nil, DialContext: d.DialContext, TLSHandshakeTimeout: 5 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second, MaxIdleConnsPerHost: 4, IdleConnTimeout: 60 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Send encrypts payload for sub and posts it.
func (w *WebPush) Send(ctx context.Context, sub Subscription, payload []byte) error {
	subject := strings.TrimPrefix(w.vapid.Subject, "mailto:") // the library adds the scheme
	resp, err := webpush.SendNotificationWithContext(ctx, payload, &webpush.Subscription{
		Endpoint: sub.Endpoint, Keys: webpush.Keys{Auth: sub.Keys.Auth, P256dh: sub.Keys.P256dh},
	}, &webpush.Options{
		HTTPClient: w.client, Subscriber: subject,
		VAPIDPublicKey: w.vapid.Public, VAPIDPrivateKey: w.vapid.Private,
		TTL: 3600, Urgency: webpush.UrgencyHigh,
	})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// The error text can carry the endpoint URL; keep it out.
		return &RetryError{}
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return classify(resp.StatusCode, resp.Header.Get("Retry-After"))
}

func classify(status int, retryAfter string) error {
	switch {
	case status >= 200 && status < 300:
		return nil
	case status == http.StatusNotFound || status == http.StatusGone:
		return ErrGone
	case status == http.StatusTooManyRequests || status >= 500:
		e := &RetryError{}
		if n, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil && n > 0 {
			e.After = time.Duration(n) * time.Second
		}
		return e
	default:
		return fmt.Errorf("%w: HTTP %d", ErrPermanent, status)
	}
}
