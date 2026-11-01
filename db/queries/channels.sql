-- USSD and WhatsApp (milestone 7).

-- name: LatestChannelSession :one
-- The caller's latest conversation under this key, locked for this request.
SELECT * FROM channel_sessions
WHERE channel = $1 AND session_key = $2
ORDER BY id DESC LIMIT 1
FOR UPDATE;

-- name: CreateChannelSession :one
INSERT INTO channel_sessions (channel, session_key, phone, user_id, service_code, expires_at)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (channel, session_key) WHERE ended_at IS NULL DO NOTHING
RETURNING *;

-- name: SaveChannelSession :exec
UPDATE channel_sessions SET
    state = $2, group_id = $3, steps = $4, last_request = $5, last_response = $6,
    inputs = inputs + sqlc.arg(added)::int,
    ended_at = CASE WHEN sqlc.arg(ended)::bool THEN now() ELSE NULL END,
    expires_at = $7, updated_at = now()
WHERE id = $1;

-- name: EndChannelSession :exec
UPDATE channel_sessions SET ended_at = now(), updated_at = now() WHERE id = $1 AND ended_at IS NULL;

-- name: EndExpiredChannelSessions :execrows
UPDATE channel_sessions SET ended_at = expires_at, updated_at = now()
WHERE ended_at IS NULL AND expires_at < now();

-- name: DeleteOldChannelSessions :execrows
DELETE FROM channel_sessions WHERE created_at < $1;

-- name: AddChannelMessage :exec
INSERT INTO channel_messages (session_id, input, response, error) VALUES ($1, $2, $3, $4);

-- name: ListChannelSessions :many
SELECT s.id, s.channel, s.phone, s.user_id, coalesce(trim(u.first_name || ' ' || u.last_name), '')::text AS user_name,
       s.group_id, coalesce(g.name, '')::text AS group_name, s.inputs, s.ended_at, s.created_at, s.updated_at,
       (s.state ->> 'screen')::text AS screen,
       (SELECT count(*) FROM channel_messages m WHERE m.session_id = s.id AND m.error <> '') AS errors,
       count(*) OVER () AS total
FROM channel_sessions s
LEFT JOIN users u ON u.id = s.user_id
LEFT JOIN groups g ON g.id = s.group_id
WHERE (sqlc.narg(channel)::channel_kind IS NULL OR s.channel = sqlc.narg(channel))
  AND (sqlc.narg(phone)::text IS NULL OR s.phone LIKE '%' || sqlc.narg(phone) || '%')
  AND (NOT sqlc.arg(errors_only)::bool OR EXISTS (SELECT 1 FROM channel_messages m WHERE m.session_id = s.id AND m.error <> ''))
ORDER BY s.created_at DESC, s.id DESC
LIMIT sqlc.arg(lim) OFFSET sqlc.arg(off);

-- name: GetChannelSession :one
SELECT s.id, s.channel, s.phone, s.user_id, coalesce(trim(u.first_name || ' ' || u.last_name), '')::text AS user_name,
       s.group_id, coalesce(g.name, '')::text AS group_name, s.service_code, s.inputs, s.ended_at, s.expires_at,
       s.created_at, s.updated_at
FROM channel_sessions s
LEFT JOIN users u ON u.id = s.user_id
LEFT JOIN groups g ON g.id = s.group_id
WHERE s.id = $1;

-- name: ListChannelMessages :many
SELECT id, input, response, error, created_at FROM channel_messages WHERE session_id = $1 ORDER BY id;

-- name: ChannelStats :one
SELECT count(*) FILTER (WHERE channel = 'ussd')::bigint AS ussd_sessions,
       count(*) FILTER (WHERE channel = 'whatsapp')::bigint AS whatsapp_sessions,
       count(DISTINCT phone)::bigint AS callers
FROM channel_sessions WHERE created_at >= $1;

-- name: ChannelUserGroups :many
-- The groups a caller can use on USSD and WhatsApp: live groups where they
-- are an active member.
SELECT g.id AS group_id, g.name, g.currency, g.timezone, g.settings,
       m.id AS membership_id, m.is_official, m.group_role_id
FROM memberships m
JOIN groups g ON g.id = m.group_id
WHERE m.user_id = $1 AND m.status = 'active' AND g.status = 'active'
ORDER BY g.name, g.id;

-- name: MemberRecentDeposits :many
-- A member's latest payments, for the mini statement.
SELECT d.deposit_date, d.kind, d.amount::text AS amount,
       coalesce(c.name, f.name, lt.name, '')::text AS name
FROM deposits d
LEFT JOIN contributions c ON c.id = d.contribution_id
LEFT JOIN fine_categories f ON f.id = d.fine_category_id
LEFT JOIN loans l ON l.id = d.loan_id
LEFT JOIN loan_types lt ON lt.id = l.loan_type_id
WHERE d.group_id = $1 AND d.membership_id = $2 AND d.voided_at IS NULL
ORDER BY d.deposit_date DESC, d.id DESC
LIMIT $3;
