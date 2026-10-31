# Mobile app API (legacy compatible)

The released mobile app keeps working against Legion. It calls the same
actions with the same encrypted payloads; only its base URL changes:

```
old: https://<chamasoft host>/mobile/<action>
new: https://legion.com/v1/legacy/mobile/<action>
```

So the app's base URL becomes `https://legion.com/v1/legacy/`. It sits under
`/v1`, so the web server's existing `/v1` proxy covers it.

The full list of legacy actions, and which milestone ports each one, is in
`docs/mobile/legacy-mobile-api.md`.

## Setup

The API needs the two legacy key files. Copy them out of the old
`assets/certificates/` folder to the server, outside the repository, and set:

```sh
LEGION_MOBILE_SERVER_KEY=/etc/legion/10.184.38.64.key   # opens the app's requests
LEGION_MOBILE_APP_CERT=/etc/legion/mobile.crt           # seals replies for the app
```

With neither set, `/v1/legacy` isn't served. Setting only one is an error.
`mobile.crt` expired in 2021; that doesn't matter, because only its public key is
used, as before.

## What carries over

- **Encryption, both ways.** A request body is `base64(AES-128-CBC(json)):base64(iv)`, and its key travels RSA-wrapped in the `Secret` header. A reply is `{"secret", "body"}`, sealed for `mobile.crt`. This was checked against the real keys using the legacy PHP `Encryptdecrypt` code: it reads what Legion sends, and Legion reads what it sends.
- **Headers and answers.**
  - `Authorization: Bearer <token>`, `Secret` and `Versioncode` (or `VersionCode`) work as before.
  - Builds 73 and older are told to update.
  - Builds 74 to 77 get `{"response": {...}}`; newer builds get the object unwrapped.
  - Errors keep their legacy `status` codes and messages: `401` with no token, `400` for an expired token or a `user_id` that isn't the token's, `2` for an empty body, `3` for an unreadable one, and HTTP 404 for an unknown action.
- **Tokens.**
  - 32 characters, valid for 100 days, and each use keeps one alive for at least 30 more days.
  - The import carries over live app sessions from `user_pin_access_token`, so nobody is signed out by the switch.
  - Deactivating a user ends their app sessions at once.
- **Response shapes.** Ids and other database values are strings, names are upper-cased and phones have no `+`, as MySQL and the old code returned them. `group_roles` is an id-to-name object in name order, or `[]` when empty.

## Actions served so far

| Action | Notes |
|---|---|
| `mobile/login` | Phone or email and password. Uses the web login's throttle: 3 failures in 10 minutes lock that identity and IP |
| `mobile/generate_pin`, `mobile/resend_pin` | Send a 4-digit PIN. No SMS sender is configured until milestone 5, so for now these answer "Error occured sending pin" |
| `mobile/verify_pin` | Trades the PIN for a token. For a number with no account it returns `user_exists: 0` and the `unique_code` that sign-up needs |
| `mobile/get_user_checkin_data` | The user and their groups, with role, activation status and currency |
| `mobile/get_group_data` | The check-in data for one group |
| `mobile/countries/get_country_options`, `get_currency_options` | Active countries, and their currency names |
| `mobile/banks/get_bank_options` | Active banks, without e-wallets |
| `mobile/bank_branches/get_bank_branches_options` | Branches of `bank_id`; a non-numeric id answers "Request submitted is invalid" |
| `mobile/group_roles/group_roles_list` | The group's roles, with `in_use` when an active member holds one. Paged by `lower_limit`/`upper_limit` |
| `mobile/members/get_members_and_details` | Members and suspended members, with totals when `show_full_details` is on (the default). Honours member information privacy |
| `mobile/members/get_group_member_options` | Active members for pickers. Honours member information privacy |
| `mobile/contributions/get_group_contributions` | Every contribution, with its type, schedule text and next dates |
| `mobile/contributions/get_group_contribution` | One contribution (`id`) with its schedule under the legacy column names, its fines and its members |
| `mobile/contributions/get_group_contribution_options`, `get_member_contribution_options` | Active, visible contributions; the member version adds the caller's balance on each |
| `mobile/statements/contribution_statement` | The last six months of a member's contribution statement (`member_id`, or the caller) |
| `mobile/get_member_dashboard_data`, `get_group_dashboard_data`, `get_group_member_contribution_summary` | Dashboard totals from the ledger, with bank and cash balances, expenses and recent deposits |
| `mobile/get_member_recent_transactions`, `get_group_recent_transactions` | The latest deposits (`limit`, default 5) of the caller or of the group |
| `mobile/accounts/get_group_account_options`, `get_group_active_account_options` | The group's accounts by kind for pickers; the active version leaves out closed accounts. The app sends an account back as `bank-<id>`, `sacco-<id>`, `mobile-<id>` or `petty-<id>`, as before |
| `mobile/bank_accounts/get_group_bank_accounts_list`, `sacco_accounts/…`, `mobile_money_accounts/…`, `petty_cash_accounts/get_group_petty_cash_accounts_list` | Each kind's accounts with balances. Bank accounts say whether the bank is a partner and the account verified |
| `mobile/income_categories/group_income_categories_list`, `expense_categories/get_group_expense_categories`, `fine_categories/group_fine_categories_list` | All categories, paged |
| `mobile/income_categories/get_group_income_category_options`, `expense_categories/get_group_expense_category_options`, `fine_categories/get_group_fine_category_options` | Active, visible categories |
| `mobile/depositors/get_group_depositor_options` | Outside payers, with email or phone as `identity` |
| `mobile/deposits/get_deposits_list`, `get_group_deposit` | Deposits newest first (paged, 20 by default), filtered by the app's `status` type numbers and `members`, sorted by `sort_by`; one deposit as a receipt with the amount in words |
| `mobile/withdrawals/get_group_withdrawal_list`, `get_group_withdrawal` | The same for money out |
| `mobile/invoices/get_group_invoices`, `get_group_invoice`, `fines/get_group_fines_list` | Invoices and fines with `amount_paid`. Legion keeps payments against a member's balance, not against single invoices, so `amount_paid` settles the oldest charge first (per contribution for contribution invoices) |
| `mobile/fines/get_group_fine_options` | Contributions that charge late fines and fine categories, with the caller's fine balance on each |
| `mobile/deposits/record_contribution_payments`, `record_fine_payments`, `record_miscellaneous_payments`, `record_income` | An array of payments (under any key), recorded all together or not at all. A fine payment names `fine_category_id` as `fine_category-<id>`, a plain id, or `contribution-<id>` for a contribution's late fine |
| `mobile/deposits/new_record_contribution_payments`, `new_record_fine_payments` | One payment for every active member (`amount`) or for chosen members (`individual_payments`: member id to amount). `member_type_id` 2 means everyone for contributions; for fines, anything but 1 does, as before |
| `mobile/deposits/new_record_miscellaneous_payments`, `new_record_income_payment`, `deposits/void` | One payment; void takes `id` and also takes the payment off the member's balance |
| `mobile/withdrawals/record_expenses`, `new_record_expenses`, `record_contribution_refund`, `record_funds_transfer`, `withdrawals/void` | Money out. A refund can't be more than the member paid into the contribution; a transfer can't take the source account below zero, as before. Void takes `withdrawal_id` |
| `mobile/fines/fine_members`, `new_fine_members` | Fine `members` (`[{member_id}]`, or ids with `member_type_id` 1) or every active member (`member_type_id` 2), by fine category or a contribution's late fine |
| `mobile/invoices/create`, `invoices/void` | Raise contribution (1), contribution fine (2), fine (3) or miscellaneous (4) invoices on every active member (`send_to` 1) or on `member_ids`; void any of them |
| `mobile/income_categories/…`, `expense_categories/…`, `fine_categories/…`: `create`, `edit`, `hide`, `unhide`, `delete` | Names must be unique in the group. A category that any deposit, withdrawal, request or fine names can't be deleted (hide it instead) |
| `mobile/depositors/create` | An outside payer: `name` (unique in the group), optional `email` and `phone` |
| `mobile/bank_accounts/…`, `sacco_accounts/…`, `mobile_money_accounts/…`, `petty_cash_accounts/…`: `create`, `edit`, `close`, `activate`, `delete` | Officials set up the group's accounts. Bank account numbers are 5 to 20 digits. An account's bank, sacco or provider can't change. Delete needs the caller's `password` and an account with no transactions. A verified default bank account (the old e-wallet) can't be changed from the app, as before |
| `mobile/saccos/get_sacco_options`, `sacco_branches/get_sacco_branches_options`, `mobile_money_providers/get_mobile_money_provider_options` | Pickers for opening accounts. `logo` is empty: Legion keeps no sacco or provider logos |
| `mobile/withdrawals/request_funds_transfer` | An official asks to pay money out. `withdrawal_for` 1 expense, 2 or 3 contribution refund, 5 transfer to `transfer_to`; 4 (loan disbursement) waits for milestone 4. `recipient` 1 mobile (`phone`), 2 paybill (`paybill_number`, `paybill_account_number`), 3 bank (`bank_id`, `account_number`). The money comes from `transfer_from`, or the group's default (or only) bank account. Needs two signatories |
| `mobile/withdrawals/withdrawal_request_list`, `view_withdrawal_request` | Requests by the app's status numbers (1 pending, 2 and 14 approved, 3 declined or cancelled, 15 paid, 16 failed), with each signatory's answer |
| `mobile/withdrawals/respond_to_withdrawal_request`, `cancel_withdrawal_request` | A signatory approves, or declines with a `reason`. Whoever asked can cancel a pending request with a reason |
| `mobile/reports/contribution_summary`, `fines_summary`, `expenses_summary` | Paid and arrears per active member (only the caller when the group keeps members' details private; arrears 0 when the group disables arrears), and expenses by category |
| `mobile/reports/transaction_statement` | The group's cash book across all accounts from `from` (default four months ago) to `to` (default tomorrow), sent in the URL query or the body, with the balance brought forward |
| `mobile/loan_types/create`, `edit`, `update_loan_type_fines`, `update_general_details`, `hide`, `unhide`, `delete` | Officials only. The app sends a type in three steps (general terms, then fines, then fee and guarantors); each step saves its part. The fine and fee fields are read as the legacy import reads them. A type that any live loan or application uses can't be deleted (hide it instead) |
| `mobile/loan_types/get_group_loan_type`, `get_group_loan_types_list`, `get_group_loan_type_options`, `get_group_loan_type_application_options` | Types with their legacy codes and descriptions. Members are only offered visible types to apply with |
| `mobile/loans/get_group_loans_list`, `get_group_loan`, `get_member_loan_options`, `get_members_loans_options`, `statement` | Loans with live balances. `is_member_loans` lists one member's (an official may name `member_id` unless the group keeps members' details private) |
| `mobile/loans/get_member_loan_calculator`, `loan_applications/get_group_loan_calculator` | The schedule a loan type gives `loan_amount` over `repayment_period` (or the type's fixed term) from today |
| `mobile/loans/record_member_loan`, `edit_member_loan`, `void` | Officials only. With `loan_type_id` the type's terms apply; without, the custom terms in the form. Editing voids the loan and records it again with the new terms, so it is refused once anything has been repaid |
| `mobile/deposits/record_loan_repayments` | Officials only. `loan_repayments` items (`loan_id`, `member_id`, `account_id`, `amount`, `deposit_date`, `deposit_method`): all recorded or none. The loan must be the member's |
| `mobile/reports/loans_summary`, `get_group_member_loans_summary` | One line per loan (members see their own), and the caller's open loans with their next installment. Profit is the interest and fines share of what has been repaid |
| `mobile/loan_applications/create_group_loan_application` | `loan_product_id`, `loan_amount`, `repayment_period` and the parallel `guarantors`, `amounts`, `comments` arrays. The loan type's limits and guarantor rules apply |
| `mobile/loan_applications/get_group_loan_applications`, `get_member_loan_applications`, `get_loan_application_guarantorship_requests`, `get_loan_application_approval_requests`, `get_member_action_guarantorship_requests`, `get_member_action_approval_requests` | Applications, one application's guarantors or signatories, and what waits for the caller's answer |
| `mobile/loan_applications/approve_loan_guarantorship_request`, `decline_loan_guarantorship_request` | A guarantor answers `loan_application_id`; a decline needs `decline_message` |
| `mobile/loan_applications/approve_loan_application` | A signatory answers: `action` 1 approves, 2 declines with `decline_message`. A `password` the app sends is checked |
| `mobile/loan_applications/void_group_loan_application` | The applicant withdraws an open application (`id`), or an official declines it |
| `mobile/loan_applications/upload_document`, `create_amt_loan_application`, `approve_ngo_loan_application` | Answer "This service is not available." These were for the Chamasoft NGO portal |
| `mobile/bank_loans/get_group_bank_loan_options` | Bank loans still being repaid, with `balance` |
| `mobile/deposits/record_bank_loan`, `void_bank_loan` | Officials only. `description`, `amount_loaned`, `total_loan_amount_payable`, `loan_balance` (what is still owed), `loan_start_date`, `loan_end_date`, `account_id`. Voiding (`bank_loan_id`) takes the deposit and repayments with it |
| `mobile/withdrawals/record_bank_loan_repayment` | Officials only. `bank_loan_id`, `repayment_date`, `account_id`, `repayment_method`, `description`, `amount` |
| `mobile/reports/bank_loans_summary` | One line per live bank loan and the footer totals. Arrears are the balances of loans past their end date |
| `mobile/debtors/get_debtors_list` | Active debtors |
| `mobile/debtor_loan_types/*` | The `loan_types` actions, on the loan types offered to debtors |

**Recording.** Every action that changes the books is for officials only (a group role or admin), with the legacy "For group admins only" answer. The old category and depositor actions let any member make changes; now only officials can. Payments go through the same code as the web, so a recorded deposit, its cash book entry and the member's ledger entry are written and voided together. Dates may be a unix time (seconds or milliseconds) or a date string read as PHP's `strtotime` read it (`05-10-2026` is 5 October, `10/05/2026` is 5 October too, month first). Amounts may carry commas. The app's payment method numbers map to Legion's methods; unknown numbers record no method. Validation failures answer "Form validation failed" with `validation_errors` on the field concerned. SMS and email receipts start with milestone 5.

**Request ids.** The legacy list of recording actions (`deposits/record_*`, `withdrawals/record_*`, `invoices/create` and others as they are ported) needs a `request_id` of the form `<unix time>_<anything>`, sent today. Without one the answer is status 11 (update the app), with an old or malformed one status 13, and a repeat gets status 12 "Duplicate request", so a retried request is never recorded twice. These answers are never wrapped in `response`, as before. When the action fails (status 0) the id is released so the app can try again. Ids live in `mobile_request_ids` for three days.

**Withdrawal requests.** The rules are the web's (`internal/books`): every signatory must approve, one decline declines, the requester's approval counts. Two differences from the web: the old app never asked a signatory for their password when answering, so the app doesn't have to (a `password` it sends is checked), and only whoever asked can cancel, and only while pending, as before. The legacy statement descriptions dropped punctuation (`KES 150000`); these keep it (`KES 1,500.00`). Statement lines name the kind of movement, the member and the account; the category and contribution names are on the deposit and withdrawal screens.

Group actions check the user (status 4), the group (status 5) and the caller's membership (status 6, or 0 where the old action used 0), and record the user as seen, as before. Two things differ:

- Suspended and closed groups count as not found (status 5), as on the web.
- The contribution statement only shows members of the caller's group. The old one would show a member of any group.

**Totals.** Contributions paid are payments and transfers in, less refunds, transfers out (including to fines) and transfers to loans, plus loan repayments moved into a contribution. Fines paid include contributions and loan repayments moved to fines. Arrears are the contribution and fine balances, or 0 when the group disables arrears.

**Loans.** The rules are the web's: a balance is the live installments and fines less the live repayments, and an application goes from its guarantors to the signatories (members with a group role, or only officials when the group restricts approvals to admins), then an official pays it out on the web. A loan paid out from the app is recorded with `loans/record_member_loan`. Amounts in loan rows come with two decimals (`4000.00`). `toggle_loan_application_status` was an internal helper in the old code and isn't served. Bank loans (money the group borrows) and debtors came in milestone 4b; the app had no actions for lending to debtors beyond the list and loan types.

**Not there yet.** Until later milestones port notifications and bank alerts, these come back as 0 or empty in the shapes the app expects: notification and unreconciled counts. Deposits are all "Manually recorded" until bank reconciliation arrives. Groups that disable arrears got a deposits-only statement in the old app; for now they get the regular contribution statement.

The other actions answer HTTP 404 until their milestone ports them. The old `*_categories/manage` actions, which read the request body directly and trusted the user id in it, are not carried over; the app uses `create`, `edit`, `hide`, `unhide` and `delete`. The same goes for `recipients/create`.

## Deliberately not carried over

These were security holes in the old API:

- **Fixed back doors.** The token and secret `abcdefghhgfedcba`, and the IP allowlist (`127.0.0.1`, `197.237.106.185`), skipped authentication and encryption.
- **Fixed test PINs.** `1234` worked for a few numbers and emails.
- **PINs that never expired and could be guessed without limit.** PINs now last 10 minutes and lock after 5 wrong tries. They're stored as hashes, and each one works only once.
- **Plain-text logging.** Every payload, password and PIN went to `logs/*.dat`, and every OTP was emailed to an admin.
- **`mobile/get_all_otps`.** Any signed-in user could read everyone's PINs with it.
- **Plain-text tokens.** Access tokens were stored as they are; now only their SHA-256 is kept.
