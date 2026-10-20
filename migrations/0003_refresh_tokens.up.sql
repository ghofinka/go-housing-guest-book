-- Rotating refresh tokens. Only the SHA-256 of the opaque token is stored.
-- All tokens issued from one login share a family_id so reuse of a revoked
-- token can revoke the whole family.
CREATE TABLE refresh_tokens (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid NOT NULL REFERENCES users (id),
    family_id   uuid NOT NULL,
    token_hash  text NOT NULL UNIQUE,
    expires_at  timestamptz NOT NULL,
    revoked_at  timestamptz,
    replaced_by uuid REFERENCES refresh_tokens (id),
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX refresh_tokens_user_idx ON refresh_tokens (user_id);
CREATE INDEX refresh_tokens_family_idx ON refresh_tokens (family_id);

GRANT SELECT, INSERT, UPDATE ON refresh_tokens TO guestbook_app;
