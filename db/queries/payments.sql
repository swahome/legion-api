-- ---- Transaction alerts ----

-- name: FindAccountsByNumber :many
-- Open group accounts with this account number, the bank's own first.
SELECT a.id, a.group_id, a.bank_id, a.kind
FROM group_accounts a
JOIN groups g ON g.id = a.group_id
WHERE a.account_number = sqlc.arg(account_number) AND a.account_number <> ''
  AND a.deleted_at IS NULL AND NOT a.is_closed
ORDER BY (a.bank_id IS NOT DISTINCT FROM sqlc.narg(bank_id)::bigint) DESC, a.is_default DESC, a.id;

-- name: InsertAlert :one
-- Returns nothing (no rows) when the same provider transaction is already in.
INSERT INTO transaction_alerts (source, provider_ref, bank_id, account_number, group_id, account_id, direction,
                                amount, currency, transaction_at, reference, particulars, phone, payer_name, raw)
VALUES (sqlc.arg(source), sqlc.arg(provider_ref), sqlc.narg(bank_id), sqlc.arg(account_number), sqlc.narg(group_id),
        sqlc.narg(account_id), sqlc.arg(direction), sqlc.arg(amount)::text::numeric, sqlc.arg(currency),
        sqlc.arg(transaction_at), sqlc.arg(reference), sqlc.arg(particulars), sqlc.narg(phone), sqlc.arg(payer_name),
        sqlc.arg(raw))
ON CONFLICT (source, provider_ref, direction) DO NOTHING
RETURNING id;

-- name: GetAlertByRef :one
SELECT id FROM transaction_alerts WHERE source = $1 AND provider_ref = $2 AND direction = $3;

-- name: LockAlert :one
SELECT * FROM transaction_alerts WHERE id = $1 FOR UPDATE;

-- name: GetAlertView :one
SELECT t.*, coalesce(g.name, '')::text AS group_name, coalesce(a.name, '')::text AS account_name,
       coalesce(b.name, '')::text AS bank_name,
       coalesce(trim(u.first_name || ' ' || u.last_name), '')::text AS reconciled_by_name
FROM transaction_alerts t
LEFT JOIN groups g ON g.id = t.group_id
LEFT JOIN group_accounts a ON a.id = t.account_id
LEFT JOIN banks b ON b.id = t.bank_id
LEFT JOIN users u ON u.id = t.reconciled_by
WHERE t.id = sqlc.arg(id) AND (sqlc.narg(group_id)::bigint IS NULL OR t.group_id = sqlc.narg(group_id));

-- name: ListAlerts :many
-- One page of alerts, newest first. Every filter is optional; unassigned
-- keeps only alerts no group account matched.
SELECT t.id, t.source, t.provider_ref, t.bank_id, t.account_number, t.group_id, t.account_id, t.direction,
       t.amount::text AS amount, t.currency, t.transaction_at, t.reference, t.particulars, t.phone, t.payer_name,
       t.status, t.auto_reconciled, t.reconciled_at, t.note, t.created_at,
       coalesce(g.name, '')::text AS group_name, coalesce(a.name, '')::text AS account_name,
       coalesce(b.name, '')::text AS bank_name,
       count(*) OVER () AS total
FROM transaction_alerts t
LEFT JOIN groups g ON g.id = t.group_id
LEFT JOIN group_accounts a ON a.id = t.account_id
LEFT JOIN banks b ON b.id = t.bank_id
WHERE (sqlc.narg(group_id)::bigint IS NULL OR t.group_id = sqlc.narg(group_id))
  AND (sqlc.narg(bank_id)::bigint IS NULL OR t.bank_id = sqlc.narg(bank_id)
       OR t.account_id IN (SELECT ba.id FROM group_accounts ba WHERE ba.bank_id = sqlc.narg(bank_id)))
  AND (sqlc.narg(account_id)::bigint IS NULL OR t.account_id = sqlc.narg(account_id))
  AND (sqlc.narg(account_number)::text IS NULL OR t.account_number = sqlc.narg(account_number))
  AND (sqlc.narg(status)::alert_status IS NULL OR t.status = sqlc.narg(status))
  AND (sqlc.narg(direction)::smallint IS NULL OR t.direction = sqlc.narg(direction))
  AND (sqlc.narg(source)::text IS NULL OR t.source = sqlc.narg(source))
  AND (NOT sqlc.arg(unassigned)::bool OR t.group_id IS NULL)
  AND (sqlc.narg(from_date)::date IS NULL OR t.transaction_at >= sqlc.narg(from_date)::date::timestamp AT TIME ZONE 'Africa/Nairobi')
  AND (sqlc.narg(to_date)::date IS NULL OR t.transaction_at < (sqlc.narg(to_date)::date + 1)::timestamp AT TIME ZONE 'Africa/Nairobi')
ORDER BY t.transaction_at DESC, t.id DESC
LIMIT sqlc.arg(page_size) OFFSET sqlc.arg(page_offset);

-- name: AlertCounts :many
-- Unreconciled alerts of a group by direction, for badges and the dashboard.
SELECT direction, count(*)::int AS alerts, coalesce(sum(amount), 0)::text AS amount
FROM transaction_alerts
WHERE group_id = $1 AND status = 'unreconciled'
GROUP BY direction;

-- name: SetAlertStatus :exec
UPDATE transaction_alerts
SET status = sqlc.arg(status), auto_reconciled = sqlc.arg(auto_reconciled), reconciled_by = sqlc.narg(reconciled_by),
    reconciled_at = CASE WHEN sqlc.arg(status)::alert_status = 'unreconciled' THEN NULL ELSE now() END,
    note = sqlc.arg(note), updated_at = now()
WHERE id = sqlc.arg(id);

-- name: AssignAlert :exec
UPDATE transaction_alerts SET group_id = sqlc.arg(group_id), account_id = sqlc.arg(account_id), updated_at = now()
WHERE id = sqlc.arg(id) AND group_id IS NULL;

-- name: TagAlertDeposit :exec
-- Links a deposit to the alert it accounts for. from_alert marks deposits
-- written by reconciliation (undoing it voids them) as opposed to ones an
-- official had already recorded (undoing it only unlinks them).
UPDATE deposits SET transaction_alert_id = sqlc.arg(alert_id),
       refs = CASE WHEN sqlc.arg(from_alert)::bool THEN refs || '{"from_alert": true}'::jsonb ELSE refs END
WHERE id = sqlc.arg(id);

-- name: TagAlertWithdrawal :exec
UPDATE withdrawals SET transaction_alert_id = sqlc.arg(alert_id),
       refs = CASE WHEN sqlc.arg(from_alert)::bool THEN refs || '{"from_alert": true}'::jsonb ELSE refs END
WHERE id = sqlc.arg(id);

-- name: MatchableDeposits :many
-- Live deposits not yet tied to an alert, for linking by hand.
SELECT id, account_id, amount::text AS amount
FROM deposits
WHERE group_id = sqlc.arg(group_id) AND id = ANY(sqlc.arg(ids)::bigint[]) AND voided_at IS NULL
  AND transaction_alert_id IS NULL
FOR UPDATE;

-- name: MatchableWithdrawals :many
SELECT id, account_id, amount::text AS amount
FROM withdrawals
WHERE group_id = sqlc.arg(group_id) AND id = ANY(sqlc.arg(ids)::bigint[]) AND voided_at IS NULL
  AND transaction_alert_id IS NULL
FOR UPDATE;

-- name: AlertDeposits :many
SELECT d.id, d.kind, d.amount::text AS amount, d.deposit_date, d.description, d.membership_id, d.voided_at,
       (d.refs ? 'from_alert')::bool AS from_alert,
       coalesce(trim(u.first_name || ' ' || u.last_name), '')::text AS member_name,
       coalesce(c.name, f.name, i.name, '')::text AS item_name, d.loan_id
FROM deposits d
LEFT JOIN memberships m ON m.id = d.membership_id
LEFT JOIN users u ON u.id = m.user_id
LEFT JOIN contributions c ON c.id = d.contribution_id
LEFT JOIN fine_categories f ON f.id = d.fine_category_id
LEFT JOIN income_categories i ON i.id = d.income_category_id
WHERE d.transaction_alert_id = $1 AND d.voided_at IS NULL
ORDER BY d.id;

-- name: AlertWithdrawals :many
SELECT w.id, w.kind, w.amount::text AS amount, w.withdrawal_date, w.description, w.membership_id, w.voided_at,
       (w.refs ? 'from_alert')::bool AS from_alert,
       coalesce(trim(u.first_name || ' ' || u.last_name), '')::text AS member_name,
       coalesce(e.name, c.name, '')::text AS item_name
FROM withdrawals w
LEFT JOIN memberships m ON m.id = w.membership_id
LEFT JOIN users u ON u.id = m.user_id
LEFT JOIN expense_categories e ON e.id = w.expense_category_id
LEFT JOIN contributions c ON c.id = w.contribution_id
WHERE w.transaction_alert_id = $1 AND w.voided_at IS NULL
ORDER BY w.id;

-- name: UnlinkAlertRecords :exec
UPDATE deposits SET transaction_alert_id = NULL WHERE transaction_alert_id = sqlc.arg(alert_id) AND voided_at IS NULL
  AND NOT (refs ? 'from_alert');

-- name: UnlinkAlertWithdrawals :exec
UPDATE withdrawals SET transaction_alert_id = NULL WHERE transaction_alert_id = sqlc.arg(alert_id) AND voided_at IS NULL
  AND NOT (refs ? 'from_alert');

-- name: DepositIDForRepayment :one
SELECT deposit_id::bigint FROM loan_repayments WHERE id = $1;

-- name: MembersByPhone :many
-- Active members of a group whose phone is one of these (E.164).
SELECT m.id AS membership_id, u.phone::text AS phone
FROM memberships m JOIN users u ON u.id = m.user_id
WHERE m.group_id = sqlc.arg(group_id) AND m.status = 'active' AND u.phone = ANY(sqlc.arg(phones)::text[]);

-- name: MemberNames :many
SELECT m.id AS membership_id, u.first_name, u.middle_name, u.last_name, coalesce(u.phone, '')::text AS phone
FROM memberships m JOIN users u ON u.id = m.user_id
WHERE m.group_id = $1 AND m.status = 'active';

-- name: GroupReconcileFlags :one
SELECT coalesce(settings->>'enable_auto_reconcile', '')::text AS enable_auto_reconcile,
       coalesce(settings->>'disable_auto_reconcile', '')::text AS disable_auto_reconcile
FROM groups WHERE id = $1;

-- name: SetGroupReconcileFlags :exec
UPDATE groups SET settings = settings || jsonb_build_object(
        'enable_auto_reconcile', sqlc.arg(enable_auto_reconcile)::text,
        'disable_auto_reconcile', sqlc.arg(disable_auto_reconcile)::text),
    updated_at = now()
WHERE id = sqlc.arg(id);

-- ---- What a member owes, for payments and auto-reconciliation ----

-- name: MemberLedgerArrears :many
-- What a member owes per contribution and fine, oldest item first. bucket is
-- contribution or fine; a fine has contribution_id (a late fine) or
-- fine_category_id.
SELECT r.bucket, e.contribution_id, e.fine_category_id,
       coalesce(c.name, f.name, '')::text AS name,
       sum(r.sign * e.amount)::text AS balance
FROM ledger_entries e
JOIN ledger_entry_rules r USING (entry_type)
LEFT JOIN contributions c ON c.id = e.contribution_id
LEFT JOIN fine_categories f ON f.id = e.fine_category_id
WHERE e.group_id = sqlc.arg(group_id) AND e.membership_id = sqlc.arg(membership_id) AND e.voided_at IS NULL
  AND r.bucket IN ('contribution', 'fine')
GROUP BY r.bucket, e.contribution_id, e.fine_category_id, c.name, f.name
HAVING sum(r.sign * e.amount) > 0
ORDER BY min(e.entry_date), r.bucket, e.contribution_id NULLS LAST, e.fine_category_id NULLS LAST;

-- name: MemberOpenLoans :many
SELECT l.id, coalesce(lt.name, 'Loan')::text AS name, l.disbursed_on,
       (coalesce((SELECT sum(c.amount) FROM loan_charges c WHERE c.loan_id = l.id AND c.voided_at IS NULL), 0)
      - coalesce((SELECT sum(p.amount) FROM loan_repayments p WHERE p.loan_id = l.id AND p.voided_at IS NULL), 0))::text AS balance
FROM loans l
LEFT JOIN loan_types lt ON lt.id = l.loan_type_id
WHERE l.group_id = sqlc.arg(group_id) AND l.membership_id = sqlc.arg(membership_id) AND l.voided_at IS NULL
  AND coalesce((SELECT sum(c.amount) FROM loan_charges c WHERE c.loan_id = l.id AND c.voided_at IS NULL), 0)
    > coalesce((SELECT sum(p.amount) FROM loan_repayments p WHERE p.loan_id = l.id AND p.voided_at IS NULL), 0)
ORDER BY l.disbursed_on, l.id;

-- name: PayableContributions :many
-- Contributions a member can pay into, savings first.
SELECT id, name, coalesce(category::text, '')::text AS category
FROM contributions
WHERE group_id = $1 AND is_active AND NOT is_closed AND deleted_at IS NULL
ORDER BY (category = 'savings') DESC NULLS LAST, id;

-- name: LoanOfMember :one
SELECT EXISTS (SELECT 1 FROM loans WHERE group_id = $1 AND id = $2 AND membership_id = $3 AND voided_at IS NULL);

-- ---- Payment requests ----

-- name: DefaultPaymentAccount :one
-- The account members pay into: the group's default bank account, else its
-- only open bank account with a number.
SELECT id, account_number, bank_id, coalesce(settings->>'account_password', '')::text AS account_password
FROM group_accounts
WHERE group_id = $1 AND kind = 'bank' AND deleted_at IS NULL AND NOT is_closed AND account_number <> ''
ORDER BY is_default DESC, id
LIMIT 1;

-- name: CreatePaymentRequest :one
INSERT INTO payment_requests (group_id, membership_id, user_id, account_id, account_number, phone, amount,
                              reference, lines, channel, provider)
VALUES (sqlc.arg(group_id), sqlc.narg(membership_id), sqlc.narg(user_id), sqlc.narg(account_id),
        sqlc.arg(account_number), sqlc.arg(phone), sqlc.arg(amount)::text::numeric, sqlc.arg(reference),
        sqlc.arg(lines), sqlc.arg(channel), sqlc.arg(provider))
RETURNING id;

-- name: SetPaymentRequestSent :exec
UPDATE payment_requests SET status = sqlc.arg(status), response_code = sqlc.arg(response_code),
       response_description = sqlc.arg(response_description), result_description = sqlc.arg(result_description),
       updated_at = now()
WHERE id = sqlc.arg(id);

-- name: LockPaymentRequestByReference :one
SELECT * FROM payment_requests WHERE reference = $1 FOR UPDATE;

-- name: SetPaymentRequestResult :exec
UPDATE payment_requests SET status = sqlc.arg(status), result_code = sqlc.arg(result_code),
       result_description = sqlc.arg(result_description), provider_ref = sqlc.narg(provider_ref),
       transaction_alert_id = sqlc.narg(transaction_alert_id), updated_at = now()
WHERE id = sqlc.arg(id);

-- name: ListPaymentRequests :many
SELECT p.id, p.group_id, p.membership_id, p.phone, p.amount::text AS amount, p.reference, p.lines, p.channel,
       p.status, p.response_description, p.result_code, p.result_description, p.provider_ref,
       p.transaction_alert_id, p.created_at, p.updated_at,
       coalesce(trim(u.first_name || ' ' || u.last_name), '')::text AS member_name,
       coalesce(g.name, '')::text AS group_name,
       count(*) OVER () AS total
FROM payment_requests p
JOIN groups g ON g.id = p.group_id
LEFT JOIN memberships m ON m.id = p.membership_id
LEFT JOIN users u ON u.id = m.user_id
WHERE (sqlc.narg(group_id)::bigint IS NULL OR p.group_id = sqlc.narg(group_id))
  AND (sqlc.narg(membership_id)::bigint IS NULL OR p.membership_id = sqlc.narg(membership_id))
  AND (sqlc.narg(status)::payment_request_status IS NULL OR p.status = sqlc.narg(status))
ORDER BY p.created_at DESC, p.id DESC
LIMIT sqlc.arg(page_size) OFFSET sqlc.arg(page_offset);

-- name: GetPaymentRequest :one
SELECT p.*, coalesce(trim(u.first_name || ' ' || u.last_name), '')::text AS member_name
FROM payment_requests p
LEFT JOIN memberships m ON m.id = p.membership_id
LEFT JOIN users u ON u.id = m.user_id
WHERE p.group_id = $1 AND p.id = $2;

-- name: StalePaymentRequests :many
-- Requests still waiting on MPesa after the cut-off: the member never
-- answered the prompt, or the callback was lost.
SELECT id FROM payment_requests WHERE status IN ('pending', 'sent') AND created_at < $1 LIMIT 500;

-- name: ExpirePaymentRequest :exec
UPDATE payment_requests SET status = 'failed', result_code = 'timeout',
       result_description = 'No answer from MPesa in time', updated_at = now()
WHERE id = $1 AND status IN ('pending', 'sent');

-- ---- Webhook log ----

-- name: LogWebhook :one
INSERT INTO webhook_events (source, remote_ip, body) VALUES ($1, $2, $3) RETURNING id;

-- name: FinishWebhook :exec
UPDATE webhook_events SET outcome = sqlc.arg(outcome), detail = sqlc.arg(detail), alert_id = sqlc.narg(alert_id)
WHERE id = sqlc.arg(id);

-- name: ListWebhookEvents :many
SELECT id, source, remote_ip, body, outcome, detail, alert_id, created_at, count(*) OVER () AS total
FROM webhook_events
WHERE (sqlc.narg(source)::text IS NULL OR source = sqlc.narg(source))
  AND (sqlc.narg(outcome)::text IS NULL OR outcome = sqlc.narg(outcome))
ORDER BY id DESC
LIMIT sqlc.arg(page_size) OFFSET sqlc.arg(page_offset);

-- ---- Bank panel ----

-- name: UserBanks :many
SELECT b.id, b.name, b.slug, b.logo, b.branding, bu.role
FROM bank_users bu JOIN banks b ON b.id = bu.bank_id
WHERE bu.user_id = $1
ORDER BY b.name;

-- name: BankUserRole :one
SELECT role FROM bank_users WHERE bank_id = $1 AND user_id = $2;

-- name: BanksWithAccounts :many
-- For platform admins: every bank that holds at least one group account.
SELECT b.id, b.name, b.slug, b.logo, b.branding
FROM banks b
WHERE EXISTS (SELECT 1 FROM group_accounts a WHERE a.bank_id = b.id AND a.deleted_at IS NULL)
   OR EXISTS (SELECT 1 FROM bank_users bu WHERE bu.bank_id = b.id)
ORDER BY b.name;

-- name: GetBankBasic :one
SELECT id, name, slug, logo, branding FROM banks WHERE id = $1;

-- name: BankGroups :many
-- Groups holding an account at the bank, with that bank's accounts summed.
SELECT g.id, g.name, g.status, g.created_at,
       count(DISTINCT a.id)::int AS accounts,
       string_agg(DISTINCT a.account_number, ', ')::text AS account_numbers,
       coalesce(string_agg(DISTINCT br.name, ', '), '')::text AS branches,
       (SELECT count(*) FROM memberships m WHERE m.group_id = g.id AND m.status = 'active')::int AS members,
       (SELECT count(*) FROM transaction_alerts t WHERE t.group_id = g.id AND t.status = 'unreconciled'
          AND t.account_id IN (SELECT x.id FROM group_accounts x WHERE x.bank_id = sqlc.arg(bank_id)))::int AS unreconciled,
       coalesce(sum(a.initial_balance + coalesce((SELECT sum(e.direction * e.amount) FROM account_entries e
                     WHERE e.account_id = a.id AND e.voided_at IS NULL), 0)), 0)::text AS balance,
       count(*) OVER () AS total
FROM groups g
JOIN group_accounts a ON a.group_id = g.id AND a.bank_id = sqlc.arg(bank_id) AND a.deleted_at IS NULL
LEFT JOIN bank_branches br ON br.id = a.bank_branch_id
WHERE (sqlc.narg(search)::text IS NULL OR g.name ILIKE '%' || sqlc.narg(search) || '%'
       OR a.account_number = sqlc.narg(search))
  AND (sqlc.narg(branch_id)::bigint IS NULL OR a.bank_branch_id = sqlc.narg(branch_id))
GROUP BY g.id
ORDER BY g.name
LIMIT sqlc.arg(page_size) OFFSET sqlc.arg(page_offset);

-- name: BankGroupAccounts :many
SELECT a.id, a.name, a.account_number, a.is_default, a.is_closed, coalesce(br.name, '')::text AS branch,
       (a.initial_balance + coalesce((SELECT sum(e.direction * e.amount) FROM account_entries e
          WHERE e.account_id = a.id AND e.voided_at IS NULL), 0))::text AS balance
FROM group_accounts a
LEFT JOIN bank_branches br ON br.id = a.bank_branch_id
WHERE a.group_id = sqlc.arg(group_id) AND a.bank_id = sqlc.arg(bank_id) AND a.deleted_at IS NULL
ORDER BY a.id;

-- name: BankBranchKPIs :many
-- Per branch: accounts, groups, groups that opened an account in the
-- period, and money in and out of the feeds in the period.
WITH acc AS (
    SELECT a.id, a.group_id, a.bank_branch_id, a.created_at
    FROM group_accounts a WHERE a.bank_id = sqlc.arg(bank_id) AND a.deleted_at IS NULL
), feed AS (
    SELECT acc.bank_branch_id,
           coalesce(sum(t.amount) FILTER (WHERE t.direction = 1), 0) AS money_in,
           coalesce(sum(t.amount) FILTER (WHERE t.direction = -1), 0) AS money_out,
           count(t.id) AS alerts
    FROM acc JOIN transaction_alerts t ON t.account_id = acc.id
    WHERE t.transaction_at >= sqlc.arg(from_date)::date::timestamp AT TIME ZONE 'Africa/Nairobi'
      AND t.transaction_at < (sqlc.arg(to_date)::date + 1)::timestamp AT TIME ZONE 'Africa/Nairobi'
    GROUP BY acc.bank_branch_id
)
SELECT acc.bank_branch_id, coalesce(br.name, 'No branch')::text AS branch,
       count(DISTINCT acc.id)::int AS accounts, count(DISTINCT acc.group_id)::int AS groups,
       count(DISTINCT acc.group_id) FILTER (WHERE acc.created_at >= sqlc.arg(from_date)::date::timestamp AT TIME ZONE 'Africa/Nairobi'
            AND acc.created_at < (sqlc.arg(to_date)::date + 1)::timestamp AT TIME ZONE 'Africa/Nairobi')::int AS new_groups,
       coalesce(max(f.money_in), 0)::text AS money_in, coalesce(max(f.money_out), 0)::text AS money_out,
       coalesce(max(f.alerts), 0)::int AS alerts
FROM acc
LEFT JOIN bank_branches br ON br.id = acc.bank_branch_id
LEFT JOIN feed f ON f.bank_branch_id IS NOT DISTINCT FROM acc.bank_branch_id
GROUP BY acc.bank_branch_id, br.name
ORDER BY count(DISTINCT acc.group_id) DESC, br.name;

-- name: BankGroupOK :one
SELECT EXISTS (SELECT 1 FROM group_accounts WHERE group_id = $1 AND bank_id = $2 AND deleted_at IS NULL);

-- name: ListBankUsers :many
SELECT bu.user_id, bu.role, bu.created_at, u.first_name, u.last_name, coalesce(u.phone, '')::text AS phone,
       coalesce(u.email, '')::text AS email
FROM bank_users bu JOIN users u ON u.id = bu.user_id
WHERE bu.bank_id = $1
ORDER BY u.first_name, u.last_name;

-- name: UpsertBankUser :exec
INSERT INTO bank_users (bank_id, user_id, role, created_by) VALUES ($1, $2, $3, $4)
ON CONFLICT (bank_id, user_id) DO UPDATE SET role = EXCLUDED.role;

-- name: DeleteBankUser :execrows
DELETE FROM bank_users WHERE bank_id = $1 AND user_id = $2;

-- name: UserIDByPhone :one
SELECT id FROM users WHERE phone = $1;

-- name: BankIDBySlug :one
SELECT id FROM banks WHERE slug = $1;

-- name: AccountGroup :one
SELECT group_id FROM group_accounts WHERE id = $1 AND deleted_at IS NULL;

-- name: BankGroupBasic :one
SELECT g.id, g.name, g.status, g.created_at, g.currency,
       (SELECT count(*) FROM memberships m WHERE m.group_id = g.id AND m.status = 'active')::int AS members
FROM groups g WHERE g.id = $1;
