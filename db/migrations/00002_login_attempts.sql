-- +goose Up
-- Failed login tracking for throttling. Mirrors the legacy Ion Auth settings
-- (3 failed attempts per identity and IP, then a 10 minute lockout).
CREATE TABLE login_attempts (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    identity     text NOT NULL,
    ip_address   inet,
    attempted_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX login_attempts_lookup_idx ON login_attempts (identity, attempted_at);

-- +goose Down
DROP TABLE login_attempts;
