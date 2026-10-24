// Package server is the SanHolo Hosted control plane's HTTP surface: sign-up
// and sign-in, the account pages, the authenticating proxy to each tenant's
// Bridge, and the internal endpoints tenant instances call. It starts with
// the health endpoint only; each story in prds/ adds its routes here.
package server

import (
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/recover"
)

// New returns the control plane's Fiber app.
func New() *fiber.App {
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
	return app
}
