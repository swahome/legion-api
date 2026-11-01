-- +goose Up
-- Milestone 7: USSD and WhatsApp. One menu engine serves both; each
-- conversation is a channel session whose menu position lives in state, so
-- a retried request or a dropped reply never moves the caller twice.

CREATE TYPE channel_kind AS ENUM ('ussd', 'whatsapp', 'test');

CREATE TABLE channel_sessions (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    channel       channel_kind NOT NULL,
    -- Africa's Talking sessionId for USSD; the phone number for WhatsApp.
    session_key   text NOT NULL,
    phone         text NOT NULL,
    user_id       bigint REFERENCES users (id) ON DELETE SET NULL,
    group_id      bigint REFERENCES groups (id) ON DELETE SET NULL,
    service_code  text NOT NULL DEFAULT '',
    state         jsonb NOT NULL DEFAULT '{}'::jsonb,
    -- USSD sends every answer so far ("1*2*500"); steps is how many of
    -- them were already acted on.
    steps         integer NOT NULL DEFAULT 0,
    last_request  text NOT NULL DEFAULT '',
    last_response text NOT NULL DEFAULT '',
    inputs        integer NOT NULL DEFAULT 0,
    ended_at      timestamptz,
    expires_at    timestamptz NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

-- One open conversation per USSD session or WhatsApp number.
CREATE UNIQUE INDEX channel_sessions_open_idx ON channel_sessions (channel, session_key) WHERE ended_at IS NULL;
CREATE INDEX channel_sessions_created_idx ON channel_sessions (created_at DESC);
CREATE INDEX channel_sessions_phone_idx ON channel_sessions (phone);

-- Every exchange, for support: what the caller sent and what they saw.
CREATE TABLE channel_messages (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    session_id bigint NOT NULL REFERENCES channel_sessions (id) ON DELETE CASCADE,
    input      text NOT NULL DEFAULT '',
    response   text NOT NULL,
    error      text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX channel_messages_session_idx ON channel_messages (session_id, id);

-- +goose Down
DROP TABLE channel_messages;
DROP TABLE channel_sessions;
DROP TYPE channel_kind;
