-- Withdrawals: money out of a group account.

-- name: ListWithdrawals :many
SELECT w.id, w.kind, w.method, w.amount::text AS amount, w.withdrawal_date, w.description,
       w.account_id, a.name AS account_name, w.account_kind,
       w.membership_id, coalesce(trim(u.first_name || ' ' || u.last_name), '')::text AS member_name,
       w.contribution_id, c.name AS contribution_name,
       w.expense_category_id, ec.name AS expense_category_name,
       w.created_at, w.voided_at,
       count(*) OVER () AS total
FROM withdrawals w
LEFT JOIN group_accounts a ON a.id = w.account_id
LEFT JOIN memberships m ON m.id = w.membership_id
LEFT JOIN users u ON u.id = m.user_id
LEFT JOIN contributions c ON c.id = w.contribution_id
LEFT JOIN expense_categories ec ON ec.id = w.expense_category_id
WHERE w.group_id = sqlc.arg(group_id)
  AND (sqlc.arg(include_voided)::bool OR w.voided_at IS NULL)
  AND (sqlc.narg(kind)::withdrawal_kind IS NULL OR w.kind = sqlc.narg(kind))
  AND (sqlc.narg(membership_id)::bigint IS NULL OR w.membership_id = sqlc.narg(membership_id))
  AND (sqlc.narg(account_id)::bigint IS NULL OR w.account_id = sqlc.narg(account_id))
  AND (sqlc.narg(from_date)::date IS NULL OR w.withdrawal_date >= sqlc.narg(from_date))
  AND (sqlc.narg(to_date)::date IS NULL OR w.withdrawal_date <= sqlc.narg(to_date))
ORDER BY w.withdrawal_date DESC, w.id DESC
LIMIT sqlc.arg(page_size) OFFSET sqlc.arg(page_offset);

-- name: WithdrawalTotals :many
SELECT kind, sum(amount)::text AS amount, count(*) AS withdrawals
FROM withdrawals
WHERE group_id = sqlc.arg(group_id) AND voided_at IS NULL
  AND (sqlc.narg(membership_id)::bigint IS NULL OR membership_id = sqlc.narg(membership_id))
  AND (sqlc.narg(account_id)::bigint IS NULL OR account_id = sqlc.narg(account_id))
  AND (sqlc.narg(from_date)::date IS NULL OR withdrawal_date >= sqlc.narg(from_date))
  AND (sqlc.narg(to_date)::date IS NULL OR withdrawal_date <= sqlc.narg(to_date))
GROUP BY kind
ORDER BY kind;

-- name: GetWithdrawalView :one
-- Same columns as ListWithdrawals, so the rows convert.
SELECT w.id, w.kind, w.method, w.amount::text AS amount, w.withdrawal_date, w.description,
       w.account_id, a.name AS account_name, w.account_kind,
       w.membership_id, coalesce(trim(u.first_name || ' ' || u.last_name), '')::text AS member_name,
       w.contribution_id, c.name AS contribution_name,
       w.expense_category_id, ec.name AS expense_category_name,
       w.created_at, w.voided_at,
       1::bigint AS total
FROM withdrawals w
LEFT JOIN group_accounts a ON a.id = w.account_id
LEFT JOIN memberships m ON m.id = w.membership_id
LEFT JOIN users u ON u.id = m.user_id
LEFT JOIN contributions c ON c.id = w.contribution_id
LEFT JOIN expense_categories ec ON ec.id = w.expense_category_id
WHERE w.group_id = $1 AND w.id = $2;

-- name: GetWithdrawal :one
SELECT * FROM withdrawals WHERE group_id = $1 AND id = $2;

-- name: CreateWithdrawal :one
INSERT INTO withdrawals (group_id, account_id, account_kind, kind, method, membership_id, contribution_id,
                         expense_category_id, amount, withdrawal_date, description, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, sqlc.arg(amount)::numeric, $9, $10, $11)
RETURNING id;

-- name: CreateRefundLedgerEntry :exec
INSERT INTO ledger_entries (group_id, membership_id, contribution_id, entry_type, amount, entry_date,
                            description, withdrawal_id)
VALUES ($1, $2, $3, 'contribution_refund', sqlc.arg(amount)::numeric, $4, $5, $6);

-- name: VoidWithdrawal :execrows
UPDATE withdrawals SET voided_at = now(), voided_by = $3
WHERE group_id = $1 AND id = $2 AND voided_at IS NULL;

-- name: VoidWithdrawalEntries :exec
UPDATE account_entries SET voided_at = now() WHERE withdrawal_id = $1 AND voided_at IS NULL;

-- name: VoidWithdrawalLedger :exec
UPDATE ledger_entries SET voided_at = now() WHERE withdrawal_id = $1 AND voided_at IS NULL;

-- name: ExpenseCategoryInGroup :one
SELECT EXISTS (SELECT 1 FROM expense_categories WHERE group_id = $1 AND id = $2 AND deleted_at IS NULL);

-- name: AccountBalance :one
SELECT (a.initial_balance + coalesce((
    SELECT sum(direction * amount) FROM account_entries
    WHERE account_id = a.id AND voided_at IS NULL), 0))::text AS balance
FROM group_accounts a WHERE a.group_id = $1 AND a.id = $2;

-- name: ContributionPaidByMember :one
-- What a member has paid into a contribution, less refunds and transfers
-- out: the most that can be refunded.
SELECT coalesce(sum(CASE
    WHEN entry_type IN ('contribution_payment', 'contribution_transfer_in', 'loan_to_contribution_transfer') THEN amount
    WHEN entry_type IN ('contribution_refund', 'contribution_transfer_out', 'contribution_to_loan_transfer',
                        'contribution_to_fine_transfer') THEN -amount
    ELSE 0 END), 0)::text AS paid
FROM ledger_entries
WHERE group_id = $1 AND membership_id = $2 AND contribution_id = $3 AND voided_at IS NULL;

-- Transfers between the group's own accounts.

-- name: ListAccountTransfers :many
SELECT t.id, t.amount::text AS amount, t.transfer_date, t.description,
       t.from_account_id, fa.name AS from_account_name, t.to_account_id, ta.name AS to_account_name,
       t.created_at, t.voided_at, count(*) OVER () AS total
FROM account_transfers t
LEFT JOIN group_accounts fa ON fa.id = t.from_account_id
LEFT JOIN group_accounts ta ON ta.id = t.to_account_id
WHERE t.group_id = sqlc.arg(group_id)
  AND (sqlc.arg(include_voided)::bool OR t.voided_at IS NULL)
  AND (sqlc.narg(account_id)::bigint IS NULL OR sqlc.narg(account_id) IN (t.from_account_id, t.to_account_id))
  AND (sqlc.narg(from_date)::date IS NULL OR t.transfer_date >= sqlc.narg(from_date))
  AND (sqlc.narg(to_date)::date IS NULL OR t.transfer_date <= sqlc.narg(to_date))
ORDER BY t.transfer_date DESC, t.id DESC
LIMIT sqlc.arg(page_size) OFFSET sqlc.arg(page_offset);

-- name: GetAccountTransferView :one
SELECT t.id, t.amount::text AS amount, t.transfer_date, t.description,
       t.from_account_id, fa.name AS from_account_name, t.to_account_id, ta.name AS to_account_name,
       t.created_at, t.voided_at, 1::bigint AS total
FROM account_transfers t
LEFT JOIN group_accounts fa ON fa.id = t.from_account_id
LEFT JOIN group_accounts ta ON ta.id = t.to_account_id
WHERE t.group_id = $1 AND t.id = $2;

-- name: CreateAccountTransfer :one
INSERT INTO account_transfers (group_id, from_account_id, to_account_id, amount, transfer_date, description, created_by)
VALUES ($1, $2, $3, sqlc.arg(amount)::numeric, $4, $5, $6)
RETURNING id;

-- name: VoidAccountTransfer :execrows
UPDATE account_transfers SET voided_at = now(), voided_by = $3
WHERE group_id = $1 AND id = $2 AND voided_at IS NULL;

-- name: AccountTransferExists :one
SELECT EXISTS (SELECT 1 FROM account_transfers WHERE group_id = $1 AND id = $2);

-- name: VoidAccountTransferEntries :exec
UPDATE account_entries SET voided_at = now() WHERE account_transfer_id = $1 AND voided_at IS NULL;

-- Withdrawal requests and their approvals.

-- name: ListWithdrawalRequests :many
SELECT r.id, r.reference_number, r.kind, r.status, r.amount::text AS amount, r.description,
       r.account_id, a.name AS account_name, r.to_account_id, ta.name AS to_account_name,
       r.expense_category_id, ec.name AS expense_category_name,
       r.membership_id, coalesce(trim(u.first_name || ' ' || u.last_name), '')::text AS member_name,
       r.contribution_id, c.name AS contribution_name,
       r.requested_by, coalesce(trim(ru.first_name || ' ' || ru.last_name), '')::text AS requested_by_name,
       r.recipient, r.decided_at, r.decline_reason, r.withdrawal_id, r.account_transfer_id, r.created_at,
       (SELECT count(*) FROM withdrawal_approvals x WHERE x.withdrawal_request_id = r.id AND x.decision = 'approved') AS approvals,
       (SELECT count(*) FROM withdrawal_approvals x WHERE x.withdrawal_request_id = r.id) AS signatories,
       count(*) OVER () AS total
FROM withdrawal_requests r
LEFT JOIN group_accounts a ON a.id = r.account_id
LEFT JOIN group_accounts ta ON ta.id = r.to_account_id
LEFT JOIN expense_categories ec ON ec.id = r.expense_category_id
LEFT JOIN memberships m ON m.id = r.membership_id
LEFT JOIN users u ON u.id = m.user_id
LEFT JOIN contributions c ON c.id = r.contribution_id
LEFT JOIN memberships rm ON rm.id = r.requested_by
LEFT JOIN users ru ON ru.id = rm.user_id
WHERE r.group_id = sqlc.arg(group_id)
  AND (sqlc.narg(status)::withdrawal_request_status IS NULL OR r.status = sqlc.narg(status))
  AND (sqlc.narg(awaiting)::bigint IS NULL OR EXISTS (
        SELECT 1 FROM withdrawal_approvals x
        WHERE x.withdrawal_request_id = r.id AND x.membership_id = sqlc.narg(awaiting) AND x.decision = 'pending'))
ORDER BY r.created_at DESC, r.id DESC
LIMIT sqlc.arg(page_size) OFFSET sqlc.arg(page_offset);

-- name: GetWithdrawalRequestView :one
-- Same columns as ListWithdrawalRequests, so the rows convert.
SELECT r.id, r.reference_number, r.kind, r.status, r.amount::text AS amount, r.description,
       r.account_id, a.name AS account_name, r.to_account_id, ta.name AS to_account_name,
       r.expense_category_id, ec.name AS expense_category_name,
       r.membership_id, coalesce(trim(u.first_name || ' ' || u.last_name), '')::text AS member_name,
       r.contribution_id, c.name AS contribution_name,
       r.requested_by, coalesce(trim(ru.first_name || ' ' || ru.last_name), '')::text AS requested_by_name,
       r.recipient, r.decided_at, r.decline_reason, r.withdrawal_id, r.account_transfer_id, r.created_at,
       (SELECT count(*) FROM withdrawal_approvals x WHERE x.withdrawal_request_id = r.id AND x.decision = 'approved') AS approvals,
       (SELECT count(*) FROM withdrawal_approvals x WHERE x.withdrawal_request_id = r.id) AS signatories,
       1::bigint AS total
FROM withdrawal_requests r
LEFT JOIN group_accounts a ON a.id = r.account_id
LEFT JOIN group_accounts ta ON ta.id = r.to_account_id
LEFT JOIN expense_categories ec ON ec.id = r.expense_category_id
LEFT JOIN memberships m ON m.id = r.membership_id
LEFT JOIN users u ON u.id = m.user_id
LEFT JOIN contributions c ON c.id = r.contribution_id
LEFT JOIN memberships rm ON rm.id = r.requested_by
LEFT JOIN users ru ON ru.id = rm.user_id
WHERE r.group_id = $1 AND r.id = $2;

-- name: GetWithdrawalRequestForUpdate :one
SELECT * FROM withdrawal_requests WHERE group_id = $1 AND id = $2 FOR UPDATE;

-- name: CountPendingWithdrawalRequests :one
SELECT count(*) FROM withdrawal_requests WHERE group_id = $1 AND status = 'pending';

-- name: CreateWithdrawalRequest :one
INSERT INTO withdrawal_requests (group_id, reference_number, kind, requested_by, account_id, to_account_id,
                                 amount, description, expense_category_id, membership_id, contribution_id,
                                 recipient, created_by)
VALUES ($1, $2, $3, $4, $5, $6, sqlc.arg(amount)::numeric, $7, $8, $9, $10, $11, $12)
RETURNING id;

-- name: SetWithdrawalRequestStatus :exec
UPDATE withdrawal_requests SET
    status         = sqlc.arg(status)::withdrawal_request_status,
    decided_at     = CASE WHEN sqlc.arg(status)::withdrawal_request_status IN ('approved', 'declined', 'cancelled')
                          THEN now() ELSE decided_at END,
    decline_reason = coalesce(sqlc.narg(decline_reason), decline_reason),
    updated_at     = now()
WHERE group_id = sqlc.arg(group_id) AND id = sqlc.arg(id);

-- name: MarkWithdrawalRequestPaid :exec
UPDATE withdrawal_requests SET status = 'paid', withdrawal_id = $3, account_transfer_id = $4, updated_at = now()
WHERE group_id = $1 AND id = $2;

-- name: GroupSignatories :many
-- Signatories: active members who hold a group role (legacy role holders),
-- only officials among them when the group restricts approvals.
SELECT m.id FROM memberships m
WHERE m.group_id = sqlc.arg(group_id) AND m.status = 'active' AND m.group_role_id IS NOT NULL
  AND (NOT sqlc.arg(officials_only)::bool OR m.is_official)
ORDER BY m.id;

-- name: CreateWithdrawalApproval :exec
INSERT INTO withdrawal_approvals (group_id, withdrawal_request_id, membership_id, decision, decided_at, decided_by)
VALUES ($1, $2, $3, sqlc.arg(decision)::approval_decision,
        CASE WHEN sqlc.arg(decision)::approval_decision = 'pending' THEN NULL ELSE now() END, sqlc.narg(decided_by));

-- name: ListWithdrawalApprovals :many
SELECT x.id, x.membership_id, coalesce(trim(u.first_name || ' ' || u.last_name), '')::text AS member_name,
       r.name AS role_name, x.decision, x.decided_at, x.comment
FROM withdrawal_approvals x
JOIN memberships m ON m.id = x.membership_id
JOIN users u ON u.id = m.user_id
LEFT JOIN group_roles r ON r.id = m.group_role_id
WHERE x.withdrawal_request_id = $1
ORDER BY x.id;

-- name: DecideWithdrawalApproval :execrows
UPDATE withdrawal_approvals SET decision = sqlc.arg(decision), decided_at = now(),
    comment = sqlc.arg(comment), decided_by = sqlc.arg(decided_by)
WHERE withdrawal_request_id = sqlc.arg(withdrawal_request_id) AND membership_id = sqlc.arg(membership_id)
  AND decision = 'pending';

-- name: ApprovalTally :one
SELECT count(*) FILTER (WHERE decision = 'approved') AS approved,
       count(*) FILTER (WHERE decision = 'declined') AS declined,
       count(*) AS total
FROM withdrawal_approvals WHERE withdrawal_request_id = $1;

-- name: IsSignatory :one
SELECT EXISTS (SELECT 1 FROM memberships
               WHERE group_id = $1 AND id = $2 AND status = 'active' AND group_role_id IS NOT NULL);
