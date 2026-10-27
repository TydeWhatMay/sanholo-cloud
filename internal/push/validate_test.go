package push_test

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/lhaig/sanholo-cloud/internal/push"
)

// keypair is a browser's subscription keys.
type keypair struct {
	priv *ecdh.PrivateKey
	auth []byte
}

func newKeys(t testing.TB) keypair {
	t.Helper()
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	a := make([]byte, 16)
	_, _ = rand.Read(a)
	return keypair{priv: k, auth: a}
}

func (k keypair) sub(endpoint string) push.Subscription {
	var s push.Subscription
	s.Endpoint = endpoint
	s.Keys.P256dh = base64.RawURLEncoding.EncodeToString(k.priv.PublicKey().Bytes())
	s.Keys.Auth = base64.RawURLEncoding.EncodeToString(k.auth)
	return s
}

const goodEndpoint = "https://fcm.googleapis.com/fcm/send/abc123"

func TestValidateEndpoint(t *testing.T) {
	ok := []string{
		goodEndpoint,
		"https://updates.push.services.mozilla.com/wpush/v2/gAAAA",
		"https://web.push.apple.com/QGxyz",
		"https://wns2-par02p.notify.windows.com/w/?token=abc",
		"https://fcm.googleapis.com:443/fcm/send/x",
	}
	bad := map[string]string{
		"http":                "http://fcm.googleapis.com/x",
		"empty":               "",
		"ftp":                 "ftp://fcm.googleapis.com/x",
		"ipv4 loopback":       "https://127.0.0.1/x",
		"ipv4 private":        "https://10.0.0.5/x",
		"metadata":            "https://169.254.169.254/latest/meta-data",
		"ipv6 loopback":       "https://[::1]/x",
		"ipv6 mapped":         "https://[::ffff:127.0.0.1]/x",
		"decimal ip":          "https://2130706433/x",
		"shorthand ip":        "https://127.1/x",
		"localhost":           "https://localhost/x",
		"sub of localhost":    "https://push.localhost/x",
		"single label":        "https://intranet/x",
		"internal suffix":     "https://metadata.google.internal/x",
		"local suffix":        "https://printer.local/x",
		"other port":          "https://fcm.googleapis.com:8443/x",
		"userinfo":            "https://user:pw@fcm.googleapis.com/x",
		"userinfo trick":      "https://fcm.googleapis.com@evil.example/x",
		"fragment":            "https://fcm.googleapis.com/x#y",
		"space":               "https://fcm.googleapis.com/x y",
		"newline":             "https://fcm.googleapis.com/x\nHost: evil",
		"underscore host":     "https://a_b.example.com/x",
		"unicode host":        "https://fçm.example.com/x",
		"empty label":         "https://a..example.com/x",
		"too long":            "https://fcm.googleapis.com/" + strings.Repeat("a", push.MaxEndpointLen),
		"just under, no host": "https:///x",
	}
	for _, e := range ok {
		if err := push.ValidateEndpoint(e); err != nil {
			t.Errorf("%q refused: %v", e, err)
		}
	}
	for name, e := range bad {
		if err := push.ValidateEndpoint(e); err == nil {
			t.Errorf("%s: %q accepted", name, e)
		}
	}
	// The longest allowed endpoint is accepted (boundary).
	edge := "https://fcm.googleapis.com/" + strings.Repeat("a", push.MaxEndpointLen-len("https://fcm.googleapis.com/"))
	if len(edge) != push.MaxEndpointLen || push.ValidateEndpoint(edge) != nil {
		t.Errorf("an endpoint of exactly %d bytes was refused", push.MaxEndpointLen)
	}
}

func TestValidateKeys(t *testing.T) {
	k := newKeys(t)
	if err := k.sub(goodEndpoint).Validate(); err != nil {
		t.Fatalf("good subscription: %v", err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	pub := k.priv.PublicKey().Bytes()
	offCurve := append([]byte{4}, make([]byte, 64)...)
	cases := map[string]func(s *push.Subscription){
		"p256dh empty":        func(s *push.Subscription) { s.Keys.P256dh = "" },
		"p256dh short":        func(s *push.Subscription) { s.Keys.P256dh = enc(pub[:64]) },
		"p256dh long":         func(s *push.Subscription) { s.Keys.P256dh = enc(append(pub[:len(pub):len(pub)], 0)) },
		"p256dh off curve":    func(s *push.Subscription) { s.Keys.P256dh = enc(offCurve) },
		"p256dh not base64":   func(s *push.Subscription) { s.Keys.P256dh = "!!!" },
		"p256dh padded":       func(s *push.Subscription) { s.Keys.P256dh += "=" },
		"auth empty":          func(s *push.Subscription) { s.Keys.Auth = "" },
		"auth 15 bytes":       func(s *push.Subscription) { s.Keys.Auth = enc(k.auth[:15]) },
		"auth 17 bytes":       func(s *push.Subscription) { s.Keys.Auth = enc(append(k.auth[:16:16], 1)) },
		"auth standard b64":   func(s *push.Subscription) { s.Keys.Auth = base64.StdEncoding.EncodeToString(k.auth) },
		"endpoint private ip": func(s *push.Subscription) { s.Endpoint = "https://192.168.1.1/x" },
	}
	for name, mut := range cases {
		s := k.sub(goodEndpoint)
		mut(&s)
		if name == "auth standard b64" && !strings.ContainsAny(s.Keys.Auth, "+/=") {
			s.Keys.Auth += "=" // this auth value happens to be url-safe already; force a non-url character
		}
		if err := s.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestPublicIP(t *testing.T) {
	for ip, want := range map[string]bool{
		"8.8.8.8": true, "142.250.1.1": true, "2a00:1450:4001::1": true,
		"127.0.0.1": false, "10.1.2.3": false, "172.16.0.1": false, "192.168.0.1": false,
		"169.254.169.254": false, "100.64.0.1": false, "0.0.0.0": false, "::": false, "::1": false,
		"fe80::1": false, "fc00::1": false, "224.0.0.1": false, "::ffff:127.0.0.1": false,
		"::ffff:10.0.0.1": false, "198.18.0.1": false, "255.255.255.255": false, "64:ff9b::7f00:1": false,
	} {
		if got := push.PublicIP(netip.MustParseAddr(ip)); got != want {
			t.Errorf("PublicIP(%s) = %v, want %v", ip, got, want)
		}
	}
}

// The sender's client refuses to connect to loopback even when the URL passes
// the string checks (a name that resolves to an internal address).
func TestSafeHTTPClientRefusesInternalAddresses(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	defer srv.Close()
	_, err := push.SafeHTTPClient().Get(srv.URL)
	if err == nil || hit {
		t.Fatalf("connected to a loopback server: err=%v hit=%v", err, hit)
	}
	// And via a name that resolves to loopback.
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	if _, err := push.SafeHTTPClient().Get("http://localhost:" + port); err == nil || hit {
		t.Fatalf("connected through localhost: err=%v hit=%v", err, hit)
	}
}
