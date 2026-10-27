package deletionlog

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAppendThenReadRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deletions.jsonl")
	l := New(path)
	at := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	want := []Entry{{At: at, Tenant: "t1", Account: "a1"}, {At: at.Add(time.Hour), Tenant: "t2", Account: "a2"}}
	for _, e := range want {
		if err := l.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	got, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Tenant != "t1" || got[1].Account != "a2" || !got[1].At.Equal(want[1].At) {
		t.Errorf("read %+v", got)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", fi.Mode().Perm())
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "@") {
		t.Errorf("the log holds something that looks like an address: %s", raw)
	}
}

func TestAppendIsSafeForConcurrentWriters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.jsonl")
	l := New(path)
	var wg sync.WaitGroup
	for range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := l.Append(Entry{Tenant: "t", Account: "a"}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	got, err := Read(path)
	if err != nil || len(got) != 40 {
		t.Errorf("read %d entries, err %v, want 40", len(got), err)
	}
}

func TestNilLogDoesNothingAndAMissingFileIsEmpty(t *testing.T) {
	if New("") != nil || New("").Enabled() {
		t.Error("an empty path should give no log")
	}
	if err := New("").Append(Entry{Tenant: "t"}); err != nil {
		t.Error(err)
	}
	if got, err := Read(filepath.Join(t.TempDir(), "none")); err != nil || got != nil {
		t.Errorf("missing file: %v %v", got, err)
	}
}

func TestAppendReportsAFailure(t *testing.T) {
	if err := New(filepath.Join(t.TempDir(), "no-such-dir", "x")).Append(Entry{Tenant: "t"}); err == nil {
		t.Error("an unwritable log reported success")
	}
}

func TestReadRefusesALineThatIsNotAnEntry(t *testing.T) {
	for name, content := range map[string]string{
		"garbage":     "not json\n",
		"no tenant":   `{"account":"a"}` + "\n",
		"half a line": `{"tenant":"t","acc`,
	} {
		path := filepath.Join(t.TempDir(), "d")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Read(path); err == nil {
			t.Errorf("%s: a restore would skip a deletion without telling", name)
		}
	}
}
