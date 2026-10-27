package server

import (
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/lhaig/sanholo-cloud/internal/auth"
	"github.com/lhaig/sanholo-cloud/internal/push"
	"github.com/lhaig/sanholo-cloud/internal/workload"
)

// Web Push on the control plane (US-016).
//
// /push/subscribe and /push/unsubscribe are answered on a tenant's own host,
// by the control plane instead of the Bridge: the instance has no such route,
// and the subscription belongs to the control plane. They need the Bridge
// session of that tenant (a session of another tenant is the same 404 as any
// other path) and the Bridge's own double-submit CSRF check, because the
// Bridge's script sends its token and this handler answers in its place.
//
// POST /internal/alerts is on the app host. Tenants already reach the app
// origin (TENANT_ALERT_URL defaults to it), a second listener would need its
// own job argument, edge and firewall rule for no gain, and the endpoint
// accepts nothing without a valid Nomad workload token. The edge may still
// restrict /internal/ to the platform network as a second layer.

const (
	pushSubscribePath   = "/push/subscribe"
	pushUnsubscribePath = "/push/unsubscribe"
	// pushBodyMax is the largest subscription body read (a real one is about 400 bytes).
	pushBodyMax = 4 << 10
	// pushRateLimit is subscribe and unsubscribe calls per tenant per hour.
	pushRateLimit = 60
	// alertIntakeLimit is alert posts accepted per tenant per hour. The bot
	// caps itself far below; this bounds a runaway or hostile one before the
	// 30 pushes an hour cap even applies.
	alertIntakeLimit = 300
	// bridgeCSRFCookie and bridgeCSRFHeader are the Bridge's own scheme.
	bridgeCSRFCookie = "__Host-sanholo_csrf"
	bridgeCSRFHeader = "X-CSRF-Token"
)

func isPushPath(p string) bool { return p == pushSubscribePath || p == pushUnsubscribePath }

func jsonStatus(c *fiber.Ctx, status int, msg string) error {
	c.Set("Cache-Control", "no-store")
	return c.Status(status).JSON(fiber.Map{"error": msg})
}

// validBridgeCSRF is the Bridge's check: a 64 hex character token in the
// __Host-sanholo_csrf cookie, repeated in X-CSRF-Token.
func validBridgeCSRF(c *fiber.Ctx) bool {
	cookie := c.Cookies(bridgeCSRFCookie)
	if len(cookie) != 64 {
		return false
	}
	if _, err := hex.DecodeString(cookie); err != nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(c.Get(bridgeCSRFHeader)), []byte(cookie)) == 1
}

// bridgePush answers /push/subscribe and /push/unsubscribe for a tenant whose
// Bridge session has already been checked.
func (a *App) bridgePush(c *fiber.Ctx, tenantID string, bs auth.BridgeSession) error {
	if c.Method() != fiber.MethodPost {
		c.Set("Allow", fiber.MethodPost)
		return jsonStatus(c, fiber.StatusMethodNotAllowed, "method not allowed")
	}
	if a.PushStore == nil {
		return jsonStatus(c, fiber.StatusServiceUnavailable, "notifications are not available")
	}
	if !validBridgeCSRF(c) {
		return jsonStatus(c, fiber.StatusForbidden, "forbidden")
	}
	if o := c.Get("Origin"); o != "" && o != a.bridges.origin(tenantID) {
		return jsonStatus(c, fiber.StatusForbidden, "forbidden")
	}
	if s := c.Get("Sec-Fetch-Site"); s != "" && s != "same-origin" {
		return jsonStatus(c, fiber.StatusForbidden, "forbidden")
	}
	if !strings.HasPrefix(strings.ToLower(c.Get("Content-Type")), "application/json") {
		return jsonStatus(c, fiber.StatusUnsupportedMediaType, "send JSON")
	}
	body := c.Body()
	if len(body) > pushBodyMax {
		return jsonStatus(c, fiber.StatusRequestEntityTooLarge, "too large")
	}
	var sub push.Subscription
	if err := json.Unmarshal(body, &sub); err != nil {
		return jsonStatus(c, fiber.StatusBadRequest, "not a subscription")
	}
	ok, err := a.allow(c, "push:tenant:"+tenantID, pushRateLimit)
	if err != nil {
		return jsonStatus(c, fiber.StatusInternalServerError, "something went wrong")
	}
	if !ok {
		c.Set("Retry-After", "3600")
		return jsonStatus(c, fiber.StatusTooManyRequests, "too many requests")
	}

	if c.Path() == pushUnsubscribePath {
		// Only the endpoint matters; the keys need not be present.
		if err := push.ValidateEndpoint(sub.Endpoint); err != nil {
			return jsonStatus(c, fiber.StatusBadRequest, "not a subscription")
		}
		if err := a.PushStore.Remove(c.UserContext(), tenantID, sub.Endpoint); err != nil {
			return jsonStatus(c, fiber.StatusInternalServerError, "something went wrong")
		}
		return c.JSON(fiber.Map{"ok": true})
	}
	if err := sub.Validate(); err != nil {
		return jsonStatus(c, fiber.StatusBadRequest, "not a valid subscription")
	}
	created, err := a.PushStore.Add(c.UserContext(), tenantID, bs.AccountID, bs.AccountSessionID, sub)
	switch {
	case errors.Is(err, push.ErrCapReached):
		return jsonStatus(c, fiber.StatusConflict, "too many devices: remove one first")
	case errors.Is(err, push.ErrEndpointTaken):
		return jsonStatus(c, fiber.StatusConflict, "already registered")
	case err != nil:
		return jsonStatus(c, fiber.StatusInternalServerError, "something went wrong")
	}
	c.Set("Cache-Control", "no-store")
	if created {
		return c.Status(fiber.StatusCreated).JSON(fiber.Map{"ok": true})
	}
	return c.JSON(fiber.Map{"ok": true})
}

// alertIntake is POST /internal/alerts: a bot's alert, authenticated by its
// Nomad workload identity. The tenant is the verified job id; the
// X-SanHolo-Tenant header is never consulted.
func (a *App) alertIntake(c *fiber.Ctx) error {
	c.Set("Cache-Control", "no-store")
	if a.Workload == nil || a.Push == nil || a.PushStore == nil {
		return jsonStatus(c, fiber.StatusServiceUnavailable, "alerts are not available")
	}
	h := c.Get("Authorization")
	token, ok := strings.CutPrefix(h, "Bearer ")
	if !ok {
		token, ok = strings.CutPrefix(h, "bearer ")
	}
	if !ok {
		c.Set("WWW-Authenticate", "Bearer")
		return jsonStatus(c, fiber.StatusUnauthorized, "unauthorized")
	}
	claims, err := a.Workload.Verify(c.UserContext(), strings.TrimSpace(token))
	switch {
	case errors.Is(err, workload.ErrUnavailable):
		slog.Warn("alerts: Nomad signing keys unavailable")
		return jsonStatus(c, fiber.StatusServiceUnavailable, "try again later")
	case err != nil:
		c.Set("WWW-Authenticate", "Bearer")
		return jsonStatus(c, fiber.StatusUnauthorized, "unauthorized")
	}
	body := c.Body()
	if len(body) > push.MaxBody {
		return jsonStatus(c, fiber.StatusRequestEntityTooLarge, "too large")
	}
	alert, err := push.ParseAlert(body)
	if err != nil {
		return jsonStatus(c, fiber.StatusBadRequest, "not an alert")
	}
	active, err := a.PushStore.TenantActive(c.UserContext(), claims.TenantID)
	if err != nil {
		return jsonStatus(c, fiber.StatusInternalServerError, "something went wrong")
	}
	if !active {
		return jsonStatus(c, fiber.StatusForbidden, "forbidden")
	}
	ok, err = a.Auth.Allow(c.UserContext(), "alerts:tenant:"+claims.TenantID, alertIntakeLimit, time.Hour)
	if err != nil {
		return jsonStatus(c, fiber.StatusInternalServerError, "something went wrong")
	}
	if !ok {
		c.Set("Retry-After", "3600")
		return jsonStatus(c, fiber.StatusTooManyRequests, "too many requests")
	}
	if err := a.Push.Submit(claims.TenantID, alert); err != nil {
		return jsonStatus(c, fiber.StatusServiceUnavailable, "try again later")
	}
	return c.Status(fiber.StatusAccepted).JSON(fiber.Map{"queued": true})
}
