package export_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lhaig/sanholo-cloud/internal/auth"
	"github.com/lhaig/sanholo-cloud/internal/dbtest"
	"github.com/lhaig/sanholo-cloud/internal/export"
	"github.com/lhaig/sanholo-cloud/internal/export/instancefake"
	"github.com/lhaig/sanholo-cloud/internal/provision"
)

// Fixture secrets: none of these may appear in an archive.
const (
	sessionSecret = "SESSION-COOKIE-SECRET-aaaa1111"
	pushAuthKey   = "PUSH-AUTH-KEY-bbbb2222"
	pushP256dh    = "PUSH-P256DH-KEY-cccc3333"
	pushPath      = "PUSH-ENDPOINT-PATH-SECRET-dddd4444"
	passkeyKey    = "PASSKEY-PUBLIC-KEY-MATERIAL-eeee5555"
	passkeyID     = "PASSKEY-CREDENTIAL-ID-ffff6666"
	inviteCode    = "INVITE-CODE-SECRET-gggg7777"
	csrfToken     = "CSRF-TOKEN-hhhh8888"
	vapidPrivate  = "VAPID-PRIVATE-KEY-iiii9999"
	qmToken       = "QM-CLIENT-TOKEN-jjjj0000"
	gameToken     = "GAME-API-TOKEN-kkkk1111"
	magicToken    = "MAGIC-LINK-TOKEN-llll2222"
)

type resolver struct {
	mu   sync.Mutex
	addr string
	err  error
}

func (r *resolver) Upstream(context.Context, string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.addr, r.err
}

func (r *resolver) set(addr string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.addr, r.err = addr, err
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type fixture struct {
	pool     *pgxpool.Pool
	svc      *export.Service
	inst     *instancefake.Fake
	res      *resolver
	clk      *clock
	pub      ed25519.PublicKey
	priv     ed25519.PrivateKey
	tenant   string
	account  string
	email    string
	other    string // a second account on another tenant
	otherTen string
}

func newFixture(t *testing.T, mod func(*export.Config)) *fixture {
	t.Helper()
	pool := dbtest.Migrated(t)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{pool: pool, pub: pub, priv: priv, res: &resolver{}, clk: &clock{t: time.Now().UTC().Truncate(time.Second)},
		tenant: "ann0000000000001", email: "ann@example.org", otherTen: "bob0000000000002"}
	f.inst = instancefake.New(t, pub, f.tenant)
	f.res.set(f.inst.Addr(), nil)
	cfg := export.Config{
		Pool: pool, BaseURL: "https://app.sanholo.test", Key: priv, Resolver: f.res, Now: f.clk.now,
		BusyWait: 5 * time.Millisecond, BusyRetries: 2,
	}
	if mod != nil {
		mod(&cfg)
	}
	f.svc = export.New(cfg)

	ctx := context.Background()
	f.account = f.addAccount(t, f.tenant, f.email)
	f.other = f.addAccount(t, f.otherTen, "bob@example.org")
	mustExec(t, pool, `INSERT INTO account_sessions (secret_hash, account_id, created_at, last_seen_at, expires_at, user_agent, method)
		VALUES ($1, $2, $3, $3, $4, 'Firefox on Linux', 'passkey')`, auth.HashSecret(sessionSecret), f.account, f.clk.now().Add(-time.Hour), f.clk.now().Add(time.Hour))
	mustExec(t, pool, `INSERT INTO passkeys (id, account_id, credential, created_at) VALUES ($1, $2, $3, $4)`,
		[]byte(passkeyID), f.account, `{"publicKey":"`+passkeyKey+`"}`, f.clk.now().Add(-48*time.Hour))
	mustExec(t, pool, `INSERT INTO push_subscriptions (tenant_id, account_id, endpoint, p256dh, auth, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)`, f.tenant, f.account, "https://fcm.googleapis.com/fcm/send/"+pushPath, pushP256dh, pushAuthKey, f.clk.now().Add(-time.Hour))
	mustExec(t, pool, `INSERT INTO invites (code_hash, email, created_at, expires_at, used_at) VALUES ($1, $2, $3, $4, $3)`,
		auth.HashSecret(inviteCode), strings.ToUpper(f.email), f.clk.now().Add(-72*time.Hour), f.clk.now().Add(-58*time.Hour))
	mustExec(t, pool, `INSERT INTO invites (code_hash, email, created_at, expires_at) VALUES ($1, 'someone-else@example.org', $2, $3)`,
		auth.HashSecret("OTHER-INVITE"), f.clk.now(), f.clk.now().Add(time.Hour))
	mustExec(t, pool, `INSERT INTO game_accounts (tenant_id, game_account_id, replicant_name, replicant_code, connected_at) VALUES ($1, 'game-acct-1', 'Alpha', 'AAAA1111', $2)`, f.tenant, f.clk.now().Add(-24*time.Hour))
	mustExec(t, pool, `INSERT INTO tenant_instances (tenant_id, job_id, image_version, state) VALUES ($1, 'sanholo-tenant-'||$1, '0.7.1', 'running')`, f.tenant)
	f.inst.SetEntries(
		instancefake.Entry{Name: "catalog.sqlite", Data: bytes.Repeat([]byte("SQLITE-CATALOG-"), 2000)},
		instancefake.Entry{Name: "goals.json", Data: []byte("[]")},
		instancefake.Entry{Name: "README.txt", Data: []byte("from the bot")},
	)
	_ = ctx
	return f
}

func mustExec(t *testing.T, pool *pgxpool.Pool, q string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func (f *fixture) addAccount(t *testing.T, tenant, email string) string {
	t.Helper()
	mustExec(t, f.pool, `INSERT INTO tenants (id, status) VALUES ($1, 'active')`, tenant)
	var id string
	if err := f.pool.QueryRow(context.Background(),
		`INSERT INTO accounts (tenant_id, email, email_hash, created_at) VALUES ($1, $2, $3, $4) RETURNING id::text`,
		tenant, email, auth.HashEmail(email), f.clk.now().Add(-96*time.Hour)).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// link issues a link for the fixture's account and returns its secret.
func (f *fixture) link(t *testing.T) string {
	t.Helper()
	return f.linkFor(t, f.account, f.tenant, f.email)
}

// linkFor issues a link for an account and returns its secret.
func (f *fixture) linkFor(t *testing.T, accountID, tenant, email string) string {
	t.Helper()
	l, err := f.svc.CreateLink(context.Background(), accountID, tenant, email)
	if err != nil {
		t.Fatal(err)
	}
	_, secret, ok := strings.Cut(l.URL, "token=")
	if !ok {
		t.Fatalf("no token in %q", l.URL)
	}
	return secret
}

// download spends the link and returns the decompressed entries.
func (f *fixture) download(t *testing.T, secret string) (map[string][]byte, []byte) {
	t.Helper()
	d, err := f.svc.Open(context.Background(), secret, f.account)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()
	var buf bytes.Buffer
	if err := d.WriteTo(context.Background(), &buf); err != nil {
		t.Fatalf("write: %v", err)
	}
	return unzip(t, buf.Bytes()), buf.Bytes()
}

func unzip(t *testing.T, data []byte) map[string][]byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("the archive is not a zip: %v", err)
	}
	out := map[string][]byte{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatalf("%s: %v", f.Name, err)
		}
		out[f.Name] = b
	}
	return out
}

func TestLinkIsStoredAsAHashBoundToTheAccountAndMailed(t *testing.T) {
	f := newFixture(t, nil)
	secret := f.link(t)
	var hash, acct, tenant string
	var expires, created time.Time
	if err := f.pool.QueryRow(context.Background(),
		`SELECT token_hash, account_id::text, tenant_id, created_at, expires_at FROM export_links`).Scan(&hash, &acct, &tenant, &created, &expires); err != nil {
		t.Fatal(err)
	}
	if hash != auth.HashSecret(secret) || strings.Contains(hash, secret) {
		t.Errorf("stored %q for secret %q", hash, secret)
	}
	if acct != f.account || tenant != f.tenant {
		t.Errorf("bound to %s/%s", acct, tenant)
	}
	if expires.Sub(created) != 24*time.Hour {
		t.Errorf("valid for %v, want 24h", expires.Sub(created))
	}
	// The secret is nowhere in the database but the queued email.
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM export_links WHERE token_hash = $1 OR token_hash LIKE '%'||$1||'%'`, secret).Scan(&n); err != nil || n != 0 {
		t.Errorf("the link secret is in export_links (%d, %v)", n, err)
	}
	var body, to, subject string
	if err := f.pool.QueryRow(context.Background(), `SELECT to_email, subject, body FROM mail_outbox`).Scan(&to, &subject, &body); err != nil {
		t.Fatal(err)
	}
	if to != f.email || !strings.Contains(body, "https://app.sanholo.test/account/export/download?token="+secret) ||
		!strings.Contains(body, "The link works once. It expires in 24 hours.") || !strings.Contains(body, "never contains your game token") {
		t.Errorf("mail to %q %q:\n%s", to, subject, body)
	}
}

func TestLinkIsSingleUse(t *testing.T) {
	f := newFixture(t, nil)
	secret := f.link(t)
	if ok, err := f.svc.Valid(context.Background(), secret, f.account); err != nil || !ok {
		t.Fatalf("valid before use: %v %v", ok, err)
	}
	// Looking at the link spends nothing.
	if ok, _ := f.svc.Valid(context.Background(), secret, f.account); !ok {
		t.Fatal("Valid spent the link")
	}
	f.download(t, secret)
	if _, err := f.svc.Open(context.Background(), secret, f.account); !errors.Is(err, export.ErrLinkInvalid) {
		t.Errorf("second use: %v, want ErrLinkInvalid", err)
	}
	if ok, _ := f.svc.Valid(context.Background(), secret, f.account); ok {
		t.Error("a spent link is still valid")
	}
}

func TestParallelUsesOfOneLinkSpendItOnce(t *testing.T) {
	f := newFixture(t, func(c *export.Config) { c.MaxStreams = 16 })
	secret := f.link(t)
	var won, lost atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := f.svc.Open(context.Background(), secret, f.account)
			switch {
			case err == nil:
				won.Add(1)
				d.Close()
			case errors.Is(err, export.ErrLinkInvalid), errors.Is(err, export.ErrBusy):
				// Busy: another use of the same account is running.
				lost.Add(1)
			default:
				t.Errorf("open: %v", err)
			}
		}()
	}
	wg.Wait()
	if won.Load() != 1 || lost.Load() != 7 {
		t.Errorf("won %d, lost %d, want 1 and 7", won.Load(), lost.Load())
	}
}

func TestExpiredLinkIsRefused(t *testing.T) {
	f := newFixture(t, nil)
	secret := f.link(t)
	f.clk.advance(24*time.Hour - time.Second)
	if ok, _ := f.svc.Valid(context.Background(), secret, f.account); !ok {
		t.Error("the link expired early")
	}
	f.clk.advance(2 * time.Second)
	if ok, _ := f.svc.Valid(context.Background(), secret, f.account); ok {
		t.Error("Valid accepts an expired link")
	}
	if _, err := f.svc.Open(context.Background(), secret, f.account); !errors.Is(err, export.ErrLinkInvalid) {
		t.Errorf("expired link: %v, want ErrLinkInvalid", err)
	}
	if n := countRows(t, f.pool, `SELECT count(*) FROM export_links WHERE used_at IS NOT NULL`); n != 0 {
		t.Error("an expired link was marked used")
	}
}

func TestAnotherAccountsLinkIsRefusedAndNotSpent(t *testing.T) {
	f := newFixture(t, nil)
	secret := f.link(t)
	if ok, _ := f.svc.Valid(context.Background(), secret, f.other); ok {
		t.Error("another account may look at the link")
	}
	if _, err := f.svc.Open(context.Background(), secret, f.other); !errors.Is(err, export.ErrLinkInvalid) {
		t.Errorf("another account's use: %v, want ErrLinkInvalid", err)
	}
	// The owner still has it.
	entries, _ := f.download(t, secret)
	if _, ok := entries["account.json"]; !ok {
		t.Error("the owner's download failed after the other account tried")
	}
}

func TestUnknownLinkIsRefused(t *testing.T) {
	f := newFixture(t, nil)
	for _, s := range []string{"", "nope", auth.NewSecret()} {
		if _, err := f.svc.Open(context.Background(), s, f.account); !errors.Is(err, export.ErrLinkInvalid) {
			t.Errorf("link %q: %v", s, err)
		}
	}
}

func TestLinkIsRefusedWhileTheAccountIsBeingDeleted(t *testing.T) {
	f := newFixture(t, nil)
	secret := f.link(t)
	mustExec(t, f.pool, `UPDATE tenants SET status = 'deleting' WHERE id = $1`, f.tenant)
	if _, err := f.svc.Open(context.Background(), secret, f.account); !errors.Is(err, export.ErrLinkInvalid) {
		t.Errorf("use while deleting: %v", err)
	}
	if _, err := f.svc.CreateLink(context.Background(), f.account, f.tenant, f.email); !errors.Is(err, export.ErrDeleting) {
		t.Errorf("new link while deleting: %v", err)
	}
}

func TestArchiveHoldsTheAccountRecordTheBotsFilesAndAReadme(t *testing.T) {
	f := newFixture(t, nil)
	entries, _ := f.download(t, f.link(t))
	for _, name := range []string{"account.json", "instance/catalog.sqlite", "instance/goals.json", "instance/README.txt", "README.txt"} {
		if _, ok := entries[name]; !ok {
			t.Errorf("the archive lacks %s (has %v)", name, keys(entries))
		}
	}
	if !bytes.Equal(entries["instance/catalog.sqlite"], bytes.Repeat([]byte("SQLITE-CATALOG-"), 2000)) || string(entries["instance/README.txt"]) != "from the bot" {
		t.Error("the bot's files were changed on the way through")
	}
	var doc struct {
		Email     string `json:"email"`
		CreatedAt string `json:"created_at"`
		Invites   []struct {
			UsedAt *string `json:"used_at"`
		} `json:"invites"`
		Sessions []struct {
			Method    string `json:"method"`
			UserAgent string `json:"user_agent"`
		} `json:"sessions"`
		Passkeys []struct {
			AddedAt string `json:"added_at"`
		} `json:"passkeys"`
		Push []struct {
			Host string `json:"endpoint_host"`
		} `json:"push_subscriptions"`
		Game *struct {
			Name string `json:"replicant_name"`
			Code string `json:"replicant_code"`
		} `json:"game_account"`
		Instance *struct {
			State   string `json:"state"`
			Version string `json:"version"`
		} `json:"instance"`
	}
	if err := json.Unmarshal(entries["account.json"], &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Email != f.email || doc.CreatedAt == "" {
		t.Errorf("email %q created %q", doc.Email, doc.CreatedAt)
	}
	if len(doc.Invites) != 1 || doc.Invites[0].UsedAt == nil {
		t.Errorf("invites = %+v, want only this address's (case-insensitive)", doc.Invites)
	}
	if len(doc.Sessions) != 1 || doc.Sessions[0].Method != "passkey" || doc.Sessions[0].UserAgent != "Firefox on Linux" {
		t.Errorf("sessions = %+v", doc.Sessions)
	}
	if len(doc.Passkeys) != 1 || doc.Passkeys[0].AddedAt == "" {
		t.Errorf("passkeys = %+v", doc.Passkeys)
	}
	if len(doc.Push) != 1 || doc.Push[0].Host != "fcm.googleapis.com" {
		t.Errorf("push = %+v, want the host only", doc.Push)
	}
	if doc.Game == nil || doc.Game.Name != "Alpha" || doc.Game.Code != "AAAA1111" {
		t.Errorf("game = %+v", doc.Game)
	}
	if doc.Instance == nil || doc.Instance.State != "running" || doc.Instance.Version != "0.7.1" {
		t.Errorf("instance = %+v", doc.Instance)
	}
	if !strings.Contains(string(entries["README.txt"]), "instance/ holds the export of your bot's data") {
		t.Errorf("README:\n%s", entries["README.txt"])
	}
}

func keys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func countRows(t *testing.T, pool *pgxpool.Pool, q string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The archive must hold no token, link or session secret, CSRF token, push
// key, passkey key material, qm material or identity. Fixture secrets are
// planted where each could leak from (the database, the configuration, the
// request the bot receives) and the decompressed archive is scanned for
// every one of them, and for the hashes the database keeps.
func TestArchiveHoldsNoSecret(t *testing.T) {
	f := newFixture(t, nil)
	// The scan means something only if the secrets really are where they
	// could leak from.
	for what, q := range map[string]string{
		"push endpoint path": `SELECT count(*) FROM push_subscriptions WHERE endpoint LIKE '%` + pushPath + `%'`,
		"push auth key":      `SELECT count(*) FROM push_subscriptions WHERE auth = '` + pushAuthKey + `'`,
		"push p256dh key":    `SELECT count(*) FROM push_subscriptions WHERE p256dh = '` + pushP256dh + `'`,
		"passkey key":        `SELECT count(*) FROM passkeys WHERE credential::text LIKE '%` + passkeyKey + `%'`,
		"session hash":       `SELECT count(*) FROM account_sessions WHERE secret_hash = '` + auth.HashSecret(sessionSecret) + `'`,
		"invite hash":        `SELECT count(*) FROM invites WHERE code_hash = '` + auth.HashSecret(inviteCode) + `'`,
	} {
		if countRows(t, f.pool, q) != 1 {
			t.Fatalf("the fixture does not plant the %s in the database", what)
		}
	}
	secret := f.link(t)
	if countRows(t, f.pool, `SELECT count(*) FROM export_links WHERE token_hash = $1`, auth.HashSecret(secret)) != 1 {
		t.Fatal("the fixture's link hash is not in the database")
	}
	entries, raw := f.download(t, secret)

	var all bytes.Buffer
	all.Write(raw)
	for name, b := range entries {
		all.WriteString(name)
		all.Write(b)
	}
	forbidden := map[string]string{
		"link secret":            secret,
		"link hash":              auth.HashSecret(secret),
		"session secret":         sessionSecret,
		"session hash":           auth.HashSecret(sessionSecret),
		"push auth key":          pushAuthKey,
		"push p256dh key":        pushP256dh,
		"push endpoint path":     pushPath,
		"passkey key material":   passkeyKey,
		"passkey credential id":  passkeyID,
		"invite code":            inviteCode,
		"invite code hash":       auth.HashSecret(inviteCode),
		"csrf token":             csrfToken,
		"vapid private key":      vapidPrivate,
		"qm token":               qmToken,
		"game token":             gameToken,
		"magic link token":       magicToken,
		"proxy signing key seed": string(f.priv.Seed()),
		"proxy signing key":      string(f.priv),
	}
	for _, id := range f.inst.Identities() {
		forbidden["identity sent to the bot"] = id
	}
	if len(f.inst.Identities()) == 0 {
		t.Fatal("the bot was never asked, so the identity could not be checked")
	}
	for what, needle := range forbidden {
		if needle == "" {
			t.Fatalf("fixture secret %q is empty: the scan would pass on anything", what)
		}
		if bytes.Contains(all.Bytes(), []byte(needle)) {
			t.Errorf("the archive contains the %s", what)
		}
	}
	// The scan sees what it is meant to: it finds a planted marker in the
	// same bytes. (Guards against scanning compressed data only.)
	if !bytes.Contains(all.Bytes(), []byte("SQLITE-CATALOG-")) || !bytes.Contains(all.Bytes(), []byte(f.email)) {
		t.Fatal("the scan does not see the decompressed content")
	}
}

// File names from the bot become entries under instance/. Only plain names
// pass; anything that could climb out of the folder or name another place is
// dropped, and the good entries around it are kept.
func TestUnsafeEntryNamesFromTheBotAreDropped(t *testing.T) {
	f := newFixture(t, nil)
	f.inst.SetEntries(
		instancefake.Entry{Name: "../evil.txt", Data: []byte("1")},
		instancefake.Entry{Name: "goals.json", Data: []byte("[]")},
		instancefake.Entry{Name: "a/b.txt", Data: []byte("2")},
		instancefake.Entry{Name: `c\d.txt`, Data: []byte("3")},
		instancefake.Entry{Name: "/abs.txt", Data: []byte("4")},
		instancefake.Entry{Name: "..", Data: []byte("5")},
		instancefake.Entry{Name: "back..up.json", Data: []byte("6")},
		instancefake.Entry{Name: strings.Repeat("n", 101), Data: []byte("7")},
		instancefake.Entry{Name: "jobs.json", Data: []byte("[]")},
	)
	entries, _ := f.download(t, f.link(t))
	var got []string
	for n := range entries {
		if strings.HasPrefix(n, "instance/") {
			got = append(got, n)
		}
	}
	if len(got) != 2 || entries["instance/goals.json"] == nil || entries["instance/jobs.json"] == nil {
		t.Errorf("instance entries = %v, want only goals.json and jobs.json", got)
	}
	if !strings.Contains(string(entries["README.txt"]), "instance/ holds the export") {
		t.Errorf("README:\n%s", entries["README.txt"])
	}
}

func TestInstanceNotRunningStillGivesTheAccountRecord(t *testing.T) {
	for name, err := range map[string]error{
		"not running": provision.ErrNotRunning,
		"starting":    provision.ErrStarting,
		"unreadable":  errors.New("nomad: boom"),
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, nil)
			f.res.set("", err)
			entries, _ := f.download(t, f.link(t))
			if _, ok := entries["account.json"]; !ok {
				t.Fatal("no account.json")
			}
			for n := range entries {
				if strings.HasPrefix(n, "instance/") {
					t.Errorf("unexpected %s", n)
				}
			}
			readme := string(entries["README.txt"])
			if !strings.Contains(readme, "Your bot's data was not available") || !strings.Contains(readme, "Request a new export while your bot is running") {
				t.Errorf("README:\n%s", readme)
			}
			if f.inst.Requests() != 0 {
				t.Error("the bot was asked although the resolver said it is not there")
			}
		})
	}
}

func TestInstanceAnsweringWithAnErrorStillGivesTheAccountRecord(t *testing.T) {
	f := newFixture(t, nil)
	f.inst.Fail(500)
	entries, _ := f.download(t, f.link(t))
	readme := string(entries["README.txt"])
	if _, ok := entries["account.json"]; !ok || !strings.Contains(readme, "status 500") || strings.Contains(readme, "instance/ holds") {
		t.Errorf("entries %v README:\n%s", keys(entries), readme)
	}
}

func TestInstanceWithoutASigningKeyIsReportedNotFailed(t *testing.T) {
	f := newFixture(t, func(c *export.Config) { c.Key = nil })
	entries, _ := f.download(t, f.link(t))
	if !strings.Contains(string(entries["README.txt"]), "Your bot's data was not available") || f.inst.Requests() != 0 {
		t.Errorf("README:\n%s requests=%d", entries["README.txt"], f.inst.Requests())
	}
}

func TestInstanceBusyIsRetriedBrieflyThenReported(t *testing.T) {
	f := newFixture(t, nil)
	// Busy for the first request only: the retry gets the archive.
	f.inst.Busy(1)
	entries, _ := f.download(t, f.link(t))
	if _, ok := entries["instance/goals.json"]; !ok || f.inst.Requests() != 2 {
		t.Errorf("busy once: entries %v, requests %d (want 2)", keys(entries), f.inst.Requests())
	}

	// Busy for good: 1 try + 2 retries, then the README says so and the
	// download still succeeds.
	g := newFixture(t, nil)
	g.inst.Busy(100)
	entries, _ = g.download(t, g.link(t))
	readme := string(entries["README.txt"])
	if g.inst.Requests() != 3 {
		t.Errorf("busy for good: %d requests, want 3", g.inst.Requests())
	}
	if _, ok := entries["account.json"]; !ok || !strings.Contains(readme, "busy with another export") || strings.Contains(readme, "instance/ holds") {
		t.Errorf("entries %v README:\n%s", keys(entries), readme)
	}
	for n := range entries {
		if strings.HasPrefix(n, "instance/") {
			t.Errorf("unexpected %s", n)
		}
	}
}

func TestInstanceCutOffHalfWayIsReported(t *testing.T) {
	f := newFixture(t, nil)
	f.inst.CutAfter(2)
	entries, _ := f.download(t, f.link(t))
	readme := string(entries["README.txt"])
	if _, ok := entries["account.json"]; !ok || !strings.Contains(readme, "stopped part way") || strings.Contains(readme, "instance/ holds") {
		t.Errorf("entries %v README:\n%s", keys(entries), readme)
	}
}

func TestIdentityIsFreshAndSignedForTheTenant(t *testing.T) {
	// The fake sits behind the client's own guard: an identity that is
	// missing, badly signed, expired or for another tenant is a 401 or 403
	// and the README would say "status 401". Getting the archive proves it.
	f := newFixture(t, nil)
	entries, _ := f.download(t, f.link(t))
	if _, ok := entries["instance/goals.json"]; !ok {
		t.Fatalf("the bot refused the identity:\n%s", entries["README.txt"])
	}
	// A different key is refused.
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	g := newFixture(t, func(c *export.Config) { c.Key = other })
	entries, _ = g.download(t, g.link(t))
	if !strings.Contains(string(entries["README.txt"]), "status 401") {
		t.Errorf("a wrongly signed identity was accepted:\n%s", entries["README.txt"])
	}
	// Each attempt carries its own identity: a retry after a 429 is signed again.
	h := newFixture(t, nil)
	h.inst.Busy(1)
	h.download(t, h.link(t))
	ids := h.inst.Identities()
	if len(ids) != 2 || ids[0] == "" {
		t.Fatalf("identities = %v", ids)
	}
}

// The bot's part is passed through as it arrives: the first bytes reach the
// reader while the bot is still holding back the rest, so nothing waits for
// (or keeps) the whole export.
func TestInstancePartIsStreamedThrough(t *testing.T) {
	f := newFixture(t, nil)
	big := make([]byte, 1<<20) // incompressible, so the bytes really are on the wire
	if _, err := rand.Read(big); err != nil {
		t.Fatal(err)
	}
	f.inst.SetEntries(instancefake.Entry{Name: "catalog.sqlite", Data: big}, instancefake.Entry{Name: "goals.json", Data: []byte("[]")})
	release := f.inst.Gate(1) // entry 0 goes out, entry 1 waits
	d, err := f.svc.Open(context.Background(), f.link(t), f.account)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := d.WriteTo(context.Background(), pw)
		_ = pw.CloseWithError(err)
		done <- err
	}()
	// Read until the first instance entry's name has been written.
	var seen bytes.Buffer
	buf := make([]byte, 4096)
	found := make(chan struct{})
	go func() {
		for {
			n, err := pr.Read(buf)
			seen.Write(buf[:n])
			if bytes.Contains(seen.Bytes(), []byte("instance/catalog.sqlite")) {
				close(found)
				break
			}
			if err != nil {
				return
			}
		}
		_, _ = io.Copy(io.Discard, pr) // drain the rest
	}()
	select {
	case <-found:
	case <-time.After(5 * time.Second):
		t.Fatal("no byte of the bot's first file arrived while the bot was still holding back the rest")
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestAtMostMaxStreamsRunAtOnceAndBusyLeavesTheLinkUnspent(t *testing.T) {
	f := newFixture(t, func(c *export.Config) { c.MaxStreams = 1 })
	first, err := f.svc.Open(context.Background(), f.link(t), f.account)
	if err != nil {
		t.Fatal(err)
	}
	// Another account: only the global limit can refuse it.
	bob := f.other
	second := f.linkFor(t, bob, f.otherTen, "bob@example.org")
	if _, err := f.svc.Open(context.Background(), second, bob); !errors.Is(err, export.ErrBusy) {
		t.Fatalf("second stream: %v, want ErrBusy", err)
	}
	if ok, _ := f.svc.Valid(context.Background(), second, bob); !ok {
		t.Error("a refused-busy request spent the link")
	}
	first.Close()
	if d, err := f.svc.Open(context.Background(), second, bob); err != nil {
		t.Errorf("after the first finished: %v", err)
	} else {
		d.Close()
	}
}

// One account runs one export at a time, however many places are free; the
// refused request leaves its link unspent and other accounts are not held up.
func TestOneRunningExportPerAccount(t *testing.T) {
	f := newFixture(t, func(c *export.Config) { c.MaxStreams = 4 })
	first, err := f.svc.Open(context.Background(), f.link(t), f.account)
	if err != nil {
		t.Fatal(err)
	}
	second := f.link(t)
	if _, err := f.svc.Open(context.Background(), second, f.account); !errors.Is(err, export.ErrBusy) {
		t.Fatalf("a second export of the same account: %v, want ErrBusy", err)
	}
	if ok, _ := f.svc.Valid(context.Background(), second, f.account); !ok {
		t.Error("a refused-busy request spent the link")
	}
	bob := f.other
	other, err := f.svc.Open(context.Background(), f.linkFor(t, bob, f.otherTen, "bob@example.org"), bob)
	if err != nil {
		t.Fatalf("another account was held up: %v", err)
	}
	other.Close()
	first.Close()
	d, err := f.svc.Open(context.Background(), second, f.account)
	if err != nil {
		t.Fatalf("after the first finished: %v", err)
	}
	d.Close()
}

func TestDownloadStopsWhenTheContextEnds(t *testing.T) {
	f := newFixture(t, nil)
	release := f.inst.Gate(1)
	defer release()
	d, err := f.svc.Open(context.Background(), f.link(t), f.account)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.WriteTo(ctx, io.Discard) }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a cancelled download reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the download ignored the cancelled context")
	}
}

// A download whose context is already over does not write a README that
// pretends the bot's data was merely unavailable.
func TestDownloadWithACancelledContextFails(t *testing.T) {
	f := newFixture(t, nil)
	d, err := f.svc.Open(context.Background(), f.link(t), f.account)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var buf bytes.Buffer
	if err := d.WriteTo(ctx, &buf); err == nil {
		t.Error("a cancelled download reported success")
	}
	if bytes.Contains(buf.Bytes(), []byte("README.txt")) {
		t.Error("a README was written for a download that was given up on")
	}
}
