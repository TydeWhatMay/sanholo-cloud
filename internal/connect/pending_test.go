package connect

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestPendingStoreIsBounded(t *testing.T) {
	now := time.Now()
	p := newPendingStore(func() time.Time { return now })
	for i := range pendingMax {
		if _, err := p.put(pendingEntry{accountID: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := p.put(pendingEntry{accountID: "extra"}); !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
	// An account replacing its own entry does not grow the table.
	if _, err := p.put(pendingEntry{accountID: "0"}); err != nil {
		t.Fatalf("replacing an entry was refused: %v", err)
	}
	// Expired entries make room.
	now = now.Add(PendingTTL + time.Second)
	if _, err := p.put(pendingEntry{accountID: "extra"}); err != nil {
		t.Fatal(err)
	}
	if p.len() != 1 {
		t.Fatalf("%d entries after expiry sweep, want 1", p.len())
	}
}

func TestPendingStoreKeysByHash(t *testing.T) {
	p := newPendingStore(time.Now)
	secret, _ := p.put(pendingEntry{accountID: "a", token: "tok"})
	for h := range p.byHash {
		if h == secret {
			t.Fatal("the store is keyed by the cookie secret itself, not its hash")
		}
	}
	if _, ok := p.get(secret, "a"); !ok {
		t.Fatal("entry not found")
	}
}
