-- Milestone 4b: bank loans (money the group borrowed) and debtors (people
-- outside the group who borrow from it). Debtor loans themselves live in
-- loans (see loans.sql); a bank loan's balance is never stored.

-- ---- Debtors ----

-- name: ListDebtors :many
SELECT d.id, d.name, d.phone, d.email, d.id_number, d.description, d.is_active, d.created_at,
       count(l.id) FILTER (WHERE l.voided_at IS NULL) AS loans,
       coalesce(sum(l.amount) FILTER (WHERE l.voided_at IS NULL), 0)::text AS lent,
       coalesce(sum(t.payable - t.paid) FILTER (WHERE l.voided_at IS NULL), 0)::text AS balance
FROM debtors d
LEFT JOIN loans l ON l.debtor_id = d.id
LEFT JOIN LATERAL (
    SELECT coalesce((SELECT sum(c.amount) FROM loan_charges c WHERE c.loan_id = l.id AND c.voided_at IS NULL), 0) AS payable,
           coalesce((SELECT sum(r.amount) FROM loan_repayments r WHERE r.loan_id = l.id AND r.voided_at IS NULL), 0) AS paid
) t ON true
WHERE d.group_id = $1 AND d.deleted_at IS NULL AND (sqlc.arg(include_inactive)::bool OR d.is_active)
GROUP BY d.id
ORDER BY d.name, d.id;

-- name: GetDebtor :one
SELECT * FROM debtors WHERE group_id = $1 AND id = $2 AND deleted_at IS NULL;

-- name: ActiveDebtorInGroup :one
SELECT EXISTS (SELECT 1 FROM debtors WHERE group_id = $1 AND id = $2 AND deleted_at IS NULL AND is_active);

-- name: CreateDebtor :one
INSERT INTO debtors (group_id, name, phone, email, id_number, description, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING id;

-- name: UpdateDebtor :execrows
UPDATE debtors SET name = $3, phone = $4, email = $5, id_number = $6, description = $7, updated_at = now()
WHERE group_id = $1 AND id = $2 AND deleted_at IS NULL;

-- name: SetDebtorActive :execrows
UPDATE debtors SET is_active = $3, updated_at = now()
WHERE group_id = $1 AND id = $2 AND deleted_at IS NULL;

-- name: DebtorHasLoans :one
SELECT EXISTS (SELECT 1 FROM loans WHERE debtor_id = $1 AND voided_at IS NULL);

-- name: DeleteDebtor :execrows
UPDATE debtors SET deleted_at = now(), updated_at = now()
WHERE group_id = $1 AND id = $2 AND deleted_at IS NULL;

-- ---- Bank loans ----

-- name: CreateBankLoan :one
INSERT INTO bank_loans (group_id, description, amount, total_payable, opening_balance, starts_on, ends_on,
                        account_id, created_by)
VALUES ($1, $2, sqlc.arg(amount)::numeric, sqlc.arg(total_payable)::numeric, sqlc.arg(opening_balance)::numeric,
        $3, $4, $5, $6)
RETURNING id;

-- name: SetDepositBankLoan :exec
UPDATE deposits SET bank_loan_id = $2 WHERE id = $1;

-- name: SetWithdrawalBankLoan :exec
UPDATE withdrawals SET bank_loan_id = $2 WHERE id = $1;

-- name: ListBankLoans :many
-- Bank loans with what has been repaid and what is still owed. status: ''
-- live loans, 'open' (still owing), 'paid', 'voided'.
WITH t AS (
    SELECT b.id,
           coalesce((SELECT sum(w.amount) FROM withdrawals w WHERE w.bank_loan_id = b.id AND w.voided_at IS NULL
                     AND w.kind = 'bank_loan_repayment'), 0) AS repaid
    FROM bank_loans b
    WHERE b.group_id = sqlc.arg(group_id)
)
SELECT b.id, b.description, b.amount::text AS amount, b.total_payable::text AS total_payable,
       b.opening_balance::text AS opening_balance, b.starts_on, b.ends_on, b.account_id,
       coalesce(a.name, '')::text AS account_name, b.created_at, b.voided_at,
       t.repaid::text AS repaid, greatest(b.opening_balance - t.repaid, 0)::text AS balance
FROM bank_loans b
JOIN t ON t.id = b.id
LEFT JOIN group_accounts a ON a.id = b.account_id
WHERE CASE sqlc.arg(status)::text
        WHEN 'open' THEN b.voided_at IS NULL AND b.opening_balance - t.repaid > 0
        WHEN 'paid' THEN b.voided_at IS NULL AND b.opening_balance - t.repaid <= 0
        WHEN 'voided' THEN b.voided_at IS NOT NULL
        ELSE b.voided_at IS NULL
      END
ORDER BY b.starts_on DESC, b.id DESC;

-- name: GetBankLoan :one
SELECT b.id, b.description, b.amount::text AS amount, b.total_payable::text AS total_payable,
       b.opening_balance::text AS opening_balance, b.starts_on, b.ends_on, b.account_id,
       coalesce(a.name, '')::text AS account_name, b.created_at, b.voided_at,
       r.repaid::text AS repaid, greatest(b.opening_balance - r.repaid, 0)::text AS balance
FROM bank_loans b
LEFT JOIN group_accounts a ON a.id = b.account_id
CROSS JOIN LATERAL (
    SELECT coalesce(sum(w.amount), 0) AS repaid FROM withdrawals w
    WHERE w.bank_loan_id = b.id AND w.voided_at IS NULL AND w.kind = 'bank_loan_repayment'
) r
WHERE b.group_id = $1 AND b.id = $2;

-- name: LockBankLoan :one
SELECT id, voided_at, starts_on FROM bank_loans WHERE group_id = $1 AND id = $2 FOR UPDATE;

-- name: UpdateBankLoan :exec
UPDATE bank_loans SET description = $3, total_payable = sqlc.arg(total_payable)::numeric,
    opening_balance = sqlc.arg(opening_balance)::numeric, ends_on = $4, updated_at = now()
WHERE group_id = $1 AND id = $2 AND voided_at IS NULL;

-- name: BankLoanRepayments :many
SELECT w.id, w.account_id, coalesce(a.name, '')::text AS account_name, w.method, w.amount::text AS amount,
       w.withdrawal_date, w.description, w.created_at, w.voided_at
FROM withdrawals w
LEFT JOIN group_accounts a ON a.id = w.account_id
WHERE w.bank_loan_id = $1 AND w.kind = 'bank_loan_repayment'
  AND (sqlc.arg(include_voided)::bool OR w.voided_at IS NULL)
ORDER BY w.withdrawal_date, w.id;

-- name: BankLoanMoneyRows :many
-- The live deposits and withdrawals that belong to a bank loan, to void with it.
SELECT 'deposit'::text AS kind, d.id FROM deposits d WHERE d.bank_loan_id = $1 AND d.voided_at IS NULL
UNION ALL
SELECT 'withdrawal'::text, w.id FROM withdrawals w WHERE w.bank_loan_id = $1 AND w.voided_at IS NULL;

-- name: VoidBankLoan :execrows
UPDATE bank_loans SET voided_at = now(), voided_by = $3, updated_at = now()
WHERE group_id = $1 AND id = $2 AND voided_at IS NULL;

-- name: BankLoanTotals :one
SELECT count(*) AS loans, coalesce(sum(b.amount), 0)::text AS received,
       coalesce(sum(r.repaid), 0)::text AS repaid,
       coalesce(sum(greatest(b.opening_balance - r.repaid, 0)), 0)::text AS balance
FROM bank_loans b
CROSS JOIN LATERAL (
    SELECT coalesce(sum(w.amount), 0) AS repaid FROM withdrawals w
    WHERE w.bank_loan_id = b.id AND w.voided_at IS NULL AND w.kind = 'bank_loan_repayment'
) r
WHERE b.group_id = $1 AND b.voided_at IS NULL;
