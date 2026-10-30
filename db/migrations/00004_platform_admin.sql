-- +goose Up
-- Reference data and platform settings managed from the admin panel
-- (legacy `countries`, `counties`, `banks`, `bank_branches`, `settings`).

CREATE TABLE countries (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    legacy_id     integer UNIQUE,
    name          text NOT NULL,
    code          char(2) NOT NULL UNIQUE,     -- ISO 3166-1 alpha-2
    currency_code char(3) NOT NULL,
    currency_name text NOT NULL DEFAULT '',
    calling_code  text NOT NULL,               -- digits only, e.g. 254
    is_default    boolean NOT NULL DEFAULT false,
    is_active     boolean NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT countries_calling_code CHECK (calling_code ~ '^[1-9][0-9]{0,3}$')
);
CREATE UNIQUE INDEX countries_one_default ON countries (is_default) WHERE is_default;

CREATE TABLE counties (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    legacy_id  integer UNIQUE,
    country_id bigint NOT NULL REFERENCES countries (id),
    name       text NOT NULL,
    code       integer,
    capital    text NOT NULL DEFAULT '',
    is_active  boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (country_id, name)
);

CREATE TABLE banks (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    legacy_id  integer UNIQUE,
    name       text NOT NULL,
    slug       text NOT NULL UNIQUE,
    code       integer,                       -- CBK bank code
    short_code text NOT NULL DEFAULT '',
    logo       text NOT NULL DEFAULT '',
    -- Branding the legacy app shows for partner banks (primary, secondary,
    -- tertiary and text colours).
    branding   jsonb NOT NULL DEFAULT '{}'::jsonb,
    -- Other legacy columns (OTP endpoints, wallet flag) until a feature needs them.
    settings   jsonb NOT NULL DEFAULT '{}'::jsonb,
    is_partner boolean NOT NULL DEFAULT false,
    is_default boolean NOT NULL DEFAULT false,
    is_active  boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE bank_branches (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    legacy_id  integer UNIQUE,
    bank_id    bigint NOT NULL REFERENCES banks (id) ON DELETE CASCADE,
    name       text NOT NULL,
    code       integer,
    is_active  boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX bank_branches_bank_idx ON bank_branches (bank_id, name);

-- Platform settings as key/value pairs. Keys keep the legacy column names
-- (application_name, trial_days, disable_smses, ...). Menu switches use
-- keys of the form 'menu.<area>.<item>' with a boolean value.
CREATE TABLE platform_settings (
    key        text PRIMARY KEY CHECK (key ~ '^[a-z0-9_.]+$'),
    value      jsonb NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    updated_by bigint REFERENCES users (id) ON DELETE SET NULL
);

-- What platform admins change, for the admin panel's activity view.
CREATE TABLE admin_audit_log (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    actor_id    bigint REFERENCES users (id) ON DELETE SET NULL,
    action      text NOT NULL,                -- e.g. 'user.update', 'group.suspend'
    target_type text NOT NULL,
    target_id   bigint,
    detail      jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX admin_audit_log_created_idx ON admin_audit_log (created_at DESC);

CREATE INDEX users_name_idx ON users (lower(first_name || ' ' || last_name));
CREATE INDEX groups_name_idx ON groups (lower(name));

-- +goose Down
DROP INDEX groups_name_idx;
DROP INDEX users_name_idx;
DROP TABLE admin_audit_log;
DROP TABLE platform_settings;
DROP TABLE bank_branches;
DROP TABLE banks;
DROP TABLE counties;
DROP TABLE countries;
