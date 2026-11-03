# Platform admin and reference data

Routes under `/v1/admin` need a Bearer token from an active user with the platform
`admin` role. The role is checked in the database on every request, so removing it
or deactivating the account takes effect at once. Anyone else gets `403 forbidden`.

Every write is recorded in `admin_audit_log` with the actor, action, target and the
request body (passwords are never recorded).

Paged lists take `page` (from 1) and `page_size` (1 to 200, default 50) and return
`{"items": [...], "total": n, "page": p, "page_size": s}`.

## Overview

| Method | Path | Purpose |
|---|---|---|
| GET | `/admin/stats` | Dashboard counts. Groups: `groups`, `active_groups`, `setup_groups`, `suspended_groups`, `closed_groups`, `groups_today`. Users: `users`, `users_today`, `users_active_30d`, `memberships`. SMS: `sms_total`, `sms_sent_today`, `sms_queued`, `sms_failed_7d`. Money: `transaction_alerts`, `alerts_unassigned`, `accounts_connected` (verified, open bank accounts), `payments_paid`, `payments_paid_amount`, `invoices_today`, `fine_invoices_today` (raised by Legion, not imported), `open_withdrawal_requests`. `audit_entries` |
| GET | `/admin/audit` | The latest 100 admin actions |
| GET | `/admin/transactions?group_id=&type=&from=&to=&include_voided=true` | Paged ledger entries across every group, newest first. Dates are `YYYY-MM-DD` |

## Background jobs

| Method | Path | Purpose |
|---|---|---|
| GET | `/admin/jobs?job=billing` | Paged run history, newest first: `run_date`, `trigger` (`schedule`, `manual` or `cli`), `status` (`running`, `succeeded`, `failed`), `summary` (counts of invoices and fines raised, schedules skipped and any `problems`), `error`, `started_by` |
| POST | `/admin/jobs/billing/run` | Runs billing now in the background, for today or `{"date": "YYYY-MM-DD"}` (not in the future). 202; 409 `job_running` if a run is in progress. Re-running a day adds nothing it already raised |

## SMS

| Method | Path | Purpose |
|---|---|---|
| GET | `/admin/sms?status=&group_id=&phone=` | Paged outbox, newest first: `to`, `body`, `kind` (`verify_phone`, `reset_password`, `invite`, `app_pin`, ...), `status` (`queued`, `sending`, `sent`, `failed`), `attempts`, Africa's Talking's `provider_status` and `cost`, `error` |

Texts are queued in the same transaction as the change that sends them, and a
dispatcher inside the binary sends them through Africa's Talking
(`LEGION_AT_USERNAME`, `LEGION_AT_API_KEY`, optional `LEGION_AT_SENDER_ID`;
username `sandbox` uses the sandbox). Failed sends are retried after 30 seconds,
2 minutes, 10 minutes, 1 hour and 6 hours, then marked failed. Without credentials
texts are written to the log instead. The legacy `disable_smses` setting marks
every text failed without sending it. A group whose notifications an admin switched
off (`notifications_off`, below) has its texts marked failed the same way.

## Groups

| Method | Path | Purpose |
|---|---|---|
| GET | `/admin/groups?q=&status=&view=&sort=` | Paged. `q` matches part of the name, the exact group id, or part of the group's or contact's phone. `view=inactive`: active groups nobody has signed in to for 90 days (candidates for archiving). `sort=newest` puts the latest sign-ups first (default: by name). Each group has `active_members`, `registered_members` (members who have ever signed in), `last_seen_at` (a member's latest sign-in) and `contact` (owner, else creator: `user_id`, `name`, `phone`, `email`) |
| GET | `/admin/groups/export` | The same list as CSV (same filters, no paging, up to 100,000 rows) |
| GET | `/admin/groups/{id}` | One group: contact details, `created_by_name`, counts of `active_members`, `registered_members`, `contributions`, `accounts`, `loans`, `last_seen_at`, `notifications_off`. Its members, contributions and accounts come from the group's own endpoints, which admins can read |
| PATCH | `/admin/groups/{id}` | Any of `status` (`active`, `suspended`, `closed`), `name`, `email`, `phone` (`null` clears), `notifications_off`. Returns the group as GET does. Members of a suspended or closed group get `403 group_suspended` on every group route |

The admin panel calls suspended groups "locked" and closed groups "archived".

## Users

| Method | Path | Purpose |
|---|---|---|
| GET | `/admin/users?q=&role=&active=` | Paged. `q` searches name, phone and email, or matches the exact user id; `role` is a platform role; `active` is `true` or `false`. Each user has `group_list` (`group_id`, `group_name`, `is_official`) |
| GET | `/admin/users/export` | The same list as CSV |
| GET | `/admin/roles` | The fixed platform roles (the old panel's "user groups") with `name`, `description` and the number of active `users` holding each |
| GET | `/admin/users/{id}` | One user, with platform roles and group memberships |
| POST | `/admin/users` | `first_name`, `last_name`, `phone` and/or `email`, `password` (8+ characters), optional `platform_roles` |
| PATCH | `/admin/users/{id}` | Any of the create fields except password, plus `is_active`. Send `null` to clear `email`, `phone` or `id_number` |
| POST | `/admin/users/{id}/password` | `{"password": "...", "require_change": false}`. Signs the user out everywhere |

Errors: `409 identity_taken` for a phone or email in use; `409 last_admin` when an
admin tries to remove their own admin role or deactivate themselves, or when the
change would leave no active admin.

## Settings

| Method | Path | Purpose |
|---|---|---|
| GET | `/admin/settings` | All settings as one JSON object |
| PATCH | `/admin/settings` | Merges keys into the settings; a `null` value removes the key. Keys use lowercase letters, digits, `_` and dots. Returns the full settings |

Menus are fixed in the web app. Admins switch items off with the
`menus.disabled` setting, a list of menu ids such as `["group.arrears", "member.members"]`.
`GET /v1/me` returns it to every user as `disabled_menus`.

The import fills the settings from the legacy `settings` row, keyed by its column
names (`application_name`, `allow_self_onboarding`, and so on).

## Countries, counties, banks and branches

Admins manage them under `/v1/admin`; any signed-in user can read the active rows
under `/v1/reference` (the group wizard and bank account forms need them).

| Method | Path | Purpose |
|---|---|---|
| GET | `/reference/countries`, `/admin/countries` | Add `?all=true` to include inactive rows (also on the other lists) |
| POST, PATCH | `/admin/countries`, `/admin/countries/{id}` | `name`, `code` (ISO alpha-2), `currency_code`, `currency_name`, `calling_code`, `is_active`, `is_default`. Setting a new default clears the old one |
| GET | `/reference/counties?country_id=` | |
| POST, PATCH | `/admin/counties`, `/admin/counties/{id}` | `country_id`, `name`, `code`, `capital`, `is_active` |
| GET | `/reference/banks?q=` | Includes `branch_count`. `q` searches the name |
| POST, PATCH | `/admin/banks`, `/admin/banks/{id}` | `name` (the slug is derived from it), `code`, `short_code`, `is_partner`, `is_active` |
| GET | `/reference/banks/{id}/branches` | |
| POST, PATCH | `/admin/banks/{id}/branches`, `/admin/banks/{id}/branches/{branchID}` | `name`, `code`, `is_active` |

Duplicates (country code, county name within a country, bank slug, branch name
within a bank) return `409`.
