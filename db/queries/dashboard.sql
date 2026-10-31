-- Group dashboard (milestone 3, part 4). See docs/api/groups.md ("dashboard").

-- name: DashboardCash :many
-- Money held, by account kind, including legacy entries whose account is unknown.
SELECT k.kind::account_kind AS kind,
       (coalesce((SELECT sum(a.initial_balance) FROM group_accounts a
                  WHERE a.group_id = sqlc.arg(group_id) AND a.kind = k.kind AND a.deleted_at IS NULL), 0)
      + coalesce((SELECT sum(e.direction * e.amount) FROM account_entries e
                  LEFT JOIN group_accounts a ON a.id = e.account_id
                  WHERE e.group_id = sqlc.arg(group_id) AND e.account_kind = k.kind AND e.voided_at IS NULL
                    AND (a.id IS NULL OR a.deleted_at IS NULL)), 0))::text AS balance,
       (SELECT count(*) FROM group_accounts a
        WHERE a.group_id = sqlc.arg(group_id) AND a.kind = k.kind AND a.deleted_at IS NULL AND NOT a.is_closed)::bigint AS accounts
FROM unnest(enum_range(NULL::account_kind)) AS k(kind)
ORDER BY k.kind;

-- name: DashboardMonthly :many
-- Money in and out per calendar month from from_month (the first of a month) to today.
-- Contributions, fines and other member payments come from the ledger; income
-- and expenses from deposits and withdrawals.
WITH months AS (
    SELECT generate_series(sqlc.arg(from_month)::date, date_trunc('month', sqlc.arg(today)::date)::date, interval '1 month')::date AS month
), ledger AS (
    SELECT date_trunc('month', e.entry_date)::date AS month,
           sum(e.amount) FILTER (WHERE e.entry_type = 'contribution_payment') AS contributions,
           sum(e.amount) FILTER (WHERE e.entry_type = 'fine_payment') AS fines,
           sum(e.amount) FILTER (WHERE e.entry_type = 'misc_payment') AS misc
    FROM ledger_entries e
    WHERE e.group_id = sqlc.arg(group_id) AND e.voided_at IS NULL
      AND e.entry_type IN ('contribution_payment', 'fine_payment', 'misc_payment')
      AND e.entry_date >= sqlc.arg(from_month)::date AND e.entry_date <= sqlc.arg(today)::date
    GROUP BY 1
), income AS (
    SELECT date_trunc('month', d.deposit_date)::date AS month, sum(d.amount) AS income
    FROM deposits d
    WHERE d.group_id = sqlc.arg(group_id) AND d.voided_at IS NULL AND d.kind = 'income'
      AND d.deposit_date >= sqlc.arg(from_month)::date AND d.deposit_date <= sqlc.arg(today)::date
    GROUP BY 1
), expenses AS (
    SELECT date_trunc('month', w.withdrawal_date)::date AS month, sum(w.amount) AS expenses
    FROM withdrawals w
    WHERE w.group_id = sqlc.arg(group_id) AND w.voided_at IS NULL AND w.kind = 'expense'
      AND w.withdrawal_date >= sqlc.arg(from_month)::date AND w.withdrawal_date <= sqlc.arg(today)::date
    GROUP BY 1
)
SELECT m.month,
       coalesce(l.contributions, 0)::text AS contributions,
       coalesce(l.fines, 0)::text AS fines,
       coalesce(l.misc, 0)::text AS misc,
       coalesce(i.income, 0)::text AS income,
       coalesce(x.expenses, 0)::text AS expenses
FROM months m
LEFT JOIN ledger l ON l.month = m.month
LEFT JOIN income i ON i.month = m.month
LEFT JOIN expenses x ON x.month = m.month
ORDER BY m.month;

-- name: DashboardOwed :many
-- What active members owe, per ledger bucket: the sum of positive balances and
-- how many members are behind.
WITH balances AS (
    SELECT m.id, r.bucket, sum(r.sign * e.amount) AS balance
    FROM memberships m
    JOIN ledger_entries e ON e.membership_id = m.id AND e.group_id = m.group_id AND e.voided_at IS NULL
    JOIN ledger_entry_rules r ON r.entry_type = e.entry_type AND r.bucket IN ('contribution', 'fine', 'misc')
    WHERE m.group_id = sqlc.arg(group_id) AND m.status = 'active'
    GROUP BY m.id, r.bucket
)
SELECT bucket::text AS bucket,
       coalesce(sum(balance) FILTER (WHERE balance > 0), 0)::text AS owed,
       count(*) FILTER (WHERE balance > 0)::bigint AS members
FROM balances
GROUP BY bucket
ORDER BY bucket;

-- name: DashboardMembersBehind :one
SELECT count(DISTINCT id)::bigint FROM (
    SELECT m.id
    FROM memberships m
    JOIN ledger_entries e ON e.membership_id = m.id AND e.group_id = m.group_id AND e.voided_at IS NULL
    JOIN ledger_entry_rules r ON r.entry_type = e.entry_type AND r.bucket IN ('contribution', 'fine', 'misc')
    WHERE m.group_id = sqlc.arg(group_id) AND m.status = 'active'
    GROUP BY m.id, r.bucket
    HAVING sum(r.sign * e.amount) > 0
) behind;

-- name: DashboardRecent :many
-- The latest cash book movements.
SELECT e.id, e.entry_date, e.direction, e.amount::text AS amount, e.description, e.account_kind,
       coalesce(a.name, '') AS account_name,
       coalesce(trim(u.first_name || ' ' || u.last_name), '')::text AS member_name,
       coalesce(d.kind::text, w.kind::text, CASE WHEN e.account_transfer_id IS NOT NULL THEN 'transfer' END, '')::text AS kind
FROM account_entries e
LEFT JOIN group_accounts a ON a.id = e.account_id
LEFT JOIN memberships m ON m.id = e.membership_id
LEFT JOIN users u ON u.id = m.user_id
LEFT JOIN deposits d ON d.id = e.deposit_id
LEFT JOIN withdrawals w ON w.id = e.withdrawal_id
WHERE e.group_id = sqlc.arg(group_id) AND e.voided_at IS NULL AND e.entry_date <= sqlc.arg(today)::date
ORDER BY e.entry_date DESC, e.id DESC
LIMIT sqlc.arg(row_limit);

-- name: DashboardPendingRequests :one
SELECT count(*)::bigint FROM withdrawal_requests
WHERE group_id = sqlc.arg(group_id) AND status = 'pending';

-- name: DashboardYearly :many
-- Money in and out per calendar year, for the whole history.
WITH flows AS (
    SELECT extract(year FROM l.entry_date)::int AS year,
           CASE l.entry_type WHEN 'contribution_payment' THEN 'contributions'
                             WHEN 'fine_payment' THEN 'fines' ELSE 'misc' END AS kind,
           l.amount
    FROM ledger_entries l
    WHERE l.group_id = sqlc.arg(group_id) AND l.voided_at IS NULL
      AND l.entry_type IN ('contribution_payment', 'fine_payment', 'misc_payment')
    UNION ALL
    SELECT extract(year FROM d.deposit_date)::int, 'income', d.amount
    FROM deposits d
    WHERE d.group_id = sqlc.arg(group_id) AND d.voided_at IS NULL AND d.kind = 'income'
    UNION ALL
    SELECT extract(year FROM w.withdrawal_date)::int, 'expenses', w.amount
    FROM withdrawals w
    WHERE w.group_id = sqlc.arg(group_id) AND w.voided_at IS NULL AND w.kind = 'expense'
)
SELECT year::int AS year,
       coalesce(sum(amount) FILTER (WHERE kind = 'contributions'), 0)::text AS contributions,
       coalesce(sum(amount) FILTER (WHERE kind = 'fines'), 0)::text AS fines,
       coalesce(sum(amount) FILTER (WHERE kind = 'misc'), 0)::text AS misc,
       coalesce(sum(amount) FILTER (WHERE kind = 'income'), 0)::text AS income,
       coalesce(sum(amount) FILTER (WHERE kind = 'expenses'), 0)::text AS expenses
FROM flows
GROUP BY year
ORDER BY year;
