-- +goose Up
-- Slice 1: identity, groups, memberships, contributions and the member ledger.
-- Legacy mapping and rationale: docs/schema/slice-1.md

-- Every table that is loaded from the legacy MySQL dump keeps the legacy
-- primary key in legacy_id so the ETL is re-runnable and auditable.

CREATE TABLE users (
    id                    bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    legacy_id             integer UNIQUE,
    first_name            text NOT NULL DEFAULT '',
    middle_name           text NOT NULL DEFAULT '',
    last_name             text NOT NULL DEFAULT '',
    email                 text,
    phone                 text,                         -- E.164, e.g. +254712345678
    id_number             text,
    password_hash         text,                         -- bcrypt; NULL until set
    password_reset_required boolean NOT NULL DEFAULT false,
    is_active             boolean NOT NULL DEFAULT true,
    last_login_at         timestamptz,
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT users_phone_e164 CHECK (phone IS NULL OR phone ~ '^\+[1-9][0-9]{6,14}$'),
    CONSTRAINT users_has_identity CHECK (phone IS NOT NULL OR email IS NOT NULL)
);
CREATE UNIQUE INDEX users_phone_key ON users (phone) WHERE phone IS NOT NULL;
CREATE UNIQUE INDEX users_email_key ON users (lower(email)) WHERE email IS NOT NULL;

-- Platform-wide roles (legacy Ion Auth `groups` + `users_groups`).
-- Per-group officials are modelled on memberships, not here.
CREATE TYPE platform_role AS ENUM (
    'admin', 'group_account_manager', 'banker', 'partner', 'sales',
    'diocese_admin', 'deanery_admin', 'parish_admin', 'sub_parish_admin'
);

CREATE TABLE user_platform_roles (
    user_id    bigint NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role       platform_role NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, role)
);

CREATE TABLE refresh_tokens (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id      bigint NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash   bytea NOT NULL UNIQUE,                 -- sha256 of the opaque token
    family_id    uuid NOT NULL,                         -- rotation family, revoked together on reuse
    expires_at   timestamptz NOT NULL,
    revoked_at   timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    user_agent   text,
    ip_address   inet
);
CREATE INDEX refresh_tokens_user_idx ON refresh_tokens (user_id);

-- Chamas / investment groups (legacy `investment_groups`).
CREATE TYPE group_status AS ENUM ('setup', 'active', 'suspended', 'closed');

CREATE TABLE groups (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    legacy_id     integer UNIQUE,
    name          text NOT NULL,
    slug          text NOT NULL UNIQUE,
    email         text,
    phone         text,
    address       text,
    country_code  char(2) NOT NULL DEFAULT 'KE',
    currency      char(3) NOT NULL DEFAULT 'KES',
    timezone      text NOT NULL DEFAULT 'Africa/Nairobi',
    status        group_status NOT NULL DEFAULT 'setup',
    owner_user_id bigint REFERENCES users (id),
    -- The ~100 legacy feature/visibility flags live here until a slice needs
    -- one as a real column. Keys keep their legacy names.
    settings      jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE group_roles (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    legacy_id   integer UNIQUE,
    group_id    bigint NOT NULL REFERENCES groups (id) ON DELETE CASCADE,
    name        text NOT NULL,
    description text NOT NULL DEFAULT '',
    is_editable boolean NOT NULL DEFAULT true,
    is_active   boolean NOT NULL DEFAULT true,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (group_id, id)
);

-- A user's membership of a group (legacy `members`).
CREATE TYPE membership_status AS ENUM ('active', 'suspended', 'removed');

CREATE TABLE memberships (
    id                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    legacy_id         integer UNIQUE,
    group_id          bigint NOT NULL REFERENCES groups (id) ON DELETE CASCADE,
    user_id           bigint NOT NULL REFERENCES users (id),
    is_official       boolean NOT NULL DEFAULT false,   -- legacy members.is_admin
    group_role_id     bigint,
    membership_number text,
    date_of_birth     date,
    physical_address  text,
    postal_address    text,
    status            membership_status NOT NULL DEFAULT 'active',
    joined_at         timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    profile           jsonb NOT NULL DEFAULT '{}'::jsonb, -- KYC/survey fields not yet used by the app
    UNIQUE (group_id, user_id),
    UNIQUE (group_id, id),
    FOREIGN KEY (group_id, group_role_id) REFERENCES group_roles (group_id, id)
);
CREATE INDEX memberships_user_idx ON memberships (user_id);

-- Contributions (legacy `contributions` + regular/one-time settings).
CREATE TYPE contribution_kind AS ENUM ('regular', 'one_time', 'non_scheduled');
CREATE TYPE contribution_category AS ENUM ('share_capital', 'savings', 'welfare', 'membership_fee', 'investment', 'other');
CREATE TYPE contribution_frequency AS ENUM (
    'daily', 'weekly', 'fortnightly', 'monthly', 'twice_monthly',
    'bimonthly', 'quarterly', 'biannually', 'annually'
);

CREATE TABLE contributions (
    id                  bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    legacy_id           integer UNIQUE,
    group_id            bigint NOT NULL REFERENCES groups (id) ON DELETE CASCADE,
    name                text NOT NULL,
    kind                contribution_kind NOT NULL,
    category            contribution_category,
    amount              numeric(14, 2) NOT NULL DEFAULT 0 CHECK (amount >= 0),
    is_active           boolean NOT NULL DEFAULT true,
    is_hidden           boolean NOT NULL DEFAULT false,
    is_closed           boolean NOT NULL DEFAULT false,
    is_non_refundable   boolean NOT NULL DEFAULT false,
    is_equity           boolean NOT NULL DEFAULT false,
    hidden_in_statement boolean NOT NULL DEFAULT false,
    show_arrears_cumulatively boolean NOT NULL DEFAULT false,
    applies_to_all_members boolean NOT NULL DEFAULT true, -- false => see contribution_members
    deleted_at          timestamptz,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    UNIQUE (group_id, id)
);

-- Invoicing schedule. One row per regular or one-time contribution.
-- Legacy day/week pickers (month_day_monthly, week_day_fortnight, ...) are
-- kept verbatim in `rule` until the invoicing engine is ported.
CREATE TABLE contribution_schedules (
    contribution_id      bigint PRIMARY KEY REFERENCES contributions (id) ON DELETE CASCADE,
    frequency            contribution_frequency,         -- NULL for one-time
    rule                 jsonb NOT NULL DEFAULT '{}'::jsonb,
    invoice_date         date,
    contribution_date    date,
    invoice_days_before  integer,
    last_invoiced_on     date,
    invoicing_active     boolean NOT NULL DEFAULT true,
    sms_notifications    boolean NOT NULL DEFAULT false,
    email_notifications  boolean NOT NULL DEFAULT false,
    sms_template         text,
    disable_overpayments boolean NOT NULL DEFAULT false,
    fines_enabled        boolean NOT NULL DEFAULT false,
    updated_at           timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE contribution_members (
    contribution_id bigint NOT NULL REFERENCES contributions (id) ON DELETE CASCADE,
    membership_id   bigint NOT NULL REFERENCES memberships (id) ON DELETE CASCADE,
    created_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (contribution_id, membership_id)
);

CREATE TYPE fine_kind AS ENUM ('fixed', 'percentage');

CREATE TABLE contribution_fine_rules (
    id                  bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    legacy_id           integer UNIQUE,
    contribution_id     bigint NOT NULL REFERENCES contributions (id) ON DELETE CASCADE,
    kind                fine_kind NOT NULL,
    fixed_amount        numeric(14, 2),
    percentage_rate     numeric(7, 4),
    percentage_of       text,                            -- legacy percentage_fine_on
    chargeable_on       text NOT NULL,                   -- legacy *_chargeable_on: day offset or 'last_day_of_the_month' etc.
    mode                text,
    frequency           text,
    fine_limit          integer,
    sms_notifications   boolean NOT NULL DEFAULT false,
    email_notifications boolean NOT NULL DEFAULT false,
    is_active           boolean NOT NULL DEFAULT true,
    created_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT fine_rule_amount CHECK (
        (kind = 'fixed' AND fixed_amount IS NOT NULL) OR
        (kind = 'percentage' AND percentage_rate IS NOT NULL)
    )
);

-- Member ledger (legacy `statements`). Append-only; balances are derived,
-- not stored. Positive amount = owed by the member (invoice), negative =
-- credited (payment, refund reversal, transfer out of the contribution).
CREATE TYPE ledger_entry_type AS ENUM (
    'contribution_invoice', 'contribution_fine_invoice', 'fine_invoice', 'misc_invoice',
    'contribution_payment', 'fine_payment', 'misc_payment',
    'contribution_refund', 'contribution_transfer_in', 'contribution_transfer_out',
    'contribution_to_fine_transfer', 'contribution_to_loan_transfer',
    'loan_to_contribution_transfer', 'loan_to_fine_transfer'
);

CREATE TABLE ledger_entries (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    legacy_id       integer UNIQUE,
    legacy_type     smallint,                            -- original statements.transaction_type
    group_id        bigint NOT NULL REFERENCES groups (id),
    membership_id   bigint,
    contribution_id bigint,
    entry_type      ledger_entry_type NOT NULL,
    amount          numeric(14, 2) NOT NULL,
    entry_date      date NOT NULL,
    due_date        date,
    description     text NOT NULL DEFAULT '',
    voided_at       timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (group_id, membership_id) REFERENCES memberships (group_id, id),
    FOREIGN KEY (group_id, contribution_id) REFERENCES contributions (group_id, id)
);
CREATE INDEX ledger_member_idx ON ledger_entries (membership_id, entry_date) WHERE voided_at IS NULL;
CREATE INDEX ledger_group_idx ON ledger_entries (group_id, entry_date) WHERE voided_at IS NULL;

-- +goose Down
DROP TABLE ledger_entries;
DROP TYPE ledger_entry_type;
DROP TABLE contribution_fine_rules;
DROP TYPE fine_kind;
DROP TABLE contribution_members;
DROP TABLE contribution_schedules;
DROP TABLE contributions;
DROP TYPE contribution_frequency;
DROP TYPE contribution_category;
DROP TYPE contribution_kind;
DROP TABLE memberships;
DROP TYPE membership_status;
DROP TABLE group_roles;
DROP TABLE groups;
DROP TYPE group_status;
DROP TABLE refresh_tokens;
DROP TABLE user_platform_roles;
DROP TYPE platform_role;
DROP TABLE users;
