package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/lhaig/sanholo-cloud/internal/mail"
)

// ChallengeTTL is how long a registration or sign-in ceremony may take.
const ChallengeTTL = 5 * time.Minute

var (
	// ErrPasskeysDisabled means no relying party is configured.
	ErrPasskeysDisabled = errors.New("passkeys are not enabled")
	// ErrPasskeyChallenge means the ceremony state is missing, spent,
	// expired, or not bound to this browser and account.
	ErrPasskeyChallenge = errors.New("passkey challenge is missing, used or expired")
	// ErrPasskeyFailed means the authenticator's answer did not verify.
	ErrPasskeyFailed = errors.New("passkey could not be verified")
	// ErrPasskeyStepUp means the account already has a passkey or is an
	// operator, so another passkey may be added only from a session that was
	// itself opened with a passkey. A sign-in link, which anyone with the
	// mailbox holds, must not be able to enrol a key of its own.
	ErrPasskeyStepUp = errors.New("adding a passkey needs a session opened with a passkey")
)

const (
	kindRegister = "register"
	kindLogin    = "login"
)

// NewWebAuthn builds the relying party. Passkeys are discoverable (the
// sign-in page needs no email address) and always require user verification
// (a PIN or biometric), because a passkey also guards /admin.
func NewWebAuthn(rpID, origin, name string) (*webauthn.WebAuthn, error) {
	return webauthn.New(&webauthn.Config{
		RPID:          rpID,
		RPDisplayName: name,
		RPOrigins:     []string{origin},
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey:        protocol.ResidentKeyRequirementRequired,
			RequireResidentKey: protocol.ResidentKeyRequired(),
			UserVerification:   protocol.VerificationRequired,
		},
	})
}

// PasskeysEnabled reports whether passkeys can be used.
func (s *Service) PasskeysEnabled() bool { return s.opt.WebAuthn != nil }

// Passkey is a registered credential as the account page shows it.
type Passkey struct {
	ID         string // base64url of the credential id
	CreatedAt  time.Time
	LastUsedAt *time.Time
}

type passkeyUser struct {
	id    uuid.UUID
	email string
	creds []webauthn.Credential
}

func (u *passkeyUser) WebAuthnID() []byte                         { return u.id[:] }
func (u *passkeyUser) WebAuthnName() string                       { return u.email }
func (u *passkeyUser) WebAuthnDisplayName() string                { return u.email }
func (u *passkeyUser) WebAuthnCredentials() []webauthn.Credential { return u.creds }

func (s *Service) loadUser(ctx context.Context, accountID uuid.UUID) (*passkeyUser, error) {
	u := &passkeyUser{id: accountID}
	err := s.pool.QueryRow(ctx, `SELECT email FROM accounts WHERE id = $1`, accountID).Scan(&u.email)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrPasskeyFailed
	}
	if err != nil {
		return nil, fmt.Errorf("auth: passkey user: %w", err)
	}
	rows, err := s.pool.Query(ctx, `SELECT credential, sign_count FROM passkeys WHERE account_id = $1 ORDER BY created_at`, accountID)
	if err != nil {
		return nil, fmt.Errorf("auth: passkey credentials: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		var count int64
		if err := rows.Scan(&raw, &count); err != nil {
			return nil, fmt.Errorf("auth: passkey credentials: %w", err)
		}
		var c webauthn.Credential
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, fmt.Errorf("auth: passkey credential record: %w", err)
		}
		if count < 0 || count > math.MaxUint32 {
			return nil, fmt.Errorf("auth: passkey sign count %d out of range", count)
		}
		c.Authenticator.SignCount = uint32(count)
		u.creds = append(u.creds, c)
	}
	return u, rows.Err()
}

// storeChallenge saves ceremony state and returns the secret for the cookie.
func (s *Service) storeChallenge(ctx context.Context, kind string, accountID *uuid.UUID, sd *webauthn.SessionData) (string, error) {
	data, err := json.Marshal(sd)
	if err != nil {
		return "", fmt.Errorf("auth: challenge: %w", err)
	}
	secret := NewSecret()
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO webauthn_challenges (id_hash, kind, account_id, data, expires_at) VALUES ($1, $2, $3, $4, $5)`,
		HashSecret(secret), kind, accountID, data, s.now().Add(ChallengeTTL)); err != nil {
		return "", fmt.Errorf("auth: challenge: %w", err)
	}
	return secret, nil
}

// takeChallenge reads and deletes the ceremony state in one statement, so a
// challenge works once whatever happens next. It matches only the secret the
// browser holds, the kind, and (for registration) the signed-in account; a
// mismatch deletes nothing.
func (s *Service) takeChallenge(ctx context.Context, secret, kind string, accountID *uuid.UUID) (webauthn.SessionData, error) {
	var sd webauthn.SessionData
	if secret == "" {
		return sd, ErrPasskeyChallenge
	}
	var raw []byte
	err := s.pool.QueryRow(ctx, `
		DELETE FROM webauthn_challenges
		WHERE id_hash = $1 AND kind = $2 AND expires_at > $3 AND account_id IS NOT DISTINCT FROM $4
		RETURNING data`, HashSecret(secret), kind, s.now(), accountID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return sd, ErrPasskeyChallenge
	}
	if err != nil {
		return sd, fmt.Errorf("auth: challenge: %w", err)
	}
	if err := json.Unmarshal(raw, &sd); err != nil {
		return sd, ErrPasskeyChallenge
	}
	return sd, nil
}

func parseAccountID(id string) (uuid.UUID, error) {
	u, err := uuid.Parse(id)
	if err != nil {
		return uuid.Nil, ErrPasskeyFailed
	}
	return u, nil
}

// BeginPasskeyRegistration starts a registration for a signed-in account. It
// returns the options JSON for navigator.credentials.create and the secret
// that binds the ceremony to the browser (set it in a short-lived cookie).
// method is how the calling session was opened (MethodMagicLink or
// MethodPasskey); see ErrPasskeyStepUp.
func (s *Service) BeginPasskeyRegistration(ctx context.Context, accountID, method string) (options []byte, secret string, err error) {
	if !s.PasskeysEnabled() {
		return nil, "", ErrPasskeysDisabled
	}
	id, err := parseAccountID(accountID)
	if err != nil {
		return nil, "", err
	}
	if err := s.checkStepUp(ctx, s.pool, id, method); err != nil {
		return nil, "", err
	}
	user, err := s.loadUser(ctx, id)
	if err != nil {
		return nil, "", err
	}
	var exclude []protocol.CredentialDescriptor
	for _, c := range user.creds {
		exclude = append(exclude, c.Descriptor())
	}
	creation, sd, err := s.opt.WebAuthn.BeginRegistration(user, webauthn.WithExclusions(exclude))
	if err != nil {
		return nil, "", fmt.Errorf("auth: begin registration: %w", err)
	}
	secret, err = s.storeChallenge(ctx, kindRegister, &id, sd)
	if err != nil {
		return nil, "", err
	}
	options, err = json.Marshal(creation)
	return options, secret, err
}

// checkStepUp refuses a session that was not opened with a passkey when the
// account already has a passkey or is an operator. A magic-link session may
// still add the very first passkey of an ordinary account.
func (s *Service) checkStepUp(ctx context.Context, q queryRower, accountID uuid.UUID, method string) error {
	if method == MethodPasskey {
		return nil
	}
	var locked bool
	err := q.QueryRow(ctx, `
		SELECT is_operator OR EXISTS (SELECT 1 FROM passkeys WHERE account_id = accounts.id)
		FROM accounts WHERE id = $1`, accountID).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrPasskeyFailed
	}
	if err != nil {
		return fmt.Errorf("auth: passkey step-up: %w", err)
	}
	if locked {
		return ErrPasskeyStepUp
	}
	return nil
}

// PasskeyAddedMessage is the email that tells an account holder a passkey
// was added, in the company register.
func PasskeyAddedMessage(to string, at time.Time) mail.Message {
	return mail.Message{
		To:      to,
		Subject: "A passkey was added to your SanHolo account",
		Body: "TydeWhatMay sends this email because a passkey was added to the SanHolo account for this address on " +
			at.UTC().Format("2006-01-02 15:04 UTC") + ".\n\n" +
			"If you added it, you need do nothing.\n" +
			"If you did not, sign in, remove the passkey on the account page and sign out the sessions you do not know. " +
			"Write to hello@tydewhatmay.eu if you cannot sign in.\n",
	}
}

// FinishPasskeyRegistration verifies the authenticator's answer and stores
// the credential on the account, and queues the email that tells the account
// holder, in the same transaction. The challenge is spent first, whatever the
// outcome.
func (s *Service) FinishPasskeyRegistration(ctx context.Context, accountID, method, secret string, body []byte) (Passkey, error) {
	if !s.PasskeysEnabled() {
		return Passkey{}, ErrPasskeysDisabled
	}
	id, err := parseAccountID(accountID)
	if err != nil {
		return Passkey{}, err
	}
	sd, err := s.takeChallenge(ctx, secret, kindRegister, &id)
	if err != nil {
		return Passkey{}, err
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes(body)
	if err != nil {
		return Passkey{}, ErrPasskeyFailed
	}
	user, err := s.loadUser(ctx, id)
	if err != nil {
		return Passkey{}, err
	}
	cred, err := s.opt.WebAuthn.CreateCredential(user, sd, parsed)
	if err != nil {
		return Passkey{}, ErrPasskeyFailed
	}
	raw, err := json.Marshal(cred)
	if err != nil {
		return Passkey{}, fmt.Errorf("auth: passkey record: %w", err)
	}
	now := s.now()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Passkey{}, fmt.Errorf("auth: store passkey: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Lock the account row, so two registrations racing from sign-in links
	// cannot both see "no passkey yet".
	if _, err := tx.Exec(ctx, `SELECT 1 FROM accounts WHERE id = $1 FOR UPDATE`, id); err != nil {
		return Passkey{}, fmt.Errorf("auth: store passkey: %w", err)
	}
	if err := s.checkStepUp(ctx, tx, id, method); err != nil {
		return Passkey{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO passkeys (id, account_id, credential, sign_count, created_at) VALUES ($1, $2, $3, $4, $5)`,
		cred.ID, id, raw, int64(cred.Authenticator.SignCount), now); err != nil {
		return Passkey{}, fmt.Errorf("auth: store passkey: %w", err)
	}
	if err := mail.Enqueue(ctx, tx, PasskeyAddedMessage(user.email, now), time.Time{}); err != nil {
		return Passkey{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Passkey{}, fmt.Errorf("auth: store passkey: commit: %w", err)
	}
	return Passkey{ID: base64.RawURLEncoding.EncodeToString(cred.ID), CreatedAt: now}, nil
}

// BeginPasskeyLogin starts a discoverable sign-in: no email address is
// needed, the authenticator names the account.
func (s *Service) BeginPasskeyLogin(ctx context.Context) (options []byte, secret string, err error) {
	if !s.PasskeysEnabled() {
		return nil, "", ErrPasskeysDisabled
	}
	assertion, sd, err := s.opt.WebAuthn.BeginDiscoverableLogin()
	if err != nil {
		return nil, "", fmt.Errorf("auth: begin login: %w", err)
	}
	secret, err = s.storeChallenge(ctx, kindLogin, nil, sd)
	if err != nil {
		return nil, "", err
	}
	options, err = json.Marshal(assertion)
	return options, secret, err
}

// FinishPasskeyLogin verifies the assertion, advances the credential's sign
// counter and opens a session exactly as a magic link does (same lifetime,
// same hashed secret), marked with the passkey method. A counter that does
// not advance is a possible clone and signs nobody in.
func (s *Service) FinishPasskeyLogin(ctx context.Context, secret string, body []byte, userAgent string) (string, time.Time, error) {
	if !s.PasskeysEnabled() {
		return "", time.Time{}, ErrPasskeysDisabled
	}
	sd, err := s.takeChallenge(ctx, secret, kindLogin, nil)
	if err != nil {
		return "", time.Time{}, err
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(body)
	if err != nil {
		return "", time.Time{}, ErrPasskeyFailed
	}
	handler := func(rawID, userHandle []byte) (webauthn.User, error) {
		id, err := uuid.FromBytes(userHandle)
		if err != nil {
			return nil, ErrPasskeyFailed
		}
		return s.loadUser(ctx, id)
	}
	user, cred, err := s.opt.WebAuthn.ValidatePasskeyLogin(handler, sd, parsed)
	if err != nil || cred.Authenticator.CloneWarning {
		return "", time.Time{}, ErrPasskeyFailed
	}
	raw, err := json.Marshal(cred)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("auth: passkey record: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("auth: passkey sign-in: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	now := s.now()
	if s.beforeCounterUpdate != nil {
		s.beforeCounterUpdate()
	}
	// Compare-and-set: the counter must still be below the one presented
	// (or the authenticator has no counter). Two logins that presented the
	// same value cannot both pass the clone check in the library, which read
	// the old value before either wrote.
	tag, err := tx.Exec(ctx, `
		UPDATE passkeys SET sign_count = $3, last_used_at = $4, credential = $5
		WHERE id = $1 AND account_id = $2 AND (sign_count < $3 OR $3 = 0)`,
		cred.ID, uuid.UUID(user.WebAuthnID()), int64(cred.Authenticator.SignCount), now, raw)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("auth: passkey sign-in: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return "", time.Time{}, ErrPasskeyFailed
	}
	sess, expires, err := s.createSession(ctx, tx, uuid.UUID(user.WebAuthnID()).String(), userAgent, MethodPasskey, now)
	if err != nil {
		return "", time.Time{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", time.Time{}, fmt.Errorf("auth: passkey sign-in: commit: %w", err)
	}
	return sess, expires, nil
}

// ListPasskeys returns the account's passkeys, oldest first.
func (s *Service) ListPasskeys(ctx context.Context, accountID string) ([]Passkey, error) {
	id, err := parseAccountID(accountID)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT id, created_at, last_used_at FROM passkeys WHERE account_id = $1 ORDER BY created_at, id`, id)
	if err != nil {
		return nil, fmt.Errorf("auth: list passkeys: %w", err)
	}
	defer rows.Close()
	var out []Passkey
	for rows.Next() {
		var raw []byte
		var p Passkey
		if err := rows.Scan(&raw, &p.CreatedAt, &p.LastUsedAt); err != nil {
			return nil, fmt.Errorf("auth: list passkeys: %w", err)
		}
		p.ID = base64.RawURLEncoding.EncodeToString(raw)
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeletePasskey removes one of the account's own passkeys and signs out the
// account's sessions that were opened with a passkey (sessions do not record
// which one), so a stolen device's session ends with its key. An id that is
// malformed or belongs to another account removes nothing and reports false.
func (s *Service) DeletePasskey(ctx context.Context, accountID, passkeyID string) (bool, error) {
	id, err := parseAccountID(accountID)
	if err != nil {
		return false, err
	}
	raw, err := base64.RawURLEncoding.DecodeString(passkeyID)
	if err != nil {
		return false, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("auth: delete passkey: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `DELETE FROM passkeys WHERE id = $1 AND account_id = $2`, raw, id)
	if err != nil {
		return false, fmt.Errorf("auth: delete passkey: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `DELETE FROM account_sessions WHERE account_id = $1 AND method = $2`, id, MethodPasskey); err != nil {
		return false, fmt.Errorf("auth: delete passkey: sessions: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("auth: delete passkey: commit: %w", err)
	}
	return true, nil
}
