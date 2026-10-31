-- Writes the mobile app makes that the web API doesn't (yet).

-- name: ClaimMobileRequestID :execrows
-- Records a request id; 0 rows means it was seen before.
WITH purge AS (
    DELETE FROM mobile_request_ids WHERE seen_at < now() - interval '3 days'
)
INSERT INTO mobile_request_ids (request_id) VALUES ($1) ON CONFLICT DO NOTHING;

-- name: ReleaseMobileRequestID :exec
DELETE FROM mobile_request_ids WHERE request_id = $1;

-- name: CategoryNameTaken :one
-- kind is income, expense or fine.
SELECT EXISTS (
    SELECT 1 FROM income_categories t
    WHERE sqlc.arg(kind)::text = 'income' AND t.group_id = sqlc.arg(group_id) AND t.deleted_at IS NULL
      AND lower(t.name) = lower(sqlc.arg(name)::text) AND t.id <> sqlc.arg(except_id)
    UNION ALL
    SELECT 1 FROM expense_categories t
    WHERE sqlc.arg(kind)::text = 'expense' AND t.group_id = sqlc.arg(group_id) AND t.deleted_at IS NULL
      AND lower(t.name) = lower(sqlc.arg(name)::text) AND t.id <> sqlc.arg(except_id)
    UNION ALL
    SELECT 1 FROM fine_categories t
    WHERE sqlc.arg(kind)::text = 'fine' AND t.group_id = sqlc.arg(group_id) AND t.deleted_at IS NULL
      AND lower(t.name) = lower(sqlc.arg(name)::text) AND t.id <> sqlc.arg(except_id)
);

-- name: CategoryUsed :one
-- Whether any money movement, request or charge (voided or not) names the category.
SELECT CASE sqlc.arg(kind)::text
    WHEN 'income' THEN EXISTS (SELECT 1 FROM deposits d WHERE d.income_category_id = sqlc.arg(id)::bigint)
    WHEN 'expense' THEN EXISTS (SELECT 1 FROM withdrawals w WHERE w.expense_category_id = sqlc.arg(id)::bigint)
                     OR EXISTS (SELECT 1 FROM withdrawal_requests wr WHERE wr.expense_category_id = sqlc.arg(id)::bigint)
    WHEN 'fine' THEN EXISTS (SELECT 1 FROM deposits d WHERE d.fine_category_id = sqlc.arg(id)::bigint)
                  OR EXISTS (SELECT 1 FROM ledger_entries l WHERE l.fine_category_id = sqlc.arg(id)::bigint)
    ELSE true END::bool AS used;

-- name: DeleteCategory :one
-- Soft-deletes a category; returns how many rows changed (0 or 1).
WITH a AS (
    UPDATE income_categories t SET deleted_at = now(), updated_at = now()
    WHERE sqlc.arg(kind)::text = 'income' AND t.group_id = sqlc.arg(group_id)::bigint AND t.id = sqlc.arg(id)::bigint AND t.deleted_at IS NULL
    RETURNING 1
), b AS (
    UPDATE expense_categories t SET deleted_at = now(), updated_at = now()
    WHERE sqlc.arg(kind)::text = 'expense' AND t.group_id = sqlc.arg(group_id)::bigint AND t.id = sqlc.arg(id)::bigint AND t.deleted_at IS NULL
    RETURNING 1
), c AS (
    UPDATE fine_categories t SET deleted_at = now(), updated_at = now()
    WHERE sqlc.arg(kind)::text = 'fine' AND t.group_id = sqlc.arg(group_id)::bigint AND t.id = sqlc.arg(id)::bigint AND t.deleted_at IS NULL
    RETURNING 1
)
SELECT ((SELECT count(*) FROM a) + (SELECT count(*) FROM b) + (SELECT count(*) FROM c))::int AS changed;

-- name: CreateInvoice :one
-- An invoice raised by hand: a contribution, contribution fine, fine or
-- miscellaneous charge on one member.
INSERT INTO ledger_entries (group_id, membership_id, contribution_id, fine_category_id, entry_type, amount,
                            entry_date, due_date, description, created_by, is_backdated)
VALUES (sqlc.arg(group_id), sqlc.arg(membership_id), sqlc.narg(contribution_id), sqlc.narg(fine_category_id),
        sqlc.arg(entry_type), sqlc.arg(amount)::text::numeric, sqlc.arg(invoice_date), sqlc.arg(due_date),
        sqlc.arg(description), sqlc.arg(created_by), sqlc.arg(invoice_date)::date < current_date)
RETURNING id;

-- name: VoidInvoice :execrows
UPDATE ledger_entries SET voided_at = now(), voided_by = sqlc.arg(voided_by)
WHERE group_id = sqlc.arg(group_id) AND id = sqlc.arg(id) AND voided_at IS NULL
  AND entry_type IN ('contribution_invoice', 'contribution_fine_invoice', 'fine_invoice', 'misc_invoice');
