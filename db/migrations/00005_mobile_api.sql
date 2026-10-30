-- +goose Up

-- Profile fields the mobile app shows.
ALTER TABLE users
    ADD COLUMN avatar       text NOT NULL DEFAULT '',
    ADD COLUMN is_validated boolean NOT NULL DEFAULT false;

-- One-time PINs for the mobile app's phone or email sign-in (legacy
-- user_pin_access_token.pin). Unlike the legacy table, PINs are hashed,
-- expire, and lock after too many wrong tries.
CREATE TABLE mobile_pins (
    identity    text PRIMARY KEY,                   -- E.164 phone or lower-case email
    pin_hash    bytea NOT NULL,
    unique_code text NOT NULL,                      -- legacy unique_code, echoed to the app
    attempts    integer NOT NULL DEFAULT 0,
    expires_at  timestamptz NOT NULL,
    -- Set when the PIN was right but no account has the identity yet:
    -- sign-up (register_user) then proves the PIN with unique_code.
    verified_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- Mobile app access tokens (legacy user_pin_access_token.access_token). The
-- app sends the token as a Bearer header; only its SHA-256 is stored.
CREATE TABLE mobile_access_tokens (
    token_hash   bytea PRIMARY KEY,
    legacy_id    integer UNIQUE,
    user_id      bigint NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    last_used_at timestamptz
);
CREATE INDEX mobile_access_tokens_user ON mobile_access_tokens (user_id);

-- +goose Down
DROP TABLE mobile_access_tokens;
DROP TABLE mobile_pins;
ALTER TABLE users DROP COLUMN is_validated, DROP COLUMN avatar;
