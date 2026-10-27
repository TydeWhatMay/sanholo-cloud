package game_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lhaig/sanholo-cloud/internal/game"
	"github.com/lhaig/sanholo-cloud/internal/game/gamefake"
)

const tok = "GAMETOKEN-d41d8cd98f00b204"

func newClient(t *testing.T, url string) *game.Client {
	t.Helper()
	c, err := game.New(game.Config{BaseURL: url, Version: "1.2.3", Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestMeReturnsTheEarliestReplicant(t *testing.T) {
	f := gamefake.New(t)
	f.AddAccount(tok,
		gamefake.Replicant{Code: "BBBB2222", Name: "Second", CreatedAt: "2026-02-01T00:00:00Z"},
		gamefake.Replicant{Code: "AAAA1111", Name: "First", CreatedAt: "2026-01-01T00:00:00Z"},
	)
	acct, err := newClient(t, f.URL()).Me(context.Background(), tok)
	if err != nil {
		t.Fatal(err)
	}
	want := game.Account{ID: "replicant:AAAA1111", ReplicantCode: "AAAA1111", ReplicantName: "First"}
	if acct != want {
		t.Fatalf("account = %+v, want %+v", acct, want)
	}
	reqs := f.Requests()
	if len(reqs) != 1 {
		t.Fatalf("%d requests, want exactly one game read", len(reqs))
	}
	r := reqs[0]
	if r.Method != "GET" || r.URL != "/v1/accounts/me" {
		t.Errorf("request = %s %s", r.Method, r.URL)
	}
	if r.Header.Get("Authorization") != "Bearer "+tok {
		t.Error("token not sent as a bearer token")
	}
	if r.Header.Get("User-Agent") != "SanHolo-Cloud/1.2.3" {
		t.Errorf("User-Agent = %q", r.Header.Get("User-Agent"))
	}
	if strings.Contains(r.URL, tok) {
		t.Error("token in the URL")
	}
}

func TestMeErrors(t *testing.T) {
	f := gamefake.New(t)
	f.AddAccount(tok, gamefake.Replicant{Code: "AAAA1111", Name: "First"})
	f.AddAccount("empty-account")
	c := newClient(t, f.URL())
	cases := []struct {
		name   string
		token  string
		status int
		want   error
	}{
		{"unknown token", "nope", 0, game.ErrRejected},
		{"forced 401", tok, 401, game.ErrRejected},
		{"forced 403", tok, 403, game.ErrRejected},
		{"forced 429", tok, 429, game.ErrUnavailable},
		{"forced 503", tok, 503, game.ErrUnavailable},
		{"forced 404", tok, 404, game.ErrProtocol},
		{"no replicant", "empty-account", 0, game.ErrNoReplicant},
		{"empty token", "", 0, game.ErrRejected},
		{"header injection", "abc\r\nX-Evil: 1", 0, game.ErrRejected},
		{"space in token", "abc def", 0, game.ErrRejected},
		{"huge token", strings.Repeat("a", 513), 0, game.ErrRejected},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f.SetStatus(tc.status)
			_, err := c.Me(context.Background(), tc.token)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if tc.token != "" && strings.Contains(err.Error(), tc.token) {
				t.Error("error message contains the token")
			}
		})
	}
	f.SetStatus(0)
}

func TestMeRejectedTokenNeverReachesTheGame(t *testing.T) {
	f := gamefake.New(t)
	c := newClient(t, f.URL())
	_, _ = c.Me(context.Background(), "abc\r\nX-Evil: 1")
	_, _ = c.Me(context.Background(), "")
	if n := len(f.Requests()); n != 0 {
		t.Fatalf("%d requests for malformed tokens", n)
	}
}

func TestMeUnreachable(t *testing.T) {
	f := gamefake.New(t)
	c := newClient(t, f.URL())
	f.Close()
	_, err := c.Me(context.Background(), tok)
	if !errors.Is(err, game.ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "127.0.0.1") || strings.Contains(err.Error(), tok) {
		t.Errorf("error leaks the address or token: %v", err)
	}
}

func TestMeTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)
	c, err := game.New(game.Config{BaseURL: srv.URL, Timeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := c.Me(context.Background(), tok); !errors.Is(err, game.ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > time.Second {
		t.Error("the timeout did not apply")
	}
}

func TestMeDoesNotFollowRedirects(t *testing.T) {
	var hits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	t.Cleanup(other.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	_, err := newClient(t, srv.URL).Me(context.Background(), tok)
	if !errors.Is(err, game.ErrProtocol) {
		t.Fatalf("err = %v", err)
	}
	if hits.Load() != 0 {
		t.Fatal("a redirect was followed")
	}
}

func TestMeBoundsTheBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"replicants":[],"pad":"` + strings.Repeat("x", 2<<20) + `"}`))
	}))
	t.Cleanup(srv.Close)
	if _, err := newClient(t, srv.URL).Me(context.Background(), tok); !errors.Is(err, game.ErrProtocol) {
		t.Fatalf("err = %v, want ErrProtocol for an oversized body", err)
	}
}

func TestMeStopsReadingAnEndlessBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := []byte(strings.Repeat("x", 64<<10))
		for r.Context().Err() == nil {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	c, err := game.New(game.Config{BaseURL: srv.URL, Timeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := c.Me(context.Background(), tok); !errors.Is(err, game.ErrProtocol) {
		t.Fatalf("err = %v, want ErrProtocol: the body must be cut at the bound", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Error("reading took until the timeout")
	}
}

func TestMeRefusesUnsafeReplicantFields(t *testing.T) {
	for name, body := range map[string]string{
		"bad code":     `{"replicants":[{"replicant_code":"../x","name":"n"}]}`,
		"empty name":   `{"replicants":[{"replicant_code":"AAAA1111","name":""}]}`,
		"control name": `{"replicants":[{"replicant_code":"AAAA1111","name":"a\u0000b"}]}`,
		"not json":     `<html>`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
			t.Cleanup(srv.Close)
			if _, err := newClient(t, srv.URL).Me(context.Background(), tok); !errors.Is(err, game.ErrProtocol) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestParseURL(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		ok       bool
	}{
		{"https://api.replicant.space", "https://api.replicant.space", true},
		{"https://api.replicant.space/", "https://api.replicant.space", true},
		{"http://127.0.0.1:9000", "http://127.0.0.1:9000", true},
		{"http://localhost:9000", "http://localhost:9000", true},
		{"http://[::1]:9000", "http://[::1]:9000", true},
		{"http://api.replicant.space", "", false},
		{"ftp://api.replicant.space", "", false},
		{"https://api.replicant.space/v1", "", false},
		{"https://user:pw@api.replicant.space", "", false},
		{"https://api.replicant.space?x=1", "", false},
		{"", "", false},
	} {
		got, err := game.ParseURL(tc.in)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("ParseURL(%q) = %q, %v", tc.in, got, err)
		}
	}
}

func TestNewDefaultsToTheRealGame(t *testing.T) {
	if _, err := game.New(game.Config{}); err != nil {
		t.Fatal(err)
	}
	if _, err := game.New(game.Config{BaseURL: "http://example.org"}); err == nil {
		t.Fatal("plain http to a real host accepted")
	}
}

// The earliest replicant is chosen by time, not by the text of the
// timestamp: the text of RFC 3339 compares wrongly across fractional
// precisions and offsets, and the choice names the game account.
func TestMeChoosesTheEarliestReplicantByTime(t *testing.T) {
	for _, tc := range []struct {
		name string
		reps []gamefake.Replicant
		want string
	}{
		{"fraction after the whole second", []gamefake.Replicant{
			{Code: "AAAA0001", Name: "A", CreatedAt: "2026-01-01T00:00:00.5Z"},
			{Code: "BBBB0002", Name: "B", CreatedAt: "2026-01-01T00:00:00Z"},
		}, "BBBB0002"},
		{"different precisions", []gamefake.Replicant{
			{Code: "AAAA0001", Name: "A", CreatedAt: "2026-01-01T00:00:00.123456789Z"},
			{Code: "BBBB0002", Name: "B", CreatedAt: "2026-01-01T00:00:00.12Z"},
		}, "BBBB0002"},
		{"offset", []gamefake.Replicant{
			{Code: "AAAA0001", Name: "A", CreatedAt: "2026-01-01T00:00:00Z"},
			{Code: "BBBB0002", Name: "B", CreatedAt: "2026-01-01T01:00:00+02:00"},
		}, "BBBB0002"},
		{"same instant, the code decides", []gamefake.Replicant{
			{Code: "BBBB0002", Name: "B", CreatedAt: "2026-01-01T00:00:00Z"},
			{Code: "AAAA0001", Name: "A", CreatedAt: "2026-01-01T02:00:00+02:00"},
		}, "AAAA0001"},
		{"unreadable time sorts last", []gamefake.Replicant{
			{Code: "AAAA0001", Name: "A", CreatedAt: "yesterday"},
			{Code: "ZZZZ0009", Name: "Z", CreatedAt: "2030-01-01T00:00:00Z"},
		}, "ZZZZ0009"},
		{"all unreadable, the code decides", []gamefake.Replicant{
			{Code: "BBBB0002", Name: "B", CreatedAt: ""},
			{Code: "AAAA0001", Name: "A", CreatedAt: "x"},
		}, "AAAA0001"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := gamefake.New(t)
			f.AddAccount(tok, tc.reps...)
			acct, err := newClient(t, f.URL()).Me(context.Background(), tok)
			if err != nil {
				t.Fatal(err)
			}
			if acct.ReplicantCode != tc.want {
				t.Errorf("chose %s, want %s", acct.ReplicantCode, tc.want)
			}
		})
	}
}
