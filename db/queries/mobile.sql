-- name: CreateMobileToken :exec
INSERT INTO mobile_access_tokens (token_hash, user_id, expires_at) VALUES ($1, $2, $3);

-- name: UseMobileToken :one
-- Looks up a live token and slides its expiry, as the legacy app did on
-- every call (at least 30 days from now).
UPDATE mobile_access_tokens t
SET last_used_at = now(), expires_at = greatest(t.expires_at, sqlc.arg(min_expires_at)::timestamptz)
FROM users u
WHERE t.token_hash = $1 AND t.expires_at > now() AND u.id = t.user_id AND u.is_active
RETURNING t.user_id;

-- name: DeleteMobileToken :exec
DELETE FROM mobile_access_tokens WHERE token_hash = $1;

-- name: DeleteUserMobileTokens :exec
DELETE FROM mobile_access_tokens WHERE user_id = $1;

-- name: UpsertMobilePin :exec
INSERT INTO mobile_pins (identity, pin_hash, unique_code, expires_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT (identity) DO UPDATE
SET pin_hash = excluded.pin_hash, unique_code = excluded.unique_code, attempts = 0,
    expires_at = excluded.expires_at, verified_at = NULL, created_at = now();

-- name: GetMobilePin :one
SELECT * FROM mobile_pins WHERE identity = $1;

-- name: FailMobilePin :exec
UPDATE mobile_pins SET attempts = attempts + 1 WHERE identity = $1;

-- name: MarkMobilePinVerified :exec
UPDATE mobile_pins SET verified_at = now() WHERE identity = $1;

-- name: DeleteMobilePin :exec
DELETE FROM mobile_pins WHERE identity = $1;

-- name: MobileUserGroups :many
-- The groups a user belongs to, for the app's check-in data.
SELECT g.id, g.name, g.slug, g.email, g.phone, g.country_code, g.currency, g.status, g.settings,
       g.owner_user_id, m.id AS member_id, m.is_official, m.group_role_id,
       c.name AS country_name, c.id AS country_id,
       (SELECT count(*) FROM memberships x WHERE x.group_id = g.id AND x.status = 'active')::bigint AS size
FROM memberships m
JOIN groups g ON g.id = m.group_id
LEFT JOIN countries c ON c.code = g.country_code
WHERE m.user_id = $1 AND m.status = 'active'
ORDER BY g.name DESC;

-- name: GroupRoleNames :many
SELECT id, group_id, name FROM group_roles WHERE group_id = ANY(sqlc.arg(group_ids)::bigint[]) AND is_active ORDER BY name;

-- name: MobileCountries :many
SELECT id, name, currency_name FROM countries WHERE is_active ORDER BY id;

-- name: MobileBanks :many
-- Banks for the app's pickers. The legacy list left out e-wallets.
SELECT id, name, logo FROM banks
WHERE is_active AND coalesce(settings->>'wallet', '0') IN ('', '0')
ORDER BY name;

-- name: MobileBankBranches :many
SELECT id, name FROM bank_branches WHERE bank_id = $1 AND is_active ORDER BY id;

-- name: MobileGroupRoles :many
SELECT r.id, r.name, r.description, r.is_active, r.is_editable,
       EXISTS (SELECT 1 FROM memberships m WHERE m.group_role_id = r.id AND m.status = 'active') AS in_use
FROM group_roles r
WHERE r.group_id = $1
ORDER BY r.name, r.id
LIMIT sqlc.arg(lim) OFFSET sqlc.arg(off);

-- name: MobileGroupMembers :many
-- Members as the app lists them; removed members are left out.
SELECT m.id, m.user_id, m.is_official, m.group_role_id, r.name AS role_name, m.status,
       u.first_name, u.middle_name, u.last_name, u.phone, u.email, u.avatar, u.last_login_at
FROM memberships m
JOIN users u ON u.id = m.user_id
LEFT JOIN group_roles r ON r.id = m.group_role_id AND r.is_active
WHERE m.group_id = sqlc.arg(group_id) AND m.status <> 'removed'
  AND (NOT sqlc.arg(active_only)::bool OR m.status = 'active')
  AND (sqlc.narg(membership_id)::bigint IS NULL OR m.id = sqlc.narg(membership_id))
ORDER BY u.first_name, u.last_name, m.id
LIMIT sqlc.arg(lim) OFFSET sqlc.arg(off);

-- name: MobileMemberTotals :many
-- What each member has paid and owes, as the legacy dashboards add it up.
-- Contributions paid: payments and transfers in, less refunds, transfers out
-- (including to fines) and transfers to loans, plus loan repayments moved
-- into a contribution. Fines paid include contributions and loan repayments
-- moved to fines. Arrears are the contribution and fine balances.
SELECT m.id AS membership_id,
       coalesce(sum(CASE
           WHEN e.entry_type IN ('contribution_payment', 'contribution_transfer_in', 'loan_to_contribution_transfer') THEN e.amount
           WHEN e.entry_type IN ('contribution_refund', 'contribution_transfer_out', 'contribution_to_loan_transfer') THEN -e.amount
       END), 0)::text AS contributions_paid,
       coalesce(sum(r.sign * e.amount) FILTER (WHERE r.bucket = 'contribution'), 0)::text AS contribution_arrears,
       coalesce(sum(e.amount) FILTER (WHERE e.entry_type IN ('fine_payment', 'contribution_to_fine_transfer', 'loan_to_fine_transfer')), 0)::text AS fines_paid,
       coalesce(sum(r.sign * e.amount) FILTER (WHERE r.bucket = 'fine'), 0)::text AS fine_arrears
FROM memberships m
LEFT JOIN ledger_entries e ON e.membership_id = m.id AND e.group_id = m.group_id AND e.voided_at IS NULL
LEFT JOIN ledger_entry_rules r ON r.entry_type = e.entry_type
WHERE m.group_id = sqlc.arg(group_id)
  AND (sqlc.narg(membership_id)::bigint IS NULL OR m.id = sqlc.narg(membership_id))
GROUP BY m.id;

-- name: MobileGroupTotals :one
SELECT coalesce(sum(CASE
           WHEN e.entry_type IN ('contribution_payment', 'contribution_transfer_in', 'loan_to_contribution_transfer') THEN e.amount
           WHEN e.entry_type IN ('contribution_refund', 'contribution_transfer_out', 'contribution_to_loan_transfer') THEN -e.amount
       END), 0)::text AS contributions_paid,
       coalesce(sum(e.amount) FILTER (WHERE e.entry_type IN ('fine_payment', 'contribution_to_fine_transfer', 'loan_to_fine_transfer')), 0)::text AS fines_paid
FROM ledger_entries e
WHERE e.group_id = $1 AND e.voided_at IS NULL;

-- name: MobileMemberContributionSummary :many
-- Paid and balance per open contribution for one member, largest paid first.
SELECT c.id, c.name,
       coalesce(sum(CASE
           WHEN e.entry_type IN ('contribution_payment', 'contribution_transfer_in') THEN e.amount
           WHEN e.entry_type IN ('contribution_refund', 'contribution_transfer_out', 'contribution_to_loan_transfer') THEN -e.amount
       END), 0)::text AS paid,
       coalesce(sum(r.sign * e.amount) FILTER (WHERE r.bucket = 'contribution'), 0)::text AS balance
FROM contributions c
JOIN ledger_entries e ON e.contribution_id = c.id AND e.membership_id = sqlc.arg(membership_id) AND e.voided_at IS NULL
JOIN ledger_entry_rules r ON r.entry_type = e.entry_type
WHERE c.group_id = sqlc.arg(group_id) AND c.deleted_at IS NULL AND c.is_active AND NOT c.is_closed
GROUP BY c.id, c.name
ORDER BY 3 DESC, c.name;

-- name: NextContributionDate :one
-- The legacy dashboard's next contribution date: the earliest upcoming
-- date of an active regular contribution, else of a one-time one.
SELECT s.contribution_date
FROM contribution_schedules s
JOIN contributions c ON c.id = s.contribution_id
WHERE c.group_id = $1 AND c.is_active AND c.deleted_at IS NULL AND c.kind IN ('regular', 'one_time')
  AND s.contribution_date >= sqlc.arg(today)::date
ORDER BY c.kind = 'regular' DESC, s.contribution_date
LIMIT 1;

-- name: MobileGroupContributions :many
SELECT c.id, c.name, c.kind, c.amount::text AS amount, c.is_active, c.is_hidden,
       s.frequency, s.rule, s.invoice_date, s.contribution_date,
       coalesce(s.invoicing_active, false)::bool AS invoicing_active
FROM contributions c
LEFT JOIN contribution_schedules s ON s.contribution_id = c.id
WHERE c.group_id = $1 AND c.deleted_at IS NULL
ORDER BY c.name, c.id;

-- name: MobileContributionOptions :many
SELECT id, name FROM contributions
WHERE group_id = $1 AND is_active AND NOT is_hidden AND deleted_at IS NULL
ORDER BY name, id;

-- name: MobileContributionBalances :many
-- One member's balance per contribution.
SELECT e.contribution_id::bigint AS contribution_id, coalesce(sum(r.sign * e.amount), 0)::text AS balance
FROM ledger_entries e
JOIN ledger_entry_rules r ON r.entry_type = e.entry_type AND r.bucket = 'contribution'
WHERE e.group_id = $1 AND e.membership_id = $2 AND e.voided_at IS NULL AND e.contribution_id IS NOT NULL
GROUP BY e.contribution_id;

-- name: ActiveMembershipIDs :many
SELECT id FROM memberships WHERE group_id = $1 AND status = 'active' ORDER BY id;

-- name: MobileStatementEntries :many
-- One member's entries on open contributions, for the contribution statement.
SELECT e.id, e.entry_date, e.entry_type, e.legacy_type, e.amount::text AS amount, r.sign, c.name AS contribution_name
FROM ledger_entries e
JOIN ledger_entry_rules r ON r.entry_type = e.entry_type AND r.bucket = 'contribution'
JOIN contributions c ON c.id = e.contribution_id
WHERE e.group_id = sqlc.arg(group_id) AND e.membership_id = sqlc.arg(membership_id) AND e.voided_at IS NULL
  AND c.deleted_at IS NULL AND NOT c.is_closed
ORDER BY e.entry_date, e.id;
