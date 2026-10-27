package push_test

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"

	"github.com/lhaig/sanholo-cloud/internal/push"
)

// decrypt is an independent RFC 8291 (aes128gcm) receiver, written from the
// RFC: it is what the browser does with the body.
func decrypt(t testing.TB, k keypair, body []byte) []byte {
	t.Helper()
	if len(body) < 86 {
		t.Fatalf("body of %d bytes", len(body))
	}
	salt := body[:16]
	rs := binary.BigEndian.Uint32(body[16:20])
	idlen := int(body[20])
	asPub := body[21 : 21+idlen]
	ct := body[21+idlen:]
	if int(rs) < len(ct) {
		t.Fatalf("record size %d smaller than the ciphertext %d", rs, len(ct))
	}
	peer, err := ecdh.P256().NewPublicKey(asPub)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := k.priv.ECDH(peer)
	if err != nil {
		t.Fatal(err)
	}
	keyInfo := append(append([]byte("WebPush: info\x00"), k.priv.PublicKey().Bytes()...), asPub...)
	ikm, _ := hkdf.Key(sha256.New, secret, k.auth, string(keyInfo), 32)
	cek, _ := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16)
	nonce, _ := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12)
	blk, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(blk)
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		t.Fatalf("the body does not decrypt for the subscriber: %v", err)
	}
	plain = bytes.TrimRight(plain, "\x00")
	if len(plain) == 0 || plain[len(plain)-1] != 0x02 {
		t.Fatalf("no final record delimiter")
	}
	return plain[:len(plain)-1]
}

func vapidKeys(t testing.TB) push.VAPID {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ek, _ := k.ECDH()
	return push.VAPID{
		Private: base64.RawURLEncoding.EncodeToString(ek.Bytes()),
		Public:  base64.RawURLEncoding.EncodeToString(ek.PublicKey().Bytes()),
		Subject: "mailto:ops@example.org",
	}
}

func TestWebPushEncryptsAndSignsPerRFC8291And8292(t *testing.T) {
	keys := newKeys(t)
	v := vapidKeys(t)
	var got struct {
		body []byte
		h    http.Header
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.body, _ = io.ReadAll(r.Body)
		got.h = r.Header.Clone()
		w.WriteHeader(201)
	}))
	defer srv.Close()
	s := push.NewWebPushWithClient(v, srv.Client())
	msg := []byte(`{"title":"SanHolo: asteroid","body":"Rock inbound","link":"https://a.test/","key":"k","kind":"asteroid"}`)
	if err := s.Send(context.Background(), keys.sub(srv.URL+"/push/abc"), msg); err != nil {
		t.Fatal(err)
	}
	if string(decrypt(t, keys, got.body)) != string(msg) {
		t.Fatal("decrypted payload differs")
	}
	if len(got.body) > 4096 {
		t.Fatalf("body is %d bytes; push services accept 4096", len(got.body))
	}
	if got.h.Get("Content-Encoding") != "aes128gcm" || got.h.Get("TTL") == "" {
		t.Fatalf("headers: %v", got.h)
	}
	// RFC 8292: Authorization: vapid t=<jwt>, k=<public key>.
	auth := got.h.Get("Authorization")
	tok, ok := strings.CutPrefix(auth, "vapid t=")
	if !ok {
		t.Fatalf("Authorization = %q", auth)
	}
	tokStr, k, _ := strings.Cut(tok, ", k=")
	if k != v.Public {
		t.Fatalf("k = %q, want the VAPID public key", k)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(v.Public)
	x, y := new(big.Int).SetBytes(raw[1:33]), new(big.Int).SetBytes(raw[33:])
	pub := &ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}
	var claims jwt.MapClaims
	if _, err := jwt.NewParser(jwt.WithValidMethods([]string{"ES256"}), jwt.WithExpirationRequired()).ParseWithClaims(tokStr, &claims, func(*jwt.Token) (any, error) { return pub, nil }); err != nil {
		t.Fatalf("VAPID JWT: %v", err)
	}
	if claims["aud"] != srv.URL || claims["sub"] != "mailto:ops@example.org" {
		t.Fatalf("VAPID claims = %v (want aud %s)", claims, srv.URL)
	}
}

func TestWebPushStatusMapping(t *testing.T) {
	keys := newKeys(t)
	for _, tc := range []struct {
		status int
		retry  string
		check  func(error) bool
	}{
		{201, "", func(e error) bool { return e == nil }},
		{200, "", func(e error) bool { return e == nil }},
		{404, "", func(e error) bool { return errors.Is(e, push.ErrGone) }},
		{410, "", func(e error) bool { return errors.Is(e, push.ErrGone) }},
		{429, "17", func(e error) bool {
			var r *push.RetryError
			return errors.As(e, &r) && r.After.Seconds() == 17
		}},
		{500, "", func(e error) bool { var r *push.RetryError; return errors.As(e, &r) }},
		{503, "", func(e error) bool { var r *push.RetryError; return errors.As(e, &r) }},
		{400, "", func(e error) bool { return errors.Is(e, push.ErrPermanent) }},
		{403, "", func(e error) bool { return errors.Is(e, push.ErrPermanent) }},
		{413, "", func(e error) bool { return errors.Is(e, push.ErrPermanent) }},
	} {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if tc.retry != "" {
				w.Header().Set("Retry-After", tc.retry)
			}
			w.WriteHeader(tc.status)
		}))
		err := push.NewWebPushWithClient(vapidKeys(t), srv.Client()).Send(context.Background(), keys.sub(srv.URL+"/x"), []byte("{}"))
		srv.Close()
		if !tc.check(err) {
			t.Errorf("status %d: err = %v", tc.status, err)
		}
	}
	// A network failure is retryable and its text carries no endpoint.
	err := push.NewWebPushWithClient(vapidKeys(t), &http.Client{}).Send(context.Background(), keys.sub("https://127.0.0.1:1/secret-path"), []byte("{}"))
	var r *push.RetryError
	if !errors.As(err, &r) || strings.Contains(err.Error(), "secret-path") {
		t.Fatalf("network failure: %v", err)
	}
}
