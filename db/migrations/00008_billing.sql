-- +goose Up
-- Milestone 3, part 3: the nightly billing job (contribution invoices and
-- late-payment fines) and fines that officials raise by hand.

-- Where an entry came from. NULL = imported or entered by a person;
-- 'invoicing' and 'fines' = raised by the billing job. A generated entry is
-- never raised twice for the same member, date and cause, even after it is
-- voided: the unique index below is what makes a re-run harmless.
ALTER TABLE ledger_entries
    ADD COLUMN generated_by    text CHECK (generated_by IN ('invoicing', 'fines')),
    ADD COLUMN parent_entry_id bigint REFERENCES ledger_entries (id),
    ADD COLUMN fine_rule_id    bigint REFERENCES contribution_fine_rules (id) ON DELETE SET NULL,
    ADD COLUMN created_by      bigint REFERENCES users (id),
    ADD COLUMN voided_by       bigint REFERENCES users (id);
COMMENT ON COLUMN ledger_entries.parent_entry_id IS
    'For a contribution fine: the contribution invoice it fines (legacy invoices.parent_id)';
CREATE UNIQUE INDEX ledger_generated_once ON ledger_entries (
    membership_id, contribution_id, entry_type, entry_date,
    (coalesce(parent_entry_id, 0)), (coalesce(fine_rule_id, 0))
) WHERE generated_by IS NOT NULL;
CREATE INDEX ledger_parent_idx ON ledger_entries (parent_entry_id) WHERE parent_entry_id IS NOT NULL;
CREATE INDEX ledger_contribution_idx ON ledger_entries (contribution_id, membership_id) WHERE voided_at IS NULL;
CREATE INDEX ledger_fines_idx ON ledger_entries (group_id, entry_date DESC, id DESC)
    WHERE entry_type IN ('fine_invoice', 'contribution_fine_invoice');

-- The next day a fine rule charges (legacy contribution_fine_settings.fine_date).
ALTER TABLE contribution_fine_rules
    ADD COLUMN next_fine_date date,
    ADD COLUMN updated_at timestamptz NOT NULL DEFAULT now();

-- One row per run of a background job, for the admin panel.
CREATE TABLE job_runs (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    job         text NOT NULL,
    run_date    date NOT NULL,
    trigger     text NOT NULL CHECK (trigger IN ('schedule', 'manual', 'cli')),
    started_at  timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz,
    status      text NOT NULL DEFAULT 'running' CHECK (status IN ('running', 'succeeded', 'failed')),
    summary     jsonb NOT NULL DEFAULT '{}'::jsonb,
    error       text NOT NULL DEFAULT '',
    started_by  bigint REFERENCES users (id)
);
CREATE INDEX job_runs_job_idx ON job_runs (job, started_at DESC);

-- +goose Down
DROP TABLE job_runs;
ALTER TABLE contribution_fine_rules DROP COLUMN updated_at, DROP COLUMN next_fine_date;
DROP INDEX ledger_fines_idx;
DROP INDEX ledger_contribution_idx;
DROP INDEX ledger_parent_idx;
DROP INDEX ledger_generated_once;
ALTER TABLE ledger_entries
    DROP COLUMN voided_by,
    DROP COLUMN created_by,
    DROP COLUMN fine_rule_id,
    DROP COLUMN parent_entry_id,
    DROP COLUMN generated_by;
