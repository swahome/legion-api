-- +goose Up
-- Milestone 4b: money the group borrows from banks (legacy bank_loans and
-- bank_loan_repayments) and loans the group makes to people who are not
-- members (legacy debtors, debtor_loans, debtor_loan_types and their
-- invoices, repayments and guarantors).

-- A loan the group took from a bank or another lender. The money came into
-- account_id as a bank_loan deposit; each repayment is a bank_loan_repayment
-- withdrawal that points back here. What is still owed is opening_balance
-- less the live repayments: opening_balance is total_payable for a new loan,
-- or what was left when an older loan was first entered.
CREATE TABLE bank_loans (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    legacy_id       integer UNIQUE,
    group_id        bigint NOT NULL REFERENCES groups (id),
    description     text NOT NULL DEFAULT '',
    amount          numeric(14, 2) NOT NULL CHECK (amount > 0),         -- legacy amount_loaned
    total_payable   numeric(14, 2) NOT NULL CHECK (total_payable >= 0), -- legacy total_loan_amount_payable
    opening_balance numeric(14, 2) NOT NULL CHECK (opening_balance >= 0),
    starts_on       date NOT NULL,
    ends_on         date NOT NULL CHECK (ends_on >= starts_on),
    account_id      bigint,
    created_by      bigint REFERENCES users (id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    voided_at       timestamptz,
    voided_by       bigint REFERENCES users (id),
    UNIQUE (group_id, id),
    FOREIGN KEY (group_id, account_id) REFERENCES group_accounts (group_id, id)
);
CREATE INDEX bank_loans_group_idx ON bank_loans (group_id, starts_on DESC, id DESC);

ALTER TABLE deposits ADD COLUMN bank_loan_id bigint,
    ADD FOREIGN KEY (group_id, bank_loan_id) REFERENCES bank_loans (group_id, id);
ALTER TABLE withdrawals ADD COLUMN bank_loan_id bigint,
    ADD FOREIGN KEY (group_id, bank_loan_id) REFERENCES bank_loans (group_id, id);
CREATE INDEX deposits_bank_loan_idx ON deposits (bank_loan_id) WHERE bank_loan_id IS NOT NULL;
CREATE INDEX withdrawals_bank_loan_idx ON withdrawals (bank_loan_id) WHERE bank_loan_id IS NOT NULL;

-- Someone outside the group who borrows from it.
CREATE TABLE debtors (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    legacy_id   integer UNIQUE,
    group_id    bigint NOT NULL REFERENCES groups (id),
    name        text NOT NULL CHECK (name <> ''),
    phone       text NOT NULL DEFAULT '',
    email       text NOT NULL DEFAULT '',
    id_number   text NOT NULL DEFAULT '',
    description text NOT NULL DEFAULT '',
    is_active   boolean NOT NULL DEFAULT true,
    created_by  bigint REFERENCES users (id),
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    deleted_at  timestamptz,
    UNIQUE (group_id, id)
);
CREATE INDEX debtors_group_idx ON debtors (group_id) WHERE deleted_at IS NULL;

-- Debtor loans run on the same tables as member loans, so schedules, fines,
-- repayments and statements work the same way: a loan belongs to exactly
-- one member or one debtor. Loan types are offered to one or the other.
ALTER TABLE loan_types ADD COLUMN for_debtors boolean NOT NULL DEFAULT false;

ALTER TABLE loans ALTER COLUMN membership_id DROP NOT NULL,
    ADD COLUMN debtor_id bigint,
    ADD FOREIGN KEY (group_id, debtor_id) REFERENCES debtors (group_id, id),
    ADD CONSTRAINT loans_one_borrower CHECK ((membership_id IS NULL) <> (debtor_id IS NULL));
CREATE INDEX loans_debtor_idx ON loans (debtor_id) WHERE debtor_id IS NOT NULL;

-- +goose Down
DROP INDEX loans_debtor_idx;
ALTER TABLE loans DROP CONSTRAINT loans_one_borrower, DROP COLUMN debtor_id;
ALTER TABLE loans ALTER COLUMN membership_id SET NOT NULL;
ALTER TABLE loan_types DROP COLUMN for_debtors;
DROP TABLE debtors;
DROP INDEX withdrawals_bank_loan_idx;
DROP INDEX deposits_bank_loan_idx;
ALTER TABLE withdrawals DROP COLUMN bank_loan_id;
ALTER TABLE deposits DROP COLUMN bank_loan_id;
DROP TABLE bank_loans;
