# Notes from the client (TydeWhatMay/san-holo Phase 86) for the cloud side

Recorded 2026-10-24 while building the client's hosted mode. Each is a
requirement or open point the control plane must cover.

- Identity: the instance accepts only requests carrying `X-SanHolo-Identity`
  (Ed25519; payload base64url JSON `{tenant, aud, exp}`, signature over
  `"sanholo-identity/v1." + payload`; exp at most 5 minutes ahead; two public
  keys accepted for rotation; `/healthz` exempt). The proxy must sign per
  request and strip any client-supplied copy of the header. A hosted instance
  without its verifier answers 503 to everything (fails closed).
- CSRF: the instance sets `__Host-sanholo_csrf` and wants `X-CSRF-Token` on
  every state-changing request; the proxy must pass both through unchanged.
- Alerts from the instance arrive at `-hosted-alert-url` as JSON
  `{kind, severity, text, key, at, link}` with header `X-SanHolo-Tenant`. The
  header is only a claim (the instance has no signing key): the control plane
  must check it against the network path the request came from (per-tenant
  network identity) before trusting it.
- Push subscriptions: the Bridge POSTs the browser's subscription to
  `/push/subscribe` on its own origin; the proxy must intercept that path (the
  instance answers 404 for it). VAPID public key goes to the instance as
  `-hosted-vapid-public-key`.
- Service worker push payload expected: `{title, body, link, key, kind}`.
- iOS ignores SVG manifest icons: serve a PNG apple-touch-icon (proxy or a
  client follow-up).
- Pass `-public-url https://<tenant-id>.sanholo.space` to each instance, or
  alerts carry no link.
- Alert text is cut to 280 characters and scrubbed of the game token, but
  other players' coordinates in game text are not removed: decide whether the
  push body should be reduced further.
- `/healthz` answers `503 starting` until the first poll on a fresh instance:
  give the Nomad check a grace period (startup itself is about 1 s).
- Credentials: tenant from `NOMAD_JOB_ID` = `sanholo-tenant-<id>`; identity
  file `${NOMAD_SECRETS_DIR}/nomad_qm.sanholo.jwt`; qm wire shapes live in the
  client's `internal/qmcreds/wire.go` until qm's integration contract lands.
- A second replicant created from the Bridge: its vessel is not reserved
  automatically; the README tells the player to reserve it.
