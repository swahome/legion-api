-- name: DueSchedules :many
-- Schedules whose invoice date has come. One-time schedules are invoiced
-- once per invoice date (last_invoiced_on records it), and only while the
-- invoice date is inside the catch-up window.
SELECT s.contribution_id, c.group_id, c.name, c.kind, c.amount::text AS amount,
       s.frequency, s.rule, s.invoice_date, s.contribution_date, s.invoice_days_before, s.last_invoiced_on
FROM contribution_schedules s
JOIN contributions c ON c.id = s.contribution_id
JOIN groups g ON g.id = c.group_id
WHERE s.invoicing_active AND c.is_active AND NOT c.is_closed AND c.deleted_at IS NULL
  AND g.status IN ('setup', 'active')
  AND c.kind IN ('regular', 'one_time')
  AND s.invoice_date IS NOT NULL AND s.contribution_date IS NOT NULL
  AND s.invoice_date <= sqlc.arg(today)::date
  AND (c.kind = 'regular' OR (s.last_invoiced_on IS DISTINCT FROM s.invoice_date
                              AND s.invoice_date >= sqlc.arg(oldest)::date))
ORDER BY s.contribution_id;

-- name: InvoiceContributionMembers :execrows
-- Raises one contribution invoice per active member who pays the
-- contribution. A member already invoiced by the job for this date is
-- skipped, so a re-run adds nothing.
INSERT INTO ledger_entries (group_id, membership_id, contribution_id, entry_type, amount,
                            entry_date, due_date, description, generated_by)
SELECT c.group_id, m.id, c.id, 'contribution_invoice', c.amount, sqlc.arg(invoice_date)::date,
       sqlc.arg(due_date)::date, c.name, 'invoicing'
FROM contributions c
JOIN memberships m ON m.group_id = c.group_id AND m.status = 'active'
WHERE c.id = sqlc.arg(contribution_id) AND c.amount > 0
  AND (c.applies_to_all_members OR EXISTS (
        SELECT 1 FROM contribution_members cm WHERE cm.contribution_id = c.id AND cm.membership_id = m.id))
ON CONFLICT (membership_id, contribution_id, entry_type, entry_date,
             (coalesce(parent_entry_id, 0)), (coalesce(fine_rule_id, 0)))
    WHERE generated_by IS NOT NULL
DO NOTHING;

-- name: AdvanceSchedule :exec
UPDATE contribution_schedules
SET invoice_date = sqlc.arg(invoice_date), contribution_date = sqlc.arg(contribution_date),
    last_invoiced_on = coalesce(sqlc.narg(last_invoiced_on), last_invoiced_on), updated_at = now()
WHERE contribution_id = sqlc.arg(contribution_id);

-- name: DueFineRules :many
SELECT f.id, f.contribution_id, c.group_id, c.name, c.amount::text AS contribution_amount,
       f.kind, coalesce(f.fixed_amount, 0)::text AS fixed_amount, coalesce(f.percentage_rate, 0)::text AS percentage_rate,
       f.percentage_of, f.chargeable_on, f.mode, f.frequency, f.fine_limit, f.next_fine_date,
       s.frequency AS schedule_frequency, s.rule, s.contribution_date
FROM contribution_fine_rules f
JOIN contributions c ON c.id = f.contribution_id
JOIN contribution_schedules s ON s.contribution_id = c.id
JOIN groups g ON g.id = c.group_id
WHERE f.is_active AND s.fines_enabled AND s.invoicing_active
  AND c.kind = 'regular' AND c.is_active AND NOT c.is_closed AND c.deleted_at IS NULL
  AND g.status IN ('setup', 'active')
  AND f.next_fine_date IS NOT NULL AND f.next_fine_date <= sqlc.arg(today)::date
ORDER BY f.id;

-- name: FinableMembers :many
SELECT m.id FROM memberships m
JOIN contributions c ON c.group_id = m.group_id
WHERE c.id = sqlc.arg(contribution_id) AND m.status = 'active'
  AND (c.applies_to_all_members OR EXISTS (
        SELECT 1 FROM contribution_members cm WHERE cm.contribution_id = c.id AND cm.membership_id = m.id))
ORDER BY m.id;

-- name: ContributionEntriesForFines :many
-- Live contribution-bucket rows of a contribution, for working out fines.
SELECT e.id, e.membership_id, e.entry_type = 'contribution_invoice' AS is_invoice, r.sign,
       e.amount::text AS amount, e.entry_date, e.due_date
FROM ledger_entries e
JOIN ledger_entry_rules r USING (entry_type)
WHERE e.contribution_id = sqlc.arg(contribution_id) AND e.voided_at IS NULL
  AND r.bucket = 'contribution' AND r.sign <> 0
  AND e.membership_id = ANY(sqlc.arg(membership_ids)::bigint[])
  AND e.entry_date <= sqlc.arg(on_date)::date
ORDER BY e.membership_id, e.entry_date, e.id;

-- name: ContributionFineBalances :many
-- Each member's unpaid fines raised on this contribution.
SELECT e.membership_id::bigint AS membership_id, coalesce(sum(r.sign * e.amount), 0)::text AS balance
FROM ledger_entries e
JOIN ledger_entry_rules r USING (entry_type)
WHERE e.contribution_id = sqlc.arg(contribution_id) AND e.voided_at IS NULL AND r.bucket = 'fine'
  AND e.membership_id = ANY(sqlc.arg(membership_ids)::bigint[])
  AND e.entry_date <= sqlc.arg(on_date)::date
GROUP BY e.membership_id;

-- name: PriorInvoiceFines :many
SELECT e.parent_entry_id::bigint AS parent_entry_id, count(*)::int AS fines, sum(e.amount)::text AS amount
FROM ledger_entries e
WHERE e.contribution_id = sqlc.arg(contribution_id) AND e.voided_at IS NULL
  AND e.entry_type = 'contribution_fine_invoice' AND e.parent_entry_id IS NOT NULL
GROUP BY e.parent_entry_id;

-- name: InsertContributionFine :execrows
INSERT INTO ledger_entries (group_id, membership_id, contribution_id, entry_type, amount,
                            entry_date, due_date, description, generated_by, parent_entry_id, fine_rule_id)
VALUES (sqlc.arg(group_id), sqlc.arg(membership_id), sqlc.arg(contribution_id), 'contribution_fine_invoice',
        sqlc.arg(amount)::text::numeric, sqlc.arg(fine_date)::date, sqlc.arg(fine_date)::date, sqlc.arg(description),
        'fines', sqlc.narg(parent_entry_id), sqlc.arg(fine_rule_id))
ON CONFLICT (membership_id, contribution_id, entry_type, entry_date,
             (coalesce(parent_entry_id, 0)), (coalesce(fine_rule_id, 0)))
    WHERE generated_by IS NOT NULL
DO NOTHING;

-- name: SetNextFineDate :exec
UPDATE contribution_fine_rules SET next_fine_date = sqlc.narg(next_fine_date), updated_at = now()
WHERE id = sqlc.arg(id);

-- name: StartJobRun :one
INSERT INTO job_runs (job, run_date, trigger, started_by)
VALUES (sqlc.arg(job), sqlc.arg(run_date), sqlc.arg(trigger), sqlc.narg(started_by))
RETURNING id;

-- name: FinishJobRun :exec
UPDATE job_runs SET finished_at = now(), status = sqlc.arg(status), summary = sqlc.arg(summary), error = sqlc.arg(error)
WHERE id = sqlc.arg(id);

-- name: LastSucceededRun :one
SELECT run_date FROM job_runs WHERE job = $1 AND status = 'succeeded' ORDER BY run_date DESC LIMIT 1;

-- name: ListJobRuns :many
SELECT r.id, r.job, r.run_date, r.trigger, r.started_at, r.finished_at, r.status, r.summary, r.error,
       r.started_by, coalesce(trim(u.first_name || ' ' || u.last_name), '')::text AS started_by_name
FROM job_runs r
LEFT JOIN users u ON u.id = r.started_by
WHERE (sqlc.narg(job)::text IS NULL OR r.job = sqlc.narg(job))
ORDER BY r.started_at DESC, r.id DESC
LIMIT sqlc.arg(lim) OFFSET sqlc.arg(off);

-- name: CountJobRuns :one
SELECT count(*) FROM job_runs WHERE (sqlc.narg(job)::text IS NULL OR job = sqlc.narg(job));

-- name: FailStaleJobRuns :exec
-- A run left 'running' by a process that died.
UPDATE job_runs SET status = 'failed', finished_at = now(), error = 'interrupted'
WHERE status = 'running' AND started_at < now() - interval '6 hours';
