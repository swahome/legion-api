-- name: ListContributions :many
SELECT c.id, c.name, c.kind, c.category, c.amount::text AS amount, c.is_active, c.is_hidden,
       c.is_closed, c.applies_to_all_members, c.created_at,
       s.frequency, s.contribution_date, s.invoice_date, s.invoicing_active, s.fines_enabled
FROM contributions c
LEFT JOIN contribution_schedules s ON s.contribution_id = c.id
WHERE c.group_id = $1 AND c.deleted_at IS NULL
  AND (sqlc.arg(include_hidden)::bool OR NOT c.is_hidden)
ORDER BY c.is_active DESC, c.name;

-- name: GetContribution :one
SELECT c.id, c.name, c.kind, c.category, c.amount::text AS amount, c.is_active, c.is_hidden,
       c.is_closed, c.is_non_refundable, c.is_equity, c.hidden_in_statement,
       c.show_arrears_cumulatively, c.applies_to_all_members, c.created_at, c.updated_at
FROM contributions c
WHERE c.group_id = $1 AND c.id = $2 AND c.deleted_at IS NULL;

-- name: GetContributionSchedule :one
SELECT * FROM contribution_schedules WHERE contribution_id = $1;

-- name: ListContributionFineRules :many
SELECT id, kind, fixed_amount, percentage_rate,
       percentage_of, chargeable_on, mode, frequency, fine_limit, is_active
FROM contribution_fine_rules WHERE contribution_id = $1 ORDER BY id;

-- name: ListContributionMemberIDs :many
SELECT membership_id FROM contribution_members WHERE contribution_id = $1 ORDER BY membership_id;

-- name: CreateContribution :one
INSERT INTO contributions (group_id, name, kind, category, amount, applies_to_all_members)
VALUES ($1, $2, $3, $4, sqlc.arg(amount)::numeric, $5)
RETURNING id;

-- name: UpdateContribution :execrows
UPDATE contributions SET
    name      = coalesce(sqlc.narg(name), name),
    category  = coalesce(sqlc.narg(category), category),
    amount    = coalesce(sqlc.narg(amount)::numeric, amount),
    is_active = coalesce(sqlc.narg(is_active), is_active),
    is_hidden = coalesce(sqlc.narg(is_hidden), is_hidden),
    applies_to_all_members = coalesce(sqlc.narg(applies_to_all_members), applies_to_all_members),
    updated_at = now()
WHERE group_id = sqlc.arg(group_id) AND id = sqlc.arg(id) AND deleted_at IS NULL;

-- name: UpsertContributionSchedule :exec
INSERT INTO contribution_schedules (contribution_id, frequency, invoice_date, contribution_date,
                                    invoice_days_before, invoicing_active, sms_notifications,
                                    email_notifications, fines_enabled)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (contribution_id) DO UPDATE SET
    frequency = EXCLUDED.frequency,
    invoice_date = EXCLUDED.invoice_date,
    contribution_date = EXCLUDED.contribution_date,
    invoice_days_before = EXCLUDED.invoice_days_before,
    invoicing_active = EXCLUDED.invoicing_active,
    sms_notifications = EXCLUDED.sms_notifications,
    email_notifications = EXCLUDED.email_notifications,
    fines_enabled = EXCLUDED.fines_enabled,
    updated_at = now();

-- name: DeleteContributionMembers :exec
DELETE FROM contribution_members WHERE contribution_id = $1;

-- name: AddContributionMembers :exec
INSERT INTO contribution_members (contribution_id, membership_id)
SELECT sqlc.arg(contribution_id), m.id FROM memberships m
WHERE m.group_id = sqlc.arg(group_id) AND m.id = ANY(sqlc.arg(membership_ids)::bigint[])
ON CONFLICT DO NOTHING;
