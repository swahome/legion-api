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
| POST | `/contributions` | official | `name`, `kind` (`regular`, `one_time` or `non_scheduled`), `amount`, optional `category`, `applies_to_all_members`, `member_ids`, `schedule`, `fine_rules`. Regular contributions need `schedule.frequency` and `schedule.contribution_date`. `schedule.invoice_date` defaults to `invoice_days_before` days before the contribution date |
| PATCH | `/contributions/{id}` | official | Any field except `kind`. `schedule`, `member_ids` and `fine_rules` replace the existing ones when sent. A fine rule sent with its `id` is updated; rules left out are deleted |
| GET | `/dashboard?months=` | member | The group home page (legacy group dashboard and mobile `get_group_dashboard_data`), see below |
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
| GET | `/fines?member_id=&kind=&from=&to=&include_voided=&page=&page_size=` | member (own) or official | Fines, newest first, paged. `kind` is `fine` (raised by an official, with a `fine_category`) or `contribution_fine` (a late-payment fine on a contribution, `automatic` when the billing job raised it, with the fined invoice's `invoice_due_date`) |
| GET | `/fine-balances` | member (own) or official | Each active member's fines `fined`, `paid` and `balance`, plus `total_owed` |
| POST | `/fines` | official | Fine members: `member_ids` (up to 500), `fine_category_id`, `amount`, `date` (not in the future), optional `description`. Returns `fine_ids` |
| POST | `/fines/{id}/void` | official | 204; 404 if not found or already voided |
| GET | `/income-categories`, `/expense-categories`, `/fine-categories` | member | Active, visible ones. Officials can add `?all=true` |
| POST, PATCH | `/income-categories[/{id}]`, `/expense-categories[/{id}]`, `/fine-categories[/{id}]` | official | `name`, `description`, `is_hidden`, `is_active`; fine categories also take `amount` |
| GET, POST, PATCH | `/depositors[/{id}]` | official | Outside payers for income: `name`, `email`, `phone`, `description`, `is_active` |

`GET /v1/groups/{id}` also says whether the caller `is_signatory` and how many
requests are `withdrawal_requests_awaiting_me`.

**Signatories** are active members who hold a group role, as in the legacy
e-wallet; when the group's legacy `restrict_wallet_approval_to_group_admins` flag is
set, only officials among them. Every signatory must approve; one decline declines
the request. The official who asks counts as approving. Paying is a manual step
until the payment API is wired in.

Reference lists for opening accounts: `/v1/reference/banks` (and branches),
`/v1/reference/saccos`, `/v1/reference/saccos/{id}/branches` and
`/v1/reference/mobile-money-providers`. See `docs/schema/money.md` for the tables
and the legacy type codes.

### Loans (milestone 4)

| Method | Path | Who | Notes |
|---|---|---|---|
| GET | `/loan-types?include_hidden=` | member | Loan types; hidden ones only for officials who ask |
| POST, PATCH | `/loan-types[/{id}]` | official | `name`, `interest_type` (`fixed_balance`, `reducing_balance`, `fixed_amount`), `interest_rate`, `rate_per` (`day`, `week`, `month`, `year`, `term`), `reducing_method`, `fixed_interest` or `fixed_interest_rate`, `frequency` (`daily`, `weekly`, `monthly`), `fixed_term` or `min_term`/`max_term`, `grace_periods`, `amount_basis` (`range` with `min_amount`/`max_amount`, or `savings` with `savings_multiple`), `guarantors` (`none`, `always`, `over_max`) with `min_guarantors`/`max_guarantors`, `late_fine`, `balance_fine`, `processing_fee`, `fine_deferment`, `needs_approval`, `is_hidden`. PATCH replaces the whole type |
| DELETE | `/loan-types/{id}` | official | Existing loans keep their copied terms |
| GET | `/loans?status=&member_id=&loan_type_id=&page=&page_size=` | member (own) or official | Paged, with totals. `status` is empty (live), `open`, `paid` or `voided` |
| GET | `/loans/{id}` | member (own) or official | The loan, `totals` (`payable`, `principal`, `interest`, `fines`, `paid`, `balance`, `overdue`, `overpaid`), `charges` with what is paid on each, `repayments`, `guarantors`, `statement` with a running balance, `next_due` and `status` |
| POST | `/loans/preview` | member | `loan_type_id` or full terms, `amount`, `installments`: the schedule (as `installments`), `interest`, `total`, `processing_fee` and `ends_on`, without saving |
| POST | `/loans` | official | Records a loan already paid out: `member_id`, `account_id`, `amount`, `disbursed_on`, `installments`, optional `method`, `first_due_on`, `description`, `guarantors` (`member_id`, `amount`, `comment`), `skip_processing_fee`. With `loan_type_id` the type's terms and limits apply; without one every term is given. Writes the disbursement withdrawal, the schedule and the fee deposit |
| PATCH | `/loans/{id}` | official | `description`, `is_bad_debt` |
| POST | `/loans/{id}/void` | official | Voids the loan with its disbursement, fee, charges and repayments. 409 `loan_voided` |
| POST | `/loans/{id}/repayments` | official | `account_id`, `amount`, `paid_on`, optional `method`, `description`. Writes a `loan_repayment` deposit |
| POST | `/loans/{id}/repayments/{rid}/void` | official | Also voids the deposit. Voiding the deposit voids the repayment too |
| POST | `/loans/{id}/fines/{cid}/void` | official | Waives one fine |

A loan's disbursement withdrawal can't be voided on its own (409
`loan_disbursement`); void the loan. See `docs/schema/loans.md` for the
schedule rules and legacy codes.

### Dashboard

`GET /dashboard` returns, as of today in the server's time zone (`as_of`):

- `cash`: `at_bank` (bank and sacco accounts), `at_hand` (mobile money and petty
  cash), `total`, `by_kind` balances and the number of open `accounts` per kind.
  Legacy entries whose account is unknown count towards their kind.
- `totals`: all-time `contributions`, `fines` and `misc` paid by members (from the
  ledger), `income` and `expenses` (from deposits and withdrawals). Voided rows are
  left out.
- `months`: the same five figures per calendar month, for the last `months`
  months including this one (default 12, at most 36). `years`: per calendar year
  over the whole history, only years with activity.
- `owed`: for `contributions`, `fines` and `misc`, what active members owe (the
  sum of positive balances) and how many `members` owe it. `members_behind`
  counts members who owe anything.
- Officials also get `recent` (the latest 10 cash book movements: `date`,
  `direction` in or out, `kind`, `amount`, `account`, `member`) and
  `pending_withdrawal_requests`.

Loan figures (the legacy `total_loan_balances` and friends) join later in milestone 4.

## Conventions

- Money is a decimal string with two places, such as `"4200.00"`. Requests accept a
  string or a number with at most two decimals.
- Dates are `YYYY-MM-DD` calendar dates in the group's timezone (Africa/Nairobi).
- A positive balance means the member owes the group. A negative one means they're
  ahead.
- `schedule.rule` is the day picker, in the legacy setting names: for example
  `{"month_day_monthly": 5}` (the 5th), `{"month_day_monthly": 2, "week_day_monthly": 3}`
  (the 2nd Tuesday; weekdays run 1 = Sunday to 7 = Saturday, and 32 means "last"),
  `{"week_day_weekly": 6}` (every Friday), or `start_month_multiple` with
  `month_day_multiple` and `week_day_multiple` for bimonthly to yearly. See
  `internal/billing/schedule.go` and `docs/schema/billing.md`.
- A fine rule is `kind` (`fixed` with `fixed_amount`, or `percentage` with
  `percentage_rate` and `percentage_of`: `"1"` contribution amount, `"2"` unpaid
  balance, `"3"` balance plus unpaid fines), `chargeable_on` (`"1"` to `"56"` days
  after the contribution date, `last_day_of_the_month` or `first_day_of_the_month`),
  `mode` (`"1"` a fine per unpaid invoice, `"2"` one fine on the balance),
  `frequency` (`"0"` once, `"1"` daily, `"2"` weekly, `"3"` monthly, `"4"` quarterly,
  `"5"` half-yearly, `"6"` yearly), `fine_limit` (fines per invoice, 0 = no limit),
  `is_active`. The response adds `next_fine_date`. Fines are charged only while the
  schedule has `fines_enabled`.
