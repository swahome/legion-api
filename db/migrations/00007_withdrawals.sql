-- +goose Up
-- Milestone 3, part 2: recording money out, transfers between accounts and
-- withdrawal requests that the group's signatories approve.

-- A refund paid out of an account takes the money off the member's
-- contribution balance, so its ledger row points back at the withdrawal
-- and is voided with it.
ALTER TABLE ledger_entries
    ADD COLUMN withdrawal_id bigint,
    ADD CONSTRAINT ledger_entries_withdrawal_fk FOREIGN KEY (group_id, withdrawal_id) REFERENCES withdrawals (group_id, id);
CREATE INDEX ledger_withdrawal_idx ON ledger_entries (withdrawal_id) WHERE withdrawal_id IS NOT NULL;

ALTER TABLE account_transfers ADD COLUMN voided_by bigint REFERENCES users (id);
CREATE INDEX account_transfers_group_idx ON account_transfers (group_id, transfer_date DESC, id DESC);
CREATE INDEX account_entries_transfer_idx ON account_entries (account_transfer_id) WHERE account_transfer_id IS NOT NULL;

-- Legacy withdrawal_requests (the e-wallet "withdraw money" flow). An
-- official asks to pay money out; every signatory gets an approval row;
-- one decline declines it and all approving approves it. An approved
-- request becomes a withdrawal (or transfer) when it is paid.
CREATE TYPE withdrawal_request_status AS ENUM ('pending', 'approved', 'declined', 'cancelled', 'paid', 'failed');
CREATE TYPE approval_decision AS ENUM ('pending', 'approved', 'declined');

CREATE TABLE withdrawal_requests (
    id                  bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    legacy_id           integer UNIQUE,
    group_id            bigint NOT NULL REFERENCES groups (id),
    reference_number    text NOT NULL DEFAULT '',
    kind                withdrawal_kind NOT NULL,
    status              withdrawal_request_status NOT NULL DEFAULT 'pending',
    requested_by        bigint,                  -- membership that asked
    account_id          bigint,                  -- pay from
    to_account_id       bigint,                  -- funds transfers
    amount              numeric(14, 2) NOT NULL CHECK (amount > 0),
    description         text NOT NULL DEFAULT '',
    expense_category_id bigint,
    membership_id       bigint,                  -- member paid (refunds)
    contribution_id     bigint,
    recipient           jsonb NOT NULL DEFAULT '{}'::jsonb, -- phone, paybill or bank details
    decided_at          timestamptz,
    decline_reason      text NOT NULL DEFAULT '',
    withdrawal_id       bigint,
    account_transfer_id bigint,
    refs                jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_by          bigint REFERENCES users (id),
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    UNIQUE (group_id, id),
    FOREIGN KEY (group_id, requested_by) REFERENCES memberships (group_id, id),
    FOREIGN KEY (group_id, account_id) REFERENCES group_accounts (group_id, id),
    FOREIGN KEY (group_id, to_account_id) REFERENCES group_accounts (group_id, id),
    FOREIGN KEY (group_id, expense_category_id) REFERENCES expense_categories (group_id, id),
    FOREIGN KEY (group_id, membership_id) REFERENCES memberships (group_id, id),
    FOREIGN KEY (group_id, contribution_id) REFERENCES contributions (group_id, id),
    FOREIGN KEY (group_id, withdrawal_id) REFERENCES withdrawals (group_id, id),
    FOREIGN KEY (group_id, account_transfer_id) REFERENCES account_transfers (group_id, id)
);
CREATE INDEX withdrawal_requests_group_idx ON withdrawal_requests (group_id, created_at DESC, id DESC);

-- Legacy withdrawal_approval_requests: one row per signatory per request.
CREATE TABLE withdrawal_approvals (
    id                    bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    legacy_id             integer UNIQUE,
    group_id              bigint NOT NULL REFERENCES groups (id),
    withdrawal_request_id bigint NOT NULL,
    membership_id         bigint NOT NULL,
    decision              approval_decision NOT NULL DEFAULT 'pending',
    decided_at            timestamptz,
    comment               text NOT NULL DEFAULT '',
    decided_by            bigint REFERENCES users (id),
    created_at            timestamptz NOT NULL DEFAULT now(),
    UNIQUE (withdrawal_request_id, membership_id),
    FOREIGN KEY (group_id, withdrawal_request_id) REFERENCES withdrawal_requests (group_id, id),
    FOREIGN KEY (group_id, membership_id) REFERENCES memberships (group_id, id)
);
CREATE INDEX withdrawal_approvals_member_idx ON withdrawal_approvals (membership_id) WHERE decision = 'pending';

-- +goose Down
DROP TABLE withdrawal_approvals;
DROP TABLE withdrawal_requests;
DROP TYPE approval_decision;
DROP TYPE withdrawal_request_status;
DROP INDEX account_entries_transfer_idx;
DROP INDEX account_transfers_group_idx;
ALTER TABLE account_transfers DROP COLUMN voided_by;
DROP INDEX ledger_withdrawal_idx;
ALTER TABLE ledger_entries
    DROP CONSTRAINT ledger_entries_withdrawal_fk,
    DROP COLUMN withdrawal_id;
