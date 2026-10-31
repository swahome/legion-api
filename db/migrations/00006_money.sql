-- +goose Up
-- Milestone 3, part 1: group accounts, categories, deposits, withdrawals,
-- account transfers and the group cash book.
-- Legacy mapping and rationale: docs/schema/money.md

-- Reference lists for sacco and mobile money accounts (banks already exist).
CREATE TABLE saccos (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    legacy_id  integer UNIQUE,
    name       text NOT NULL,
    slug       text NOT NULL UNIQUE,
    is_active  boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE sacco_branches (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    legacy_id  integer UNIQUE,
    sacco_id   bigint NOT NULL REFERENCES saccos (id) ON DELETE CASCADE,
    name       text NOT NULL,
    is_active  boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX sacco_branches_sacco_idx ON sacco_branches (sacco_id, name);

CREATE TABLE mobile_money_providers (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    legacy_id  integer UNIQUE,
    name       text NOT NULL,
    slug       text NOT NULL UNIQUE,
    is_active  boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- A group's accounts. The legacy app keeps four tables (bank_accounts,
-- sacco_accounts, mobile_money_accounts, petty_cash_accounts) whose ids
-- overlap, so legacy_id is only unique together with kind.
CREATE TYPE account_kind AS ENUM ('bank', 'sacco', 'mobile_money', 'petty_cash');

CREATE TABLE group_accounts (
    id               bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    legacy_id        integer,
    group_id         bigint NOT NULL REFERENCES groups (id) ON DELETE CASCADE,
    kind             account_kind NOT NULL,
    name             text NOT NULL,
    account_number   text NOT NULL DEFAULT '',
    bank_id          bigint REFERENCES banks (id),
    bank_branch_id   bigint REFERENCES bank_branches (id),
    sacco_id         bigint REFERENCES saccos (id),
    sacco_branch_id  bigint REFERENCES sacco_branches (id),
    provider_id      bigint REFERENCES mobile_money_providers (id),
    initial_balance  numeric(14, 2) NOT NULL DEFAULT 0,
    is_default       boolean NOT NULL DEFAULT false,
    is_verified      boolean NOT NULL DEFAULT false,
    is_closed        boolean NOT NULL DEFAULT false,
    -- Signatories, alert switches and other legacy columns until a feature needs them.
    settings         jsonb NOT NULL DEFAULT '{}'::jsonb,
    deleted_at       timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (kind, legacy_id),
    UNIQUE (group_id, id),
    CONSTRAINT group_account_institution CHECK (
        (kind = 'bank'         AND sacco_id IS NULL AND provider_id IS NULL) OR
        (kind = 'sacco'        AND bank_id IS NULL AND provider_id IS NULL) OR
        (kind = 'mobile_money' AND bank_id IS NULL AND sacco_id IS NULL) OR
        (kind = 'petty_cash'   AND bank_id IS NULL AND sacco_id IS NULL AND provider_id IS NULL)
    )
);
CREATE INDEX group_accounts_group_idx ON group_accounts (group_id) WHERE deleted_at IS NULL;

-- Per-group category lists (legacy income_categories, expense_categories,
-- fine_categories) and outside depositors (legacy depositors).
CREATE TABLE income_categories (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    legacy_id   integer UNIQUE,
    group_id    bigint NOT NULL REFERENCES groups (id) ON DELETE CASCADE,
    name        text NOT NULL,
    description text NOT NULL DEFAULT '',
    is_hidden   boolean NOT NULL DEFAULT false,
    is_active   boolean NOT NULL DEFAULT true,
    deleted_at  timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (group_id, id)
);

CREATE TABLE expense_categories (
    id                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    legacy_id         integer UNIQUE,
    group_id          bigint NOT NULL REFERENCES groups (id) ON DELETE CASCADE,
    name              text NOT NULL,
    description       text NOT NULL DEFAULT '',
    is_administrative boolean NOT NULL DEFAULT false,
    is_hidden         boolean NOT NULL DEFAULT false,
    is_active         boolean NOT NULL DEFAULT true,
    deleted_at        timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (group_id, id)
);

CREATE TABLE fine_categories (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    legacy_id   integer UNIQUE,
    group_id    bigint NOT NULL REFERENCES groups (id) ON DELETE CASCADE,
    name        text NOT NULL,
    description text NOT NULL DEFAULT '',
    amount      numeric(14, 2) NOT NULL DEFAULT 0 CHECK (amount >= 0), -- default fine amount
    is_hidden   boolean NOT NULL DEFAULT false,
    is_active   boolean NOT NULL DEFAULT true,
    deleted_at  timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (group_id, id)
);

CREATE TABLE depositors (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    legacy_id   integer UNIQUE,
    group_id    bigint NOT NULL REFERENCES groups (id) ON DELETE CASCADE,
    name        text NOT NULL,
    email       text,
    phone       text,
    description text NOT NULL DEFAULT '',
    is_active   boolean NOT NULL DEFAULT true,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (group_id, id)
);

-- Money in (legacy deposits). legacy_type keeps the original code, which
-- also encodes the account kind; kind is what the deposit is for.
CREATE TYPE deposit_kind AS ENUM (
    'contribution', 'fine', 'misc', 'income', 'loan_repayment', 'bank_loan',
    'stock_sale', 'money_market', 'asset_sale', 'funds_transfer',
    'loan_processing_income', 'external_lending_processing_income', 'external_lending_repayment'
);
CREATE TYPE payment_method AS ENUM ('mpesa', 'cash', 'cheque', 'funds_transfer', 'equitel', 'standing_order');

CREATE TABLE deposits (
    id                 bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    legacy_id          integer UNIQUE,
    legacy_type        smallint,
    group_id           bigint NOT NULL REFERENCES groups (id),
    account_id         bigint,                        -- NULL only for legacy rows whose account is unknown
    account_kind       account_kind NOT NULL,
    kind               deposit_kind NOT NULL,
    method             payment_method,
    membership_id      bigint,
    contribution_id    bigint,
    fine_category_id   bigint,
    income_category_id bigint,
    depositor_id       bigint,
    amount             numeric(14, 2) NOT NULL CHECK (amount > 0),
    deposit_date       date NOT NULL,
    description        text NOT NULL DEFAULT '',
    is_backdated       boolean NOT NULL DEFAULT false,
    -- Legacy links to loans, stocks, assets, money market investments and
    -- bank loans, until those milestones give them tables.
    refs               jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_by         bigint REFERENCES users (id),
    created_at         timestamptz NOT NULL DEFAULT now(),
    voided_at          timestamptz,
    voided_by          bigint REFERENCES users (id),
    UNIQUE (group_id, id),
    FOREIGN KEY (group_id, account_id) REFERENCES group_accounts (group_id, id),
    FOREIGN KEY (group_id, membership_id) REFERENCES memberships (group_id, id),
    FOREIGN KEY (group_id, contribution_id) REFERENCES contributions (group_id, id),
    FOREIGN KEY (group_id, fine_category_id) REFERENCES fine_categories (group_id, id),
    FOREIGN KEY (group_id, income_category_id) REFERENCES income_categories (group_id, id),
    FOREIGN KEY (group_id, depositor_id) REFERENCES depositors (group_id, id)
);
CREATE INDEX deposits_group_idx ON deposits (group_id, deposit_date DESC, id DESC);
CREATE INDEX deposits_member_idx ON deposits (membership_id) WHERE membership_id IS NOT NULL;

-- Money out (legacy withdrawals). Recording withdrawals and the approval
-- step come in the next part of milestone 3; the import fills it now so the
-- cash book is complete.
CREATE TYPE withdrawal_kind AS ENUM (
    'expense', 'asset_purchase', 'loan_disbursement', 'stock_purchase', 'money_market_investment',
    'contribution_refund', 'bank_loan_repayment', 'funds_transfer', 'external_loan_disbursement', 'dividend'
);

CREATE TABLE withdrawals (
    id                  bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    legacy_id           integer UNIQUE,
    legacy_type         smallint,
    group_id            bigint NOT NULL REFERENCES groups (id),
    account_id          bigint,
    account_kind        account_kind NOT NULL,
    kind                withdrawal_kind NOT NULL,
    method              payment_method,
    membership_id       bigint,
    contribution_id     bigint,
    expense_category_id bigint,
    amount              numeric(14, 2) NOT NULL CHECK (amount > 0),
    withdrawal_date     date NOT NULL,
    description         text NOT NULL DEFAULT '',
    is_backdated        boolean NOT NULL DEFAULT false,
    refs                jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_by          bigint REFERENCES users (id),
    created_at          timestamptz NOT NULL DEFAULT now(),
    voided_at           timestamptz,
    voided_by           bigint REFERENCES users (id),
    UNIQUE (group_id, id),
    FOREIGN KEY (group_id, account_id) REFERENCES group_accounts (group_id, id),
    FOREIGN KEY (group_id, membership_id) REFERENCES memberships (group_id, id),
    FOREIGN KEY (group_id, contribution_id) REFERENCES contributions (group_id, id),
    FOREIGN KEY (group_id, expense_category_id) REFERENCES expense_categories (group_id, id)
);
CREATE INDEX withdrawals_group_idx ON withdrawals (group_id, withdrawal_date DESC, id DESC);

CREATE TABLE account_transfers (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    legacy_id       integer UNIQUE,
    group_id        bigint NOT NULL REFERENCES groups (id),
    from_account_id bigint,
    to_account_id   bigint,
    amount          numeric(14, 2) NOT NULL CHECK (amount > 0),
    transfer_date   date NOT NULL,
    description     text NOT NULL DEFAULT '',
    created_by      bigint REFERENCES users (id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    voided_at       timestamptz,
    UNIQUE (group_id, id),
    FOREIGN KEY (group_id, from_account_id) REFERENCES group_accounts (group_id, id),
    FOREIGN KEY (group_id, to_account_id) REFERENCES group_accounts (group_id, id)
);

-- The group cash book (legacy transaction_statements): one row per
-- movement of money in or out of an account. Account balances are
-- initial_balance + sum(direction * amount) over live rows; nothing is
-- stored. account_kind comes from the legacy type code, so balances by
-- kind stay right even where a legacy row's account is unknown.
CREATE TABLE account_entries (
    id                  bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    legacy_id           integer UNIQUE,
    legacy_type         smallint,
    group_id            bigint NOT NULL REFERENCES groups (id),
    account_id          bigint,
    account_kind        account_kind NOT NULL,
    direction           smallint NOT NULL CHECK (direction IN (-1, 1)),
    amount              numeric(14, 2) NOT NULL CHECK (amount > 0),
    entry_date          date NOT NULL,
    description         text NOT NULL DEFAULT '',
    deposit_id          bigint,
    withdrawal_id       bigint,
    account_transfer_id bigint,
    membership_id       bigint,
    is_backdated        boolean NOT NULL DEFAULT false,
    created_at          timestamptz NOT NULL DEFAULT now(),
    voided_at           timestamptz,
    FOREIGN KEY (group_id, account_id) REFERENCES group_accounts (group_id, id),
    FOREIGN KEY (group_id, deposit_id) REFERENCES deposits (group_id, id),
    FOREIGN KEY (group_id, withdrawal_id) REFERENCES withdrawals (group_id, id),
    FOREIGN KEY (group_id, account_transfer_id) REFERENCES account_transfers (group_id, id),
    FOREIGN KEY (group_id, membership_id) REFERENCES memberships (group_id, id)
);
CREATE INDEX account_entries_group_idx ON account_entries (group_id, entry_date, id) WHERE voided_at IS NULL;
CREATE INDEX account_entries_account_idx ON account_entries (account_id, entry_date, id) WHERE voided_at IS NULL;
CREATE INDEX account_entries_deposit_idx ON account_entries (deposit_id) WHERE deposit_id IS NOT NULL;
CREATE INDEX account_entries_withdrawal_idx ON account_entries (withdrawal_id) WHERE withdrawal_id IS NOT NULL;

-- Member ledger rows created by a deposit (payments) point back at it so
-- voiding the deposit voids them too; fine rows carry their category.
ALTER TABLE ledger_entries
    ADD COLUMN deposit_id bigint,
    ADD COLUMN fine_category_id bigint,
    ADD CONSTRAINT ledger_entries_deposit_fk FOREIGN KEY (group_id, deposit_id) REFERENCES deposits (group_id, id),
    ADD CONSTRAINT ledger_entries_fine_category_fk FOREIGN KEY (group_id, fine_category_id) REFERENCES fine_categories (group_id, id);
CREATE INDEX ledger_deposit_idx ON ledger_entries (deposit_id) WHERE deposit_id IS NOT NULL;

-- +goose Down
DROP INDEX ledger_deposit_idx;
ALTER TABLE ledger_entries
    DROP CONSTRAINT ledger_entries_fine_category_fk,
    DROP CONSTRAINT ledger_entries_deposit_fk,
    DROP COLUMN fine_category_id,
    DROP COLUMN deposit_id;
DROP TABLE account_entries;
DROP TABLE account_transfers;
DROP TABLE withdrawals;
DROP TYPE withdrawal_kind;
DROP TABLE deposits;
DROP TYPE payment_method;
DROP TYPE deposit_kind;
DROP TABLE depositors;
DROP TABLE fine_categories;
DROP TABLE expense_categories;
DROP TABLE income_categories;
DROP TABLE group_accounts;
DROP TYPE account_kind;
DROP TABLE mobile_money_providers;
DROP TABLE sacco_branches;
DROP TABLE saccos;
