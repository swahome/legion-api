-- name: MemberContributionStatement :many
-- Contribution-bucket entries for one member with a running balance
-- (positive = the member owes). Opening balance covers entries before `from`.
WITH entries AS (
    SELECT e.id, e.entry_date, e.entry_type, e.contribution_id, c.name AS contribution_name,
           e.description, e.amount, r.sign, e.is_backdated, e.due_date,
           sum(r.sign * e.amount) OVER (ORDER BY e.entry_date, e.id) AS running_balance
    FROM ledger_entries e
    JOIN ledger_entry_rules r USING (entry_type)
    LEFT JOIN contributions c ON c.id = e.contribution_id
    WHERE e.group_id = sqlc.arg(group_id) AND e.membership_id = sqlc.arg(membership_id)
      AND e.voided_at IS NULL AND r.bucket = 'contribution'
      AND (sqlc.narg(contribution_id)::bigint IS NULL OR e.contribution_id = sqlc.narg(contribution_id))
      AND (c.id IS NULL OR NOT c.hidden_in_statement OR sqlc.arg(include_hidden)::bool)
)
SELECT id, entry_date, entry_type, contribution_id, contribution_name, description,
       amount::text AS amount, sign, is_backdated, due_date, running_balance::text AS running_balance
FROM entries
WHERE (sqlc.narg(from_date)::date IS NULL OR entry_date >= sqlc.narg(from_date))
  AND (sqlc.narg(to_date)::date IS NULL OR entry_date <= sqlc.narg(to_date))
ORDER BY entry_date, id;

-- name: MemberContributionTotals :many
-- Per-contribution totals for one member: invoiced, paid and balance.
SELECT e.contribution_id, c.name AS contribution_name,
       coalesce(sum(e.amount) FILTER (WHERE r.sign > 0), 0)::text AS payable,
       coalesce(sum(e.amount) FILTER (WHERE r.sign < 0), 0)::text AS paid,
       coalesce(sum(r.sign * e.amount), 0)::text AS balance
FROM ledger_entries e
JOIN ledger_entry_rules r USING (entry_type)
LEFT JOIN contributions c ON c.id = e.contribution_id
WHERE e.group_id = $1 AND e.membership_id = $2 AND e.voided_at IS NULL AND r.bucket = 'contribution'
GROUP BY e.contribution_id, c.name
ORDER BY c.name NULLS LAST;

-- name: OpeningBalance :one
SELECT coalesce(sum(r.sign * e.amount), 0)::text
FROM ledger_entries e
JOIN ledger_entry_rules r USING (entry_type)
LEFT JOIN contributions c ON c.id = e.contribution_id
WHERE e.group_id = sqlc.arg(group_id) AND e.membership_id = sqlc.arg(membership_id)
  AND e.voided_at IS NULL AND r.bucket = 'contribution'
  AND (sqlc.narg(contribution_id)::bigint IS NULL OR e.contribution_id = sqlc.narg(contribution_id))
  AND (c.id IS NULL OR NOT c.hidden_in_statement OR sqlc.arg(include_hidden)::bool)
  AND e.entry_date < sqlc.arg(before)::date;

-- name: GroupContributionBalances :many
-- Every active member's contribution totals, for the officials' arrears view.
SELECT m.id AS membership_id, u.first_name, u.last_name, m.membership_number,
       coalesce(sum(e.amount) FILTER (WHERE r.sign > 0), 0)::text AS payable,
       coalesce(sum(e.amount) FILTER (WHERE r.sign < 0), 0)::text AS paid,
       coalesce(sum(r.sign * e.amount), 0)::text AS balance
FROM memberships m
JOIN users u ON u.id = m.user_id
LEFT JOIN ledger_entries e ON e.membership_id = m.id AND e.group_id = m.group_id AND e.voided_at IS NULL
LEFT JOIN ledger_entry_rules r ON r.entry_type = e.entry_type AND r.bucket = 'contribution'
WHERE m.group_id = $1 AND m.status = 'active'
GROUP BY m.id, u.first_name, u.last_name, m.membership_number
ORDER BY coalesce(sum(r.sign * e.amount), 0) DESC, u.first_name;
