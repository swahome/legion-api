-- name: ListCountries :many
SELECT * FROM countries
WHERE sqlc.arg(include_inactive)::bool OR is_active
ORDER BY is_default DESC, name;

-- name: GetCountry :one
SELECT * FROM countries WHERE id = $1;

-- name: CreateCountry :one
INSERT INTO countries (name, code, currency_code, currency_name, calling_code, is_active)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: UpdateCountry :one
UPDATE countries SET
    name          = coalesce(sqlc.narg(name), name),
    currency_code = coalesce(sqlc.narg(currency_code), currency_code),
    currency_name = coalesce(sqlc.narg(currency_name), currency_name),
    calling_code  = coalesce(sqlc.narg(calling_code), calling_code),
    is_active     = coalesce(sqlc.narg(is_active), is_active),
    updated_at    = now()
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: ClearDefaultCountry :exec
UPDATE countries SET is_default = false, updated_at = now() WHERE is_default;

-- name: SetDefaultCountry :exec
UPDATE countries SET is_default = true, is_active = true, updated_at = now() WHERE id = $1;

-- name: ListCounties :many
SELECT * FROM counties
WHERE (sqlc.narg(country_id)::bigint IS NULL OR country_id = sqlc.narg(country_id))
  AND (sqlc.arg(include_inactive)::bool OR is_active)
ORDER BY name;

-- name: CreateCounty :one
INSERT INTO counties (country_id, name, code, capital, is_active)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: UpdateCounty :one
UPDATE counties SET
    name       = coalesce(sqlc.narg(name), name),
    code       = coalesce(sqlc.narg(code), code),
    capital    = coalesce(sqlc.narg(capital), capital),
    is_active  = coalesce(sqlc.narg(is_active), is_active),
    updated_at = now()
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: ListBanks :many
SELECT b.*, (SELECT count(*) FROM bank_branches br WHERE br.bank_id = b.id AND br.is_active)::bigint AS branch_count
FROM banks b
WHERE (sqlc.arg(include_inactive)::bool OR b.is_active)
  AND (sqlc.narg(search)::text IS NULL OR b.name ILIKE '%' || sqlc.narg(search) || '%')
ORDER BY b.is_default DESC, b.name;

-- name: GetBank :one
SELECT * FROM banks WHERE id = $1;

-- name: CreateBank :one
INSERT INTO banks (name, slug, code, short_code, is_partner, is_active)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: UpdateBank :one
UPDATE banks SET
    name       = coalesce(sqlc.narg(name), name),
    code       = coalesce(sqlc.narg(code), code),
    short_code = coalesce(sqlc.narg(short_code), short_code),
    is_partner = coalesce(sqlc.narg(is_partner), is_partner),
    is_active  = coalesce(sqlc.narg(is_active), is_active),
    updated_at = now()
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: ListBankBranches :many
SELECT * FROM bank_branches
WHERE bank_id = sqlc.arg(bank_id)
  AND (sqlc.arg(include_inactive)::bool OR is_active)
  AND (sqlc.narg(search)::text IS NULL OR name ILIKE '%' || sqlc.narg(search) || '%')
ORDER BY name;

-- name: CreateBankBranch :one
INSERT INTO bank_branches (bank_id, name, code, is_active)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: UpdateBankBranch :one
UPDATE bank_branches SET
    name       = coalesce(sqlc.narg(name), name),
    code       = coalesce(sqlc.narg(code), code),
    is_active  = coalesce(sqlc.narg(is_active), is_active),
    updated_at = now()
WHERE id = sqlc.arg(id) AND bank_id = sqlc.arg(bank_id)
RETURNING *;

-- name: ListPlatformSettings :many
SELECT key, value, updated_at FROM platform_settings ORDER BY key;

-- name: UpsertPlatformSetting :exec
INSERT INTO platform_settings (key, value, updated_by) VALUES ($1, $2, $3)
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_by = EXCLUDED.updated_by, updated_at = now();

-- name: DeletePlatformSetting :exec
DELETE FROM platform_settings WHERE key = $1;

-- name: GetPlatformSetting :one
SELECT value FROM platform_settings WHERE key = $1;
