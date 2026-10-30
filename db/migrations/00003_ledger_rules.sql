-- +goose Up
-- Ledger amounts are stored as positive numbers, exactly as the legacy
-- `statements` table holds them. What an entry does to a member's balance
-- lives in ledger_entry_rules, which encodes the type lists the legacy
-- Statements_m balance functions use:
--   contribution owed:  1, 21-24, 25, 27, 30    contribution paid: 9, 10, 11, 15, 26
--   fine owed:          2, 3                    fine paid:         12, 13, 14, 16, 28
-- Legacy types 29 and 31 (loan payment moved to a contribution or fine) are
-- not counted by any legacy balance, so their bucket is 'none'.
COMMENT ON COLUMN ledger_entries.amount IS 'Always positive; see ledger_entry_rules for the effect on balances';

ALTER TABLE ledger_entries ADD COLUMN is_backdated boolean NOT NULL DEFAULT false;

CREATE TABLE ledger_entry_rules (
    entry_type ledger_entry_type PRIMARY KEY,
    bucket     text NOT NULL CHECK (bucket IN ('contribution', 'fine', 'misc', 'none')),
    sign       smallint NOT NULL CHECK (sign IN (-1, 0, 1)) -- +1 increases what the member owes
);

INSERT INTO ledger_entry_rules (entry_type, bucket, sign) VALUES
    ('contribution_invoice',          'contribution',  1),
    ('contribution_refund',           'contribution',  1),
    ('contribution_transfer_out',     'contribution',  1),
    ('contribution_to_loan_transfer', 'contribution',  1),
    ('contribution_payment',          'contribution', -1),
    ('contribution_transfer_in',      'contribution', -1),
    ('contribution_fine_invoice',     'fine',          1),
    ('fine_invoice',                  'fine',          1),
    ('fine_payment',                  'fine',         -1),
    ('contribution_to_fine_transfer', 'fine',         -1),
    ('misc_invoice',                  'misc',          1),
    ('misc_payment',                  'misc',         -1),
    ('loan_to_contribution_transfer', 'none',          0),
    ('loan_to_fine_transfer',         'none',          0);

-- +goose Down
DROP TABLE ledger_entry_rules;
ALTER TABLE ledger_entries DROP COLUMN is_backdated;
COMMENT ON COLUMN ledger_entries.amount IS NULL;
