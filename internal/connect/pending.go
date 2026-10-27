package connect

import (
	"sync"
	"time"

	"github.com/lhaig/sanholo-cloud/internal/auth"
	"github.com/lhaig/sanholo-cloud/internal/game"
)

// PendingTTL is how long a validated token waits for its confirmation.
const PendingTTL = 10 * time.Minute

// pendingMax bounds the table. Validation is rate limited per account and
// per address, so reaching it means abuse; new validations are then refused.
const pendingMax = 1000

type pendingEntry struct {
	accountID string
	tenantID  string
	token     string
	acct      game.Account
	expires   time.Time
}

// pendingStore holds validated, unconfirmed connections in process memory
// only. The token must never reach PostgreSQL, so there is no table for it;
// the cost is that a restart (or a second instance) loses the entry and the
// customer enters the token again. Entries are keyed by the hash of a
// random secret held in the customer's cookie, expire after PendingTTL and
// are bound to the account that created them.
type pendingStore struct {
	mu        sync.Mutex
	now       func() time.Time
	byHash    map[string]*pendingEntry
	byAccount map[string]string // account id -> hash, one entry per account
}

func newPendingStore(now func() time.Time) *pendingStore {
	return &pendingStore{now: now, byHash: map[string]*pendingEntry{}, byAccount: map[string]string{}}
}

// put stores e, replacing the account's earlier entry, and returns the
// secret for the cookie.
func (p *pendingStore) put(e pendingEntry) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	for h, old := range p.byHash {
		if !old.expires.After(now) {
			p.removeLocked(h)
		}
	}
	if h, ok := p.byAccount[e.accountID]; ok {
		p.removeLocked(h)
	}
	if len(p.byHash) >= pendingMax {
		return "", ErrBusy
	}
	secret := auth.NewSecret()
	h := auth.HashSecret(secret)
	e.expires = now.Add(PendingTTL)
	p.byHash[h] = &e
	p.byAccount[e.accountID] = h
	return secret, nil
}

// get returns the live entry for secret that belongs to accountID. An entry
// for another account is not revealed and not removed.
func (p *pendingStore) get(secret, accountID string) (pendingEntry, bool) {
	if secret == "" {
		return pendingEntry{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	h := auth.HashSecret(secret)
	e, ok := p.byHash[h]
	if !ok {
		return pendingEntry{}, false
	}
	if !e.expires.After(p.now()) {
		p.removeLocked(h)
		return pendingEntry{}, false
	}
	if e.accountID != accountID {
		return pendingEntry{}, false
	}
	return *e, true
}

// drop removes the entry for secret if it belongs to accountID.
func (p *pendingStore) drop(secret, accountID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	h := auth.HashSecret(secret)
	if e, ok := p.byHash[h]; ok && e.accountID == accountID {
		p.removeLocked(h)
	}
}

func (p *pendingStore) removeLocked(h string) {
	if e, ok := p.byHash[h]; ok {
		delete(p.byAccount, e.accountID)
		delete(p.byHash, h)
	}
}

func (p *pendingStore) len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.byHash)
}
