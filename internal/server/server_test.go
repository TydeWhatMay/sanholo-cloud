package server

import (
	"io"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
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
		resp, err := New().Test(httptest.NewRequest(tc.method, tc.path, nil), -1)
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
	app := New()
	app.Get("/boom", func(c *fiber.Ctx) error { panic("boom") })
	resp, err := app.Test(httptest.NewRequest("GET", "/boom", nil), -1)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 500 {
		t.Fatalf("status %d, want 500", resp.StatusCode)
	}
}
