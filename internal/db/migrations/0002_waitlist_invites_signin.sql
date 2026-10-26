-- Waitlist, invites, magic-link sign-in, sessions, mail outbox and rate
-- counters. Secrets (invite codes, link tokens, session ids) are stored as
-- SHA-256 hex only. None of these tables has a tenant_id column: they hang
-- off accounts (which cascade with the tenant) or are not customer data.

CREATE TABLE waitlist (
    id         bigserial PRIMARY KEY,
    email      text NOT NULL CONSTRAINT waitlist_email_nonempty CHECK (email <> ''),
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX waitlist_email_lower_key ON waitlist (lower(email));

CREATE TABLE invites (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    code_hash  text NOT NULL UNIQUE,
    email      text NOT NULL CONSTRAINT invites_email_nonempty CHECK (email <> ''),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    used_at    timestamptz
);

-- Sign-in looks accounts up by the hash of the lower-cased address, so the
-- request path can store the hash and never the address of a stranger. No
-- index: convert_to is not immutable, and the beta holds a few dozen accounts.

-- One row per POST /signin, whether or not the address has an account.
CREATE TABLE signin_requests (
    id           bigserial PRIMARY KEY,
    email_hash   text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    processed_at timestamptz
);
CREATE INDEX signin_requests_pending_idx ON signin_requests (id) WHERE processed_at IS NULL;

CREATE TABLE magic_links (
    token_hash text PRIMARY KEY,
    account_id uuid NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    used_at    timestamptz
);

CREATE TABLE account_sessions (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    secret_hash  text NOT NULL UNIQUE,
    account_id   uuid NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    user_agent   text NOT NULL DEFAULT ''
);
CREATE INDEX account_sessions_account_idx ON account_sessions (account_id);

CREATE TABLE mail_outbox (
    id              bigserial PRIMARY KEY,
    to_email        text NOT NULL,
    subject         text NOT NULL,
    body            text NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    expires_at      timestamptz,
    attempts        int NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    sent_at         timestamptz,
    failed_at       timestamptz,
    last_error      text NOT NULL DEFAULT ''
);
CREATE INDEX mail_outbox_due_idx ON mail_outbox (next_attempt_at) WHERE sent_at IS NULL AND failed_at IS NULL;

-- Fixed-window counters shared by every instance.
CREATE TABLE rate_counters (
    key          text NOT NULL,
    window_start timestamptz NOT NULL,
    count        int NOT NULL,
    PRIMARY KEY (key, window_start)
);
