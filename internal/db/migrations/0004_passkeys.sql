-- Passkeys (WebAuthn). A credential belongs to one account and goes with it.
-- "credential" is the library's JSON record (public key, flags, transports);
-- sign_count is kept beside it so the counter is a plain column that the
-- sign-in path updates and the tests read. Nothing here is a secret: a
-- public key is public, and the private key never leaves the authenticator.

CREATE TABLE passkeys (
    id           bytea PRIMARY KEY,
    account_id   uuid NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    credential   jsonb NOT NULL,
    sign_count   bigint NOT NULL DEFAULT 0,
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz
);
CREATE INDEX passkeys_account_idx ON passkeys (account_id);

-- Ceremony state for one registration or sign-in. The browser holds a random
-- secret in a short-lived __Host- cookie; only its SHA-256 is stored. A row
-- is deleted when read, so a challenge works once, and it expires in 5 minutes.
CREATE TABLE webauthn_challenges (
    id_hash    text PRIMARY KEY,
    kind       text NOT NULL CONSTRAINT webauthn_challenges_kind CHECK (kind IN ('register', 'login')),
    account_id uuid REFERENCES accounts (id) ON DELETE CASCADE,
    data       jsonb NOT NULL,
    expires_at timestamptz NOT NULL
);

-- How the session was opened. /admin accepts only 'passkey'.
ALTER TABLE account_sessions
    ADD COLUMN method text NOT NULL DEFAULT 'magic_link'
    CONSTRAINT account_sessions_method_known CHECK (method IN ('magic_link', 'passkey'));
