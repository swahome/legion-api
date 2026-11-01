-- +goose Up
-- Milestone 5: self sign-up with a phone code, password reset by SMS, member
-- invites and the group setup wizard. Every SMS goes through an outbox, so a
-- message is written in the same transaction as the change that causes it
-- and is retried until Africa's Talking takes it.

CREATE TYPE sms_status AS ENUM ('queued', 'sending', 'sent', 'failed');

CREATE TABLE sms_messages (
    id                  bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    to_phone            text NOT NULL CHECK (to_phone ~ '^\+[1-9][0-9]{6,14}$'),
    body                text NOT NULL CHECK (body <> ''),
    kind                text NOT NULL,          -- signup_code, password_reset, invite, ...
    user_id             bigint REFERENCES users (id) ON DELETE SET NULL,
    group_id            bigint REFERENCES groups (id) ON DELETE SET NULL,
    status              sms_status NOT NULL DEFAULT 'queued',
    attempts            integer NOT NULL DEFAULT 0,
    next_attempt_at     timestamptz NOT NULL DEFAULT now(),
    claimed_at          timestamptz,
    provider_message_id text,
    provider_status     text,                   -- e.g. Success, InvalidPhoneNumber
    cost                text,                   -- as the provider reports it, e.g. "KES 0.8000"
    error               text,
    created_at          timestamptz NOT NULL DEFAULT now(),
    sent_at             timestamptz
);
CREATE INDEX sms_messages_due_idx ON sms_messages (next_attempt_at) WHERE status IN ('queued', 'sending');
CREATE INDEX sms_messages_user_idx ON sms_messages (user_id, created_at DESC);
CREATE INDEX sms_messages_group_idx ON sms_messages (group_id, created_at DESC);

-- One live code per user and purpose: a phone verification code after
-- sign-up, or a password reset code. Codes are hashed, expire and lock
-- after too many wrong tries. A reset code that was checked on its own (the
-- mobile app does this) is swapped for a one-use reset token.
CREATE TYPE user_code_purpose AS ENUM ('verify_phone', 'reset_password');

CREATE TABLE user_codes (
    user_id     bigint NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    purpose     user_code_purpose NOT NULL,
    code_hash   bytea NOT NULL,
    attempts    integer NOT NULL DEFAULT 0,
    expires_at  timestamptz NOT NULL,
    token_hash  bytea UNIQUE,
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, purpose)
);

-- A link sent by SMS to someone added to a group, to set their password
-- (legacy users.join_code). Only the token's SHA-256 is stored.
CREATE TABLE invites (
    token_hash    bytea PRIMARY KEY,
    user_id       bigint NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    group_id      bigint NOT NULL REFERENCES groups (id) ON DELETE CASCADE,
    membership_id bigint NOT NULL REFERENCES memberships (id) ON DELETE CASCADE,
    invited_by    bigint REFERENCES users (id),
    created_at    timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz NOT NULL,
    used_at       timestamptz
);
CREATE INDEX invites_membership_idx ON invites (membership_id);

-- A group created by sign-up stays in 'setup' until its official confirms
-- the wizard's review step. Setup groups are not billed and their members
-- get no SMS until then.
ALTER TABLE groups
    ADD COLUMN created_by         bigint REFERENCES users (id),
    ADD COLUMN setup_completed_at timestamptz;

ALTER TABLE memberships ADD COLUMN invited_at timestamptz;

-- +goose Down
ALTER TABLE memberships DROP COLUMN invited_at;
ALTER TABLE groups DROP COLUMN setup_completed_at, DROP COLUMN created_by;
DROP TABLE invites;
DROP TABLE user_codes;
DROP TYPE user_code_purpose;
DROP TABLE sms_messages;
DROP TYPE sms_status;
