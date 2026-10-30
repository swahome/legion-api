-- name: ListUserMemberships :many
SELECT m.id AS membership_id,
       g.id AS group_id,
       g.name AS group_name,
       g.slug AS group_slug,
       g.status AS group_status,
       g.currency,
       m.is_official,
       r.name AS role_name,
       m.status
FROM memberships m
JOIN groups g ON g.id = m.group_id
LEFT JOIN group_roles r ON r.id = m.group_role_id
WHERE m.user_id = $1 AND m.status <> 'removed'
ORDER BY g.name;
