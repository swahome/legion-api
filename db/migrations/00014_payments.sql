-- +goose Up
-- Milestone 6: money that arrives from outside. Bank feeds (Equity, Co-op
-- Bank), MPesa paybill payments and the results of payments members start
-- from the app all land as transaction alerts: one row per movement on a
-- group's account as the bank or MPesa reports it. Reconciling an alert
-- means writing the deposits (or withdrawals) that account for it, or linking
-- ones an official already recorded by hand.

CREATE TYPE alert_status AS ENUM ('unreconciled', 'reconciled', 'ignored');

CREATE TABLE transaction_alerts (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    legacy_id       integer UNIQUE,
    -- Who reported it: equity, coop, chamasoft (a payment a member started),
    -- chamasoft_direct (paid straight to the group's paybill account),
    -- mpesa (Daraja C2B) or legacy (imported).
    source          text NOT NULL CHECK (source ~ '^[a-z_]+$'),
    provider_ref    text NOT NULL CHECK (provider_ref <> ''), -- the bank's or MPesa's transaction id
    bank_id         bigint REFERENCES banks (id),
    account_number  text NOT NULL DEFAULT '',
    group_id        bigint REFERENCES groups (id),           -- NULL: no group account has this number
    account_id      bigint,
    direction       smallint NOT NULL CHECK (direction IN (-1, 1)), -- 1 money in, -1 money out
    amount          numeric(14, 2) NOT NULL CHECK (amount > 0),
    currency        text NOT NULL DEFAULT 'KES',
    transaction_at  timestamptz NOT NULL,
    reference       text NOT NULL DEFAULT '',
    particulars     text NOT NULL DEFAULT '',
    phone           text,                                    -- payer's phone when the feed gives one
    payer_name      text NOT NULL DEFAULT '',
    status          alert_status NOT NULL DEFAULT 'unreconciled',
    auto_reconciled boolean NOT NULL DEFAULT false,
    reconciled_by   bigint REFERENCES users (id),
    reconciled_at   timestamptz,
    note            text NOT NULL DEFAULT '',
    raw             jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (source, provider_ref, direction),
    UNIQUE (group_id, id),
    FOREIGN KEY (group_id, account_id) REFERENCES group_accounts (group_id, id),
    CHECK (account_id IS NULL OR group_id IS NOT NULL)
);
CREATE INDEX transaction_alerts_group_idx ON transaction_alerts (group_id, status, transaction_at DESC);
CREATE INDEX transaction_alerts_number_idx ON transaction_alerts (account_number);
CREATE INDEX transaction_alerts_bank_idx ON transaction_alerts (bank_id, transaction_at DESC);
CREATE INDEX transaction_alerts_unassigned_idx ON transaction_alerts (transaction_at DESC) WHERE group_id IS NULL;

ALTER TABLE deposits ADD COLUMN transaction_alert_id bigint REFERENCES transaction_alerts (id);
ALTER TABLE withdrawals ADD COLUMN transaction_alert_id bigint REFERENCES transaction_alerts (id);
CREATE INDEX deposits_alert_idx ON deposits (transaction_alert_id) WHERE transaction_alert_id IS NOT NULL;
CREATE INDEX withdrawals_alert_idx ON withdrawals (transaction_alert_id) WHERE transaction_alert_id IS NOT NULL;

-- A payment a member starts: the app asks the Chamasoft payment API to send
-- an MPesa prompt (STK push) to the member's phone. The callback says whether
-- it was paid; a paid request becomes a transaction alert and is reconciled
-- from its lines straight away.
CREATE TYPE payment_request_status AS ENUM ('pending', 'sent', 'failed', 'paid');

CREATE TABLE payment_requests (
    id                   bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    legacy_id            integer UNIQUE,
    group_id             bigint NOT NULL REFERENCES groups (id),
    membership_id        bigint,
    user_id              bigint REFERENCES users (id),
    account_id           bigint,
    account_number       text NOT NULL DEFAULT '',
    phone                text NOT NULL,
    amount               numeric(14, 2) NOT NULL CHECK (amount > 0),
    reference            text NOT NULL UNIQUE,
    -- What the money is for: [{"kind":"contribution","contribution_id":1,"amount":"500.00"}, ...].
    -- Kinds: contribution, fine (contribution_id for a late fine or fine_category_id), loan, misc.
    lines                jsonb NOT NULL DEFAULT '[]'::jsonb,
    channel              text NOT NULL DEFAULT 'web',         -- web, app, ussd
    provider             text NOT NULL DEFAULT 'chamasoft',
    status               payment_request_status NOT NULL DEFAULT 'pending',
    response_code        text NOT NULL DEFAULT '',
    response_description text NOT NULL DEFAULT '',
    result_code          text NOT NULL DEFAULT '',
    result_description   text NOT NULL DEFAULT '',
    provider_ref         text,                                -- MPesa receipt, once paid
    transaction_alert_id bigint REFERENCES transaction_alerts (id),
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (group_id, membership_id) REFERENCES memberships (group_id, id),
    FOREIGN KEY (group_id, account_id) REFERENCES group_accounts (group_id, id)
);
CREATE INDEX payment_requests_group_idx ON payment_requests (group_id, created_at DESC);
CREATE INDEX payment_requests_member_idx ON payment_requests (membership_id, created_at DESC);
CREATE INDEX payment_requests_open_idx ON payment_requests (created_at) WHERE status IN ('pending', 'sent');

-- Every call a bank, MPesa or Chamasoft makes to us, kept as received
-- (legacy wrote these to log files). Support can see what arrived and what
-- we answered.
CREATE TABLE webhook_events (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    source     text NOT NULL,
    remote_ip  text NOT NULL DEFAULT '',
    body       text NOT NULL,
    outcome    text NOT NULL DEFAULT '',   -- accepted, duplicate, rejected, error, ...
    detail     text NOT NULL DEFAULT '',
    alert_id   bigint REFERENCES transaction_alerts (id),
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX webhook_events_created_idx ON webhook_events (created_at DESC);
CREATE INDEX webhook_events_source_idx ON webhook_events (source, created_at DESC);

-- Bank staff (legacy user_bank_pairings + bank_user_role_pairings): they see
-- the groups that hold accounts at their bank and those accounts' feeds.
CREATE TYPE bank_user_role AS ENUM ('admin', 'staff');

CREATE TABLE bank_users (
    bank_id    bigint NOT NULL REFERENCES banks (id) ON DELETE CASCADE,
    user_id    bigint NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role       bank_user_role NOT NULL DEFAULT 'staff',
    created_by bigint REFERENCES users (id),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (bank_id, user_id)
);
CREATE INDEX bank_users_user_idx ON bank_users (user_id);

-- +goose Down
DROP TABLE bank_users;
DROP TYPE bank_user_role;
DROP TABLE webhook_events;
DROP TABLE payment_requests;
DROP TYPE payment_request_status;
DROP INDEX withdrawals_alert_idx;
DROP INDEX deposits_alert_idx;
ALTER TABLE withdrawals DROP COLUMN transaction_alert_id;
ALTER TABLE deposits DROP COLUMN transaction_alert_id;
DROP TABLE transaction_alerts;
DROP TYPE alert_status;
