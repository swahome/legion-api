-- name: CreateMobileToken :exec
INSERT INTO mobile_access_tokens (token_hash, user_id, expires_at) VALUES ($1, $2, $3);

-- name: UseMobileToken :one
-- Looks up a live token and slides its expiry, as the legacy app did on
-- every call (at least 30 days from now).
UPDATE mobile_access_tokens t
SET last_used_at = now(), expires_at = greatest(t.expires_at, sqlc.arg(min_expires_at)::timestamptz)
FROM users u
WHERE t.token_hash = $1 AND t.expires_at > now() AND u.id = t.user_id AND u.is_active
RETURNING t.user_id;

-- name: DeleteMobileToken :exec
DELETE FROM mobile_access_tokens WHERE token_hash = $1;

-- name: DeleteUserMobileTokens :exec
DELETE FROM mobile_access_tokens WHERE user_id = $1;

-- name: UpsertMobilePin :exec
INSERT INTO mobile_pins (identity, pin_hash, unique_code, expires_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT (identity) DO UPDATE
SET pin_hash = excluded.pin_hash, unique_code = excluded.unique_code, attempts = 0,
    expires_at = excluded.expires_at, verified_at = NULL, created_at = now();

-- name: GetMobilePin :one
SELECT * FROM mobile_pins WHERE identity = $1;

-- name: FailMobilePin :exec
UPDATE mobile_pins SET attempts = attempts + 1 WHERE identity = $1;

-- name: MarkMobilePinVerified :exec
UPDATE mobile_pins SET verified_at = now() WHERE identity = $1;

-- name: DeleteMobilePin :exec
DELETE FROM mobile_pins WHERE identity = $1;

-- name: MobileUserGroups :many
-- The groups a user belongs to, for the app's check-in data.
SELECT g.id, g.name, g.slug, g.email, g.phone, g.country_code, g.currency, g.status, g.settings,
       g.owner_user_id, m.id AS member_id, m.is_official, m.group_role_id,
       c.name AS country_name, c.id AS country_id,
       (SELECT count(*) FROM memberships x WHERE x.group_id = g.id AND x.status = 'active')::bigint AS size
FROM memberships m
JOIN groups g ON g.id = m.group_id
LEFT JOIN countries c ON c.code = g.country_code
WHERE m.user_id = $1 AND m.status = 'active'
ORDER BY g.name DESC;

-- name: GroupRoleNames :many
SELECT id, group_id, name FROM group_roles WHERE group_id = ANY(sqlc.arg(group_ids)::bigint[]) AND is_active ORDER BY name;
