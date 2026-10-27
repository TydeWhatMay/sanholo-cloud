// Package deletionlog is the append-only record of account deletions that
// lives outside the database. A restore from backup brings back accounts that
// were deleted after the backup was taken; the log is not part of the backup,
// so `sanholo-cloud deletions reapply` can delete them again (deploy/README.md,
// "After a restore").
//
// An entry holds the tenant id, the account id and the time, nothing else: no
// email address, no hash of one. Both ids are random.
package deletionlog

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// Entry is one line of the log.
type Entry struct {
	At      time.Time `json:"at"`
	Tenant  string    `json:"tenant"`
	Account string    `json:"account"`
}

// Log appends entries to a file.
type Log struct {
	path string
	mu   sync.Mutex
}

// New returns a Log for path. An empty path gives a nil Log, whose Append
// does nothing (development).
func New(path string) *Log {
	if path == "" {
		return nil
	}
	return &Log{path: path}
}

// Enabled reports whether entries are recorded.
func (l *Log) Enabled() bool { return l != nil }

// Append writes e and flushes it to disk before it returns. A deletion
// records its entry first and goes on only when this succeeds, so a deletion
// that happened is always in the log.
func (l *Log) Append(e Entry) error {
	if l == nil {
		return nil
	}
	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("deletionlog: %w", err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("deletionlog: open: %w", err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return fmt.Errorf("deletionlog: write: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("deletionlog: sync: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("deletionlog: close: %w", err)
	}
	return nil
}

// Read returns the entries of the log at path, oldest first. A missing file
// is an empty log. A line that is not an entry is an error: a restore must
// not quietly skip a deletion.
func Read(path string) ([]Entry, error) {
	f, err := os.Open(path) // #nosec G304 -- the operator names the file
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("deletionlog: %w", err)
	}
	defer func() { _ = f.Close() }()
	var out []Entry
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil || e.Tenant == "" {
			return nil, fmt.Errorf("deletionlog: line %d is not an entry", n)
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("deletionlog: %w", err)
	}
	return out, nil
}
