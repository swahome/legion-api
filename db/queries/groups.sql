-- name: GetGroup :one
SELECT * FROM groups WHERE id = $1;

-- name: GetActiveMembership :one
SELECT * FROM memberships
WHERE group_id = $1 AND user_id = $2 AND status = 'active';

-- name: GroupSummary :one
SELECT
    (SELECT count(*) FROM memberships m WHERE m.group_id = $1 AND m.status = 'active')::bigint AS active_members,
    (SELECT count(*) FROM memberships m WHERE m.group_id = $1 AND m.status = 'active' AND m.is_official)::bigint AS officials,
    (SELECT count(*) FROM contributions c WHERE c.group_id = $1 AND c.deleted_at IS NULL AND c.is_active)::bigint AS active_contributions,
    (SELECT coalesce(sum(e.amount), 0) FROM ledger_entries e
       WHERE e.group_id = $1 AND e.voided_at IS NULL AND e.entry_type = 'contribution_payment')::text AS total_contributions_paid;

-- name: ListGroupRoles :many
SELECT * FROM group_roles WHERE group_id = $1 AND is_active ORDER BY name;

-- name: ListMembers :many
SELECT m.id, m.user_id, m.is_official, m.group_role_id, r.name AS role_name,
       m.membership_number, m.status, m.joined_at,
       u.first_name, u.middle_name, u.last_name, u.phone, u.email
FROM memberships m
JOIN users u ON u.id = m.user_id
LEFT JOIN group_roles r ON r.id = m.group_role_id
WHERE m.group_id = sqlc.arg(group_id)
  AND (sqlc.narg(status)::membership_status IS NULL OR m.status = sqlc.narg(status))
  AND (sqlc.narg(search)::text IS NULL
       OR (u.first_name || ' ' || u.middle_name || ' ' || u.last_name) ILIKE '%' || sqlc.narg(search) || '%'
       OR u.phone ILIKE '%' || sqlc.narg(search) || '%'
       OR m.membership_number ILIKE '%' || sqlc.narg(search) || '%')
ORDER BY m.is_official DESC, u.first_name, u.last_name, m.id;

-- name: GetMember :one
SELECT m.id, m.user_id, m.is_official, m.group_role_id, r.name AS role_name,
       m.membership_number, m.status, m.joined_at, m.date_of_birth,
       m.physical_address, m.postal_address,
       u.first_name, u.middle_name, u.last_name, u.phone, u.email
FROM memberships m
JOIN users u ON u.id = m.user_id
LEFT JOIN group_roles r ON r.id = m.group_role_id
WHERE m.group_id = $1 AND m.id = $2;

-- name: CreateMembership :one
INSERT INTO memberships (group_id, user_id, is_official, group_role_id, membership_number)
VALUES ($1, $2, $3, $4, $5)
RETURNING id;

-- name: GetMembershipByUser :one
SELECT * FROM memberships WHERE group_id = $1 AND user_id = $2;

-- name: ReactivateMembership :exec
UPDATE memberships SET status = 'active', updated_at = now() WHERE id = $1;

-- name: UpdateMembership :exec
UPDATE memberships SET
    is_official       = coalesce(sqlc.narg(is_official), is_official),
    group_role_id     = CASE WHEN sqlc.arg(clear_role)::bool THEN NULL
                             ELSE coalesce(sqlc.narg(group_role_id), group_role_id) END,
    membership_number = coalesce(sqlc.narg(membership_number), membership_number),
    status            = coalesce(sqlc.narg(status), status),
    updated_at        = now()
WHERE group_id = sqlc.arg(group_id) AND id = sqlc.arg(id);

-- name: CreateUserWithoutPassword :one
INSERT INTO users (first_name, last_name, phone, email, password_reset_required)
VALUES ($1, $2, $3, $4, true)
RETURNING id;
