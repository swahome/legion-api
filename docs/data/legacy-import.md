# Legacy data import

`legion-api import-legacy` is the cutover data migration. It reads a LegionCore MySQL
dump, maps it to the Legion schema, loads it in one transaction and then checks the
result.

```sh
# Anonymized dump (dev): phones are placeholders, so synthesise them.
legion-api import-legacy \
  -dump anonymized_db.sql \
  -users-groups users_groups.sql \
  -synthetic-phones -replace

# Production cutover: real phones, empty database.
legion-api import-legacy -dump chamasoft_prod.sql
```

| Flag | Meaning |
|---|---|
| `-dump` | MySQL / phpMyAdmin dump with `INSERT INTO` statements |
| `-users-groups` | Separate `users_groups` dump. The anonymized dump's copy has every `user_id` NULL |
| `-synthetic-phones` | Gives every user the fake phone `+2547` + their id padded to 8 digits (user 525 is `+254700000525`) so phone login works in dev. Never use it for production |
| `-replace` | Wipes the imported tables (plus refresh tokens and login attempts) first. Without it the import refuses to touch a database that has users or groups |
| `-dry-run` | Parse and transform only, then print the report |

Settings (`DATABASE_URL`, `LEGION_DEFAULT_CALLING_CODE`) come from `.env` like every
other command.

## What it does

- **Keeps legacy ids.** Every row keeps its legacy id as both `id` and `legacy_id`,
  and identity sequences are moved past the highest id afterwards. Old URLs, support
  tickets and SQL can still find the same record.
- **Applies the mapping and data rules** in `docs/schema/slice-1.md`. Every row it
  drops or changes is counted under "data issues" in the report.
- **Imports reference data and settings.** `countries`, `counties`, `banks` and
  `bank_branches` keep their ids. Counties with no country (all 47 legacy rows) go
  to the default country. Bank colours move into `banks.branding`; the other bank
  columns (OTP endpoints and the like) are kept in `banks.settings` until a feature
  needs them. The single legacy `settings` row becomes one `platform_settings` key
  per non-empty column, with integers stored as JSON numbers. Nothing is decrypted
  or encrypted: values are copied as they are.
- **Imports the money tables** (milestone 3): saccos, mobile money providers, the
  four legacy account tables (merged into `group_accounts`, with new ids since the
  legacy ids overlap), income, expense and fine categories, depositors, deposits,
  withdrawals, account transfers and `transaction_statements` (the cash book, now
  `account_entries`). See `docs/schema/money.md`, including how a row's account is
  found when the dump has lost `account_id`.
- **Keeps the mobile app signed in.** Live tokens in `user_pin_access_token`
  move to `mobile_access_tokens` (hashed); expired ones and PINs are dropped.
- **Reconciles balances.** For every member it recomputes the contribution balance in
  two ways and requires them to match to the cent:
  1. in Postgres, from `ledger_entries` joined to `ledger_entry_rules`;
  2. in Go, from the raw `statements` rows, using the same type lists as the legacy
     `Statements_m::get_member_contribution_balance`.

  Any mismatch fails the command (exit 1).
- **Reports account balances.** It compares each account's balance with the legacy
  `current_balance` and counts cash book rows with no known account. Informational
  only, like the cache below.
- **Reports the legacy cache.** It also prints how many members differ from the
  legacy cache table `member_contributions_fines_balances`. This is informational
  only. That table is a whole-shilling cache the old app refreshes on its own
  schedule, and in the anonymized dump it was last written in December 2018.

## Ledger signs

Amounts stay positive, as in the legacy `statements` table. Each entry's effect on
a balance is defined in `ledger_entry_rules` (migration `00003`):

| Bucket | Increases what the member owes | Decreases it |
|---|---|---|
| contribution | invoice (1), refund (21–24), transfer out (25, 27), to loan (30) | payment (9, 10, 11, 15), transfer in (26) |
| fine | contribution fine invoice (2), fine invoice (3) | fine payment (12–14, 16), contribution to fine (28) |
| misc | misc invoice (4) | misc payment (17–20) |
| none | loan to contribution (29) and loan to fine (31). The legacy app doesn't count these in any balance | |

A member's contribution balance is `sum(sign * amount)` over non-voided entries in
the contribution bucket.

## Result on the anonymized dump (2026-10-31)

```
imported rows:
  users                          868
  user_platform_roles              8
  groups                          85
  group_roles                    364
  memberships                    790
  contributions                  161
  contribution_schedules         128
  contribution_members            92
  contribution_fine_rules         59
  countries                      233
  counties                        47
  banks                           65
  bank_branches                 1936
  saccos                         172
  sacco_branches                 342
  mobile_money_providers           8
  group_accounts                 158
  income_categories              499
  expense_categories            1921
  fine_categories                571
  depositors                      16
  deposits                     23029
  withdrawals                   2572
  account_transfers              890
  account_entries              24218
  ledger_entries               49649
  platform_settings               34
  mobile_access_tokens             0
data issues (rows):
     5891  account_entries: account taken as the group's only account of that kind
    18327  account_entries: account unknown (several accounts of that kind), kept without account
       13  account_entries: contribution missing or in another group, kept without contribution
      150  account_entries: deposit_id missing or in another group, cleared
     1229  account_entries: member missing or in another group, kept without member
      167  account_entries: withdrawal_id missing or in another group, cleared
      890  account_transfers: account unknown, kept without it
       36  contribution_schedules: regular with no frequency
        1  countries: invalid currency or calling code, skipped
        7  depositors: blank name, skipped
     4624  deposits: account taken as the group's only account of that kind
    18405  deposits: account unknown (several accounts of that kind), kept without account
       13  deposits: contribution missing or in another group, kept without contribution
      251  deposits: depositor_id missing or in another group, cleared
     1398  deposits: member missing or in another group, kept without member
       32  fine_categories: blank name, skipped
        1  groups: owner user missing, cleared
       26  ledger_entries: contribution missing or in another group, kept without contribution
      171  ledger_entries: deposit_id missing or in another group, cleared
     2508  ledger_entries: member missing or in another group, kept without member
        1  memberships: duplicate user in group, kept one
        4  memberships: role missing or from another group, cleared
        3  memberships: user missing, skipped
        2  regular_contribution_settings: contribution missing, skipped
       14  users: inactive
       33  users: non-bcrypt password, reset required
     1233  withdrawals: account taken as the group's only account of that kind
     1339  withdrawals: account unknown (several accounts of that kind), kept without account
       46  withdrawals: member missing or in another group, kept without member
        1  withdrawals: unknown type 0, skipped
contribution balances: 623 members checked against the legacy algorithm, 0 mismatches
legacy balance cache (informational): 73 members compared, 67 differ by 1 KES or more
account balances (informational): 31 accounts compared with legacy current_balance, 1 differ by 1 KES or more; 18327 cash book entries have no known account
done in 4.968s
```
