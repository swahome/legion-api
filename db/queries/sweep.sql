-- Milestone 8: the rest of the mobile app's actions.

-- ---- Profile ----

-- name: UpdateUserNames :exec
UPDATE users SET first_name = $2, last_name = $3, updated_at = now() WHERE id = $1;

-- name: UpdateUserEmail :exec
UPDATE users SET email = sqlc.narg(email), updated_at = now() WHERE id = sqlc.arg(id);

-- name: UpdateUserPhone :exec
UPDATE users SET phone = $2, updated_at = now() WHERE id = $1;

-- name: UpdateUserContact :exec
-- An official correcting the details of a member who has never signed in.
UPDATE users SET first_name = $2, last_name = $3, email = sqlc.narg(email), phone = sqlc.narg(phone), updated_at = now()
WHERE id = $1;

-- name: SetUserAvatar :exec
UPDATE users SET avatar = $2, updated_at = now() WHERE id = $1;

-- name: SetUserMobileToken :exec
UPDATE users SET mobile_token = $2, updated_at = now() WHERE id = $1;

-- name: EmailTaken :one
SELECT EXISTS (SELECT 1 FROM users WHERE lower(email) = lower(sqlc.arg(email)::text) AND id <> sqlc.arg(except_id));

-- name: PhoneTaken :one
SELECT EXISTS (SELECT 1 FROM users WHERE phone = sqlc.arg(phone)::text AND id <> sqlc.arg(except_id));

-- name: PutPhoneChange :exec
INSERT INTO phone_changes (user_id, old_phone, new_phone) VALUES ($1, $2, $3)
ON CONFLICT (user_id) DO UPDATE SET old_phone = EXCLUDED.old_phone, new_phone = EXCLUDED.new_phone, created_at = now();

-- name: GetPhoneChange :one
SELECT * FROM phone_changes WHERE user_id = $1;

-- name: DeletePhoneChange :exec
DELETE FROM phone_changes WHERE user_id = $1;

-- name: GetCountryByID :one
SELECT * FROM countries WHERE id = $1;

-- name: SetGroupFields :exec
-- Group profile changes from the app; NULL leaves a field as it is.
UPDATE groups SET
    name         = coalesce(sqlc.narg(name), name),
    email        = coalesce(sqlc.narg(email), email),
    phone        = coalesce(sqlc.narg(phone), phone),
    country_code = coalesce(sqlc.narg(country_code), country_code),
    currency     = coalesce(sqlc.narg(currency), currency),
    settings     = settings || sqlc.arg(settings)::jsonb,
    updated_at   = now()
WHERE id = sqlc.arg(id);

-- ---- Group roles and members ----

-- name: GetGroupRole :one
SELECT * FROM group_roles WHERE group_id = $1 AND id = $2;

-- name: GroupRoleNameTaken :one
SELECT EXISTS (SELECT 1 FROM group_roles WHERE group_id = sqlc.arg(group_id) AND lower(name) = lower(sqlc.arg(name)::text)
               AND id <> sqlc.arg(except_id));

-- name: CreateEditableGroupRole :one
INSERT INTO group_roles (group_id, name, description, is_editable) VALUES ($1, $2, $3, true) RETURNING id;

-- name: UpdateGroupRole :exec
UPDATE group_roles SET name = $3, description = $4, updated_at = now() WHERE group_id = $1 AND id = $2;

-- name: SetGroupRoleActive :exec
UPDATE group_roles SET is_active = $3, updated_at = now() WHERE group_id = $1 AND id = $2;

-- name: GroupRoleAssignments :many
-- Active roles, by name, and whether an active member holds each.
SELECT r.id, r.name,
       EXISTS (SELECT 1 FROM memberships m WHERE m.group_id = r.group_id AND m.group_role_id = r.id
               AND m.status <> 'removed')::bool AS assigned
FROM group_roles r
WHERE r.group_id = $1 AND r.is_active
ORDER BY r.name, r.id;

-- name: GetGroupMemberUser :one
SELECT m.id, m.user_id, m.status, u.first_name, u.last_name, u.email, u.phone, u.avatar, u.last_login_at
FROM memberships m JOIN users u ON u.id = m.user_id
WHERE m.group_id = $1 AND m.id = $2 AND m.status <> 'removed';

-- name: SetMembershipStatus :exec
UPDATE memberships SET status = $3, updated_at = now() WHERE group_id = $1 AND id = $2;

-- ---- Contributions ----

-- name: SetContributionHidden :exec
UPDATE contributions SET is_hidden = $3, updated_at = now() WHERE group_id = $1 AND id = $2;

-- name: ContributionHasMoney :one
SELECT (EXISTS (SELECT 1 FROM ledger_entries e WHERE e.group_id = sqlc.arg(group_id)::bigint AND e.contribution_id = sqlc.arg(id)::bigint AND e.voided_at IS NULL)
    OR EXISTS (SELECT 1 FROM deposits d WHERE d.group_id = sqlc.arg(group_id)::bigint AND d.contribution_id = sqlc.arg(id)::bigint AND d.voided_at IS NULL)
    OR EXISTS (SELECT 1 FROM withdrawals w WHERE w.group_id = sqlc.arg(group_id)::bigint AND w.contribution_id = sqlc.arg(id)::bigint AND w.voided_at IS NULL))::bool AS has_money;

-- name: DeleteContribution :exec
UPDATE contributions SET deleted_at = now(), is_active = false, updated_at = now()
WHERE group_id = $1 AND id = $2 AND deleted_at IS NULL;

-- ---- Notifications ----

-- name: CreateNotification :one
INSERT INTO notifications (group_id, membership_id, user_id, from_user_id, subject, message, category,
                           call_to_action, link, file_path, file_type, file_size, refs)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
RETURNING id;

-- name: ListMemberNotifications :many
SELECT * FROM notifications
WHERE group_id = $1 AND membership_id = $2
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(lim) OFFSET sqlc.arg(off);

-- name: CountUnreadNotifications :one
SELECT count(*)::int FROM notifications WHERE group_id = $1 AND membership_id = $2 AND NOT is_read;

-- name: MarkMemberNotifications :exec
UPDATE notifications SET is_read = sqlc.arg(is_read), read_at = CASE WHEN sqlc.arg(is_read)::bool THEN now() END
WHERE group_id = sqlc.arg(group_id) AND membership_id = sqlc.arg(membership_id);

-- name: MarkNotificationRead :execrows
UPDATE notifications SET is_read = true, read_at = now() WHERE group_id = $1 AND id = $2;

-- name: GroupMembersToNotify :many
-- Active members of a group with their user, for group-wide notices.
SELECT m.id, m.user_id FROM memberships m WHERE m.group_id = $1 AND m.status = 'active';

-- ---- Assets ----

-- name: ListAssetCategories :many
SELECT * FROM asset_categories WHERE group_id = $1 ORDER BY name, id LIMIT sqlc.arg(lim) OFFSET sqlc.arg(off);

-- name: GetAssetCategory :one
SELECT * FROM asset_categories WHERE group_id = $1 AND id = $2;

-- name: AssetCategorySlugTaken :one
SELECT EXISTS (SELECT 1 FROM asset_categories WHERE group_id = sqlc.arg(group_id) AND slug = sqlc.arg(slug)::text
               AND id <> sqlc.arg(except_id));

-- name: CreateAssetCategory :one
INSERT INTO asset_categories (group_id, name, slug, description, created_by) VALUES ($1, $2, $3, $4, $5) RETURNING id;

-- name: UpdateAssetCategory :exec
UPDATE asset_categories SET name = $3, slug = $4, description = $5, updated_at = now() WHERE group_id = $1 AND id = $2;

-- name: SetAssetCategoryHidden :exec
UPDATE asset_categories SET is_hidden = $3, updated_at = now() WHERE group_id = $1 AND id = $2;

-- name: CreateAsset :one
INSERT INTO assets (group_id, asset_category_id, name, cost, description, created_by)
VALUES ($1, $2, $3, sqlc.arg(cost)::numeric, $4, $5) RETURNING id;

-- name: ListAssets :many
SELECT a.id, a.name, a.cost::text AS cost, a.description, coalesce(c.name, '')::text AS category_name
FROM assets a LEFT JOIN asset_categories c ON c.id = a.asset_category_id
WHERE a.group_id = $1 AND a.voided_at IS NULL
ORDER BY a.name, a.id
LIMIT sqlc.arg(lim) OFFSET sqlc.arg(off);

-- name: GetAsset :one
SELECT * FROM assets WHERE group_id = $1 AND id = $2 AND voided_at IS NULL;

-- name: VoidAsset :execrows
UPDATE assets SET voided_at = now() WHERE group_id = $1 AND id = $2 AND voided_at IS NULL;

-- name: AssetPurchasePayments :many
SELECT id, account_id, amount::text AS amount, description, withdrawal_date
FROM withdrawals
WHERE group_id = $1 AND asset_id = $2 AND kind = 'asset_purchase' AND voided_at IS NULL
ORDER BY withdrawal_date DESC, id DESC;

-- name: InvestmentDeposits :many
-- Live deposits of one kind (asset_sale, stock_sale, money_market) with
-- what they came from.
SELECT d.id, d.account_id, d.amount::text AS amount, d.description, d.deposit_date,
       coalesce(a.name, '')::text AS asset_name, coalesce(st.name, '')::text AS stock_name,
       coalesce(ss.shares, 0)::text AS shares_sold, coalesce(ss.price_per_share, 0)::text AS price_per_share,
       coalesce(mm.institution, '')::text AS institution, coalesce(mm.amount, 0)::text AS invested
FROM deposits d
LEFT JOIN assets a ON a.id = d.asset_id
LEFT JOIN stocks st ON st.id = d.stock_id
LEFT JOIN stock_sales ss ON ss.id = d.stock_sale_id
LEFT JOIN money_market_investments mm ON mm.id = d.money_market_investment_id
WHERE d.group_id = $1 AND d.kind = sqlc.arg(kind)::deposit_kind AND d.voided_at IS NULL
ORDER BY d.deposit_date DESC, d.id DESC
LIMIT sqlc.arg(lim) OFFSET sqlc.arg(off);

-- name: LinkedDepositIDs :many
-- Live deposits tied to an asset, stock or money market investment.
SELECT id FROM deposits
WHERE group_id = sqlc.arg(group_id) AND voided_at IS NULL
  AND (asset_id = sqlc.narg(asset_id) OR stock_id = sqlc.narg(stock_id)
       OR money_market_investment_id = sqlc.narg(mmi_id));

-- name: LinkedWithdrawalIDs :many
SELECT id FROM withdrawals
WHERE group_id = sqlc.arg(group_id) AND voided_at IS NULL
  AND (asset_id = sqlc.narg(asset_id) OR stock_id = sqlc.narg(stock_id)
       OR money_market_investment_id = sqlc.narg(mmi_id));

-- name: SetDepositLinks :exec
UPDATE deposits SET asset_id = sqlc.narg(asset_id), stock_id = sqlc.narg(stock_id), stock_sale_id = sqlc.narg(stock_sale_id),
    money_market_investment_id = sqlc.narg(mmi_id), meeting_id = sqlc.narg(meeting_id)
WHERE id = sqlc.arg(id);

-- name: SetWithdrawalLinks :exec
UPDATE withdrawals SET asset_id = sqlc.narg(asset_id), stock_id = sqlc.narg(stock_id),
    money_market_investment_id = sqlc.narg(mmi_id), meeting_id = sqlc.narg(meeting_id)
WHERE id = sqlc.arg(id);

-- ---- Stocks ----

-- name: CreateStock :one
INSERT INTO stocks (group_id, name, purchase_date, shares, purchase_price, current_price, account_id, created_by)
VALUES ($1, $2, $3, sqlc.arg(shares)::numeric, sqlc.arg(price)::numeric, sqlc.arg(price)::numeric, $4, $5)
RETURNING id;

-- name: ListStocks :many
SELECT id, name, purchase_date, shares::text AS shares, shares_sold::text AS shares_sold,
       purchase_price::text AS purchase_price, current_price::text AS current_price
FROM stocks WHERE group_id = $1 AND voided_at IS NULL
ORDER BY purchase_date DESC, id DESC
LIMIT sqlc.arg(lim) OFFSET sqlc.arg(off);

-- name: LockStock :one
SELECT id, name, shares::text AS shares, shares_sold::text AS shares_sold
FROM stocks WHERE group_id = $1 AND id = $2 AND voided_at IS NULL FOR UPDATE;

-- name: SetStockPrice :exec
UPDATE stocks SET current_price = sqlc.arg(price)::numeric WHERE group_id = $1 AND id = $2;

-- name: AddStockSharesSold :exec
UPDATE stocks SET shares_sold = shares_sold + sqlc.arg(shares)::numeric WHERE group_id = $1 AND id = $2;

-- name: CreateStockSale :one
INSERT INTO stock_sales (group_id, stock_id, account_id, sale_date, shares, price_per_share, created_by)
VALUES ($1, $2, $3, $4, sqlc.arg(shares)::numeric, sqlc.arg(price)::numeric, $5)
RETURNING id;

-- name: VoidStock :execrows
UPDATE stocks SET voided_at = now() WHERE group_id = $1 AND id = $2 AND voided_at IS NULL;

-- name: VoidStockSales :exec
UPDATE stock_sales SET voided_at = now() WHERE group_id = $1 AND stock_id = $2 AND voided_at IS NULL;

-- ---- Money market investments ----

-- name: CreateMoneyMarketInvestment :one
INSERT INTO money_market_investments (group_id, institution, investment_date, amount, description, account_id, created_by)
VALUES ($1, $2, $3, sqlc.arg(amount)::numeric, $4, $5, $6)
RETURNING id;

-- name: LockMoneyMarketInvestment :one
SELECT id, institution, is_closed, investment_date FROM money_market_investments
WHERE group_id = $1 AND id = $2 AND voided_at IS NULL FOR UPDATE;

-- name: TopUpMoneyMarketInvestment :exec
UPDATE money_market_investments SET amount = amount + sqlc.arg(amount)::numeric WHERE group_id = $1 AND id = $2;

-- name: CashInMoneyMarketInvestment :exec
UPDATE money_market_investments SET is_closed = true, cash_in_amount = coalesce(cash_in_amount, 0) + sqlc.arg(amount)::numeric,
    cash_in_date = sqlc.arg(cash_in_date)
WHERE group_id = sqlc.arg(group_id) AND id = sqlc.arg(id);

-- name: ListMoneyMarketInvestments :many
SELECT id, institution, investment_date, amount::text AS amount, description, is_closed
FROM money_market_investments WHERE group_id = $1 AND voided_at IS NULL
ORDER BY investment_date DESC, id DESC
LIMIT sqlc.arg(lim) OFFSET sqlc.arg(off);

-- name: VoidMoneyMarketInvestment :execrows
UPDATE money_market_investments SET voided_at = now() WHERE group_id = $1 AND id = $2 AND voided_at IS NULL;

-- ---- Contribution transfers ----

-- name: CreateContributionTransfer :one
INSERT INTO contribution_transfers (group_id, membership_id, from_contribution_id, from_loan_id, to_member, to_kind,
    to_membership_id, to_contribution_id, to_fine_category_id, to_loan_id, amount, transfer_date, description, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, sqlc.arg(amount)::numeric, $11, $12, $13)
RETURNING id;

-- name: CreateTransferLedgerEntry :exec
INSERT INTO ledger_entries (group_id, membership_id, contribution_id, fine_category_id, entry_type, amount,
                            entry_date, description, contribution_transfer_id, created_by)
VALUES ($1, $2, $3, $4, $5, sqlc.arg(amount)::numeric, $6, $7, $8, $9);

-- name: CreateTransferCharge :exec
-- Money moved off a loan's repayments: a transfer_out charge raises what
-- the loan owes again.
INSERT INTO loan_charges (group_id, loan_id, kind, invoice_on, due_on, amount, contribution_transfer_id, created_by)
VALUES ($1, $2, 'transfer_out', $3, $3, sqlc.arg(amount)::numeric, $4, $5);

-- name: CreateTransferRepayment :exec
INSERT INTO loan_repayments (group_id, loan_id, membership_id, amount, paid_on, source, description,
                             contribution_transfer_id, created_by)
VALUES ($1, $2, $3, sqlc.arg(amount)::numeric, $4, 'transfer', $5, $6, $7);

-- name: LoanRepaidLessTransfers :one
-- What a loan's repayments add up to, less what was already moved off it.
SELECT ((SELECT coalesce(sum(amount), 0) FROM loan_repayments r WHERE r.loan_id = $1 AND r.voided_at IS NULL)
      - (SELECT coalesce(sum(amount), 0) FROM loan_charges c WHERE c.loan_id = $1 AND c.kind = 'transfer_out' AND c.voided_at IS NULL)
       )::text AS available;

-- name: ListContributionTransfers :many
SELECT t.id, t.transfer_date, t.amount::text AS amount, t.from_loan_id, t.to_member, t.to_kind, t.membership_id,
       coalesce(trim(u.first_name || ' ' || u.last_name), '')::text AS member_name
FROM contribution_transfers t
JOIN memberships m ON m.id = t.membership_id
JOIN users u ON u.id = m.user_id
WHERE t.group_id = sqlc.arg(group_id) AND t.voided_at IS NULL
  AND (sqlc.narg(membership_id)::bigint IS NULL OR t.membership_id = sqlc.narg(membership_id))
ORDER BY t.transfer_date DESC, t.id DESC
LIMIT sqlc.arg(lim) OFFSET sqlc.arg(off);

-- name: LockContributionTransfer :one
SELECT * FROM contribution_transfers WHERE group_id = $1 AND id = $2 FOR UPDATE;

-- name: VoidContributionTransfer :exec
UPDATE contribution_transfers SET voided_at = now(), voided_by = $3 WHERE group_id = $1 AND id = $2;

-- name: VoidTransferEntries :exec
UPDATE ledger_entries SET voided_at = now() WHERE contribution_transfer_id = $1 AND voided_at IS NULL;

-- name: VoidTransferCharges :exec
UPDATE loan_charges SET voided_at = now() WHERE contribution_transfer_id = $1 AND voided_at IS NULL;

-- name: VoidTransferRepayments :exec
UPDATE loan_repayments SET voided_at = now(), voided_by = $2 WHERE contribution_transfer_id = $1 AND voided_at IS NULL;

-- ---- Meetings ----

-- name: CreateMeeting :one
INSERT INTO meetings (group_id, title, venue, purpose, meeting_at, created_by) VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id;

-- name: AddMeetingAttendance :exec
INSERT INTO meeting_attendance (meeting_id, group_id, membership_id, status) VALUES ($1, $2, $3, $4)
ON CONFLICT (meeting_id, membership_id) DO UPDATE SET status = EXCLUDED.status;

-- name: AddMeetingItem :exec
INSERT INTO meeting_items (meeting_id, kind, name, position) VALUES ($1, $2, $3, $4);

-- name: ListMeetings :many
SELECT * FROM meetings WHERE group_id = $1 ORDER BY meeting_at DESC, id DESC LIMIT sqlc.arg(lim) OFFSET sqlc.arg(off);

-- name: MeetingAttendance :many
SELECT a.meeting_id, a.membership_id, a.status, m.user_id,
       coalesce(trim(u.first_name || ' ' || u.last_name), '')::text AS name, coalesce(u.email, '')::text AS email
FROM meeting_attendance a
JOIN memberships m ON m.id = a.membership_id
JOIN users u ON u.id = m.user_id
WHERE a.meeting_id = ANY(sqlc.arg(meeting_ids)::bigint[])
ORDER BY u.first_name, u.last_name, a.membership_id;

-- name: MeetingItems :many
SELECT meeting_id, kind, name FROM meeting_items WHERE meeting_id = ANY(sqlc.arg(meeting_ids)::bigint[])
ORDER BY meeting_id, kind, position, id;

-- name: MeetingDeposits :many
SELECT d.meeting_id, d.kind, d.account_id, d.amount::text AS amount, d.membership_id, m.user_id,
       coalesce(trim(u.first_name || ' ' || u.last_name), '')::text AS member_name, coalesce(u.email, '')::text AS email,
       d.contribution_id, coalesce(c.name, '')::text AS contribution_name, coalesce(c.amount, 0)::text AS contribution_amount,
       c.kind AS contribution_kind, d.fine_category_id, coalesce(f.name, '')::text AS fine_name,
       coalesce(f.amount, 0)::text AS fine_amount, coalesce(f.is_hidden, false)::bool AS fine_hidden
FROM deposits d
JOIN memberships m ON m.id = d.membership_id
JOIN users u ON u.id = m.user_id
LEFT JOIN contributions c ON c.id = d.contribution_id
LEFT JOIN fine_categories f ON f.id = d.fine_category_id
WHERE d.group_id = $1 AND d.meeting_id = ANY(sqlc.arg(meeting_ids)::bigint[]) AND d.voided_at IS NULL
ORDER BY d.id;

-- name: MeetingLoans :many
SELECT l.meeting_id, l.id, l.disbursed_on, l.amount::text AS amount, l.account_id, l.membership_id, m.user_id,
       coalesce(trim(u.first_name || ' ' || u.last_name), '')::text AS member_name, coalesce(u.email, '')::text AS email,
       l.loan_type_id
FROM loans l
JOIN memberships m ON m.id = l.membership_id
JOIN users u ON u.id = m.user_id
WHERE l.group_id = $1 AND l.meeting_id = ANY(sqlc.arg(meeting_ids)::bigint[]) AND l.voided_at IS NULL
ORDER BY l.id;

-- name: SetLoanMeeting :exec
UPDATE loans SET meeting_id = $2 WHERE id = $1;

-- ---- Statement requests ----

-- name: CreateStatementRequest :one
INSERT INTO statement_requests (group_id, membership_id, user_id, kind, action, file_type, date_from, date_to, email, loan_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING id;

-- name: ClaimStatementRequests :many
UPDATE statement_requests SET attempts = attempts + 1
WHERE id IN (SELECT id FROM statement_requests WHERE status = 'pending' AND attempts < 3
             ORDER BY created_at LIMIT sqlc.arg(lim) FOR UPDATE SKIP LOCKED)
RETURNING *;

-- name: FinishStatementRequest :exec
UPDATE statement_requests SET status = $2, file_path = $3, error = $4, done_at = now() WHERE id = $1;

-- name: RetryStatementRequest :exec
UPDATE statement_requests SET error = $2, status = CASE WHEN attempts >= 3 THEN 'failed' ELSE 'pending' END WHERE id = $1;

-- name: MemberLedgerEntries :many
-- One member's ledger entries of a bucket (contribution or fine) in a date
-- range, oldest first, for statements.
SELECT e.id, e.entry_date, e.entry_type, e.legacy_type, e.amount::text AS amount, r.sign,
       e.contribution_id, coalesce(c.name, '')::text AS contribution_name,
       e.fine_category_id, coalesce(f.name, '')::text AS fine_name, e.deposit_id, e.description
FROM ledger_entries e
JOIN ledger_entry_rules r ON r.entry_type = e.entry_type AND r.bucket = sqlc.arg(bucket)::text
LEFT JOIN contributions c ON c.id = e.contribution_id
LEFT JOIN fine_categories f ON f.id = e.fine_category_id
WHERE e.group_id = sqlc.arg(group_id) AND e.membership_id = sqlc.arg(membership_id) AND e.voided_at IS NULL
  AND e.entry_date BETWEEN sqlc.arg(date_from) AND sqlc.arg(date_to)
  AND (c.id IS NULL OR c.deleted_at IS NULL)
ORDER BY e.entry_date, e.id;

-- name: MemberBucketBalanceBefore :one
-- What a member owed in a bucket before a date (positive = owes).
SELECT coalesce(sum(r.sign * e.amount), 0)::text AS balance
FROM ledger_entries e
JOIN ledger_entry_rules r ON r.entry_type = e.entry_type AND r.bucket = sqlc.arg(bucket)::text
LEFT JOIN contributions c ON c.id = e.contribution_id
WHERE e.group_id = sqlc.arg(group_id) AND e.membership_id = sqlc.arg(membership_id) AND e.voided_at IS NULL
  AND e.entry_date < sqlc.arg(before) AND (c.id IS NULL OR c.deleted_at IS NULL);

-- name: GroupContributionsPaid :one
SELECT coalesce(sum(-r.sign * e.amount), 0)::text AS paid
FROM ledger_entries e
JOIN ledger_entry_rules r ON r.entry_type = e.entry_type AND r.bucket = 'contribution' AND r.sign < 0
JOIN contributions c ON c.id = e.contribution_id
WHERE e.group_id = $1 AND e.voided_at IS NULL AND c.deleted_at IS NULL AND NOT c.hidden_in_statement;

-- ---- Charts ----

-- name: MonthlyCashFlow :many
-- Money in and out of the group's accounts per month from a date,
-- leaving out transfers between its own accounts. mine counts the
-- member's own deposits.
SELECT to_char(date_trunc('month', e.entry_date), 'YYYY-MM')::text AS month,
       coalesce(sum(e.amount) FILTER (WHERE e.direction = 1), 0)::text AS money_in,
       coalesce(sum(e.amount) FILTER (WHERE e.direction = -1), 0)::text AS money_out,
       coalesce(sum(e.amount) FILTER (WHERE e.direction = 1 AND e.membership_id = sqlc.arg(membership_id)), 0)::text AS mine,
       count(*) FILTER (WHERE e.direction = 1 AND e.membership_id = sqlc.arg(membership_id))::int AS mine_count,
       count(*)::int AS rows
FROM account_entries e
WHERE e.group_id = sqlc.arg(group_id) AND e.voided_at IS NULL AND e.account_transfer_id IS NULL
  AND e.entry_date >= sqlc.arg(since)
GROUP BY 1
ORDER BY 1;

-- ---- Billing history ----

-- name: ListBillingInvoices :many
SELECT * FROM billing_invoices WHERE group_id = $1 ORDER BY due_date DESC NULLS LAST, id DESC;

-- name: ListBillingPayments :many
SELECT * FROM billing_payments WHERE group_id = $1 ORDER BY receipt_date DESC NULLS LAST, id DESC;

-- ---- App activity ----

-- name: InsertAppActivityLog :exec
INSERT INTO app_activity_logs (user_id, group_id, data) VALUES ($1, $2, $3);

-- name: AccountForNumber :one
-- A group's bank account by number, for the bank's account linking.
SELECT * FROM group_accounts WHERE group_id = $1 AND id = $2 AND kind = 'bank' AND deleted_at IS NULL;

-- name: SetAccountVerified :exec
UPDATE group_accounts SET is_verified = $3, updated_at = now() WHERE group_id = $1 AND id = $2;
