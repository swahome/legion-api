-- name: GetUserByPhone :one
SELECT * FROM users WHERE phone = $1;

-- name: GetUserByEmail :one
SELECT * FROM users WHERE lower(email) = lower(sqlc.arg(email));

-- name: GetUserByID :one
SELECT * FROM users WHERE id = $1;

-- name: CreateUser :one
INSERT INTO users (first_name, last_name, email, phone, password_hash)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: TouchLastLogin :exec
UPDATE users SET last_login_at = now() WHERE id = $1;

-- name: AddPlatformRole :exec
INSERT INTO user_platform_roles (user_id, role) VALUES ($1, $2)
ON CONFLICT DO NOTHING;

-- name: ListUserPlatformRoles :many
SELECT role FROM user_platform_roles WHERE user_id = $1 ORDER BY role;

-- name: CountRecentLoginAttempts :one
SELECT count(*) FROM login_attempts
WHERE identity = $1 AND ip_address IS NOT DISTINCT FROM sqlc.narg(ip_address)
  AND attempted_at > sqlc.arg(since);

-- name: OldestRecentLoginAttempt :one
SELECT min(attempted_at)::timestamptz FROM login_attempts
WHERE identity = $1 AND ip_address IS NOT DISTINCT FROM sqlc.narg(ip_address)
  AND attempted_at > sqlc.arg(since);

-- name: RecordLoginAttempt :exec
INSERT INTO login_attempts (identity, ip_address) VALUES ($1, $2);

-- name: ClearLoginAttempts :exec
DELETE FROM login_attempts
WHERE identity = $1 AND ip_address IS NOT DISTINCT FROM sqlc.narg(ip_address);

-- name: CreateRefreshToken :exec
INSERT INTO refresh_tokens (user_id, token_hash, family_id, expires_at, user_agent, ip_address)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: GetRefreshTokenByHash :one
SELECT * FROM refresh_tokens WHERE token_hash = $1;

-- name: RevokeRefreshToken :execrows
UPDATE refresh_tokens SET revoked_at = now()
WHERE id = $1 AND revoked_at IS NULL;

-- name: RevokeRefreshTokenFamily :exec
UPDATE refresh_tokens SET revoked_at = now()
WHERE family_id = $1 AND revoked_at IS NULL;
