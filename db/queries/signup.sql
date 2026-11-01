-- Milestone 5: SMS outbox, sign-up and reset codes, invites, group setup.

-- ---- SMS outbox ----

-- name: QueueSMS :one
INSERT INTO sms_messages (to_phone, body, kind, user_id, group_id)
VALUES ($1, $2, $3, $4, $5)
RETURNING id;

-- name: ClaimDueSMS :many
-- Takes up to `lim` messages that are due, including any left 'sending' by
-- a process that died mid-send. SKIP LOCKED lets several servers share the
-- outbox without sending a message twice.
UPDATE sms_messages SET status = 'sending', claimed_at = now(), attempts = attempts + 1
WHERE id IN (
    SELECT id FROM sms_messages
    WHERE (status = 'queued' AND next_attempt_at <= now())
       OR (status = 'sending' AND claimed_at < now() - interval '2 minutes')
    ORDER BY next_attempt_at, id
    LIMIT sqlc.arg(lim)
    FOR UPDATE SKIP LOCKED
)
RETURNING id, to_phone, body, attempts;

-- name: MarkSMSSent :exec
UPDATE sms_messages SET status = 'sent', sent_at = now(), provider_message_id = $2,
    provider_status = $3, cost = $4, error = NULL
WHERE id = $1;

-- name: MarkSMSRetry :exec
UPDATE sms_messages SET status = 'queued', next_attempt_at = $2, provider_status = $3, error = $4
WHERE id = $1;

-- name: MarkSMSFailed :exec
UPDATE sms_messages SET status = 'failed', provider_status = $2, error = $3
WHERE id = $1;

-- name: ListSMS :many
-- The outbox for the platform admin, newest first.
SELECT s.id, s.to_phone, s.body, s.kind, s.user_id, s.group_id, coalesce(g.name, '')::text AS group_name,
       s.status, s.attempts, s.provider_status, s.cost, s.error, s.created_at, s.sent_at
FROM sms_messages s
LEFT JOIN groups g ON g.id = s.group_id
WHERE (sqlc.narg(status)::sms_status IS NULL OR s.status = sqlc.narg(status))
  AND (sqlc.narg(group_id)::bigint IS NULL OR s.group_id = sqlc.narg(group_id))
  AND (sqlc.narg(phone)::text IS NULL OR s.to_phone = sqlc.narg(phone))
ORDER BY s.id DESC
LIMIT sqlc.arg(lim) OFFSET sqlc.arg(off);

-- name: CountRecentSMS :one
-- Messages of a kind sent to a user lately, to stop code requests flooding
-- someone's phone.
SELECT count(*) FROM sms_messages
WHERE user_id = $1 AND kind = $2 AND created_at > sqlc.arg(since);

-- ---- Sign-up and reset codes ----

-- name: PutUserCode :exec
INSERT INTO user_codes (user_id, purpose, code_hash, expires_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT (user_id, purpose) DO UPDATE
SET code_hash = EXCLUDED.code_hash, expires_at = EXCLUDED.expires_at, attempts = 0,
    token_hash = NULL, created_at = now();

-- name: GetUserCode :one
SELECT * FROM user_codes WHERE user_id = $1 AND purpose = $2 FOR UPDATE;

-- name: FailUserCode :exec
UPDATE user_codes SET attempts = attempts + 1 WHERE user_id = $1 AND purpose = $2;

-- name: SetUserCodeToken :exec
UPDATE user_codes SET token_hash = $3 WHERE user_id = $1 AND purpose = $2;

-- name: GetUserCodeByToken :one
SELECT * FROM user_codes WHERE token_hash = $1 AND purpose = 'reset_password' FOR UPDATE;

-- name: DeleteUserCode :exec
DELETE FROM user_codes WHERE user_id = $1 AND purpose = $2;

-- name: SetUserValidated :exec
UPDATE users SET is_validated = true, updated_at = now() WHERE id = $1;

-- name: CreateSignupUser :one
INSERT INTO users (first_name, last_name, phone, email, password_hash, is_validated)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: ResetUserPassword :exec
UPDATE users SET password_hash = $2, password_reset_required = false, updated_at = now()
WHERE id = $1;

-- ---- Invites ----

-- name: CreateInvite :exec
INSERT INTO invites (token_hash, user_id, group_id, membership_id, invited_by, expires_at)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: GetInvite :one
SELECT i.token_hash, i.user_id, i.group_id, i.membership_id, i.expires_at, i.used_at,
       u.first_name, u.last_name, u.phone, (u.password_hash IS NOT NULL AND NOT u.password_reset_required)::bool AS has_password,
       u.is_active AS user_active, g.name AS group_name, m.status AS membership_status,
       coalesce(nullif(trim(b.first_name || ' ' || b.last_name), ''), '')::text AS invited_by_name
FROM invites i
JOIN users u ON u.id = i.user_id
JOIN groups g ON g.id = i.group_id
JOIN memberships m ON m.id = i.membership_id
LEFT JOIN users b ON b.id = i.invited_by
WHERE i.token_hash = $1;

-- name: UseInvite :execrows
UPDATE invites SET used_at = now() WHERE token_hash = $1 AND used_at IS NULL AND expires_at > now();

-- name: ExpireMembershipInvites :exec
-- A new invite replaces the member's earlier links.
UPDATE invites SET expires_at = least(expires_at, now()) WHERE membership_id = $1 AND used_at IS NULL;

-- name: MarkMemberInvited :exec
UPDATE memberships SET invited_at = now() WHERE id = $1;

-- name: MembersToInvite :many
-- Members of a group who were never sent an invite, except the official
-- finishing setup.
SELECT m.id, m.user_id, u.first_name, u.phone
FROM memberships m
JOIN users u ON u.id = m.user_id
WHERE m.group_id = $1 AND m.status = 'active' AND m.invited_at IS NULL AND m.user_id <> sqlc.arg(except_user)
ORDER BY m.id;

-- ---- Groups and setup ----

-- name: SlugTaken :one
SELECT EXISTS (SELECT 1 FROM groups WHERE slug = $1);

-- name: CreateSignupGroup :one
INSERT INTO groups (name, slug, email, phone, country_code, currency, status, owner_user_id, created_by, settings)
VALUES ($1, $2, $3, $4, $5, $6, 'setup', $7, $7, $8)
RETURNING *;

-- name: CreateGroupRole :one
INSERT INTO group_roles (group_id, name, is_editable) VALUES ($1, $2, $3) RETURNING id;

-- name: UpdateGroupProfile :one
UPDATE groups SET
    name         = coalesce(sqlc.narg(name), name),
    email        = CASE WHEN sqlc.arg(set_email)::bool THEN sqlc.narg(email) ELSE email END,
    phone        = CASE WHEN sqlc.arg(set_phone)::bool THEN sqlc.narg(phone) ELSE phone END,
    address      = CASE WHEN sqlc.arg(set_address)::bool THEN sqlc.narg(address) ELSE address END,
    country_code = coalesce(sqlc.narg(country_code), country_code),
    currency     = coalesce(sqlc.narg(currency), currency),
    settings     = settings || sqlc.arg(settings)::jsonb,
    updated_at   = now()
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: CompleteGroupSetup :execrows
UPDATE groups SET status = 'active', setup_completed_at = now(), updated_at = now()
WHERE id = $1 AND status = 'setup';

-- name: SetupCounts :one
SELECT
    (SELECT count(*) FROM memberships m WHERE m.group_id = $1 AND m.status = 'active')::bigint AS members,
    (SELECT count(*) FROM memberships m WHERE m.group_id = $1 AND m.status = 'active' AND m.is_official)::bigint AS officials,
    (SELECT count(*) FROM contributions c WHERE c.group_id = $1 AND c.deleted_at IS NULL)::bigint AS contributions,
    (SELECT count(*) FROM contribution_fine_rules f JOIN contributions c ON c.id = f.contribution_id
       WHERE c.group_id = $1 AND c.deleted_at IS NULL AND f.is_active)::bigint AS fine_rules,
    (SELECT count(*) FROM group_accounts a WHERE a.group_id = $1 AND NOT a.is_closed AND a.kind <> 'petty_cash')::bigint AS accounts,
    (SELECT count(*) FROM loan_types t WHERE t.group_id = $1 AND t.deleted_at IS NULL AND NOT t.for_debtors)::bigint AS loan_types;

-- name: GetActiveCountryByCode :one
SELECT * FROM countries WHERE code = $1 AND is_active;

-- name: GetDefaultCountry :one
SELECT * FROM countries WHERE is_default AND is_active LIMIT 1;

-- name: CreatePettyCashAccount :one
INSERT INTO group_accounts (group_id, kind, name, initial_balance, is_default)
VALUES ($1, 'petty_cash', $2, 0, true)
RETURNING id;

-- name: MarkMemberInvitedByUser :exec
UPDATE memberships SET invited_at = coalesce(invited_at, now()) WHERE group_id = $1 AND user_id = $2;
