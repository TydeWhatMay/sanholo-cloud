package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/lhaig/sanholo-cloud/internal/config"
)

// The generated pair is one the service accepts, and each run is new.
func TestVAPIDGenerateMakesAPairTheConfigAccepts(t *testing.T) {
	var a, b bytes.Buffer
	if err := vapidGenerate(&a); err != nil {
		t.Fatal(err)
	}
	_ = vapidGenerate(&b)
	if a.String() == b.String() {
		t.Fatal("two runs gave the same pair")
	}
	env := map[string]string{"DATABASE_URL": "postgres://u:p@h/d", "VAPID_SUBJECT": "mailto:ops@example.org"}
	for _, line := range strings.Split(strings.TrimSpace(a.String()), "\n") {
		k, v, _ := strings.Cut(line, "=")
		env[k] = v
	}
	if _, err := config.Load(func(k string) string { return env[k] }); err != nil {
		t.Fatalf("config refuses the generated pair: %v", err)
	}
}
