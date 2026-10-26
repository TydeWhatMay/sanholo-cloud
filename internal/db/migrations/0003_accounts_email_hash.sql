-- accounts.email_hash is SHA-256 hex of the lower-cased address, computed by
-- the application at insert. Sign-in looks accounts up by it, so a request
-- for an address the service has never seen is stored only as this hash.
ALTER TABLE accounts ADD COLUMN email_hash text;
UPDATE accounts SET email_hash = encode(sha256(convert_to(lower(email), 'UTF8')), 'hex');
ALTER TABLE accounts ALTER COLUMN email_hash SET NOT NULL;
CREATE UNIQUE INDEX accounts_email_hash_key ON accounts (email_hash);
