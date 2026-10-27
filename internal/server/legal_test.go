package server

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/lhaig/sanholo-cloud/internal/auth"
)

func TestLegalPages(t *testing.T) {
	e := newEnv(t, auth.Options{})
	c := e.client(t)
	for path, want := range map[string][]string{
		"/privacy": {"data controller", "Hetzner", "AhaSend", "30 days", "hello@tydewhatmay.eu"},
		"/terms":   {"free of charge", "30 days' notice", "third party"},
		"/imprint": {"5 DDG", "Faelmannweg 12A", "14089 Berlin"},
	} {
		r := c.get(path, nil)
		if r.status != 200 {
			t.Fatalf("%s: %d", path, r.status)
		}
		for _, w := range want {
			if !strings.Contains(r.body, w) {
				t.Errorf("%s does not say %q", path, w)
			}
		}
		if csp := r.header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "script-src 'self'") {
			t.Errorf("%s CSP = %q", path, csp)
		}
	}
	// Every page links the three, the waiting list included.
	home := c.get("/", nil)
	for _, href := range []string{`href="/privacy"`, `href="/terms"`, `href="/imprint"`} {
		if !strings.Contains(home.body, href) {
			t.Errorf("home page lacks %s", href)
		}
	}
}

// No template may load anything from another origin: no analytics, no CDN,
// no tracking pixel. The CSP refuses it at run time; this refuses it at
// build time, for every template, including pages no other test renders.
func TestTemplatesLoadNothingFromElsewhere(t *testing.T) {
	external := regexp.MustCompile(`(?i)<(script|link|img|iframe|source|video|audio)\b[^>]*\b(src|href)\s*=\s*["']?\s*(https?:)?//`)
	inline := regexp.MustCompile(`(?i)<script\b[^>]*>\s*[^<\s]`)
	n := 0
	err := fs.WalkDir(templateFiles, "templates", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(templateFiles, path)
		if err != nil {
			return err
		}
		n++
		if m := external.Find(b); m != nil {
			t.Errorf("%s loads from another origin: %s", path, m)
		}
		if m := inline.Find(b); m != nil {
			t.Errorf("%s has an inline script: %s", path, m)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n < 10 {
		t.Fatalf("walked %d templates; the embed pattern changed", n)
	}
}

// The notice states retention; each claim below is one the code keeps (see
// auth.Redeem, auth.Cleanup, mail.Worker.RunOnce, auth.SessionTTL).
func TestPrivacyRetentionMatchesTheCode(t *testing.T) {
	e := newEnv(t, auth.Options{})
	body := e.client(t).get("/privacy", nil).body
	for _, want := range []string{
		"Waiting list entry: until you create an account, or until you ask for it to be removed.",
		"Invites: 14 days after they expire. An invite is valid for 14 days.",
		"Sessions: 30 days after last use.",
		"A session ends 30 days after it was last used",
		"is queued until it is sent, and its text is then erased",
		"no hashes of them",
		"identifier, not by the email address",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("privacy notice lacks %q", want)
		}
	}
	for _, stale := range []string{
		"until you are invited",
		"30 days at most",
		"The code itself is not stored",
		"by a hash of the email address",
		"Unused invite",
	} {
		if strings.Contains(body, stale) {
			t.Errorf("privacy notice still says %q", stale)
		}
	}
	if auth.SessionTTL != 30*24*time.Hour || auth.InviteTTL != 14*24*time.Hour {
		t.Errorf("session TTL %v or invite TTL %v changed: update the privacy notice", auth.SessionTTL, auth.InviteTTL)
	}
}

// A replaced token stays in qm as an older version (qm keeps up to 16), and
// a disconnect can leave a deletion pending; the notice says both.
func TestPrivacyStatesOlderTokenVersionsAndPendingDeletion(t *testing.T) {
	e := newEnv(t, auth.Options{})
	body := e.client(t).get("/privacy", nil).body
	for _, want := range []string{
		"keeps the earlier token as an older version",
		"up to 16 versions",
		"until you disconnect your game account or delete your account",
		"records it and retries until it succeeds",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("privacy notice lacks %q", want)
		}
	}
}
