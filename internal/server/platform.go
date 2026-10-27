package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"log/slog"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/recover"

	"github.com/lhaig/sanholo-cloud/internal/metrics"
	"github.com/lhaig/sanholo-cloud/internal/opalert"
)

// Platform is what the platform listener (INTERNAL_LISTEN_ADDR) serves to
// Prometheus and Alertmanager (US-023). It is a second Fiber app on its own
// address, which the edge never routes to; each route also needs its own
// bearer token. An empty token or a nil component leaves its route off
// (404).
type Platform struct {
	// MetricsToken guards GET /metrics.
	MetricsToken string
	Metrics      *metrics.Source
	// AlertsToken guards POST /internal/operator-alerts.
	AlertsToken string
	Alerts      *opalert.Intake
}

// NewPlatform returns the platform listener's app.
func NewPlatform(p Platform) *fiber.App {
	app := fiber.New(fiber.Config{
		Immutable:             true,
		DisableStartupMessage: true,
		BodyLimit:             opalert.MaxBody,
	})
	app.Use(recover.New())
	if p.MetricsToken != "" && p.Metrics != nil {
		app.Get("/metrics", requireBearer(p.MetricsToken), func(c *fiber.Ctx) error {
			c.Set("Cache-Control", "no-store")
			c.Set(fiber.HeaderContentType, metrics.ContentType)
			var b strings.Builder
			if err := p.Metrics.Write(c.UserContext(), &b); err != nil {
				return c.SendStatus(fiber.StatusInternalServerError)
			}
			return c.SendString(b.String())
		})
	}
	if p.AlertsToken != "" && p.Alerts != nil {
		app.Post("/internal/operator-alerts", requireBearer(p.AlertsToken), func(c *fiber.Ctx) error {
			n, err := opalert.Parse(c.Body())
			if err != nil {
				return jsonStatus(c, fiber.StatusBadRequest, "not an Alertmanager notification")
			}
			if err := p.Alerts.Handle(n); err != nil {
				// Alertmanager retries a 5xx; what was pushed is remembered.
				slog.Warn("operator alerts: a push could not be queued")
				return jsonStatus(c, fiber.StatusServiceUnavailable, "try again later")
			}
			c.Set("Cache-Control", "no-store")
			return c.JSON(fiber.Map{"ok": true})
		})
	}
	return app
}

// bearerToken returns the token of an "Authorization: Bearer <token>"
// header (the scheme is case-insensitive).
func bearerToken(c *fiber.Ctx) (string, bool) {
	scheme, token, ok := strings.Cut(c.Get(fiber.HeaderAuthorization), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

// requireBearer answers 401 unless the request carries want as its bearer.
// Both sides are hashed first so the comparison takes the same time
// whatever the length of what was sent.
func requireBearer(want string) fiber.Handler {
	wantSum := sha256.Sum256([]byte(want))
	return func(c *fiber.Ctx) error {
		got, ok := bearerToken(c)
		gotSum := sha256.Sum256([]byte(got))
		if !ok || subtle.ConstantTimeCompare(gotSum[:], wantSum[:]) != 1 {
			c.Set("WWW-Authenticate", "Bearer")
			return jsonStatus(c, fiber.StatusUnauthorized, "unauthorized")
		}
		return c.Next()
	}
}
