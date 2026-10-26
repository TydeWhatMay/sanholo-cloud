// Package server is the SanHolo Hosted control plane's HTTP surface: sign-up
// and sign-in, the account pages, the authenticating proxy to each tenant's
// Bridge, and the internal endpoints tenant instances call. It starts with
// the health endpoint only; each story in prds/ adds its routes here.
package server

import (
	"context"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/recover"
)

// Pinger is the part of the database the readiness check needs; *pgxpool.Pool
// satisfies it.
type Pinger interface {
	Ping(ctx context.Context) error
}

// New returns the control plane's Fiber app. db may be nil, in which case
// /readyz never reports ready.
func New(db Pinger) *fiber.App {
	app := fiber.New(fiber.Config{
		// Values from FormValue/Params alias fasthttp buffers the next
		// request reuses; Immutable copies them (learned on SanHolo).
		Immutable:             true,
		DisableStartupMessage: true,
	})
	// A handler panic must not end the process (Fiber does not recover by
	// default), so recover is first in the chain.
	app.Use(recover.New())
	app.Get("/healthz", func(c *fiber.Ctx) error {
		return c.SendString("ok")
	})
	// /healthz says the process is up and touches nothing else; /readyz says
	// it can serve, which needs the database.
	app.Get("/readyz", func(c *fiber.Ctx) error {
		if db == nil {
			return c.Status(fiber.StatusServiceUnavailable).SendString("not ready")
		}
		ctx, cancel := context.WithTimeout(c.UserContext(), 2*time.Second)
		defer cancel()
		if err := db.Ping(ctx); err != nil {
			// The driver error can name hosts; keep it out of the response.
			return c.Status(fiber.StatusServiceUnavailable).SendString("not ready")
		}
		return c.SendString("ready")
	})
	return app
}
