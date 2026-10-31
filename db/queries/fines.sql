-- name: ListFines :many
-- Fines charged to members: ones officials raised by hand (fine_invoice)
-- and late-payment fines on contributions (contribution_fine_invoice).
SELECT e.id, e.entry_type, e.membership_id, coalesce(trim(u.first_name || ' ' || u.last_name), '')::text AS member_name,
       e.amount::text AS amount, e.entry_date, e.description, e.generated_by,
       e.contribution_id, c.name AS contribution_name,
       e.fine_category_id, fc.name AS fine_category_name,
       e.parent_entry_id, p.due_date AS parent_due_date,
       e.created_at, e.voided_at, count(*) OVER () AS total
FROM ledger_entries e
LEFT JOIN memberships m ON m.id = e.membership_id
LEFT JOIN users u ON u.id = m.user_id
LEFT JOIN contributions c ON c.id = e.contribution_id
LEFT JOIN fine_categories fc ON fc.id = e.fine_category_id
LEFT JOIN ledger_entries p ON p.id = e.parent_entry_id
WHERE e.group_id = sqlc.arg(group_id)
  AND e.entry_type IN ('fine_invoice', 'contribution_fine_invoice')
  AND (sqlc.arg(include_voided)::bool OR e.voided_at IS NULL)
  AND (sqlc.narg(membership_id)::bigint IS NULL OR e.membership_id = sqlc.narg(membership_id))
  AND (sqlc.narg(entry_type)::ledger_entry_type IS NULL OR e.entry_type = sqlc.narg(entry_type))
  AND (sqlc.narg(from_date)::date IS NULL OR e.entry_date >= sqlc.narg(from_date))
  AND (sqlc.narg(to_date)::date IS NULL OR e.entry_date <= sqlc.narg(to_date))
ORDER BY e.entry_date DESC, e.id DESC
LIMIT sqlc.arg(page_size) OFFSET sqlc.arg(page_offset);

-- name: FineBalances :many
-- Each active member's fines: charged, paid and still owed.
SELECT m.id AS membership_id, trim(u.first_name || ' ' || u.last_name)::text AS member_name,
       coalesce(sum(e.amount) FILTER (WHERE r.sign > 0), 0)::text AS fined,
       coalesce(sum(e.amount) FILTER (WHERE r.sign < 0), 0)::text AS paid,
       coalesce(sum(r.sign * e.amount), 0)::text AS balance
FROM memberships m
JOIN users u ON u.id = m.user_id
LEFT JOIN ledger_entries e ON e.membership_id = m.id AND e.group_id = m.group_id AND e.voided_at IS NULL
LEFT JOIN ledger_entry_rules r ON r.entry_type = e.entry_type AND r.bucket = 'fine'
WHERE m.group_id = sqlc.arg(group_id) AND m.status = 'active'
  AND (sqlc.narg(membership_id)::bigint IS NULL OR m.id = sqlc.narg(membership_id))
GROUP BY m.id, u.first_name, u.last_name
ORDER BY coalesce(sum(r.sign * e.amount), 0) DESC, u.first_name;

-- name: CreateManualFine :one
INSERT INTO ledger_entries (group_id, membership_id, entry_type, amount, entry_date, due_date,
                            description, fine_category_id, created_by, is_backdated)
VALUES (sqlc.arg(group_id), sqlc.arg(membership_id), 'fine_invoice', sqlc.arg(amount)::text::numeric,
        sqlc.arg(fine_date), sqlc.arg(fine_date), sqlc.arg(description), sqlc.arg(fine_category_id),
        sqlc.arg(created_by), sqlc.arg(fine_date)::date < current_date)
RETURNING id;

-- name: VoidFine :execrows
UPDATE ledger_entries SET voided_at = now(), voided_by = sqlc.arg(voided_by)
WHERE group_id = sqlc.arg(group_id) AND id = sqlc.arg(id) AND voided_at IS NULL
  AND entry_type IN ('fine_invoice', 'contribution_fine_invoice');

-- name: CreateFineRule :one
INSERT INTO contribution_fine_rules (contribution_id, kind, fixed_amount, percentage_rate, percentage_of,
                                     chargeable_on, mode, frequency, fine_limit, sms_notifications,
                                     email_notifications, is_active, next_fine_date)
VALUES (sqlc.arg(contribution_id), sqlc.arg(kind), sqlc.narg(fixed_amount)::text::numeric,
        sqlc.narg(percentage_rate)::text::numeric, sqlc.narg(percentage_of), sqlc.arg(chargeable_on),
        sqlc.arg(mode), sqlc.arg(frequency), sqlc.arg(fine_limit), sqlc.arg(sms_notifications),
        sqlc.arg(email_notifications), sqlc.arg(is_active), sqlc.narg(next_fine_date))
RETURNING id;

-- name: UpdateFineRule :execrows
UPDATE contribution_fine_rules SET
    kind = sqlc.arg(kind), fixed_amount = sqlc.narg(fixed_amount)::text::numeric,
    percentage_rate = sqlc.narg(percentage_rate)::text::numeric, percentage_of = sqlc.narg(percentage_of),
    chargeable_on = sqlc.arg(chargeable_on), mode = sqlc.arg(mode), frequency = sqlc.arg(frequency),
    fine_limit = sqlc.arg(fine_limit), sms_notifications = sqlc.arg(sms_notifications),
    email_notifications = sqlc.arg(email_notifications), is_active = sqlc.arg(is_active),
    next_fine_date = sqlc.narg(next_fine_date), updated_at = now()
WHERE id = sqlc.arg(id) AND contribution_id = sqlc.arg(contribution_id);

-- name: DeleteFineRulesExcept :exec
-- Fines a deleted rule already charged stay; they just lose the link.
DELETE FROM contribution_fine_rules
WHERE contribution_id = sqlc.arg(contribution_id) AND NOT (id = ANY(sqlc.arg(keep)::bigint[]));

-- name: IsJobRunning :one
SELECT EXISTS (SELECT 1 FROM job_runs WHERE job = $1 AND status = 'running');
