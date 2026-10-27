// Package webauthntest is a software authenticator for tests. It answers the
// options JSON the server sends to a browser with the JSON a browser would
// send back, so the server's handlers run against real WebAuthn messages:
// "none" attestation, ES256 keys, user verification set. It is test support
// only; no production code imports it.
package webauthntest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"

	"github.com/fxamacker/cbor/v2"
)

// Authenticator holds one credential. Origin is what the browser puts in
// clientDataJSON; tests set it to another origin to simulate a phishing
// page. Counter is the value the next
// assertion reports; tests may set it to simulate a cloned authenticator.
type Authenticator struct {
	RPID    string
	Origin  string
	ID      []byte
	Counter uint32
	// UserHandle is returned in assertions, as a resident credential does.
	UserHandle []byte
	// NoUV leaves the user-verified flag out of what the authenticator
	// reports, as a key that only checked presence would.
	NoUV bool

	key *ecdsa.PrivateKey
}

// New returns an authenticator for the relying party.
func New(rpID, origin string) *Authenticator {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	id := make([]byte, idLen)
	_, _ = rand.Read(id)
	return &Authenticator{RPID: rpID, Origin: origin, ID: id, key: key}
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

type options struct {
	PublicKey struct {
		Challenge string `json:"challenge"`
		User      struct {
			ID string `json:"id"`
		} `json:"user"`
	} `json:"publicKey"`
}

type coseKey struct {
	Kty int    `cbor:"1,keyasint"`
	Alg int    `cbor:"3,keyasint"`
	Crv int    `cbor:"-1,keyasint"`
	X   []byte `cbor:"-2,keyasint"`
	Y   []byte `cbor:"-3,keyasint"`
}

type attestationObject struct {
	Fmt      string         `cbor:"fmt"`
	AttStmt  map[string]any `cbor:"attStmt"`
	AuthData []byte         `cbor:"authData"`
}

// idLen is the credential id length every Authenticator uses.
const idLen = 32

const (
	flagUP = 0x01
	flagUV = 0x04
	flagAT = 0x40
)

func (a *Authenticator) flags() byte {
	if a.NoUV {
		return flagUP
	}
	return flagUP | flagUV
}

// Create answers the JSON of a registration begin response.
func (a *Authenticator) Create(optionsJSON []byte) ([]byte, error) {
	var o options
	if err := json.Unmarshal(optionsJSON, &o); err != nil {
		return nil, fmt.Errorf("webauthntest: options: %w", err)
	}
	if uh, err := base64.RawURLEncoding.DecodeString(o.PublicKey.User.ID); err == nil {
		a.UserHandle = uh
	}
	cd, _ := json.Marshal(map[string]any{
		"type": "webauthn.create", "challenge": o.PublicKey.Challenge, "origin": a.Origin, "crossOrigin": false,
	})
	pub, err := a.key.PublicKey.ECDH()
	if err != nil {
		return nil, err
	}
	raw := pub.Bytes() // 0x04 || X || Y
	cose, err := cbor.Marshal(coseKey{Kty: 2, Alg: -7, Crv: 1, X: raw[1:33], Y: raw[33:65]})
	if err != nil {
		return nil, err
	}
	rp := sha256.Sum256([]byte(a.RPID))
	ad := append([]byte{}, rp[:]...)
	ad = append(ad, a.flags()|flagAT)
	ad = binary.BigEndian.AppendUint32(ad, 0)
	ad = append(ad, make([]byte, 16)...) // zero AAGUID
	ad = binary.BigEndian.AppendUint16(ad, idLen)
	ad = append(ad, a.ID...)
	ad = append(ad, cose...)
	ao, err := cbor.Marshal(attestationObject{Fmt: "none", AttStmt: map[string]any{}, AuthData: ad})
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"id": b64(a.ID), "rawId": b64(a.ID), "type": "public-key",
		"response": map[string]any{"attestationObject": b64(ao), "clientDataJSON": b64(cd)},
	})
}

// Get answers the JSON of a sign-in begin response. The counter advances by
// one before it is reported, as a real authenticator's does.
func (a *Authenticator) Get(optionsJSON []byte) ([]byte, error) {
	var o options
	if err := json.Unmarshal(optionsJSON, &o); err != nil {
		return nil, fmt.Errorf("webauthntest: options: %w", err)
	}
	a.Counter++
	cd, _ := json.Marshal(map[string]any{
		"type": "webauthn.get", "challenge": o.PublicKey.Challenge, "origin": a.Origin, "crossOrigin": false,
	})
	rp := sha256.Sum256([]byte(a.RPID))
	ad := append([]byte{}, rp[:]...)
	ad = append(ad, a.flags())
	ad = binary.BigEndian.AppendUint32(ad, a.Counter)
	cdh := sha256.Sum256(cd)
	digest := sha256.Sum256(append(append([]byte{}, ad...), cdh[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, a.key, digest[:])
	if err != nil {
		return nil, err
	}
	resp := map[string]any{"authenticatorData": b64(ad), "clientDataJSON": b64(cd), "signature": b64(sig)}
	if a.UserHandle != nil {
		resp["userHandle"] = b64(a.UserHandle)
	}
	return json.Marshal(map[string]any{
		"id": b64(a.ID), "rawId": b64(a.ID), "type": "public-key", "response": resp,
	})
}
