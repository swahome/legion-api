-- Reference lists for sacco and mobile money accounts.

-- name: ListSaccos :many
SELECT * FROM saccos WHERE sqlc.arg(include_inactive)::bool OR is_active ORDER BY name;

-- name: ListSaccoBranches :many
SELECT * FROM sacco_branches
WHERE sacco_id = $1 AND (sqlc.arg(include_inactive)::bool OR is_active)
ORDER BY name;

-- name: ListMobileMoneyProviders :many
SELECT * FROM mobile_money_providers WHERE sqlc.arg(include_inactive)::bool OR is_active ORDER BY name;

-- Group accounts. Balance = initial balance + live cash book entries.

-- name: ListGroupAccounts :many
SELECT a.id, a.kind, a.name, a.account_number, a.initial_balance::text AS initial_balance,
       a.is_default, a.is_verified, a.is_closed,
       a.bank_id, a.bank_branch_id, a.sacco_id, a.sacco_branch_id, a.provider_id,
       coalesce(b.name, s.name, p.name, '') AS institution_name,
       coalesce(bb.name, sb.name, '') AS branch_name,
       (a.initial_balance + coalesce(e.total, 0))::text AS balance
FROM group_accounts a
LEFT JOIN banks b ON b.id = a.bank_id
LEFT JOIN bank_branches bb ON bb.id = a.bank_branch_id
LEFT JOIN saccos s ON s.id = a.sacco_id
LEFT JOIN sacco_branches sb ON sb.id = a.sacco_branch_id
LEFT JOIN mobile_money_providers p ON p.id = a.provider_id
LEFT JOIN LATERAL (
    SELECT sum(direction * amount) AS total FROM account_entries
    WHERE account_id = a.id AND voided_at IS NULL
) e ON true
WHERE a.group_id = $1 AND a.deleted_at IS NULL
ORDER BY a.is_closed, a.kind, a.is_default DESC, a.name;

-- name: UnassignedBalances :many
-- Legacy cash book entries whose account could not be identified.
SELECT account_kind, sum(direction * amount)::text AS balance, count(*) AS entries
FROM account_entries
WHERE group_id = $1 AND account_id IS NULL AND voided_at IS NULL
GROUP BY account_kind
ORDER BY account_kind;

-- name: GetGroupAccount :one
SELECT * FROM group_accounts WHERE group_id = $1 AND id = $2 AND deleted_at IS NULL;

-- name: CreateGroupAccount :one
INSERT INTO group_accounts (group_id, kind, name, account_number, bank_id, bank_branch_id,
                            sacco_id, sacco_branch_id, provider_id, initial_balance, is_default)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, sqlc.arg(initial_balance)::numeric, $10)
RETURNING id;

-- name: UpdateGroupAccount :execrows
UPDATE group_accounts SET
    name            = coalesce(sqlc.narg(name), name),
    account_number  = coalesce(sqlc.narg(account_number), account_number),
    bank_branch_id  = CASE WHEN sqlc.arg(set_branch)::bool AND kind = 'bank' THEN sqlc.narg(branch_id) ELSE bank_branch_id END,
    sacco_branch_id = CASE WHEN sqlc.arg(set_branch)::bool AND kind = 'sacco' THEN sqlc.narg(branch_id) ELSE sacco_branch_id END,
    initial_balance = coalesce(sqlc.narg(initial_balance)::numeric, initial_balance),
    is_default      = coalesce(sqlc.narg(is_default), is_default),
    is_closed       = coalesce(sqlc.narg(is_closed), is_closed),
    updated_at      = now()
WHERE group_id = sqlc.arg(group_id) AND id = sqlc.arg(id) AND deleted_at IS NULL;

-- name: ClearDefaultAccounts :exec
-- One default per kind, as in the legacy app.
UPDATE group_accounts SET is_default = false, updated_at = now()
WHERE group_id = $1 AND kind = $2 AND id <> $3 AND is_default;

-- name: CountAccountEntries :one
SELECT count(*) FROM account_entries WHERE account_id = $1 AND voided_at IS NULL;

-- name: DeleteGroupAccount :exec
UPDATE group_accounts SET deleted_at = now(), is_default = false, updated_at = now()
WHERE group_id = $1 AND id = $2;

-- name: BankBranchBelongs :one
SELECT EXISTS (SELECT 1 FROM bank_branches WHERE id = $1 AND bank_id = $2);

-- name: SaccoBranchBelongs :one
SELECT EXISTS (SELECT 1 FROM sacco_branches WHERE id = $1 AND sacco_id = $2);

-- name: InstitutionExists :one
SELECT (sqlc.arg(kind)::account_kind = 'bank' AND EXISTS (SELECT 1 FROM banks b WHERE b.id = sqlc.arg(institution_id)::bigint))
    OR (sqlc.arg(kind)::account_kind = 'sacco' AND EXISTS (SELECT 1 FROM saccos s WHERE s.id = sqlc.arg(institution_id)::bigint))
    OR (sqlc.arg(kind)::account_kind = 'mobile_money' AND EXISTS (SELECT 1 FROM mobile_money_providers p WHERE p.id = sqlc.arg(institution_id)::bigint))
    AS found;

-- Cash book.

-- name: CashBookOpening :one
SELECT coalesce(sum(direction * amount), 0)::text
FROM account_entries
WHERE group_id = sqlc.arg(group_id) AND voided_at IS NULL
  AND (sqlc.narg(account_id)::bigint IS NULL OR account_id = sqlc.narg(account_id))
  AND entry_date < sqlc.arg(before)::date;

-- name: InitialBalances :one
SELECT coalesce(sum(initial_balance), 0)::text FROM group_accounts
WHERE group_id = sqlc.arg(group_id) AND deleted_at IS NULL
  AND (sqlc.narg(account_id)::bigint IS NULL OR id = sqlc.narg(account_id));

-- name: CashBook :many
SELECT e.id, e.entry_date, e.direction, e.amount::text AS amount, e.description, e.account_id,
       e.account_kind, a.name AS account_name, e.deposit_id, e.withdrawal_id, e.account_transfer_id,
       e.legacy_type, d.kind AS deposit_kind, w.kind AS withdrawal_kind,
       e.membership_id, coalesce(trim(u.first_name || ' ' || u.last_name), '')::text AS member_name
FROM account_entries e
LEFT JOIN group_accounts a ON a.id = e.account_id
LEFT JOIN deposits d ON d.id = e.deposit_id
LEFT JOIN withdrawals w ON w.id = e.withdrawal_id
LEFT JOIN memberships m ON m.id = e.membership_id
LEFT JOIN users u ON u.id = m.user_id
WHERE e.group_id = sqlc.arg(group_id) AND e.voided_at IS NULL
  AND (sqlc.narg(account_id)::bigint IS NULL OR e.account_id = sqlc.narg(account_id))
  AND e.entry_date >= sqlc.arg(from_date)::date AND e.entry_date <= sqlc.arg(to_date)::date
ORDER BY e.entry_date, e.id
LIMIT 5001;

-- Categories and depositors.

-- name: ListIncomeCategories :many
SELECT * FROM income_categories
WHERE group_id = $1 AND deleted_at IS NULL AND (sqlc.arg(include_inactive)::bool OR (is_active AND NOT is_hidden))
ORDER BY name;

-- name: CreateIncomeCategory :one
INSERT INTO income_categories (group_id, name, description) VALUES ($1, $2, $3) RETURNING *;

-- name: UpdateIncomeCategory :one
UPDATE income_categories SET
    name        = coalesce(sqlc.narg(name), name),
    description = coalesce(sqlc.narg(description), description),
    is_hidden   = coalesce(sqlc.narg(is_hidden), is_hidden),
    is_active   = coalesce(sqlc.narg(is_active), is_active),
    updated_at  = now()
WHERE group_id = sqlc.arg(group_id) AND id = sqlc.arg(id) AND deleted_at IS NULL
RETURNING *;

-- name: ListExpenseCategories :many
SELECT * FROM expense_categories
WHERE group_id = $1 AND deleted_at IS NULL AND (sqlc.arg(include_inactive)::bool OR (is_active AND NOT is_hidden))
ORDER BY name;

-- name: CreateExpenseCategory :one
INSERT INTO expense_categories (group_id, name, description) VALUES ($1, $2, $3) RETURNING *;

-- name: UpdateExpenseCategory :one
UPDATE expense_categories SET
    name        = coalesce(sqlc.narg(name), name),
    description = coalesce(sqlc.narg(description), description),
    is_hidden   = coalesce(sqlc.narg(is_hidden), is_hidden),
    is_active   = coalesce(sqlc.narg(is_active), is_active),
    updated_at  = now()
WHERE group_id = sqlc.arg(group_id) AND id = sqlc.arg(id) AND deleted_at IS NULL
RETURNING *;

-- name: ListFineCategories :many
SELECT * FROM fine_categories
WHERE group_id = $1 AND deleted_at IS NULL AND (sqlc.arg(include_inactive)::bool OR (is_active AND NOT is_hidden))
ORDER BY name;

-- name: CreateFineCategory :one
INSERT INTO fine_categories (group_id, name, description, amount)
VALUES ($1, $2, $3, sqlc.arg(amount)::numeric) RETURNING *;

-- name: UpdateFineCategory :one
UPDATE fine_categories SET
    name        = coalesce(sqlc.narg(name), name),
    description = coalesce(sqlc.narg(description), description),
    amount      = coalesce(sqlc.narg(amount)::numeric, amount),
    is_hidden   = coalesce(sqlc.narg(is_hidden), is_hidden),
    is_active   = coalesce(sqlc.narg(is_active), is_active),
    updated_at  = now()
WHERE group_id = sqlc.arg(group_id) AND id = sqlc.arg(id) AND deleted_at IS NULL
RETURNING *;

-- name: ListDepositors :many
SELECT * FROM depositors
WHERE group_id = $1 AND (sqlc.arg(include_inactive)::bool OR is_active)
ORDER BY name;

-- name: CreateDepositor :one
INSERT INTO depositors (group_id, name, email, phone, description) VALUES ($1, $2, $3, $4, $5) RETURNING *;

-- name: UpdateDepositor :one
UPDATE depositors SET
    name        = coalesce(sqlc.narg(name), name),
    email       = coalesce(sqlc.narg(email), email),
    phone       = coalesce(sqlc.narg(phone), phone),
    description = coalesce(sqlc.narg(description), description),
    is_active   = coalesce(sqlc.narg(is_active), is_active),
    updated_at  = now()
WHERE group_id = sqlc.arg(group_id) AND id = sqlc.arg(id)
RETURNING *;

-- Deposits.

-- name: ListDeposits :many
SELECT d.id, d.kind, d.method, d.amount::text AS amount, d.deposit_date, d.description,
       d.account_id, a.name AS account_name, d.account_kind,
       d.membership_id, coalesce(trim(u.first_name || ' ' || u.last_name), '')::text AS member_name,
       d.contribution_id, c.name AS contribution_name,
       d.fine_category_id, fc.name AS fine_category_name,
       d.income_category_id, ic.name AS income_category_name,
       d.depositor_id, dp.name AS depositor_name,
       d.created_at, d.voided_at,
       count(*) OVER () AS total
FROM deposits d
LEFT JOIN group_accounts a ON a.id = d.account_id
LEFT JOIN memberships m ON m.id = d.membership_id
LEFT JOIN users u ON u.id = m.user_id
LEFT JOIN contributions c ON c.id = d.contribution_id
LEFT JOIN fine_categories fc ON fc.id = d.fine_category_id
LEFT JOIN income_categories ic ON ic.id = d.income_category_id
LEFT JOIN depositors dp ON dp.id = d.depositor_id
WHERE d.group_id = sqlc.arg(group_id)
  AND (sqlc.arg(include_voided)::bool OR d.voided_at IS NULL)
  AND (sqlc.narg(kind)::deposit_kind IS NULL OR d.kind = sqlc.narg(kind))
  AND (sqlc.narg(membership_id)::bigint IS NULL OR d.membership_id = sqlc.narg(membership_id))
  AND (sqlc.narg(account_id)::bigint IS NULL OR d.account_id = sqlc.narg(account_id))
  AND (sqlc.narg(from_date)::date IS NULL OR d.deposit_date >= sqlc.narg(from_date))
  AND (sqlc.narg(to_date)::date IS NULL OR d.deposit_date <= sqlc.narg(to_date))
ORDER BY d.deposit_date DESC, d.id DESC
LIMIT sqlc.arg(page_size) OFFSET sqlc.arg(page_offset);

-- name: DepositTotals :many
SELECT kind, sum(amount)::text AS amount, count(*) AS deposits
FROM deposits
WHERE group_id = sqlc.arg(group_id) AND voided_at IS NULL
  AND (sqlc.narg(membership_id)::bigint IS NULL OR membership_id = sqlc.narg(membership_id))
  AND (sqlc.narg(account_id)::bigint IS NULL OR account_id = sqlc.narg(account_id))
  AND (sqlc.narg(from_date)::date IS NULL OR deposit_date >= sqlc.narg(from_date))
  AND (sqlc.narg(to_date)::date IS NULL OR deposit_date <= sqlc.narg(to_date))
GROUP BY kind
ORDER BY kind;

-- name: GetDepositView :one
-- Same columns as ListDeposits, so the rows convert.
SELECT d.id, d.kind, d.method, d.amount::text AS amount, d.deposit_date, d.description,
       d.account_id, a.name AS account_name, d.account_kind,
       d.membership_id, coalesce(trim(u.first_name || ' ' || u.last_name), '')::text AS member_name,
       d.contribution_id, c.name AS contribution_name,
       d.fine_category_id, fc.name AS fine_category_name,
       d.income_category_id, ic.name AS income_category_name,
       d.depositor_id, dp.name AS depositor_name,
       d.created_at, d.voided_at,
       1::bigint AS total
FROM deposits d
LEFT JOIN group_accounts a ON a.id = d.account_id
LEFT JOIN memberships m ON m.id = d.membership_id
LEFT JOIN users u ON u.id = m.user_id
LEFT JOIN contributions c ON c.id = d.contribution_id
LEFT JOIN fine_categories fc ON fc.id = d.fine_category_id
LEFT JOIN income_categories ic ON ic.id = d.income_category_id
LEFT JOIN depositors dp ON dp.id = d.depositor_id
WHERE d.group_id = $1 AND d.id = $2;

-- name: GetDeposit :one
SELECT * FROM deposits WHERE group_id = $1 AND id = $2;

-- name: CreateDeposit :one
INSERT INTO deposits (group_id, account_id, account_kind, kind, method, membership_id, contribution_id,
                      fine_category_id, income_category_id, depositor_id, amount, deposit_date,
                      description, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, sqlc.arg(amount)::numeric, $11, $12, $13)
RETURNING id;

-- name: CreateAccountEntry :exec
INSERT INTO account_entries (group_id, account_id, account_kind, direction, amount, entry_date,
                             description, deposit_id, withdrawal_id, account_transfer_id, membership_id)
VALUES ($1, $2, $3, $4, sqlc.arg(amount)::numeric, $5, $6, $7, $8, $9, $10);

-- name: CreatePaymentLedgerEntry :exec
INSERT INTO ledger_entries (group_id, membership_id, contribution_id, fine_category_id, entry_type,
                            amount, entry_date, description, deposit_id)
VALUES ($1, $2, $3, $4, $5, sqlc.arg(amount)::numeric, $6, $7, $8);

-- name: VoidDeposit :execrows
UPDATE deposits SET voided_at = now(), voided_by = $3
WHERE group_id = $1 AND id = $2 AND voided_at IS NULL;

-- name: VoidDepositEntries :exec
UPDATE account_entries SET voided_at = now() WHERE deposit_id = $1 AND voided_at IS NULL;

-- name: VoidDepositLedger :exec
UPDATE ledger_entries SET voided_at = now() WHERE deposit_id = $1 AND voided_at IS NULL;

-- name: LedgerEntriesForDeposit :many
SELECT id, entry_type, amount::text AS amount, voided_at FROM ledger_entries WHERE deposit_id = $1 ORDER BY id;

-- name: ActiveMembershipInGroup :one
SELECT EXISTS (SELECT 1 FROM memberships WHERE group_id = $1 AND id = $2 AND status <> 'removed');

-- name: ContributionInGroup :one
SELECT EXISTS (SELECT 1 FROM contributions WHERE group_id = $1 AND id = $2 AND deleted_at IS NULL);

-- name: FineCategoryInGroup :one
SELECT EXISTS (SELECT 1 FROM fine_categories WHERE group_id = $1 AND id = $2 AND deleted_at IS NULL);

-- name: IncomeCategoryInGroup :one
SELECT EXISTS (SELECT 1 FROM income_categories WHERE group_id = $1 AND id = $2 AND deleted_at IS NULL);

-- name: DepositorInGroup :one
SELECT EXISTS (SELECT 1 FROM depositors WHERE group_id = $1 AND id = $2);
