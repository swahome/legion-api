-- Loans (milestone 4). A loan's balance is its live charges less its live
-- repayments; nothing is stored.

-- ---- Loan types ----

-- name: ListLoanTypes :many
SELECT * FROM loan_types
WHERE group_id = $1 AND deleted_at IS NULL AND (sqlc.arg(include_hidden)::bool OR NOT is_hidden)
  AND for_debtors = sqlc.arg(for_debtors)::bool
ORDER BY name, id;

-- name: GetLoanType :one
SELECT * FROM loan_types WHERE group_id = $1 AND id = $2 AND deleted_at IS NULL;

-- name: LoanTypeNameTaken :one
SELECT EXISTS (
    SELECT 1 FROM loan_types
    WHERE group_id = $1 AND lower(name) = lower(sqlc.arg(name)::text) AND deleted_at IS NULL
      AND id <> sqlc.arg(except_id)::bigint
);

-- name: CreateLoanType :one
INSERT INTO loan_types (group_id, name, interest_type, interest_rate, rate_per, reducing_method, fixed_interest,
                        fixed_interest_rate, frequency, fixed_term, min_term, max_term, grace_periods, amount_basis,
                        min_amount, max_amount, savings_multiple, guarantors, min_guarantors, max_guarantors,
                        late_fine, balance_fine, processing_fee, fine_deferment, needs_approval, is_hidden, created_by,
                        for_debtors)
VALUES ($1, $2, $3, sqlc.arg(interest_rate)::numeric, $4, $5, sqlc.arg(fixed_interest)::numeric,
        sqlc.arg(fixed_interest_rate)::numeric, $6, $7, $8, $9, $10, $11,
        sqlc.narg(min_amount)::numeric, sqlc.narg(max_amount)::numeric, sqlc.narg(savings_multiple)::numeric,
        $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22)
RETURNING id;

-- name: UpdateLoanType :exec
UPDATE loan_types SET
    name = $3, interest_type = $4, interest_rate = sqlc.arg(interest_rate)::numeric, rate_per = $5,
    reducing_method = $6, fixed_interest = sqlc.arg(fixed_interest)::numeric,
    fixed_interest_rate = sqlc.arg(fixed_interest_rate)::numeric, frequency = $7, fixed_term = $8,
    min_term = $9, max_term = $10, grace_periods = $11, amount_basis = $12,
    min_amount = sqlc.narg(min_amount)::numeric, max_amount = sqlc.narg(max_amount)::numeric,
    savings_multiple = sqlc.narg(savings_multiple)::numeric, guarantors = $13, min_guarantors = $14,
    max_guarantors = $15, late_fine = $16, balance_fine = $17, processing_fee = $18, fine_deferment = $19,
    needs_approval = $20, is_hidden = $21, updated_at = now()
WHERE group_id = $1 AND id = $2 AND deleted_at IS NULL;

-- name: DeleteLoanType :execrows
UPDATE loan_types SET deleted_at = now(), updated_at = now()
WHERE group_id = $1 AND id = $2 AND deleted_at IS NULL;

-- ---- Loans ----

-- name: CreateLoan :one
INSERT INTO loans (group_id, membership_id, debtor_id, loan_type_id, amount, disbursed_on, account_id, interest_type,
                   interest_rate, rate_per, reducing_method, fixed_interest, fixed_interest_rate, frequency,
                   installments, grace_periods, first_due_on, ends_on, late_fine, balance_fine, processing_fee,
                   fine_deferment, next_balance_fine_on, description, created_by)
VALUES ($1, $2, $3, $4, sqlc.arg(amount)::numeric, $5, $6, $7, sqlc.arg(interest_rate)::numeric, $8, $9,
        sqlc.arg(fixed_interest)::numeric, sqlc.arg(fixed_interest_rate)::numeric, $10, $11, $12, $13, $14,
        $15, $16, $17, $18, $19, $20, $21)
RETURNING id;

-- name: CreateLoanCharge :one
INSERT INTO loan_charges (group_id, loan_id, kind, number, invoice_on, due_on, next_fine_on, principal, interest,
                          amount, parent_id, generated_by, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, sqlc.arg(principal)::numeric, sqlc.arg(interest)::numeric,
        sqlc.arg(amount)::numeric, $8, $9, $10)
ON CONFLICT DO NOTHING
RETURNING id;

-- name: CreateLoanGuarantor :exec
INSERT INTO loan_guarantors (group_id, loan_id, membership_id, amount, comment, created_by)
VALUES ($1, $2, $3, sqlc.narg(amount)::numeric, $4, $5);

-- name: SetWithdrawalLoan :exec
UPDATE withdrawals SET loan_id = $2 WHERE id = $1;

-- name: SetDepositLoan :exec
UPDATE deposits SET loan_id = $2 WHERE id = $1;

-- name: CreateLoanRepayment :one
INSERT INTO loan_repayments (group_id, loan_id, membership_id, deposit_id, account_id, method, amount, paid_on,
                             source, description, created_by)
VALUES ($1, $2, $3, $4, $5, $6, sqlc.arg(amount)::numeric, $7, $8, $9, $10)
RETURNING id;

-- name: GetLoanForUpdate :one
SELECT * FROM loans WHERE group_id = $1 AND id = $2 FOR UPDATE;

-- name: VoidLoan :execrows
UPDATE loans SET voided_at = now(), voided_by = $3, updated_at = now()
WHERE group_id = $1 AND id = $2 AND voided_at IS NULL;

-- name: VoidLoanCharges :exec
UPDATE loan_charges SET voided_at = now() WHERE loan_id = $1 AND voided_at IS NULL;

-- name: LoanMoneyRows :many
-- The live deposits and withdrawals that belong to a loan, to void with it.
SELECT 'deposit'::text AS kind, d.id FROM deposits d WHERE d.loan_id = $1 AND d.voided_at IS NULL
UNION ALL
SELECT 'withdrawal'::text, w.id FROM withdrawals w WHERE w.loan_id = $1 AND w.voided_at IS NULL;

-- name: VoidLoanRepayments :exec
UPDATE loan_repayments SET voided_at = now(), voided_by = $2 WHERE loan_id = $1 AND voided_at IS NULL;

-- name: VoidRepaymentByDeposit :exec
UPDATE loan_repayments SET voided_at = now(), voided_by = $2 WHERE deposit_id = $1 AND voided_at IS NULL;

-- name: GetLoanRepayment :one
SELECT * FROM loan_repayments WHERE group_id = $1 AND id = $2;

-- name: VoidLoanRepayment :execrows
UPDATE loan_repayments SET voided_at = now(), voided_by = $3
WHERE group_id = $1 AND id = $2 AND voided_at IS NULL;

-- name: VoidLoanCharge :execrows
UPDATE loan_charges SET voided_at = now()
WHERE group_id = $1 AND loan_id = $2 AND id = $3 AND voided_at IS NULL AND kind IN ('late_fine', 'balance_fine');

-- name: UpdateLoan :exec
UPDATE loans SET description = $3, is_bad_debt = $4, updated_at = now()
WHERE group_id = $1 AND id = $2 AND voided_at IS NULL;

-- name: ListLoans :many
-- One page of loans with their totals. status: '' all, 'open' (owing),
-- 'paid', 'voided'.
WITH t AS (
    SELECT l.id,
           coalesce((SELECT sum(c.amount) FROM loan_charges c WHERE c.loan_id = l.id AND c.voided_at IS NULL), 0) AS payable,
           coalesce((SELECT sum(c.amount) FROM loan_charges c WHERE c.loan_id = l.id AND c.voided_at IS NULL
                     AND c.kind IN ('late_fine', 'balance_fine')), 0) AS fines,
           coalesce((SELECT sum(c.interest) FROM loan_charges c WHERE c.loan_id = l.id AND c.voided_at IS NULL), 0) AS interest,
           coalesce((SELECT sum(r.amount) FROM loan_repayments r WHERE r.loan_id = l.id AND r.voided_at IS NULL), 0) AS paid,
           coalesce((SELECT sum(c.amount) FROM loan_charges c WHERE c.loan_id = l.id AND c.voided_at IS NULL
                     AND c.due_on < sqlc.arg(today)::date), 0) AS due_to_date
    FROM loans l
    WHERE l.group_id = sqlc.arg(group_id)
      AND (l.debtor_id IS NOT NULL) = sqlc.arg(debtors)::bool
      AND (sqlc.narg(membership_id)::bigint IS NULL OR l.membership_id = sqlc.narg(membership_id))
      AND (sqlc.narg(debtor_id)::bigint IS NULL OR l.debtor_id = sqlc.narg(debtor_id))
      AND (sqlc.narg(loan_type_id)::bigint IS NULL OR l.loan_type_id = sqlc.narg(loan_type_id))
)
SELECT l.id, l.membership_id, l.debtor_id, l.loan_type_id, l.amount::text AS amount, l.disbursed_on, l.ends_on, l.account_id,
       l.interest_type, l.interest_rate::text AS interest_rate, l.rate_per, l.frequency, l.installments,
       l.is_bad_debt, l.description, l.created_at, l.voided_at,
       coalesce(u.first_name || ' ' || u.last_name, dr.name)::text AS member_name,
       coalesce(lt.name, '')::text AS loan_type_name,
       t.payable::text AS payable, t.fines::text AS fines, t.interest::text AS interest, t.paid::text AS paid,
       (t.payable - t.paid)::text AS balance, greatest(t.due_to_date - t.paid, 0)::text AS overdue,
       count(*) OVER () AS total
FROM loans l
JOIN t ON t.id = l.id
LEFT JOIN memberships m ON m.id = l.membership_id
LEFT JOIN users u ON u.id = m.user_id
LEFT JOIN debtors dr ON dr.id = l.debtor_id
LEFT JOIN loan_types lt ON lt.id = l.loan_type_id
WHERE CASE sqlc.arg(status)::text
        WHEN 'open' THEN l.voided_at IS NULL AND t.payable - t.paid > 0
        WHEN 'paid' THEN l.voided_at IS NULL AND t.payable - t.paid <= 0
        WHEN 'voided' THEN l.voided_at IS NOT NULL
        ELSE l.voided_at IS NULL
      END
ORDER BY l.disbursed_on DESC, l.id DESC
LIMIT sqlc.arg(page_size) OFFSET sqlc.arg(page_offset);

-- name: GetLoanView :one
-- member_name is the borrower's name: the member's, or the debtor's.
SELECT l.*, coalesce(u.first_name || ' ' || u.last_name, dr.name)::text AS member_name,
       coalesce(lt.name, '')::text AS loan_type_name, coalesce(a.name, '')::text AS account_name
FROM loans l
LEFT JOIN memberships m ON m.id = l.membership_id
LEFT JOIN users u ON u.id = m.user_id
LEFT JOIN debtors dr ON dr.id = l.debtor_id
LEFT JOIN loan_types lt ON lt.id = l.loan_type_id
LEFT JOIN group_accounts a ON a.id = l.account_id
WHERE l.group_id = $1 AND l.id = $2;

-- name: LoanCharges :many
SELECT id, kind, number, invoice_on, due_on, next_fine_on, principal::text AS principal, interest::text AS interest,
       amount::text AS amount, parent_id, no_fines, sent_at, generated_by, created_at, voided_at
FROM loan_charges
WHERE loan_id = $1 AND (sqlc.arg(include_voided)::bool OR voided_at IS NULL)
ORDER BY due_on, (kind <> 'installment'), id;

-- name: LoanRepayments :many
SELECT r.id, r.deposit_id, r.account_id, coalesce(a.name, '')::text AS account_name, r.method, r.amount::text AS amount,
       r.paid_on, r.source, r.description, r.created_at, r.voided_at
FROM loan_repayments r
LEFT JOIN group_accounts a ON a.id = r.account_id
WHERE r.loan_id = $1 AND (sqlc.arg(include_voided)::bool OR r.voided_at IS NULL)
ORDER BY r.paid_on, r.id;

-- name: LoanGuarantors :many
SELECT g.id, g.membership_id, (u.first_name || ' ' || u.last_name)::text AS member_name,
       coalesce(g.amount, 0)::text AS amount, g.comment, (g.amount IS NULL)::bool AS no_amount
FROM loan_guarantors g
JOIN memberships m ON m.id = g.membership_id
JOIN users u ON u.id = m.user_id
WHERE g.loan_id = $1
ORDER BY g.id;

-- name: MemberSavings :one
-- What a member has paid into the group's contributions, net of refunds:
-- the base a "times savings" loan limit multiplies.
SELECT coalesce(sum(CASE entry_type WHEN 'contribution_payment' THEN amount
                                    WHEN 'contribution_refund' THEN -amount ELSE 0 END), 0)::text
FROM ledger_entries
WHERE group_id = $1 AND membership_id = $2 AND voided_at IS NULL;

-- name: LoanTotals :one
-- Group totals for the dashboard and loan reports.
SELECT count(*) FILTER (WHERE l.voided_at IS NULL) AS loans,
       coalesce(sum(l.amount) FILTER (WHERE l.voided_at IS NULL), 0)::text AS lent,
       coalesce(sum(c.payable) FILTER (WHERE l.voided_at IS NULL), 0)::text AS payable,
       coalesce(sum(r.paid) FILTER (WHERE l.voided_at IS NULL), 0)::text AS repaid
FROM loans l
LEFT JOIN LATERAL (SELECT sum(amount) AS payable FROM loan_charges WHERE loan_id = l.id AND voided_at IS NULL) c ON true
LEFT JOIN LATERAL (SELECT sum(amount) AS paid FROM loan_repayments WHERE loan_id = l.id AND voided_at IS NULL) r ON true
WHERE l.group_id = $1 AND (l.debtor_id IS NOT NULL) = sqlc.arg(debtors)::bool;

-- name: DueLoanFines :many
-- Live loans with a late or outstanding-balance fine due on or before today.
SELECT l.group_id, l.id FROM loans l
WHERE l.voided_at IS NULL AND NOT l.is_bad_debt AND (
    (l.balance_fine IS NOT NULL AND l.next_balance_fine_on <= sqlc.arg(today)::date)
    OR (l.late_fine IS NOT NULL AND EXISTS (
        SELECT 1 FROM loan_charges c
        WHERE c.loan_id = l.id AND c.kind = 'installment' AND c.voided_at IS NULL AND NOT c.no_fines
          AND c.next_fine_on <= sqlc.arg(today)::date)))
ORDER BY l.group_id, l.id;

-- name: SetChargeNextFine :exec
UPDATE loan_charges SET next_fine_on = $2 WHERE id = $1;

-- name: SetLoanNextBalanceFine :exec
UPDATE loans SET next_balance_fine_on = $2 WHERE id = $1;

-- name: CreateLoanApplication :one
INSERT INTO loan_applications (group_id, membership_id, loan_type_id, amount, installments, description, status, created_by)
VALUES ($1, $2, $3, sqlc.arg(amount)::numeric, $4, $5, $6, $7)
RETURNING id;

-- name: CreateApplicationGuarantor :exec
INSERT INTO loan_application_guarantors (group_id, application_id, membership_id, amount, comment)
VALUES ($1, $2, $3, sqlc.narg(amount)::numeric, $4);

-- name: CreateApplicationApproval :exec
INSERT INTO loan_application_approvals (group_id, application_id, membership_id, decision, decided_at, decided_by)
VALUES ($1, $2, $3, sqlc.arg(decision)::approval_decision,
        CASE WHEN sqlc.arg(decision)::approval_decision = 'pending' THEN NULL ELSE now() END, sqlc.narg(decided_by));

-- name: GetLoanApplicationForUpdate :one
SELECT * FROM loan_applications WHERE group_id = $1 AND id = $2 FOR UPDATE;

-- name: SetLoanApplicationStatus :exec
UPDATE loan_applications
SET status = sqlc.arg(status), decline_reason = sqlc.arg(decline_reason), loan_id = coalesce(sqlc.narg(loan_id), loan_id),
    decided_at = CASE WHEN sqlc.arg(status)::loan_application_status IN ('pending_guarantors', 'pending_approval')
                      THEN decided_at ELSE coalesce(decided_at, now()) END,
    updated_at = now()
WHERE id = sqlc.arg(id);

-- name: DecideApplicationGuarantor :execrows
UPDATE loan_application_guarantors SET decision = sqlc.arg(decision), decided_at = now(), reason = sqlc.arg(reason)
WHERE application_id = sqlc.arg(application_id) AND membership_id = sqlc.arg(membership_id) AND decision = 'pending';

-- name: DecideApplicationApproval :execrows
UPDATE loan_application_approvals SET decision = sqlc.arg(decision), decided_at = now(),
    comment = sqlc.arg(comment), decided_by = sqlc.arg(decided_by)
WHERE application_id = sqlc.arg(application_id) AND membership_id = sqlc.arg(membership_id) AND decision = 'pending';

-- name: ApplicationTally :one
SELECT (SELECT count(*) FROM loan_application_guarantors g WHERE g.application_id = $1) AS guarantors,
       (SELECT count(*) FROM loan_application_guarantors g WHERE g.application_id = $1 AND g.decision = 'approved') AS guarantors_approved,
       (SELECT count(*) FROM loan_application_guarantors g WHERE g.application_id = $1 AND g.decision = 'declined') AS guarantors_declined,
       (SELECT count(*) FROM loan_application_approvals a WHERE a.application_id = $1) AS signatories,
       (SELECT count(*) FROM loan_application_approvals a WHERE a.application_id = $1 AND a.decision = 'approved') AS signatories_approved,
       (SELECT count(*) FROM loan_application_approvals a WHERE a.application_id = $1 AND a.decision = 'declined') AS signatories_declined;

-- name: ListLoanApplications :many
-- Applications newest first. A member who isn't an official sees their own
-- and those they are asked to guarantee or approve (viewer).
SELECT a.id, a.membership_id, (u.first_name || ' ' || u.last_name)::text AS member_name, a.loan_type_id,
       t.name AS loan_type_name, a.amount::text AS amount, a.installments, a.description, a.status, a.decline_reason,
       a.decided_at, a.loan_id, a.created_at,
       EXISTS (SELECT 1 FROM loan_application_guarantors g WHERE g.application_id = a.id
               AND g.membership_id = sqlc.arg(viewer) AND g.decision = 'pending' AND a.status = 'pending_guarantors')::bool AS awaiting_my_guarantee,
       EXISTS (SELECT 1 FROM loan_application_approvals x WHERE x.application_id = a.id
               AND x.membership_id = sqlc.arg(viewer) AND x.decision = 'pending' AND a.status = 'pending_approval')::bool AS awaiting_my_approval,
       count(*) OVER () AS total
FROM loan_applications a
JOIN memberships m ON m.id = a.membership_id
JOIN users u ON u.id = m.user_id
JOIN loan_types t ON t.id = a.loan_type_id
WHERE a.group_id = sqlc.arg(group_id)
  AND (sqlc.arg(status)::text = '' OR a.status::text = sqlc.arg(status)
       OR (sqlc.arg(status) = 'open' AND a.status IN ('pending_guarantors', 'pending_approval', 'approved')))
  AND (sqlc.narg(membership_id)::bigint IS NULL OR a.membership_id = sqlc.narg(membership_id))
  AND (sqlc.arg(all_visible)::bool OR a.membership_id = sqlc.arg(viewer)
       OR EXISTS (SELECT 1 FROM loan_application_guarantors g WHERE g.application_id = a.id AND g.membership_id = sqlc.arg(viewer))
       OR EXISTS (SELECT 1 FROM loan_application_approvals x WHERE x.application_id = a.id AND x.membership_id = sqlc.arg(viewer)))
  AND (NOT sqlc.arg(awaiting_me)::bool
       OR EXISTS (SELECT 1 FROM loan_application_guarantors g WHERE g.application_id = a.id AND g.membership_id = sqlc.arg(viewer)
                  AND g.decision = 'pending' AND a.status = 'pending_guarantors')
       OR EXISTS (SELECT 1 FROM loan_application_approvals x WHERE x.application_id = a.id AND x.membership_id = sqlc.arg(viewer)
                  AND x.decision = 'pending' AND a.status = 'pending_approval'))
ORDER BY a.created_at DESC, a.id DESC
LIMIT sqlc.arg(lim) OFFSET sqlc.arg(off);

-- name: ApplicationGuarantors :many
SELECT g.id, g.membership_id, (u.first_name || ' ' || u.last_name)::text AS member_name,
       coalesce(g.amount, 0)::text AS amount, (g.amount IS NULL)::bool AS no_amount, g.comment, g.decision,
       g.decided_at, g.reason
FROM loan_application_guarantors g
JOIN memberships m ON m.id = g.membership_id
JOIN users u ON u.id = m.user_id
WHERE g.application_id = $1
ORDER BY g.id;

-- name: ApplicationApprovals :many
SELECT x.id, x.membership_id, (u.first_name || ' ' || u.last_name)::text AS member_name, r.name AS role_name,
       x.decision, x.decided_at, x.comment
FROM loan_application_approvals x
JOIN memberships m ON m.id = x.membership_id
JOIN users u ON u.id = m.user_id
LEFT JOIN group_roles r ON r.id = m.group_role_id
WHERE x.application_id = $1
ORDER BY x.id;

-- name: CountLoanApplicationsAwaiting :one
SELECT ((SELECT count(*) FROM loan_application_guarantors g JOIN loan_applications a ON a.id = g.application_id
        WHERE g.membership_id = $1 AND g.decision = 'pending' AND a.status = 'pending_guarantors')
     + (SELECT count(*) FROM loan_application_approvals x JOIN loan_applications a ON a.id = x.application_id
        WHERE x.membership_id = $1 AND x.decision = 'pending' AND a.status = 'pending_approval'))::bigint AS awaiting;

-- name: GetLoanApplication :one
SELECT a.id, a.membership_id, (u.first_name || ' ' || u.last_name)::text AS member_name, a.loan_type_id,
       t.name AS loan_type_name, a.amount::text AS amount, a.installments, a.description, a.status, a.decline_reason,
       a.decided_at, a.loan_id, a.created_at
FROM loan_applications a
JOIN memberships m ON m.id = a.membership_id
JOIN users u ON u.id = m.user_id
JOIN loan_types t ON t.id = a.loan_type_id
WHERE a.group_id = $1 AND a.id = $2;

-- name: LoanTypeInUse :one
SELECT (EXISTS (SELECT 1 FROM loans WHERE loan_type_id = sqlc.arg(id)::bigint AND voided_at IS NULL)
     OR EXISTS (SELECT 1 FROM loan_applications WHERE loan_type_id = sqlc.arg(id)::bigint))::bool AS used;
