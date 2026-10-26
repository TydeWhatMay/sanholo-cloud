package server

import (
	"crypto/subtle"

	"github.com/gofiber/fiber/v2"

	"github.com/lhaig/sanholo-cloud/internal/auth"
)

// csrfCookie is the double-submit cookie. The __Host- prefix makes browsers
// refuse it unless it is Secure, host-only and Path=/, so a sibling
// subdomain (a tenant's Bridge) cannot plant its own value.
const csrfCookie = "__Host-sanholo_csrf"

// csrf implements the double-submit check: every state-changing request must
// echo the cookie's value in the X-CSRF-Token header or the csrf form field.
// An attacking site can make the browser send the cookie but cannot read it
// to copy it into the body. The compare is constant-time.
func csrf() fiber.Handler {
	return func(c *fiber.Ctx) error {
		cookie := c.Cookies(csrfCookie)
		valid := len(cookie) == csrfTokenLen
		token := cookie
		if !valid {
			token = auth.NewSecret()
			c.Cookie(&fiber.Cookie{
				Name: csrfCookie, Value: token, Path: "/",
				Secure: true, HTTPOnly: true, SameSite: fiber.CookieSameSiteLaxMode,
			})
		}
		c.Locals("csrf", token)
		switch c.Method() {
		case fiber.MethodGet, fiber.MethodHead, fiber.MethodOptions:
			return c.Next()
		}
		sent := c.Get("X-CSRF-Token")
		if sent == "" {
			sent = c.FormValue("csrf")
		}
		if !valid || subtle.ConstantTimeCompare([]byte(sent), []byte(cookie)) != 1 {
			return c.Status(fiber.StatusForbidden).SendString("forbidden")
		}
		return c.Next()
	}
}

// csrfTokenLen is the length of a 256-bit base64url token.
const csrfTokenLen = 43
