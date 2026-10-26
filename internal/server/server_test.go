package server

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/lhaig/sanholo-cloud/internal/dbtest"
)

func TestHealthz(t *testing.T) {
	cases := []struct {
		method, path string
		code         int
		body         string
	}{
		{"GET", "/healthz", 200, "ok"},
		{"GET", "/nope", 404, ""},
	}
	for _, tc := range cases {
		resp, err := New(nil).Test(httptest.NewRequest(tc.method, tc.path, nil), -1)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != tc.code || (tc.body != "" && string(b) != tc.body) {
			t.Fatalf("%s %s: %d %q, want %d %q", tc.method, tc.path, resp.StatusCode, b, tc.code, tc.body)
		}
	}
}

// A panicking handler answers 500 instead of ending the process.
func TestRecoversPanics(t *testing.T) {
	app := New(nil)
	app.Get("/boom", func(c *fiber.Ctx) error { panic("boom") })
	resp, err := app.Test(httptest.NewRequest("GET", "/boom", nil), -1)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 500 {
		t.Fatalf("status %d, want 500", resp.StatusCode)
	}
}

type fakePinger struct{ err error }

func (f fakePinger) Ping(context.Context) error { return f.err }

func TestReadyz(t *testing.T) {
	cases := []struct {
		name string
		db   Pinger
		code int
	}{
		{"database answers", fakePinger{}, 200},
		{"database down", fakePinger{errors.New("connection refused")}, 503},
		{"no database configured", nil, 503},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := New(tc.db).Test(httptest.NewRequest("GET", "/readyz", nil), -1)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != tc.code {
				t.Fatalf("status %d, want %d", resp.StatusCode, tc.code)
			}
			// The body must not leak the driver error.
			if strings.Contains(string(b), "refused") {
				t.Fatalf("body leaks error: %q", b)
			}
		})
	}
}

// /healthz stays dependency-free: it answers 200 even when the database is down.
func TestHealthzIgnoresDatabase(t *testing.T) {
	resp, err := New(fakePinger{errors.New("down")}).Test(httptest.NewRequest("GET", "/healthz", nil), -1)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
}

// A real pool: ready while open, not ready once closed.
func TestReadyzRealPool(t *testing.T) {
	pool := dbtest.Pool(t)
	app := New(pool)
	get := func() int {
		resp, err := app.Test(httptest.NewRequest("GET", "/readyz", nil), -1)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if got := get(); got != 200 {
		t.Fatalf("open pool: %d, want 200", got)
	}
	pool.Close()
	if got := get(); got != 503 {
		t.Fatalf("closed pool: %d, want 503", got)
	}
}
