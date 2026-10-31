# Groups, members and contributions

Every route below is under `/v1/groups/{groupID}` and needs a Bearer token.

## Access

| Caller | Access |
|---|---|
| Active member | Read the group, the member directory, contributions, and their own details and statement |
| Official (`memberships.is_official`) | All of the above for every member, plus the writes marked "official" |
| Platform admin who isn't a member | The same as an official. This replaces the legacy "view group as admin" |
| Anyone else, including suspended or removed members | `404 group_not_found`, so group ids can't be probed |
| Members of a group a platform admin has suspended or closed | `403 group_suspended`. Platform admins still have access |

Access is checked against `memberships` on every request. Demoting or removing
someone takes effect immediately, without waiting for their token to expire.

## Endpoints

| Method | Path | Who | Notes |
|---|---|---|---|
| GET | `/` | member | Group details, the caller's membership and role, and a summary (active members, officials, active contributions, total contributions paid). Contact details are for officials only |
| GET | `/roles` | member | Offices in the group (Chair Person, Treasurer, ...) |
| GET | `/members?status=&q=` | member | Directory. `status` is `active` (the default) or, for officials, `suspended`, `removed` or `all`. `q` searches name, phone and membership number. Members see others' names and offices but not their phone or email. Returns 403 for members when the group set the legacy `disable_member_directory` flag |
| GET | `/members/{id}` | self or official | Full member details |
| POST | `/members` | official | Adds a person by phone. An existing user with that phone is reused. Otherwise a user is created who must reset their password before logging in. Re-adding a suspended or removed member reactivates them. 409 `already_member` or `email_taken` |
| PATCH | `/members/{id}` | official | `is_official`, `role_id` (null clears it), `membership_number`, `status`. 409 `last_official` if the change would leave no active official |
| GET | `/members/{id}/statement?from=&to=&contribution_id=` | self or official | Contribution statement with debit, credit and running balance per entry, opening and closing balance for the window, and lifetime totals per contribution. Contributions marked `hidden_in_statement` are shown to officials only |
| GET | `/contributions` | member | Hidden contributions are for officials only |
| GET | `/contributions/{id}` | member | Includes the schedule, fine rules and member list (when it doesn't apply to everyone) |
| POST | `/contributions` | official | `name`, `kind` (`regular`, `one_time` or `non_scheduled`), `amount`, optional `category`, `applies_to_all_members`, `member_ids`, `schedule`. Regular contributions need `schedule.frequency` and `schedule.contribution_date` |
| PATCH | `/contributions/{id}` | official | Any field except `kind`. `schedule` and `member_ids` replace the existing ones when sent |
| GET | `/contribution-balances` | official | Every active member's payable, paid and balance, most in arrears first |

### Money (milestone 3)

| Method | Path | Who | Notes |
|---|---|---|---|
| GET | `/accounts` | official | Bank, sacco, mobile money and petty cash accounts with balances, plus `unassigned` (legacy entries whose account is unknown, by kind) and `total_balance` |
| POST | `/accounts` | official | `kind`, `name`, optional `account_number`, `institution_id` (bank, sacco or provider id; not for petty cash), `branch_id` (bank or sacco branch), `initial_balance`, `is_default` (one default per kind) |
| PATCH | `/accounts/{id}` | official | `name`, `account_number`, `branch_id` (null clears), `initial_balance`, `is_default`, `is_closed`. Kind and institution can't change |
| DELETE | `/accounts/{id}` | official | Only for an account with no transactions (409 `account_has_transactions`; close it instead) |
| GET | `/cash-book?from=&to=&account_id=` | official | Money in and out with a running balance, opening and closing balance and totals. Defaults to the last 12 months. With `account_id` it is that account's statement. At most 5000 rows (`truncated`) |
| GET | `/deposits?kind=&member_id=&account_id=&from=&to=&include_voided=&page=&page_size=` | official | Newest first, paged, with `totals_by_kind` for the same filters |
| GET | `/deposits/{id}` | official | Includes the member ledger entries it created |
| POST | `/deposits` | official | `kind` (`contribution`, `fine`, `misc` or `income`), `account_id`, `amount`, `date`, optional `method` (`mpesa`, `cash`, `cheque`, `funds_transfer`, `equitel`, `standing_order`) and `description`. Contribution: `member_id`, `contribution_id`. Fine: `member_id` and `fine_category_id`, or `contribution_id` for a contribution's late fine. Misc: `member_id`. Income: `income_category_id`, optional `depositor_id`. Member payments credit the member's ledger |
| POST | `/deposits/{id}/void` | official | Voids the deposit, its cash book entry and its ledger entry. 409 `already_voided` |
| GET | `/withdrawals?kind=&member_id=&account_id=&from=&to=&include_voided=&page=&page_size=` | official | Newest first, paged, with `totals_by_kind` |
| GET | `/withdrawals/{id}` | official | |
| POST | `/withdrawals` | official | Money that has already left an account. `kind` (`expense` or `contribution_refund`), `account_id`, `amount`, `date`, optional `method`, `description`. Expense: `expense_category_id`. Refund: `member_id`, `contribution_id`, at most what the member has paid in (400 `refund_too_large`); it comes off the member's contribution balance |
| POST | `/withdrawals/{id}/void` | official | Voids it with its cash book and ledger entries. 409 `already_voided` |
| GET | `/transfers?account_id=&from=&to=&include_voided=&page=&page_size=` | official | Transfers between the group's own accounts; `account_id` matches either side |
| POST | `/transfers` | official | `from_account_id`, `to_account_id`, `amount`, `date`, `description` |
| POST | `/transfers/{id}/void` | official | 409 `already_voided` |
| GET | `/withdrawal-requests?status=&awaiting=me&page=&page_size=` | official or signatory | Requests to pay money out, newest first, with approval counts. `awaiting=me` lists those waiting on the caller |
| GET | `/withdrawal-requests/{id}` | official or signatory | Adds `approval_list` (each signatory's decision) and `my_decision` |
| POST | `/withdrawal-requests` | official | `kind` (`expense`, `contribution_refund` or `funds_transfer`), `account_id`, `amount`, `description`, the same fields as a withdrawal, `to_account_id` for a transfer, and `recipient` (`{type: mobile, phone}`, `{type: paybill, paybill, account_number}` or `{type: bank, bank_id, account_number, account_name}`) except for transfers. The account must hold the amount (400 `insufficient_balance`). Needs at least two signatories (409 `not_enough_signatories`) |
| POST | `/withdrawal-requests/{id}/approve` | signatory | `password`. 401 `wrong_password`, 429 after repeated wrong passwords, 409 if already answered or the request is closed |
| POST | `/withdrawal-requests/{id}/decline` | signatory | `password`, `reason` |
| POST | `/withdrawal-requests/{id}/cancel` | official | Optional `reason`. Pending or approved requests only |
| POST | `/withdrawal-requests/{id}/pay` | official | Records the money leaving an approved request: `date`, optional `method`. Writes the withdrawal (or transfer) and sets `withdrawal_id` or `transfer_id` |
| GET | `/income-categories`, `/expense-categories`, `/fine-categories` | member | Active, visible ones. Officials can add `?all=true` |
| POST, PATCH | `/income-categories[/{id}]`, `/expense-categories[/{id}]`, `/fine-categories[/{id}]` | official | `name`, `description`, `is_hidden`, `is_active`; fine categories also take `amount` |
| GET, POST, PATCH | `/depositors[/{id}]` | official | Outside payers for income: `name`, `email`, `phone`, `description`, `is_active` |

**Signatories** are active members who hold a group role, as in the legacy
e-wallet; when the group's legacy `restrict_wallet_approval_to_group_admins` flag is
set, only officials among them. Every signatory must approve; one decline declines
the request. The official who asks counts as approving. Paying is a manual step
until the payment API is wired in.

Reference lists for opening accounts: `/v1/reference/banks` (and branches),
`/v1/reference/saccos`, `/v1/reference/saccos/{id}/branches` and
`/v1/reference/mobile-money-providers`. See `docs/schema/money.md` for the tables
and the legacy type codes.

## Conventions

- Money is a decimal string with two places, such as `"4200.00"`. Requests accept a
  string or a number with at most two decimals.
- Dates are `YYYY-MM-DD` calendar dates in the group's timezone (Africa/Nairobi).
- A positive balance means the member owes the group. A negative one means they're
  ahead.
- The `schedule.rule` object carries the legacy day and week picker settings. It's
  read-only until the invoicing engine is ported.
