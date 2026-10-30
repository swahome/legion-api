# Slice 1 schema: identity, groups, members, contributions

This covers the first end-to-end slice: login, group switching, the member list,
contribution setup and a member's contribution statement. The migration is
`db/migrations/00001_slice1_core.sql`.

Source: the anonymized dump in `legioncore.zip` (`anonymized_db.sql`, 193 tables)
plus the legacy PHP in `app/modules/{users,groups,members,contributions,statements}`
and `app/libraries/{Contribution_invoices,Transactions}.php`.

## Design rules

- **Typed columns.** Legacy stores dates as epoch ints or strings, booleans as
  `int`/`varchar`, and money as `decimal(10,2)` or truncated `int`. The new schema uses
  `timestamptz`, `date` (in Africa/Nairobi), `boolean` and `numeric(14,2)`.
- **Real foreign keys.** Tenant-scoped tables use composite keys (`group_id, id`),
  so a ledger row cannot point at a member or contribution from another group.
- **`legacy_id` on every migrated table.** This makes the ETL idempotent, so it can
  be re-run in cutover rehearsals, and lets us trace any row back to MySQL.
- **Long-tail flags go in `jsonb`.** `investment_groups` has ~120 columns and
  `members` has ~20 survey/KYC fields the slice doesn't use. They go in
  `groups.settings` and `memberships.profile` with their legacy key names, and get
  promoted to columns when a feature needs them.
- **Encryption is not needed for this data.** The dump is already plaintext, so the
  AES/SALT step only matters for the real production export at cutover.
  Field-level app encryption for PII (`id_number`, KYC) is a later decision.
- **Balances are derived.** Legacy `statements` keeps running `balance` /
  `contribution_balance` integer columns, and `member_contributions_fines_balances`
  caches totals. We store entries only and compute balances with SQL.

## Table mapping

| New table | Legacy source | Notes |
|---|---|---|
| `users` | `users` | Login identity is **phone** (Ion Auth `identity = 'phone'`), normalised to E.164. Email is optional and unique case-insensitively. The 30+ KYC columns are dropped from users and the useful ones move to `memberships.profile`. |
| `user_platform_roles` | `groups` + `users_groups` (Ion Auth) | Legacy role ids: 1 admin, 2 member, 3 group-account-manager, 4 banker, 5 partner, 6 sales. "member" is implicit and not stored. |
| `refresh_tokens` | none (replaces CI file sessions and `remember_code`) | Opaque rotating refresh token, stored only as a SHA-256 hash. |
| `groups` | `investment_groups` | `status` comes from `active`/`status`/`group_setup_status`. All other flags go in `settings` jsonb. |
| `group_roles` | `group_roles` | Per-group offices (Chair Person, Treasurer, ...). |
| `memberships` | `members` | `is_admin` becomes `is_official`. `active` / `is_deleted` become `status`. Unique on `(group_id, user_id)`. |
| `contributions` | `contributions` | `type` 1/2/3 becomes `kind` regular/one_time/non_scheduled. `category` 1–6 becomes the enum. `is_deleted` becomes `deleted_at`. |
| `contribution_schedules` | `regular_contribution_settings` + `one_time_contribution_settings` | One row per contribution. Frequency codes: 1 monthly, 2 bimonthly, 3 quarterly, 4 biannually, 5 annually, 6 weekly, 7 fortnightly, 8 daily, 9 twice_monthly. Day/week picker fields are kept as-is in `rule`. |
| `contribution_members` | `contribution_member_pairings` | When there are no rows (`enable_contribution_member_list` off), `applies_to_all_members` is true. |
| `contribution_fine_rules` | `contribution_fine_settings` | `fine_type` 1 fixed, 2 percentage. |
| `ledger_entries` | `statements` | See below. `active = 0` becomes `voided_at`. |

### Ledger entry types (`statements.transaction_type`)

From `Transactions::$statement_transaction_names`, with row counts from the dump:

| Legacy | New `entry_type` | Rows |
|---|---|---|
| 1 | contribution_invoice | 19,814 |
| 2 | contribution_fine_invoice | 294 |
| 3 | fine_invoice | 592 |
| 4 | misc_invoice | 33 |
| 9, 10, 11, 15 | contribution_payment (bank, sacco, mobile, other) | 26,750 |
| 12, 13, 14, 16 | fine_payment | 356 |
| 17–20 | misc_payment | 415 |
| 21–24 | contribution_refund | 403 |
| 25 / 26 | contribution_transfer_in / _out | 967 |
| 27 | contribution_transfer_out (to another contribution) | 1 |
| 28 | contribution_to_fine_transfer | 1 |
| 29 | loan_to_contribution_transfer | 1 |
| 30 | contribution_to_loan_transfer | 22 |
| 31 | loan_to_fine_transfer | 0 |

Backdated variants (5–8 and 18 in the old docs) don't appear as separate codes in
this data. Statements use `is_a_back_dating_record` instead, which is carried as a flag.

## Data quality findings (anonymized dump)

These need decisions in the ETL (step 3). None of them block the schema.

1. **`users_groups.user_id` is NULL in all 874 rows.** Anonymisation broke platform
   roles, so we can't tell who the platform admins are from this dump. For dev we
   seed an admin. At cutover the production export needs this table intact.
2. **Phones are 4-digit placeholders** in the dump (`users.phone` is `int(11)`, which
   also overflows real Kenyan numbers in production). `calling_code` is mixed:
   `254`, `+254`, `+25`, or NULL. The ETL normalises to E.164 and sets phone to NULL
   when it can't be normalised. Dev logins will use email or a seeded phone.
3. **Password hashes:** 835 bcrypt (`$2y$08`, which Go's bcrypt verifies directly),
   21 legacy SHA-1 hex hashes and 12 rows with a phone number in the password
   column. The last two groups get `password_reset_required = true`.
4. **Orphans:** 2,491 `statements` rows point at deleted members (they're kept with
   `membership_id` NULL), 26 point at missing contributions, 14
   `contribution_member_pairings` point at missing members (dropped), 3 `members`
   point at missing users (dropped), 2 regular settings point at missing
   contributions (dropped), and 4 `members.group_role_id` point at missing roles
   (set to NULL).
5. **One duplicate `(user_id, group_id)` membership.** The ETL keeps the active,
   most recent one.
6. **36% of statement rows are voided** (`active = 0`, 17,878 of 49,649). They're
   kept, with `voided_at` set, for audit.
7. **Legacy balances are `int`** (`statements.balance`,
   `member_contributions_fines_balances.*`), so cents are lost. We'll recompute from
   entries and compare totals as an ETL check.

## Open questions for step 3

- **Sign of transfer entries (25–30).** The legacy code computes balances per type.
  I'll confirm each sign against `Statements_m` balance functions before writing the
  ETL, and add a test that recomputed balances match legacy totals for every member.
- **Invoicing engine.** This slice shows schedules and statements but doesn't
  generate invoices. Porting `Contribution_invoices` (the cron that creates type-1
  entries) is its own slice.
