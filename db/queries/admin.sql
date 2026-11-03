-- name: IsPlatformAdmin :one
SELECT EXISTS (
    SELECT 1 FROM user_platform_roles r JOIN users u ON u.id = r.user_id
    WHERE r.user_id = $1 AND r.role = 'admin' AND u.is_active
);

-- name: AdminStats :one
SELECT
    (SELECT count(*) FROM groups)::bigint AS groups,
    (SELECT count(*) FROM groups WHERE status = 'active')::bigint AS active_groups,
    (SELECT count(*) FROM groups WHERE status = 'setup')::bigint AS setup_groups,
    (SELECT count(*) FROM groups WHERE status = 'suspended')::bigint AS suspended_groups,
    (SELECT count(*) FROM groups WHERE status = 'closed')::bigint AS closed_groups,
    (SELECT count(*) FROM groups WHERE created_at >= date_trunc('day', now()))::bigint AS groups_today,
    (SELECT count(*) FROM users)::bigint AS users,
    (SELECT count(*) FROM users WHERE created_at >= date_trunc('day', now()))::bigint AS users_today,
    (SELECT count(*) FROM users WHERE last_login_at > now() - interval '30 days')::bigint AS users_active_30d,
    (SELECT count(*) FROM memberships WHERE status = 'active')::bigint AS memberships,
    (SELECT count(*) FROM sms_messages)::bigint AS sms_total,
    (SELECT count(*) FROM sms_messages WHERE status = 'sent' AND created_at >= date_trunc('day', now()))::bigint AS sms_sent_today,
    (SELECT count(*) FROM sms_messages WHERE status IN ('queued', 'sending'))::bigint AS sms_queued,
    (SELECT count(*) FROM sms_messages WHERE status = 'failed' AND created_at > now() - interval '7 days')::bigint AS sms_failed_7d,
    (SELECT count(*) FROM transaction_alerts)::bigint AS transaction_alerts,
    (SELECT count(*) FROM transaction_alerts WHERE group_id IS NULL)::bigint AS alerts_unassigned,
    (SELECT count(*) FROM group_accounts WHERE kind = 'bank' AND is_verified AND NOT is_closed AND deleted_at IS NULL)::bigint AS accounts_connected,
    (SELECT count(*) FROM payment_requests WHERE status = 'paid')::bigint AS payments_paid,
    (SELECT coalesce(sum(amount), 0) FROM payment_requests WHERE status = 'paid')::text AS payments_paid_amount,
    (SELECT count(*) FROM ledger_entries WHERE entry_type = 'contribution_invoice' AND legacy_id IS NULL
        AND voided_at IS NULL AND created_at >= date_trunc('day', now()))::bigint AS invoices_today,
    (SELECT count(*) FROM ledger_entries WHERE entry_type IN ('contribution_fine_invoice', 'fine_invoice') AND legacy_id IS NULL
        AND voided_at IS NULL AND created_at >= date_trunc('day', now()))::bigint AS fine_invoices_today,
    (SELECT count(*) FROM withdrawal_requests WHERE status IN ('pending', 'approved'))::bigint AS open_withdrawal_requests,
    (SELECT count(*) FROM admin_audit_log)::bigint AS audit_entries;

-- name: AdminListGroups :many
-- view: '' (all), 'inactive' (active groups no member has signed in to for
-- 90 days: candidates for archiving). sort: 'name' or 'newest'.
SELECT g.id, g.legacy_id, g.name, g.slug, g.status, g.currency, g.country_code, g.created_at,
       g.email AS group_email, g.phone AS group_phone,
       st.active_members, st.registered_members, ls.last_login_at AS last_seen_at,
       coalesce(c.id, 0)::bigint AS contact_id,
       coalesce(c.first_name || ' ' || c.last_name, '')::text AS contact_name,
       c.phone AS contact_phone, c.email AS contact_email,
       count(*) OVER ()::bigint AS total
FROM groups g
LEFT JOIN users c ON c.id = coalesce(g.owner_user_id, g.created_by)
CROSS JOIN LATERAL (
    SELECT count(*) FILTER (WHERE m.status = 'active')::bigint AS active_members,
           count(*) FILTER (WHERE m.status = 'active' AND u.last_login_at IS NOT NULL)::bigint AS registered_members
    FROM memberships m JOIN users u ON u.id = m.user_id
    WHERE m.group_id = g.id
) st
LEFT JOIN LATERAL (
    SELECT u.last_login_at FROM memberships m JOIN users u ON u.id = m.user_id
    WHERE m.group_id = g.id AND u.last_login_at IS NOT NULL
    ORDER BY u.last_login_at DESC LIMIT 1
) ls ON true
WHERE (sqlc.narg(status)::group_status IS NULL OR g.status = sqlc.narg(status))
  AND (sqlc.narg(search)::text IS NULL OR g.name ILIKE '%' || sqlc.narg(search) || '%'
       OR g.id::text = sqlc.narg(search)
       OR c.phone ILIKE '%' || sqlc.narg(search) || '%'
       OR g.phone ILIKE '%' || sqlc.narg(search) || '%')
  AND (sqlc.arg(view)::text <> 'inactive'
       OR (g.status = 'active' AND (ls.last_login_at IS NULL OR ls.last_login_at < now() - interval '90 days')))
ORDER BY CASE WHEN sqlc.arg(sort)::text = 'newest' THEN g.created_at END DESC,
         g.name, g.id
LIMIT sqlc.arg(page_size) OFFSET sqlc.arg(page_offset);

-- name: AdminGetGroup :one
SELECT g.id, g.legacy_id, g.name, g.slug, g.status, g.currency, g.country_code, g.timezone,
       g.email, g.phone, g.address, g.created_at, g.updated_at, g.settings,
       coalesce(c.id, 0)::bigint AS contact_id,
       coalesce(c.first_name || ' ' || c.last_name, '')::text AS contact_name,
       c.phone AS contact_phone, c.email AS contact_email,
       coalesce(cb.first_name || ' ' || cb.last_name, '')::text AS created_by_name,
       (SELECT count(*) FROM memberships m WHERE m.group_id = g.id AND m.status = 'active')::bigint AS active_members,
       (SELECT count(*) FROM memberships m JOIN users u ON u.id = m.user_id
         WHERE m.group_id = g.id AND m.status = 'active' AND u.last_login_at IS NOT NULL)::bigint AS registered_members,
       ls.last_login_at AS last_seen_at,
       (SELECT count(*) FROM contributions x WHERE x.group_id = g.id AND x.deleted_at IS NULL)::bigint AS contributions,
       (SELECT count(*) FROM group_accounts x WHERE x.group_id = g.id AND x.deleted_at IS NULL)::bigint AS accounts,
       (SELECT count(*) FROM loans x WHERE x.group_id = g.id)::bigint AS loans
FROM groups g
LEFT JOIN users c ON c.id = coalesce(g.owner_user_id, g.created_by)
LEFT JOIN users cb ON cb.id = g.created_by
LEFT JOIN LATERAL (
    SELECT u.last_login_at FROM memberships m JOIN users u ON u.id = m.user_id
    WHERE m.group_id = g.id AND u.last_login_at IS NOT NULL
    ORDER BY u.last_login_at DESC LIMIT 1
) ls ON true
WHERE g.id = $1;

-- name: AdminUpdateGroupProfile :one
UPDATE groups SET
    name = coalesce(sqlc.narg(name), name),
    email = CASE WHEN sqlc.arg(set_email)::bool THEN sqlc.narg(email) ELSE email END,
    phone = CASE WHEN sqlc.arg(set_phone)::bool THEN sqlc.narg(phone) ELSE phone END,
    settings = CASE WHEN sqlc.narg(notifications_off)::bool IS NULL THEN settings
                    ELSE jsonb_set(settings, '{disable_notifications}',
                                   to_jsonb(CASE WHEN sqlc.narg(notifications_off)::bool THEN '1' ELSE '0' END)) END,
    updated_at = now()
WHERE id = sqlc.arg(id)
RETURNING id;

-- name: GroupNotificationsOff :one
SELECT coalesce(settings->>'disable_notifications', '0') IN ('1', 'true')
FROM groups WHERE id = $1;

-- name: AdminRoleCounts :many
SELECT r.role::text AS role, count(*)::bigint AS users
FROM user_platform_roles r JOIN users u ON u.id = r.user_id
WHERE u.is_active
GROUP BY r.role ORDER BY r.role;

-- name: AdminUsersGroupNames :many
-- The groups of each user on a page of the user list.
SELECT m.user_id, m.group_id, g.name AS group_name, m.is_official
FROM memberships m JOIN groups g ON g.id = m.group_id
WHERE m.user_id = ANY(sqlc.arg(user_ids)::bigint[]) AND m.status = 'active'
ORDER BY m.user_id, g.name;

-- name: SetGroupStatus :one
UPDATE groups SET status = $2, updated_at = now() WHERE id = $1 RETURNING *;

-- name: AdminListUsers :many
SELECT u.id, u.first_name, u.middle_name, u.last_name, u.phone, u.email, u.is_active,
       u.last_login_at, u.created_at,
       coalesce((SELECT array_agg(r.role::text ORDER BY r.role) FROM user_platform_roles r WHERE r.user_id = u.id), '{}')::text[] AS platform_roles,
       (SELECT count(*) FROM memberships m WHERE m.user_id = u.id AND m.status = 'active')::bigint AS groups,
       count(*) OVER ()::bigint AS total
FROM users u
WHERE (sqlc.narg(search)::text IS NULL
       OR (u.first_name || ' ' || u.middle_name || ' ' || u.last_name) ILIKE '%' || sqlc.narg(search) || '%'
       OR u.phone ILIKE '%' || sqlc.narg(search) || '%'
       OR u.email ILIKE '%' || sqlc.narg(search) || '%'
       OR u.id::text = sqlc.narg(search))
  AND (sqlc.narg(role)::platform_role IS NULL
       OR EXISTS (SELECT 1 FROM user_platform_roles r WHERE r.user_id = u.id AND r.role = sqlc.narg(role)))
  AND (sqlc.narg(is_active)::bool IS NULL OR u.is_active = sqlc.narg(is_active))
ORDER BY u.first_name, u.last_name, u.id
LIMIT sqlc.arg(page_size) OFFSET sqlc.arg(page_offset);

-- name: AdminUserMemberships :many
SELECT m.id, m.group_id, g.name AS group_name, m.is_official, r.name AS role_name, m.status, m.joined_at
FROM memberships m
JOIN groups g ON g.id = m.group_id
LEFT JOIN group_roles r ON r.id = m.group_role_id
WHERE m.user_id = $1
ORDER BY g.name;

-- name: AdminCreateUser :one
INSERT INTO users (first_name, middle_name, last_name, email, phone, id_number, password_hash, password_reset_required)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: AdminUpdateUser :one
UPDATE users SET
    first_name  = coalesce(sqlc.narg(first_name), first_name),
    middle_name = coalesce(sqlc.narg(middle_name), middle_name),
    last_name   = coalesce(sqlc.narg(last_name), last_name),
    email       = CASE WHEN sqlc.arg(clear_email)::bool THEN NULL ELSE coalesce(sqlc.narg(email), email) END,
    phone       = CASE WHEN sqlc.arg(clear_phone)::bool THEN NULL ELSE coalesce(sqlc.narg(phone), phone) END,
    id_number   = coalesce(sqlc.narg(id_number), id_number),
    is_active   = coalesce(sqlc.narg(is_active), is_active),
    updated_at  = now()
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: SetUserPassword :exec
UPDATE users SET password_hash = $2, password_reset_required = $3, updated_at = now() WHERE id = $1;

-- name: ClearPlatformRoles :exec
DELETE FROM user_platform_roles WHERE user_id = $1;

-- name: CountActiveAdmins :one
SELECT count(*) FROM user_platform_roles r JOIN users u ON u.id = r.user_id
WHERE r.role = 'admin' AND u.is_active;

-- name: RevokeUserRefreshTokens :exec
UPDATE refresh_tokens SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL;

-- name: InsertAudit :exec
INSERT INTO admin_audit_log (actor_id, action, target_type, target_id, detail)
VALUES ($1, $2, $3, $4, $5);

-- name: ListAudit :many
SELECT a.id, a.actor_id, u.first_name AS actor_first_name, u.last_name AS actor_last_name,
       a.action, a.target_type, a.target_id, a.detail, a.created_at
FROM admin_audit_log a LEFT JOIN users u ON u.id = a.actor_id
ORDER BY a.created_at DESC, a.id DESC
LIMIT $1;

-- name: AdminListTransactions :many
SELECT e.id, e.group_id, g.name AS group_name, e.membership_id,
       u.first_name, u.last_name, e.entry_type, e.amount, e.entry_date,
       e.contribution_id, c.name AS contribution_name, e.description, e.voided_at,
       count(*) OVER ()::bigint AS total
FROM ledger_entries e
JOIN groups g ON g.id = e.group_id
LEFT JOIN memberships m ON m.id = e.membership_id
LEFT JOIN users u ON u.id = m.user_id
LEFT JOIN contributions c ON c.id = e.contribution_id
WHERE (sqlc.narg(group_id)::bigint IS NULL OR e.group_id = sqlc.narg(group_id))
  AND (sqlc.narg(entry_type)::ledger_entry_type IS NULL OR e.entry_type = sqlc.narg(entry_type))
  AND (sqlc.narg(from_date)::date IS NULL OR e.entry_date >= sqlc.narg(from_date))
  AND (sqlc.narg(to_date)::date IS NULL OR e.entry_date <= sqlc.narg(to_date))
  AND (sqlc.arg(include_voided)::bool OR e.voided_at IS NULL)
ORDER BY e.entry_date DESC, e.id DESC
LIMIT sqlc.arg(page_size) OFFSET sqlc.arg(page_offset);
