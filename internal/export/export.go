// Package export is the customer's data export (US-017). The account page
// issues a single-use download link (valid 24 hours, stored as a hash, bound
// to the account, mailed and shown once). Using the link streams a ZIP that
// is built while it is sent:
//
//	account.json   what SanHolo's database holds about the account
//	instance/      the entries of the bot's own /hosted/export, streamed through
//	README.txt
//
// The bot is asked with a freshly signed identity, exactly as the proxy does
// it. If the bot is not running, is busy or fails, the archive still holds
// account.json and the README says what was missing; the download does not
// fail. Nothing secret is read into the archive: no game token (the database
// has none), no link or session secret, no CSRF token, no push key, no key
// material of a passkey, no qm or identity material.
package export

import (
	"archive/zip"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lhaig/sanholo-cloud/internal/auth"
	"github.com/lhaig/sanholo-cloud/internal/identity"
	"github.com/lhaig/sanholo-cloud/internal/mail"
	"github.com/lhaig/sanholo-cloud/internal/provision"
)

// LinkTTL is how long a download link works.
const LinkTTL = 24 * time.Hour

// DailyLimit is the export links an account may ask for in 24 hours.
const DailyLimit = 3

var (
	// ErrLinkInvalid: the link is unknown, spent, expired, or belongs to
	// another account. The four cases are not told apart.
	ErrLinkInvalid = errors.New("the download link is not valid")
	// ErrBusy: too many exports are being built right now; the link was not
	// spent and the request can be repeated.
	ErrBusy = errors.New("too many exports are running")
	// ErrDeleting: the account is being deleted.
	ErrDeleting = errors.New("the account is being deleted")
)

// Resolver finds where a tenant's bot listens (*provision.Service).
type Resolver interface {
	Upstream(ctx context.Context, tenantID string) (string, error)
}

// Config configures a Service.
type Config struct {
	Pool *pgxpool.Pool
	// BaseURL is the public origin the download link is built from.
	BaseURL string
	// Key signs the identity sent to the bot; Resolver finds the bot. Without
	// both, every archive says the bot's data was not available.
	Key      ed25519.PrivateKey
	Resolver Resolver
	// MaxStreams bounds the archives built at once (default 4).
	MaxStreams int
	// BusyRetries and BusyWait: when the bot answers 429 (one export at a
	// time), the request is repeated up to BusyRetries times (default 2),
	// waiting what the bot's Retry-After says but at most BusyWait (default
	// 10 seconds).
	BusyRetries int
	BusyWait    time.Duration
	// HeaderTimeout is how long the bot may take to start answering; it
	// copies its catalog first (default 5 minutes).
	HeaderTimeout time.Duration
	// Transport and Now are for tests.
	Transport http.RoundTripper
	Now       func() time.Time
	Logger    *slog.Logger
}

// Service issues links and builds archives. Safe for concurrent use.
type Service struct {
	cfg Config
	rt  http.RoundTripper
	sem chan struct{}
	log *slog.Logger
}

// New returns a Service.
func New(cfg Config) *Service {
	if cfg.MaxStreams <= 0 {
		cfg.MaxStreams = 4
	}
	if cfg.BusyRetries <= 0 {
		cfg.BusyRetries = 2
	}
	if cfg.BusyWait <= 0 {
		cfg.BusyWait = 10 * time.Second
	}
	if cfg.HeaderTimeout <= 0 {
		cfg.HeaderTimeout = 5 * time.Minute
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	s := &Service{cfg: cfg, rt: cfg.Transport, sem: make(chan struct{}, cfg.MaxStreams), log: cfg.Logger}
	if s.rt == nil {
		s.rt = &http.Transport{
			Proxy:                 nil, // the bot is on the private network
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			DisableCompression:    true,
			ResponseHeaderTimeout: cfg.HeaderTimeout,
		}
	}
	return s
}

// Link is a new download link. URL holds the secret; it is shown once and
// mailed, and not kept.
type Link struct {
	URL     string
	Expires time.Time
}

// CreateLink issues a link for the account, mails it (through the outbox, in
// the same transaction as the link) and returns it for the page to show once.
func (s *Service) CreateLink(ctx context.Context, accountID, tenantID, email string) (Link, error) {
	secret := auth.NewSecret()
	now := s.cfg.Now()
	expires := now.Add(LinkTTL)
	link := strings.TrimRight(s.cfg.BaseURL, "/") + "/account/export/download?token=" + secret
	tx, err := s.cfg.Pool.Begin(ctx)
	if err != nil {
		return Link{}, fmt.Errorf("export: link: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM tenants WHERE id = $1 FOR SHARE`, tenantID).Scan(&status); err != nil {
		return Link{}, fmt.Errorf("export: link: %w", err)
	}
	if status == "deleting" {
		return Link{}, ErrDeleting
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO export_links (token_hash, account_id, tenant_id, created_at, expires_at)
		SELECT $1, id, tenant_id, $4, $5 FROM accounts WHERE id = $2 AND tenant_id = $3`,
		auth.HashSecret(secret), accountID, tenantID, now, expires); err != nil {
		return Link{}, fmt.Errorf("export: link: %w", err)
	}
	if err := mail.Enqueue(ctx, tx, LinkMessage(email, link), expires); err != nil {
		return Link{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Link{}, fmt.Errorf("export: link: %w", err)
	}
	return Link{URL: link, Expires: expires}, nil
}

// LinkMessage is the email that carries the download link, in the company
// register.
func LinkMessage(to, link string) mail.Message {
	return mail.Message{
		To:      to,
		Subject: "Your SanHolo data export",
		Body: "TydeWhatMay sends this link because someone asked for an export of the data SanHolo holds for this account.\n\n" +
			link + "\n\n" +
			"Open the link while you are signed in to your SanHolo account, then press the download button.\n" +
			"The link works once. It expires in 24 hours.\n" +
			"The export never contains your game token.\n" +
			"If you did not ask for it, ignore this email. Nothing is downloaded until the link is used.\n",
	}
}

// Valid reports whether secret names a live link of the account. It spends
// nothing: the page behind a GET may be fetched by a scanner or a prefetcher.
func (s *Service) Valid(ctx context.Context, secret, accountID string) (bool, error) {
	if secret == "" {
		return false, nil
	}
	var ok bool
	err := s.cfg.Pool.QueryRow(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM export_links l JOIN tenants t ON t.id = l.tenant_id
		  WHERE l.token_hash = $1 AND l.account_id = $2 AND l.used_at IS NULL AND l.expires_at > $3 AND t.status <> 'deleting')`,
		auth.HashSecret(secret), accountID, s.cfg.Now()).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("export: link: %w", err)
	}
	return ok, nil
}

// Download is a spent link and the archive it entitles to.
type Download struct {
	s        *Service
	acct     accountDoc
	tenantID string
	release  func()
}

// Open spends the link and prepares the archive. The link works once: it is
// spent here, before a byte is sent, so a second request (or a parallel one)
// is refused. ErrBusy leaves it unspent. The caller must call Close.
func (s *Service) Open(ctx context.Context, secret, accountID string) (*Download, error) {
	select {
	case s.sem <- struct{}{}:
	default:
		return nil, ErrBusy
	}
	release := func() { <-s.sem }
	d, err := s.open(ctx, secret, accountID)
	if err != nil {
		release()
		return nil, err
	}
	d.release = release
	return d, nil
}

func (s *Service) open(ctx context.Context, secret, accountID string) (*Download, error) {
	if secret == "" {
		return nil, ErrLinkInvalid
	}
	tx, err := s.cfg.Pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("export: open: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	now := s.cfg.Now()
	// The archive's account part is read first, in the same transaction: a
	// failure here leaves the link unspent. The spend is one UPDATE that also
	// checks the owner, the expiry and the first use.
	var tenantID string
	err = tx.QueryRow(ctx, `
		UPDATE export_links l SET used_at = $3
		FROM tenants t
		WHERE l.token_hash = $1 AND l.account_id = $2 AND l.used_at IS NULL AND l.expires_at > $3
		  AND t.id = l.tenant_id AND t.status <> 'deleting'
		RETURNING l.tenant_id`, auth.HashSecret(secret), accountID, now).Scan(&tenantID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrLinkInvalid
	}
	if err != nil {
		return nil, fmt.Errorf("export: open: %w", err)
	}
	doc, err := s.accountDoc(ctx, tx, accountID, tenantID, now)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("export: open: %w", err)
	}
	return &Download{s: s, acct: doc, tenantID: tenantID}, nil
}

// Close releases the download's place among the running exports.
func (d *Download) Close() {
	if d.release != nil {
		d.release()
		d.release = nil
	}
}

// Filename is the suggested name of the archive.
func (d *Download) Filename() string {
	return "sanholo-export-" + d.s.cfg.Now().UTC().Format("2006-01-02") + ".zip"
}

// WriteTo streams the archive to w. An error means the stream broke (the
// reader went away, the clock ran out); a bot that cannot be exported is not
// an error, it is a line in the README.
func (d *Download) WriteTo(ctx context.Context, w io.Writer) error {
	now := d.s.cfg.Now().UTC()
	zw := zip.NewWriter(w)
	add := func(name string) (io.Writer, error) {
		return zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: now})
	}
	aw, err := add("account.json")
	if err != nil {
		return err
	}
	enc := json.NewEncoder(aw)
	enc.SetIndent("", "  ")
	if err := enc.Encode(d.acct); err != nil {
		return err
	}
	// The first bytes go out now: the bot may take a while to start
	// answering, and a connection with nothing on it can be given up by
	// whatever sits in front.
	if f, ok := w.(interface{ Flush() error }); ok {
		if err := f.Flush(); err != nil {
			return err
		}
	}
	note, err := d.instance(ctx, zw, now)
	if err != nil {
		return err
	}
	rw, err := add("README.txt")
	if err != nil {
		return err
	}
	if _, err := io.WriteString(rw, readme(now, note)); err != nil {
		return err
	}
	return zw.Close()
}

// Notes for the README about the bot's part.
const (
	noteOK          = "instance/ holds the export of your bot's data, as your bot wrote it."
	noteBusy        = "Your bot's data was not included: your bot was busy with another export. account.json is complete. Request a new export in a few minutes."
	notePartial     = "Your bot's export stopped part way. The files under instance/ may be incomplete. account.json is complete. Request a new export while your bot is running to try again."
	noteUnavailable = "Your bot's data was not available (%s). account.json is complete. Request a new export while your bot is running to get the bot's data too."
)

func readme(now time.Time, instanceNote string) string {
	return "This archive contains an export of your SanHolo account, made on " + now.Format("2006-01-02 15:04 UTC") + ".\n\n" +
		"account.json: what SanHolo's database holds for your account: your email address and creation date, invites for your address (without their codes), your sessions, passkeys (without their keys), notification subscriptions (the push service's host name only), your game account record and the state of your bot.\n" +
		"instance/: the data your bot exports: a copy of its catalog database (catalog.sqlite) and your goals, jobs, decisions and activity log as JSON.\n\n" +
		instanceNote + "\n\n" +
		"The archive does not contain your game token, sign-in links, session or security tokens, notification keys or any other credential.\n"
}

// instance streams the bot's export into zw under instance/ and returns the
// README line for it.
func (d *Download) instance(ctx context.Context, zw *zip.Writer, now time.Time) (string, error) {
	resp, reason := d.s.fetch(ctx, d.tenantID)
	if resp == nil {
		if err := ctx.Err(); err != nil {
			return "", err // the download was given up on; nothing is a README line
		}
		if reason == reasonBusy {
			return noteBusy, nil
		}
		return fmt.Sprintf(noteUnavailable, reason), nil
	}
	defer func() { _ = resp.Body.Close() }()
	var writeErr error
	err := readZipStream(resp.Body, func(name string, body io.Reader) error {
		if !safeName(name) {
			return nil // skipped; its bytes are read and dropped
		}
		ew, err := zw.CreateHeader(&zip.FileHeader{Name: "instance/" + name, Method: zip.Deflate, Modified: now})
		if err != nil {
			writeErr = err
			return err
		}
		tw := &trackWriter{w: ew}
		if _, err := io.Copy(tw, body); err != nil {
			writeErr = tw.err // nil when it was the bot's stream that broke
			return err
		}
		return nil
	})
	if writeErr != nil {
		return "", writeErr // our own writer failed: the download is over
	}
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		d.s.log.Warn("export: the bot's export stopped", "tenant", d.tenantID, "error", err)
		return notePartial, nil
	}
	return noteOK, nil
}

// trackWriter remembers a failure of the writer it wraps, which tells the
// failure of our own output (the download is over) from a failure of the
// bot's stream (the archive carries on and says so).
type trackWriter struct {
	w   io.Writer
	err error
}

func (t *trackWriter) Write(p []byte) (int, error) {
	n, err := t.w.Write(p)
	if err != nil {
		t.err = err
	}
	return n, err
}

// safeName accepts the plain file names the bot writes and nothing that
// could escape the instance/ folder.
func safeName(name string) bool {
	if name == "" || len(name) > 100 || name == "." || name == ".." {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
		default:
			return false
		}
	}
	return !strings.Contains(name, "..")
}

// Reasons for the README, in fixed phrases.
const (
	reasonBusy = "busy"
)

// fetch asks the bot for its export with a freshly signed identity. It
// returns the response (200 and a ZIP) or the fixed reason for the README.
func (s *Service) fetch(ctx context.Context, tenantID string) (*http.Response, string) {
	if s.cfg.Resolver == nil || len(s.cfg.Key) != ed25519.PrivateKeySize {
		return nil, "the bot is not set up here"
	}
	addr, err := s.cfg.Resolver.Upstream(ctx, tenantID)
	switch {
	case errors.Is(err, provision.ErrNotRunning):
		return nil, "the bot is not running"
	case errors.Is(err, provision.ErrStarting):
		return nil, "the bot is starting"
	case err != nil:
		s.log.Warn("export: could not find the bot", "tenant", tenantID, "error", err)
		return nil, "the bot could not be reached"
	}
	for attempt := 0; ; attempt++ {
		resp, err := s.get(ctx, addr, tenantID)
		if err != nil {
			s.log.Warn("export: could not reach the bot", "tenant", tenantID, "error", err)
			return nil, "the bot could not be reached"
		}
		switch {
		case resp.StatusCode == http.StatusOK && strings.HasPrefix(resp.Header.Get("Content-Type"), "application/zip"):
			return resp, ""
		case resp.StatusCode == http.StatusTooManyRequests:
			wait := retryAfter(resp.Header.Get("Retry-After"), s.cfg.BusyWait)
			_ = resp.Body.Close()
			if attempt >= s.cfg.BusyRetries {
				return nil, reasonBusy
			}
			select {
			case <-ctx.Done():
				return nil, "the download was cancelled"
			case <-time.After(wait):
			}
		default:
			_ = resp.Body.Close()
			return nil, "the bot's export failed (status " + strconv.Itoa(resp.StatusCode) + ")"
		}
	}
}

func (s *Service) get(ctx context.Context, addr, tenantID string) (*http.Response, error) {
	tok, err := identity.Sign(s.cfg.Key, tenantID, s.cfg.Now())
	if err != nil {
		return nil, errors.New("export: could not sign the identity")
	}
	u := url.URL{Scheme: "http", Host: addr, Path: "/hosted/export"}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set(identity.Header, tok)
	return s.rt.RoundTrip(req)
}

// retryAfter reads a Retry-After in seconds, at most max.
func retryAfter(v string, max time.Duration) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return max
	}
	return min(time.Duration(n)*time.Second, max)
}
