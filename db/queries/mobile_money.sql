-- Mobile app API, milestone 3: money lists in the legacy shapes. See
-- docs/api/mobile.md.

-- name: MobileDeposits :many
SELECT d.id, d.legacy_type, d.kind, d.account_kind, d.method, d.amount::text AS amount, d.deposit_date,
       d.description, d.created_at, d.membership_id,
       coalesce(trim(u.first_name || ' ' || u.last_name), '')::text AS member_name,
       coalesce(c.name, '')::text AS contribution_name,
       coalesce(fc.name, '')::text AS fine_category_name,
       coalesce(ic.name, '')::text AS income_category_name,
       coalesce(dp.name, '')::text AS depositor_name,
       d.account_id
FROM deposits d
LEFT JOIN memberships m ON m.id = d.membership_id
LEFT JOIN users u ON u.id = m.user_id
LEFT JOIN contributions c ON c.id = d.contribution_id
LEFT JOIN fine_categories fc ON fc.id = d.fine_category_id
LEFT JOIN income_categories ic ON ic.id = d.income_category_id
LEFT JOIN depositors dp ON dp.id = d.depositor_id
WHERE d.group_id = sqlc.arg(group_id) AND d.voided_at IS NULL
  AND (coalesce(cardinality(sqlc.arg(kinds)::text[]), 0) = 0 OR d.kind::text = ANY(sqlc.arg(kinds)::text[]))
  AND (coalesce(cardinality(sqlc.arg(member_ids)::bigint[]), 0) = 0 OR d.membership_id = ANY(sqlc.arg(member_ids)::bigint[]))
  AND (sqlc.narg(deposit_id)::bigint IS NULL OR d.id = sqlc.narg(deposit_id))
ORDER BY
  CASE WHEN sqlc.arg(sort)::text = 'amount_asc' THEN d.amount END ASC,
  CASE WHEN sqlc.arg(sort)::text = 'amount_desc' THEN d.amount END DESC,
  CASE WHEN sqlc.arg(sort)::text = 'date_asc' THEN d.deposit_date END ASC,
  d.deposit_date DESC, d.id DESC
LIMIT sqlc.arg(lim) OFFSET sqlc.arg(off);

-- name: MobileWithdrawals :many
SELECT w.id, w.legacy_type, w.kind, w.account_kind, w.method, w.amount::text AS amount, w.withdrawal_date,
       w.description, w.created_at, w.membership_id,
       coalesce(trim(u.first_name || ' ' || u.last_name), '')::text AS member_name,
       coalesce(c.name, '')::text AS contribution_name,
       coalesce(ec.name, '')::text AS expense_category_name
FROM withdrawals w
LEFT JOIN memberships m ON m.id = w.membership_id
LEFT JOIN users u ON u.id = m.user_id
LEFT JOIN contributions c ON c.id = w.contribution_id
LEFT JOIN expense_categories ec ON ec.id = w.expense_category_id
WHERE w.group_id = sqlc.arg(group_id) AND w.voided_at IS NULL
  AND (coalesce(cardinality(sqlc.arg(kinds)::text[]), 0) = 0 OR w.kind::text = ANY(sqlc.arg(kinds)::text[]))
  AND (coalesce(cardinality(sqlc.arg(member_ids)::bigint[]), 0) = 0 OR w.membership_id = ANY(sqlc.arg(member_ids)::bigint[]))
  AND (sqlc.narg(withdrawal_id)::bigint IS NULL OR w.id = sqlc.narg(withdrawal_id))
ORDER BY
  CASE WHEN sqlc.arg(sort)::text = 'amount_asc' THEN w.amount END ASC,
  CASE WHEN sqlc.arg(sort)::text = 'amount_desc' THEN w.amount END DESC,
  CASE WHEN sqlc.arg(sort)::text = 'date_asc' THEN w.withdrawal_date END ASC,
  w.withdrawal_date DESC, w.id DESC
LIMIT sqlc.arg(lim) OFFSET sqlc.arg(off);

-- name: MobileInvoices :many
-- Invoices and fines with what has been paid on each. Payments settle the
-- oldest charge first: per member and contribution for contributions, per
-- member for fines and for miscellaneous invoices.
WITH charges AS (
    SELECT e.id, e.membership_id, e.contribution_id, e.entry_type, e.amount, e.entry_date, e.due_date,
           e.description, e.fine_category_id, r.bucket,
           sum(e.amount) OVER (PARTITION BY e.membership_id, r.bucket,
                                            CASE WHEN r.bucket = 'contribution' THEN e.contribution_id END
                               ORDER BY e.entry_date, e.id) AS running
    FROM ledger_entries e
    JOIN ledger_entry_rules r ON r.entry_type = e.entry_type
    WHERE e.group_id = sqlc.arg(group_id) AND e.voided_at IS NULL AND r.sign > 0
      AND r.bucket IN ('contribution', 'fine', 'misc')
), credits AS (
    SELECT e.membership_id, r.bucket, CASE WHEN r.bucket = 'contribution' THEN e.contribution_id END AS contribution_id,
           sum(e.amount) AS paid
    FROM ledger_entries e
    JOIN ledger_entry_rules r ON r.entry_type = e.entry_type
    WHERE e.group_id = sqlc.arg(group_id) AND e.voided_at IS NULL AND r.sign < 0
      AND r.bucket IN ('contribution', 'fine', 'misc')
    GROUP BY 1, 2, 3
)
SELECT ch.id, ch.entry_type, ch.amount::text AS amount, ch.entry_date, ch.due_date, ch.description,
       ch.membership_id, ch.contribution_id,
       greatest(0, least(ch.amount, coalesce(cr.paid, 0) - (ch.running - ch.amount)))::text AS paid,
       coalesce(trim(u.first_name || ' ' || u.last_name), '')::text AS member_name,
       coalesce(c.name, '')::text AS contribution_name,
       coalesce(fc.name, '')::text AS fine_category_name
FROM charges ch
LEFT JOIN credits cr ON cr.membership_id = ch.membership_id AND cr.bucket = ch.bucket
     AND cr.contribution_id IS NOT DISTINCT FROM CASE WHEN ch.bucket = 'contribution' THEN ch.contribution_id END
LEFT JOIN memberships m ON m.id = ch.membership_id
LEFT JOIN users u ON u.id = m.user_id
LEFT JOIN contributions c ON c.id = ch.contribution_id
LEFT JOIN fine_categories fc ON fc.id = ch.fine_category_id
WHERE ch.entry_type::text = ANY(sqlc.arg(types)::text[])
  AND (coalesce(cardinality(sqlc.arg(member_ids)::bigint[]), 0) = 0 OR ch.membership_id = ANY(sqlc.arg(member_ids)::bigint[]))
  AND (sqlc.narg(invoice_id)::bigint IS NULL OR ch.id = sqlc.narg(invoice_id))
ORDER BY ch.entry_date DESC, ch.id DESC
LIMIT sqlc.arg(lim) OFFSET sqlc.arg(off);

-- name: MobileFineBalances :many
-- The caller's fine balance per contribution (late fines) and per fine category.
SELECT e.contribution_id, e.fine_category_id, sum(r.sign * e.amount)::text AS balance
FROM ledger_entries e
JOIN ledger_entry_rules r ON r.entry_type = e.entry_type AND r.bucket = 'fine'
WHERE e.group_id = sqlc.arg(group_id) AND e.membership_id = sqlc.arg(membership_id) AND e.voided_at IS NULL
GROUP BY e.contribution_id, e.fine_category_id;

-- name: MobileFineContributions :many
-- Active contributions that charge late fines.
SELECT c.id, c.name FROM contributions c
JOIN contribution_schedules s ON s.contribution_id = c.id
WHERE c.group_id = $1 AND c.deleted_at IS NULL AND c.is_active AND s.fines_enabled
ORDER BY c.name;

-- name: MobileGroupExpenses :one
SELECT coalesce(sum(amount), 0)::text FROM withdrawals
WHERE group_id = $1 AND voided_at IS NULL AND kind = 'expense';

-- name: PartnerBankIDs :many
SELECT id FROM banks WHERE is_partner;

-- name: MobileWithdrawalRequests :many
-- Withdrawal requests for the app, with every signatory's tally and the
-- caller's own answer ('' when they aren't a signatory on it).
SELECT r.id, r.kind, r.status, r.amount::text AS amount, r.description, r.recipient, r.decline_reason,
       r.requested_by, r.membership_id, r.created_at, r.created_by,
       coalesce(trim(ru.first_name || ' ' || ru.last_name), '')::text AS requested_by_name,
       coalesce(trim(u.first_name || ' ' || u.last_name), '')::text AS member_name,
       coalesce(ec.name, '')::text AS expense_category_name, coalesce(c.name, '')::text AS contribution_name,
       coalesce(a.name, '')::text AS account_name, coalesce(ta.name, '')::text AS to_account_name,
       coalesce(b.name, '')::text AS recipient_bank_name,
       (SELECT count(*) FROM withdrawal_approvals x WHERE x.withdrawal_request_id = r.id AND x.decision = 'approved') AS approved,
       (SELECT count(*) FROM withdrawal_approvals x WHERE x.withdrawal_request_id = r.id AND x.decision = 'declined') AS declined,
       (SELECT count(*) FROM withdrawal_approvals x WHERE x.withdrawal_request_id = r.id AND x.decision = 'pending') AS pending,
       coalesce((SELECT x.decision::text FROM withdrawal_approvals x
                 WHERE x.withdrawal_request_id = r.id AND x.membership_id = sqlc.arg(caller)::bigint LIMIT 1), '')::text AS my_decision
FROM withdrawal_requests r
LEFT JOIN memberships rm ON rm.id = r.requested_by
LEFT JOIN users ru ON ru.id = rm.user_id
LEFT JOIN memberships m ON m.id = r.membership_id
LEFT JOIN users u ON u.id = m.user_id
LEFT JOIN expense_categories ec ON ec.id = r.expense_category_id
LEFT JOIN contributions c ON c.id = r.contribution_id
LEFT JOIN group_accounts a ON a.id = r.account_id
LEFT JOIN group_accounts ta ON ta.id = r.to_account_id
LEFT JOIN banks b ON b.id = CASE WHEN r.recipient ? 'bank_id' AND jsonb_typeof(r.recipient -> 'bank_id') = 'number'
                                  THEN (r.recipient ->> 'bank_id')::bigint END
WHERE r.group_id = sqlc.arg(group_id)
  AND (sqlc.narg(id)::bigint IS NULL OR r.id = sqlc.narg(id))
  AND (coalesce(cardinality(sqlc.arg(statuses)::text[]), 0) = 0 OR r.status::text = ANY (sqlc.arg(statuses)::text[]))
  AND (coalesce(cardinality(sqlc.arg(member_ids)::bigint[]), 0) = 0 OR r.requested_by = ANY (sqlc.arg(member_ids)::bigint[]))
ORDER BY CASE WHEN sqlc.arg(sort)::text = 'amount_asc' THEN r.amount END ASC,
         CASE WHEN sqlc.arg(sort)::text = 'amount_desc' THEN r.amount END DESC,
         CASE WHEN sqlc.arg(sort)::text = 'date_asc' THEN r.created_at END ASC,
         r.created_at DESC, r.id DESC
LIMIT sqlc.arg(lim) OFFSET sqlc.arg(off);

-- name: MobileExpenseCategoryTotals :many
SELECT coalesce(ec.name, '')::text AS name, sum(w.amount)::text AS total
FROM withdrawals w
LEFT JOIN expense_categories ec ON ec.id = w.expense_category_id
WHERE w.group_id = $1 AND w.voided_at IS NULL AND w.kind = 'expense'
GROUP BY ec.id, ec.name
HAVING sum(w.amount) <> 0
ORDER BY ec.name NULLS LAST;
