package server

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/lhaig/sanholo-cloud/internal/push"
	"github.com/lhaig/sanholo-cloud/internal/workload"
)

type recSender struct {
	mu   sync.Mutex
	sent []struct {
		endpoint string
		p        push.Payload
	}
}

func (r *recSender) Send(_ context.Context, sub push.Subscription, payload []byte) error {
	var p push.Payload
	_ = json.Unmarshal(payload, &p)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, struct {
		endpoint string
		p        push.Payload
	}{sub.Endpoint, p})
	return nil
}

func (r *recSender) endpoints() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, s := range r.sent {
		out = append(out, s.endpoint)
	}
	return out
}

type pushEnv struct {
	*bridgeEnv
	t      *testing.T
	sender *recSender
	svc    *push.Service
	signer *rsa.PrivateKey
	jwks   *httptest.Server
	jwksUp func(bool)
}

const goodCSRF = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func newPushEnv(t *testing.T) *pushEnv {
	t.Helper()
	b := newBridgeEnv(t)
	pe := &pushEnv{bridgeEnv: b, t: t, sender: &recSender{}}
	var err error
	if pe.signer, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
		t.Fatal(err)
	}
	var down sync.Mutex
	isDown := false
	pe.jwksUp = func(up bool) { down.Lock(); isDown = !up; down.Unlock() }
	pub := &pe.signer.PublicKey
	set, _ := json.Marshal(map[string]any{"keys": []map[string]string{{
		"use": "sig", "kty": "RSA", "kid": "k1", "alg": "RS256",
		"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}}})
	pe.jwks = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		down.Lock()
		d := isDown
		down.Unlock()
		if d {
			http.Error(w, "down", 500)
			return
		}
		_, _ = w.Write(set)
	}))
	t.Cleanup(pe.jwks.Close)
	ver, err := workload.New(workload.Config{JWKSURL: pe.jwks.URL, Namespace: "sanholo", MinRefresh: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	store := push.NewStore(b.pool)
	pe.svc = push.New(push.Config{
		Store: store, Sender: pe.sender, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		TenantOrigin: func(id string) string { return "https://" + id + "." + testDomain },
	})
	ctx, cancel := context.WithCancel(context.Background())
	pe.svc.Start(ctx)
	t.Cleanup(func() { pe.svc.Wait(); cancel() })
	b.extra = func(a *App) { a.Push, a.PushStore, a.Workload = pe.svc, store, ver }
	b.rebuild(t, b.res, nil)
	return pe
}

// token mints a workload token; mut edits the claims.
func (pe *pushEnv) token(t *testing.T, key *rsa.PrivateKey, jobTenant string, mut func(jwt.MapClaims)) string {
	t.Helper()
	now := time.Now()
	c := jwt.MapClaims{
		"aud": "sanholo.cloud", "iat": now.Add(-time.Minute).Unix(), "nbf": now.Add(-time.Minute).Unix(), "exp": now.Add(time.Hour).Unix(),
		"nomad_namespace": "sanholo", "nomad_job_id": "sanholo-tenant-" + jobTenant,
	}
	if mut != nil {
		mut(c)
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, c)
	tok.Header["kid"] = "k1"
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func browserKeys(t *testing.T) (p256dh, auth string) {
	t.Helper()
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	a := make([]byte, 16)
	_, _ = rand.Read(a)
	return base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes()), base64.RawURLEncoding.EncodeToString(a)
}

func subBody(t *testing.T, endpoint string) string {
	p, a := browserKeys(t)
	b, _ := json.Marshal(map[string]any{"endpoint": endpoint, "expirationTime": nil, "keys": map[string]string{"p256dh": p, "auth": a}})
	return string(b)
}

// post sends a JSON POST the way the Bridge's script does.
func post(c *client, host, path, body string, hdr map[string]string) result {
	c.t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Host = host
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", goodCSRF)
	req.Header.Set("Origin", "https://"+host)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	for k, v := range hdr {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	for _, ck := range c.cookies {
		req.AddCookie(&http.Cookie{Name: ck.Name, Value: ck.Value})
	}
	return c.send(req)
}

func (pe *pushEnv) enterWithCSRF(t *testing.T, email string) person {
	t.Helper()
	p := pe.person(t, email)
	pe.enterOK(t, p)
	p.bridge.cookies[bridgeCSRFCookie] = &http.Cookie{Name: bridgeCSRFCookie, Value: goodCSRF}
	return p
}

func (pe *pushEnv) subs(t *testing.T, tenant string) int {
	return rows(t, pe.pool, `SELECT count(*) FROM push_subscriptions WHERE tenant_id = $1`, tenant)
}

func ep(n int) string { return fmt.Sprintf("https://fcm.googleapis.com/fcm/send/dev%03d", n) }

func TestSubscribeOnTheTenantHost(t *testing.T) {
	pe := newPushEnv(t)
	p := pe.enterWithCSRF(t, "ann@example.org")
	before := pe.count()

	r := post(p.bridge, p.host, "/push/subscribe", subBody(t, ep(1)), nil)
	if r.status != 201 {
		t.Fatalf("subscribe: %d %s", r.status, r.body)
	}
	if pe.count() != before {
		t.Fatal("the subscription was forwarded to the Bridge instance")
	}
	var tenant, account, session string
	_ = pe.pool.QueryRow(context.Background(), `SELECT tenant_id, account_id::text, account_session_id::text FROM push_subscriptions WHERE endpoint = $1`, ep(1)).Scan(&tenant, &account, &session)
	var wantAccount string
	_ = pe.pool.QueryRow(context.Background(), `SELECT id::text FROM accounts WHERE tenant_id = $1`, p.tenant).Scan(&wantAccount)
	if tenant != p.tenant || account != wantAccount || session == "" {
		t.Fatalf("stored tenant %q account %q session %q", tenant, account, session)
	}
	// Again with fresh keys: 200, no second row.
	if r := post(p.bridge, p.host, "/push/subscribe", subBody(t, ep(1)), nil); r.status != 200 || pe.subs(t, p.tenant) != 1 {
		t.Fatalf("resubscribe: %d, rows %d", r.status, pe.subs(t, p.tenant))
	}
	// Other paths on the tenant host still reach the Bridge.
	if r := p.bridge.get("/push/other", nil); r.status != 200 || pe.count() != before+1 {
		t.Fatalf("/push/other: %d, upstream requests %d", r.status, pe.count()-before)
	}
	// Unsubscribe removes it (the Bridge's reply replaced the CSRF cookie in the jar).
	p.bridge.cookies[bridgeCSRFCookie] = &http.Cookie{Name: bridgeCSRFCookie, Value: goodCSRF}
	if r := post(p.bridge, p.host, "/push/unsubscribe", `{"endpoint":"`+ep(1)+`"}`, nil); r.status != 200 || pe.subs(t, p.tenant) != 0 {
		t.Fatalf("unsubscribe: %d, rows %d", r.status, pe.subs(t, p.tenant))
	}
}

func TestSubscribeRefusals(t *testing.T) {
	pe := newPushEnv(t)
	p := pe.enterWithCSRF(t, "ann@example.org")
	before := pe.count()
	good := subBody(t, ep(7))

	cases := []struct {
		name string
		do   func() result
		want int
	}{
		{"no CSRF header", func() result {
			return post(p.bridge, p.host, "/push/subscribe", good, map[string]string{"X-CSRF-Token": ""})
		}, 403},
		{"wrong CSRF header", func() result {
			return post(p.bridge, p.host, "/push/subscribe", good, map[string]string{"X-CSRF-Token": strings.Repeat("a", 64)})
		}, 403},
		{"CSRF in a form field only", func() result {
			return post(p.bridge, p.host, "/push/subscribe", good, map[string]string{"X-CSRF-Token": "", "Content-Type": "application/x-www-form-urlencoded"})
		}, 403},
		{"foreign origin", func() result {
			return post(p.bridge, p.host, "/push/subscribe", good, map[string]string{"Origin": "https://evil.example"})
		}, 403},
		{"app origin", func() result {
			return post(p.bridge, p.host, "/push/subscribe", good, map[string]string{"Origin": appOrigin})
		}, 403},
		{"cross-site fetch", func() result {
			return post(p.bridge, p.host, "/push/subscribe", good, map[string]string{"Sec-Fetch-Site": "cross-site"})
		}, 403},
		{"not JSON", func() result {
			return post(p.bridge, p.host, "/push/subscribe", good, map[string]string{"Content-Type": "text/plain"})
		}, 415},
		{"garbage body", func() result { return post(p.bridge, p.host, "/push/subscribe", "nope", nil) }, 400},
		{"http endpoint", func() result {
			return post(p.bridge, p.host, "/push/subscribe", subBody(t, "http://fcm.googleapis.com/x"), nil)
		}, 400},
		{"loopback endpoint", func() result {
			return post(p.bridge, p.host, "/push/subscribe", subBody(t, "https://127.0.0.1/x"), nil)
		}, 400},
		{"metadata endpoint", func() result {
			return post(p.bridge, p.host, "/push/subscribe", subBody(t, "https://169.254.169.254/latest"), nil)
		}, 400},
		{"private endpoint", func() result {
			return post(p.bridge, p.host, "/push/subscribe", subBody(t, "https://10.0.0.8:443/x"), nil)
		}, 400},
		{"localhost endpoint", func() result {
			return post(p.bridge, p.host, "/push/subscribe", subBody(t, "https://localhost/x"), nil)
		}, 400},
		{"internal name", func() result {
			return post(p.bridge, p.host, "/push/subscribe", subBody(t, "https://nomad.service.internal/x"), nil)
		}, 400},
		{"bad keys", func() result {
			return post(p.bridge, p.host, "/push/subscribe", `{"endpoint":"`+ep(8)+`","keys":{"p256dh":"AAAA","auth":"AAAA"}}`, nil)
		}, 400},
		{"too large", func() result {
			return post(p.bridge, p.host, "/push/subscribe", `{"endpoint":"`+ep(9)+`","pad":"`+strings.Repeat("x", pushBodyMax)+`"}`, nil)
		}, 413},
		{"GET", func() result { return p.bridge.get("/push/subscribe", nil) }, 405},
	}
	for _, tc := range cases {
		if r := tc.do(); r.status != tc.want {
			t.Errorf("%s: %d %s, want %d", tc.name, r.status, r.body, tc.want)
		}
	}
	if pe.subs(t, p.tenant) != 0 {
		t.Fatalf("a refused request stored %d subscriptions", pe.subs(t, p.tenant))
	}
	if pe.count() != before {
		t.Fatal("a refused push request reached the Bridge instance")
	}

	// No Bridge session: 401, not a redirect to sign-in.
	anon := pe.client(t)
	anon.host = p.host
	anon.cookies[bridgeCSRFCookie] = &http.Cookie{Name: bridgeCSRFCookie, Value: goodCSRF}
	if r := post(anon, p.host, "/push/subscribe", good, nil); r.status != 401 || r.header.Get("Location") != "" {
		t.Errorf("no session: %d location %q", r.status, r.header.Get("Location"))
	}
	if pe.subs(t, p.tenant) != 0 {
		t.Fatal("an unauthenticated subscription was stored")
	}
}

func TestSubscribeFromAnotherTenantsSession(t *testing.T) {
	pe := newPushEnv(t)
	ann := pe.enterWithCSRF(t, "ann@example.org")
	bob := pe.enterWithCSRF(t, "bob@example.org")

	// Ann's cookie presented on Bob's tenant host: the same 404 the proxy gives.
	ann.bridge.host = bob.host
	if r := post(ann.bridge, bob.host, "/push/subscribe", subBody(t, ep(1)), nil); r.status != 404 {
		t.Fatalf("subscribe with another tenant's session: %d", r.status)
	}
	if pe.subs(t, bob.tenant) != 0 || pe.subs(t, ann.tenant) != 0 {
		t.Fatal("a subscription was stored through another tenant's session")
	}
	// The same for unsubscribing Bob's endpoint.
	if r := post(bob.bridge, bob.host, "/push/subscribe", subBody(t, ep(2)), nil); r.status != 201 {
		t.Fatal(r.status)
	}
	if r := post(ann.bridge, bob.host, "/push/unsubscribe", `{"endpoint":"`+ep(2)+`"}`, nil); r.status != 404 || pe.subs(t, bob.tenant) != 1 {
		t.Fatalf("cross-tenant unsubscribe: %d, rows %d", r.status, pe.subs(t, bob.tenant))
	}
	// Ann cannot claim Bob's endpoint on her own host either, and cannot remove it.
	ann.bridge.host = ann.host
	if r := post(ann.bridge, ann.host, "/push/subscribe", subBody(t, ep(2)), nil); r.status != 409 {
		t.Fatalf("taking another tenant's endpoint: %d", r.status)
	}
	if r := post(ann.bridge, ann.host, "/push/unsubscribe", `{"endpoint":"`+ep(2)+`"}`, nil); r.status != 200 || pe.subs(t, bob.tenant) != 1 {
		t.Fatalf("unsubscribe of another tenant's endpoint: %d, rows %d", r.status, pe.subs(t, bob.tenant))
	}
	// A suspended tenant's session is the 404 as well.
	_, _ = pe.pool.Exec(context.Background(), `UPDATE tenants SET status = 'deleting' WHERE id = $1`, bob.tenant)
	if r := post(bob.bridge, bob.host, "/push/subscribe", subBody(t, ep(3)), nil); r.status != 404 {
		t.Fatalf("deleting tenant: %d", r.status)
	}
}

func TestSubscriptionCapPerTenant(t *testing.T) {
	pe := newPushEnv(t)
	p := pe.enterWithCSRF(t, "ann@example.org")
	for i := range push.MaxPerTenant {
		if r := post(p.bridge, p.host, "/push/subscribe", subBody(t, ep(i)), nil); r.status != 201 {
			t.Fatalf("subscription %d: %d %s", i+1, r.status, r.body)
		}
	}
	if r := post(p.bridge, p.host, "/push/subscribe", subBody(t, ep(50)), nil); r.status != 409 {
		t.Fatalf("11th: %d", r.status)
	}
	if pe.subs(t, p.tenant) != push.MaxPerTenant {
		t.Fatalf("rows = %d", pe.subs(t, p.tenant))
	}
	if r := post(p.bridge, p.host, "/push/subscribe", subBody(t, ep(0)), nil); r.status != 200 {
		t.Fatalf("an existing browser at the cap: %d", r.status)
	}
}

func alertBody(kind, text string) string {
	b, _ := json.Marshal(map[string]string{"kind": kind, "severity": "warning", "text": text, "key": "job-1", "at": "2026-10-27T12:00:00Z", "link": "https://elsewhere.example/?job=j1"})
	return string(b)
}

func postAlert(pe *pushEnv, token, body string, hdr map[string]string) result {
	c := pe.client(pe.t)
	c.host = appHost
	h := map[string]string{"Authorization": "Bearer " + token}
	if token == "" {
		delete(h, "Authorization")
	}
	for k, v := range hdr {
		h[k] = v
	}
	req := httptest.NewRequest("POST", "/internal/alerts", strings.NewReader(body))
	req.Host = appHost
	req.Header.Set("Content-Type", "application/json")
	for k, v := range h {
		req.Header.Set(k, v)
	}
	return c.send(req)
}

func TestAlertsNeedAValidNomadIdentity(t *testing.T) {
	pe := newPushEnv(t)
	ann := pe.enterWithCSRF(t, "ann@example.org")
	bob := pe.enterWithCSRF(t, "bob@example.org")
	post(ann.bridge, ann.host, "/push/subscribe", subBody(t, ep(1)), nil)
	post(bob.bridge, bob.host, "/push/subscribe", subBody(t, ep(2)), nil)

	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	body := alertBody("needs-help", "Help me")
	forged := map[string]string{
		"no header":           "",
		"wrong key":           pe.token(t, other, ann.tenant, nil),
		"wrong audience":      pe.token(t, pe.signer, ann.tenant, func(c jwt.MapClaims) { c["aud"] = "qm.sanholo" }),
		"expired":             pe.token(t, pe.signer, ann.tenant, func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-time.Hour).Unix() }),
		"other namespace":     pe.token(t, pe.signer, ann.tenant, func(c jwt.MapClaims) { c["nomad_namespace"] = "default" }),
		"job is not a tenant": pe.token(t, pe.signer, ann.tenant, func(c jwt.MapClaims) { c["nomad_job_id"] = "batch-report" }),
		"garbage":             "not.a.jwt",
	}
	for name, tok := range forged {
		r := postAlert(pe, tok, body, map[string]string{"X-SanHolo-Tenant": ann.tenant})
		if r.status != 401 {
			t.Errorf("%s: %d %s, want 401", name, r.status, r.body)
		}
		if strings.Contains(r.body, "signature") || strings.Contains(r.body, "audience") {
			t.Errorf("%s: the answer explains the failure: %s", name, r.body)
		}
	}
	pe.svc.Wait()
	if n := len(pe.sender.endpoints()); n != 0 {
		t.Fatalf("%d pushes from forged tokens", n)
	}

	// A valid token for Ann with a header naming Bob: Ann's browser only.
	r := postAlert(pe, pe.token(t, pe.signer, ann.tenant, nil), body, map[string]string{"X-SanHolo-Tenant": bob.tenant})
	if r.status != 202 {
		t.Fatalf("valid alert: %d %s", r.status, r.body)
	}
	pe.svc.Wait()
	if got := pe.sender.endpoints(); len(got) != 1 || got[0] != ep(1) {
		t.Fatalf("pushed to %v, want only %s (tenant from the token, not the header)", got, ep(1))
	}
	pe.sender.mu.Lock()
	p := pe.sender.sent[0].p
	pe.sender.mu.Unlock()
	if p.Link != "https://"+ann.tenant+"."+testDomain+"/?job=j1" || p.Kind != "needs-help" || p.Body != "Help me" {
		t.Fatalf("payload = %+v", p)
	}

	// A token for a tenant that does not exist, or is being deleted: nothing.
	if r := postAlert(pe, pe.token(t, pe.signer, "ghost", nil), body, nil); r.status != 403 {
		t.Errorf("unknown tenant: %d", r.status)
	}
	_, _ = pe.pool.Exec(context.Background(), `UPDATE tenants SET status = 'deleting' WHERE id = $1`, bob.tenant)
	if r := postAlert(pe, pe.token(t, pe.signer, bob.tenant, nil), body, nil); r.status != 403 {
		t.Errorf("deleting tenant: %d", r.status)
	}
	pe.svc.Wait()
	if n := len(pe.sender.endpoints()); n != 1 {
		t.Fatalf("pushes = %d, want 1", n)
	}
}

func TestAlertBodyRules(t *testing.T) {
	pe := newPushEnv(t)
	ann := pe.enterWithCSRF(t, "ann@example.org")
	post(ann.bridge, ann.host, "/push/subscribe", subBody(t, ep(1)), nil)
	tok := pe.token(t, pe.signer, ann.tenant, nil)
	cases := map[string]struct {
		body string
		want int
	}{
		"unknown kind": {alertBody("format-disk", "x"), 400},
		"281 runes":    {alertBody("test", strings.Repeat("é", push.MaxTextRunes+1)), 400},
		"not json":     {"kind=test", 400},
		"empty":        {"", 400},
		"oversize":     {`{"kind":"test","severity":"info","text":"x","p":"` + strings.Repeat("p", push.MaxBody) + `"}`, 413},
		"ok":           {alertBody("test", strings.Repeat("é", push.MaxTextRunes)), 202},
	}
	for name, tc := range cases {
		if r := postAlert(pe, tok, tc.body, nil); r.status != tc.want {
			t.Errorf("%s: %d %s, want %d", name, r.status, r.body, tc.want)
		}
	}
	pe.svc.Wait()
	if n := len(pe.sender.endpoints()); n != 1 {
		t.Fatalf("pushes = %d, want 1 (only the valid alert)", n)
	}
	// A garbage body with a bad token is a 401: authentication comes first.
	if r := postAlert(pe, "x.y.z", "", nil); r.status != 401 {
		t.Errorf("bad token and empty body: %d", r.status)
	}
	// The endpoint is not on the tenant host: there it is the Bridge's path.
	c := pe.client(t)
	c.host = ann.host
	req := httptest.NewRequest("POST", "/internal/alerts", strings.NewReader(alertBody("test", "x")))
	req.Host = ann.host
	req.Header.Set("Authorization", "Bearer "+tok)
	if r := c.send(req); r.status == 202 {
		t.Error("/internal/alerts answered on a tenant host")
	}
}

func TestAlertIntakeDegradesCleanly(t *testing.T) {
	pe := newPushEnv(t)
	ann := pe.enterWithCSRF(t, "ann@example.org")
	tok := pe.token(t, pe.signer, ann.tenant, nil)

	// Nomad's keys unreachable: 503, so the bot knows it is not a bad token.
	pe.jwksUp(false)
	if r := postAlert(pe, tok, alertBody("test", "x"), nil); r.status != 503 {
		t.Errorf("JWKS down: %d", r.status)
	}
	pe.jwksUp(true)
	time.Sleep(20 * time.Millisecond) // past the verifier's (test) refresh back-off
	if r := postAlert(pe, tok, alertBody("test", "x"), nil); r.status != 202 {
		t.Errorf("JWKS back: %d", r.status)
	}

	// Not configured at all.
	b := newBridgeEnv(t)
	c := b.client(t)
	c.host = appHost
	req := httptest.NewRequest("POST", "/internal/alerts", strings.NewReader("{}"))
	req.Host = appHost
	req.Header.Set("Authorization", "Bearer x")
	if r := c.send(req); r.status != 503 {
		t.Errorf("unconfigured: %d", r.status)
	}
	if r := c.get("/internal/alerts", nil); r.status == 202 || r.status == 200 {
		t.Errorf("GET /internal/alerts: %d", r.status)
	}
}

func TestBridgeCSRFTokenShape(t *testing.T) {
	pe := newPushEnv(t)
	p := pe.enterWithCSRF(t, "ann@example.org")
	// The cookie must be the Bridge's 64 hex characters, and equal the header.
	for name, v := range map[string]string{"short": "bridge-csrf", "uppercase non-hex": strings.Repeat("Z", 64), "63": goodCSRF[:63], "62 hex": goodCSRF[:62]} {
		p.bridge.cookies[bridgeCSRFCookie] = &http.Cookie{Name: bridgeCSRFCookie, Value: v}
		if r := post(p.bridge, p.host, "/push/subscribe", subBody(t, ep(1)), map[string]string{"X-CSRF-Token": v}); r.status != 403 {
			t.Errorf("%s: %d", name, r.status)
		}
	}
	if _, err := hex.DecodeString(goodCSRF); err != nil {
		t.Fatal(err)
	}
}
