-- name: IsPlatformAdmin :one
SELECT EXISTS (
    SELECT 1 FROM user_platform_roles r JOIN users u ON u.id = r.user_id
    WHERE r.user_id = $1 AND r.role = 'admin' AND u.is_active
);

-- name: AdminStats :one
SELECT
    (SELECT count(*) FROM groups)::bigint AS groups,
    (SELECT count(*) FROM groups WHERE status = 'active')::bigint AS active_groups,
    (SELECT count(*) FROM users)::bigint AS users,
    (SELECT count(*) FROM users WHERE last_login_at > now() - interval '30 days')::bigint AS users_active_30d,
    (SELECT count(*) FROM memberships WHERE status = 'active')::bigint AS memberships;

-- name: AdminListGroups :many
SELECT g.id, g.legacy_id, g.name, g.slug, g.status, g.currency, g.country_code, g.created_at,
       (SELECT count(*) FROM memberships m WHERE m.group_id = g.id AND m.status = 'active')::bigint AS active_members,
       count(*) OVER ()::bigint AS total
FROM groups g
WHERE (sqlc.narg(status)::group_status IS NULL OR g.status = sqlc.narg(status))
  AND (sqlc.narg(search)::text IS NULL OR g.name ILIKE '%' || sqlc.narg(search) || '%'
       OR g.id::text = sqlc.narg(search))
ORDER BY g.name, g.id
LIMIT sqlc.arg(page_size) OFFSET sqlc.arg(page_offset);

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
