-- +goose Up
-- Milestone 4, part 1: loan types, loans, their schedules and fines
-- (legacy loan_invoices), repayments and guarantors. See docs/schema/loans.md.

CREATE TYPE interest_type AS ENUM ('fixed_balance', 'reducing_balance', 'custom', 'fixed_amount');
CREATE TYPE interest_rate_per AS ENUM ('day', 'week', 'month', 'year', 'term');
CREATE TYPE repayment_frequency AS ENUM ('daily', 'weekly', 'monthly');

-- A loan product a group offers. Fine and fee rules are JSON objects in the
-- shapes internal/loans defines (FineRule, ProcessingFee); a loan copies
-- them when it is made, so editing a type never changes running loans.
CREATE TABLE loan_types (
    id                   bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    group_id             bigint NOT NULL REFERENCES groups (id),
    name                 text NOT NULL CHECK (name <> ''),
    interest_type        interest_type NOT NULL DEFAULT 'fixed_balance',
    interest_rate        numeric(9, 4) NOT NULL DEFAULT 0 CHECK (interest_rate >= 0),
    rate_per             interest_rate_per NOT NULL DEFAULT 'month',
    reducing_method      text NOT NULL DEFAULT 'equal_installments' CHECK (reducing_method IN ('equal_installments', 'equal_principal')),
    fixed_interest       numeric(14, 2) NOT NULL DEFAULT 0,     -- fixed_amount: interest per installment...
    fixed_interest_rate  numeric(9, 4) NOT NULL DEFAULT 0,      -- ...or this percent of the amount
    frequency            repayment_frequency NOT NULL DEFAULT 'monthly',
    -- Repayment period, in installments: fixed, or a range the official picks from.
    fixed_term           integer CHECK (fixed_term > 0),
    min_term             integer CHECK (min_term > 0),
    max_term             integer CHECK (max_term > 0),
    grace_periods        integer NOT NULL DEFAULT 1 CHECK (grace_periods BETWEEN 1 AND 24),
    -- How much a member can borrow: a range, or a multiple of their savings.
    amount_basis         text NOT NULL DEFAULT 'range' CHECK (amount_basis IN ('range', 'savings')),
    min_amount           numeric(14, 2),
    max_amount           numeric(14, 2),
    savings_multiple     numeric(6, 2),
    -- Guarantors: none, on every loan, or only when the amount is over max_amount.
    guarantors           text NOT NULL DEFAULT 'none' CHECK (guarantors IN ('none', 'always', 'over_max')),
    min_guarantors       integer,
    max_guarantors       integer,
    late_fine            jsonb,
    balance_fine         jsonb,
    processing_fee       jsonb,
    fine_deferment       boolean NOT NULL DEFAULT false, -- repayments clear installments before fines
    needs_approval       boolean NOT NULL DEFAULT true,  -- applications go to the signatories
    is_hidden            boolean NOT NULL DEFAULT false, -- not offered to members applying
    settings             jsonb NOT NULL DEFAULT '{}'::jsonb, -- legacy extras (profit fee, Islamic product, ...)
    created_by           bigint REFERENCES users (id),
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    deleted_at           timestamptz,
    UNIQUE (group_id, id)
);
CREATE INDEX loan_types_group_idx ON loan_types (group_id) WHERE deleted_at IS NULL;

-- A loan to a member. The terms are copied from the loan type (or set by
-- hand) when the loan is made. The balance is never stored: it is the live
-- charges less the live repayments.
CREATE TABLE loans (
    id                  bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    group_id            bigint NOT NULL REFERENCES groups (id),
    membership_id       bigint NOT NULL,
    loan_type_id        bigint,
    amount              numeric(14, 2) NOT NULL CHECK (amount > 0),
    disbursed_on        date NOT NULL,
    account_id          bigint,          -- the account it was paid out of
    interest_type       interest_type NOT NULL,
    interest_rate       numeric(9, 4) NOT NULL DEFAULT 0,
    rate_per            interest_rate_per NOT NULL DEFAULT 'month',
    reducing_method     text NOT NULL DEFAULT 'equal_installments' CHECK (reducing_method IN ('equal_installments', 'equal_principal')),
    fixed_interest      numeric(14, 2) NOT NULL DEFAULT 0,
    fixed_interest_rate numeric(9, 4) NOT NULL DEFAULT 0,
    frequency           repayment_frequency NOT NULL DEFAULT 'monthly',
    installments        integer NOT NULL CHECK (installments > 0),
    grace_periods       integer NOT NULL DEFAULT 1,
    first_due_on        date,            -- set when the first installment date was picked by hand
    ends_on             date NOT NULL,   -- the last installment's due date
    late_fine           jsonb,
    balance_fine        jsonb,
    processing_fee      jsonb,
    fine_deferment      boolean NOT NULL DEFAULT false,
    next_balance_fine_on date,           -- the next outstanding-balance fine (legacy outstanding_loan_balance_fine_date)
    is_bad_debt         boolean NOT NULL DEFAULT false,
    description         text NOT NULL DEFAULT '',
    settings            jsonb NOT NULL DEFAULT '{}'::jsonb, -- legacy extras (notifications, custom rates, ...)
    is_backdated        boolean NOT NULL DEFAULT false,
    created_by          bigint REFERENCES users (id),
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    voided_at           timestamptz,
    voided_by           bigint REFERENCES users (id),
    UNIQUE (group_id, id),
    FOREIGN KEY (group_id, membership_id) REFERENCES memberships (group_id, id),
    FOREIGN KEY (group_id, loan_type_id) REFERENCES loan_types (group_id, id),
    FOREIGN KEY (group_id, account_id) REFERENCES group_accounts (group_id, id)
);
CREATE INDEX loans_group_idx ON loans (group_id, disbursed_on DESC, id DESC);
CREATE INDEX loans_member_idx ON loans (membership_id);

-- What a member owes on a loan (legacy loan_invoices): the installments of
-- the schedule, late-payment fines, outstanding-balance fines, and money
-- moved out of the loan to another loan or contribution.
CREATE TYPE loan_charge_kind AS ENUM ('installment', 'late_fine', 'balance_fine', 'transfer_out');

CREATE TABLE loan_charges (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    group_id      bigint NOT NULL REFERENCES groups (id),
    loan_id       bigint NOT NULL,
    kind          loan_charge_kind NOT NULL,
    number        integer,        -- installment number in the schedule
    invoice_on    date NOT NULL,  -- when the member is reminded
    due_on        date NOT NULL,
    next_fine_on  date,           -- installments: the next day a late fine applies; NULL once it never will
    principal     numeric(14, 2) NOT NULL DEFAULT 0,
    interest      numeric(14, 2) NOT NULL DEFAULT 0,
    amount        numeric(14, 2) NOT NULL CHECK (amount >= 0),
    parent_id     bigint REFERENCES loan_charges (id), -- late fines: the installment fined
    no_fines      boolean NOT NULL DEFAULT false,      -- legacy disable_fines
    sent_at       timestamptz,    -- when the reminder went out (legacy is_sent)
    refs          jsonb NOT NULL DEFAULT '{}'::jsonb,
    legacy_type   smallint,
    generated_by  text CHECK (generated_by IN ('fines')), -- raised by the nightly job
    created_by    bigint REFERENCES users (id),
    created_at    timestamptz NOT NULL DEFAULT now(),
    voided_at     timestamptz,
    UNIQUE (group_id, id),
    FOREIGN KEY (group_id, loan_id) REFERENCES loans (group_id, id)
);
CREATE INDEX loan_charges_loan_idx ON loan_charges (loan_id, due_on, id) WHERE voided_at IS NULL;
-- A re-run of the fines job never fines the same installment (or loan) twice
-- on one day, even after a fine is voided.
CREATE UNIQUE INDEX loan_fines_once ON loan_charges (loan_id, kind, due_on, (coalesce(parent_id, 0)))
    WHERE generated_by IS NOT NULL;

-- Money paid towards a loan. Most come with a deposit (and so a cash book
-- entry); a transfer from a member's contributions or another loan has none.
CREATE TABLE loan_repayments (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    group_id      bigint NOT NULL REFERENCES groups (id),
    loan_id       bigint NOT NULL,
    membership_id bigint,
    deposit_id    bigint,
    account_id    bigint,
    method        payment_method,
    amount        numeric(14, 2) NOT NULL CHECK (amount > 0),
    paid_on       date NOT NULL,
    source        text NOT NULL DEFAULT 'deposit' CHECK (source IN ('deposit', 'transfer')),
    description   text NOT NULL DEFAULT '',
    refs          jsonb NOT NULL DEFAULT '{}'::jsonb,
    is_backdated  boolean NOT NULL DEFAULT false,
    created_by    bigint REFERENCES users (id),
    created_at    timestamptz NOT NULL DEFAULT now(),
    voided_at     timestamptz,
    voided_by     bigint REFERENCES users (id),
    UNIQUE (group_id, id),
    FOREIGN KEY (group_id, loan_id) REFERENCES loans (group_id, id),
    FOREIGN KEY (group_id, membership_id) REFERENCES memberships (group_id, id),
    FOREIGN KEY (group_id, deposit_id) REFERENCES deposits (group_id, id),
    FOREIGN KEY (group_id, account_id) REFERENCES group_accounts (group_id, id)
);
CREATE INDEX loan_repayments_loan_idx ON loan_repayments (loan_id, paid_on, id) WHERE voided_at IS NULL;
CREATE UNIQUE INDEX loan_repayments_deposit_idx ON loan_repayments (deposit_id) WHERE deposit_id IS NOT NULL;

CREATE TABLE loan_guarantors (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    group_id      bigint NOT NULL REFERENCES groups (id),
    loan_id       bigint NOT NULL,
    membership_id bigint NOT NULL,
    amount        numeric(14, 2),
    comment       text NOT NULL DEFAULT '',
    created_by    bigint REFERENCES users (id),
    created_at    timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (group_id, loan_id) REFERENCES loans (group_id, id),
    FOREIGN KEY (group_id, membership_id) REFERENCES memberships (group_id, id)
);
CREATE INDEX loan_guarantors_loan_idx ON loan_guarantors (loan_id);
CREATE INDEX loan_guarantors_member_idx ON loan_guarantors (membership_id);

-- Disbursements and repayments point at their loan.
ALTER TABLE deposits ADD COLUMN loan_id bigint,
    ADD FOREIGN KEY (group_id, loan_id) REFERENCES loans (group_id, id);
ALTER TABLE withdrawals ADD COLUMN loan_id bigint,
    ADD FOREIGN KEY (group_id, loan_id) REFERENCES loans (group_id, id);
CREATE INDEX deposits_loan_idx ON deposits (loan_id) WHERE loan_id IS NOT NULL;
CREATE INDEX withdrawals_loan_idx ON withdrawals (loan_id) WHERE loan_id IS NOT NULL;

-- +goose Down
DROP INDEX withdrawals_loan_idx;
DROP INDEX deposits_loan_idx;
ALTER TABLE withdrawals DROP COLUMN loan_id;
ALTER TABLE deposits DROP COLUMN loan_id;
DROP TABLE loan_guarantors;
DROP TABLE loan_repayments;
DROP TABLE loan_charges;
DROP TYPE loan_charge_kind;
DROP TABLE loans;
DROP TABLE loan_types;
DROP TYPE repayment_frequency;
DROP TYPE interest_rate_per;
DROP TYPE interest_type;
