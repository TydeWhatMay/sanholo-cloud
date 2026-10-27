package main

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
)

// vapidGenerate writes a new VAPID key pair as environment lines. It needs no
// configuration and touches no database: run it once, put the private key in
// qm and the public key in the service's settings.
func vapidGenerate(w io.Writer) error {
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	enc := base64.RawURLEncoding.EncodeToString
	_, err = fmt.Fprintf(w, "VAPID_PUBLIC_KEY=%s\nVAPID_PRIVATE_KEY=%s\n", enc(k.PublicKey().Bytes()), enc(k.Bytes()))
	return err
}
