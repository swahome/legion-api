# Payments, bank feeds and reconciliation

Milestone 6. Money reaches a group's account in two ways, and both end up as
**transaction alerts**: one row per credit or debit a bank or MPesa reports on an
account number.

- **Member payments.** A member asks to pay (web, app or USSD later). The API
  sends an MPesa prompt through the Chamasoft payment API; when the member enters
  their PIN, Chamasoft calls back and the payment becomes an alert.
- **Bank and MPesa feeds.** Equity Bank, Co-op Bank, Chamasoft direct paybill
  payments and Safaricom C2B post every transaction on a known account number.

An alert is then **reconciled**: turned into deposits (money in) or withdrawals
(money out) in the group's books. That happens automatically when the payer and
purpose are known, or by an official from the reconciliation screen.

The rules live in `internal/payments` and are shared by the web API, the mobile
app API and the webhooks.

## Alerts

| Field | Meaning |
|---|---|
| `source` | `chamasoft` (a payment a member started), `chamasoft_direct`, `equity`, `coop`, `mpesa`, or `legacy` (imported) |
| `provider_ref` | The bank's or MPesa's transaction id. `(source, provider_ref, direction)` is unique, so a feed that repeats itself is answered as a duplicate |
| `direction` | `in` or `out` |
| `group`, `account` | The group account whose number matched. `null` when no account has the number, or when more than one group has it (anonymized and test data do this). Platform admins assign those |
| `status` | `unreconciled`, `reconciled` or `ignored` |
| `auto_reconciled` | true when the API reconciled it without an official |

### Automatic reconciliation

- **Member payments** (`chamasoft`) are recorded as the lines the member chose,
  unless the group set `disable_auto_reconcile` (the web setting
  `auto_reconcile_payments: false`).
- **Feeds** are reconciled only when the group turned it on
  (`enable_auto_reconcile`, the web setting `auto_reconcile_bank_alerts`), as in
  the legacy app. The payer is found by phone (the feed's phone, then a phone in
  the narration), then by a unique first and last name in the narration. The
  amount is spread over what the member owes in the legacy order: contribution
  late fines, contributions, then other fines; the remainder goes to the savings
  contribution (or the first contribution, or a miscellaneous deposit).
- If the payer can't be found or the books refuse an entry, the alert is left for
  an official. Nothing is half-recorded: every reconciliation runs in one
  transaction.

## Group endpoints

All under `/v1/groups/{groupID}`. Officials only, except where noted.

| Method | Path | Purpose |
|---|---|---|
| GET | `/transaction-alerts?status=&direction=in\|out&source=&account_id=&from=&to=` | Paged alerts, newest first |
| GET | `/transaction-alerts/summary` | `{"in": {"count", "amount"}, "out": {...}}` of unreconciled alerts |
| GET | `/transaction-alerts/{id}` | The alert with its `deposits` and `withdrawals`, `reconciled_by`, and for an unreconciled credit a `suggestion`: `{"member_id", "lines"}` |
| POST | `/transaction-alerts/{id}/reconcile` | `{"lines": [...], "note"}`. Lines must add up to the alert exactly (400 `amount_mismatch`). 409 `already_reconciled` |
| POST | `/transaction-alerts/{id}/match` | `{"deposit_ids": [...]}` or `{"withdrawal_ids": [...]}`: link records already entered by hand. Same account, same total |
| POST | `/transaction-alerts/{id}/dismiss` | `{"status": "ignored" \| "reconciled", "note"}`. The note is required |
| POST | `/transaction-alerts/{id}/undo` | Voids the deposits and withdrawals the reconciliation wrote, unlinks matched ones, and reopens the alert. 409 `not_reconciled` |
| GET / PATCH | `/reconciliation-settings` | `{"auto_reconcile_payments": bool, "auto_reconcile_bank_alerts": bool}` |
| GET | `/pay/options` | Any member: `enabled`, the `account` money goes to, the caller's `phone`, what they `owe` (`kind`, ids, `name`, `owed`, `since`), the group's `contributions` and `currency` |
| POST | `/payments` | Any active member: `{"phone", "lines": [...]}` or `{"phone", "amount"}` (spread over what they owe). 201 with the payment. 503 `payments_off` when no gateway is set |
| GET | `/payments` | Paged. Members see their own; officials see everyone's |
| GET | `/payments/{id}` | One payment. Poll it after starting: `status` goes `sent` → `paid` or `failed`, with `message` and `receipt` |

A **line** is `{"kind", "amount", "description", ...ids}`:

| kind | ids | Money |
|---|---|---|
| `contribution` | `member_id`, `contribution_id` | in |
| `fine` | `member_id`, `fine_category_id`, or `contribution_id` for a late contribution fine | in |
| `misc` | `member_id` | in |
| `income` | `income_category_id`, optional `depositor_id` | in |
| `loan` | `loan_id` | in (a loan repayment) |
| `expense` | `expense_category_id` | out |
| `contribution_refund` | `member_id`, `contribution_id` | out |

For a member's own payment `member_id` is the caller and is left out.

## Platform admin

| Method | Path | Purpose |
|---|---|---|
| GET | `/v1/admin/transaction-alerts?unassigned=true&group_id=&bank_id=` | Every alert |
| GET | `/v1/admin/transaction-alerts/{id}` | One alert with its records |
| POST | `/v1/admin/transaction-alerts/{id}/assign` | `{"account_id"}`: give an unassigned alert to a group account (same number not required), then try automatic reconciliation |
| GET | `/v1/admin/payments?group_id=&status=` | Every payment request |
| GET | `/v1/admin/webhooks?source=` | The last webhook calls: source, IP, body (passwords removed), outcome |

## Bank panel

Bank staff see the groups that bank at their bank. Staff are rows in
`bank_users` (role `admin` or `staff`); platform admins can open every bank as
an admin. `GET /v1/me` lists the caller's `banks`. Anyone else gets 404.

| Method | Path | Purpose |
|---|---|---|
| GET | `/v1/bank/banks` | The banks the caller can open |
| GET | `/v1/bank/{bankID}` | The bank and the caller's `role` |
| GET | `/v1/bank/{bankID}/dashboard?from=&to=` | Per-branch `accounts`, `groups`, `new_groups`, `money_in`, `money_out`, `alerts`, and `totals` (default today) |
| GET | `/v1/bank/{bankID}/groups?search=&branch_id=` | Paged groups with an account at the bank |
| GET | `/v1/bank/{bankID}/groups/{groupID}` | The group and its accounts at the bank, with balances |
| GET | `/v1/bank/{bankID}/alerts?group_id=&status=` | Paged alerts on the bank's accounts |
| GET | `/v1/bank/{bankID}/alerts/{id}` | One alert |
| GET / POST / DELETE | `/v1/bank/{bankID}/users[/{userID}]` | Bank admins: list staff, add `{"phone", "role"}`, remove |

## Webhooks

Public, under `/v1/hooks`. Every call is logged in `webhook_events`. The
answers keep the legacy shapes, so the banks and Chamasoft need no change
beyond the address. `legion-api hook-urls` prints the addresses with their keys.

| Path | Who | Authentication | Legacy path |
|---|---|---|---|
| POST `/equity` | Equity Bank IPN | `username` and `password` in the body (`LEGION_EQUITY_USERNAME`/`PASSWORD`) or a source IP in `LEGION_EQUITY_IPS` | `/endpoint` |
| POST `/coop` | Co-op Bank IPN | HTTP basic auth or body credentials (`LEGION_COOP_*`), or `LEGION_COOP_IPS` | `/coop_bank/ipn` |
| POST `/chamasoft/payments/{reference}?key=` | Chamasoft, after an MPesa prompt | A key derived from the reference with `LEGION_HOOK_KEY`; it is in the callback URL the API sends | (per-payment callback) |
| POST `/chamasoft/direct?key=` | Chamasoft, for money paid straight to a group's paybill account | `key` from `hook-urls`, or `LEGION_CHAMASOFT_IPS` | `/transaction_alerts/reconcile_direct_online_banking_payment` |
| POST `/mpesa/validation?key=`, `/mpesa/confirmation?key=` | Safaricom C2B (Daraja), for later | `key` from `hook-urls` | — |

A feed with neither credentials nor IPs configured is switched off and answers
"Access Denied". Until the banks are pointed at the new addresses, the web
server can forward the legacy paths, e.g. in Caddy:

```
rewrite /endpoint /v1/hooks/equity
rewrite /coop_bank/ipn /v1/hooks/coop
rewrite /transaction_alerts/reconcile_direct_online_banking_payment /v1/hooks/chamasoft/direct?key=...
```

## Chamasoft payment API

Set `LEGION_CHAMASOFT_URL` and `LEGION_CHAMASOFT_AUTH` (the `user:password` for
the token endpoint), and `LEGION_CHAMASOFT_CERT` (Chamasoft's public key or
certificate, PEM) to encrypt each account's password as the `security_pass`.
The account password is the legacy `bank_accounts.account_password`, kept in
`group_accounts.settings.account_password` by the import.

The callback address is `LEGION_PUBLIC_URL` (default `LEGION_WEB_URL`), so it
must be reachable from the internet. Requests with no answer after 30 minutes
are marked failed.

For development, `LEGION_CHAMASOFT_FAKE=true` simulates the gateway: every
prompt is paid five seconds later.

## Mobile app

The legacy actions keep their request and response shapes (see
`internal/mobile/payments.go`):

- `transaction_alerts/get_group_unreconcilled_deposits`, `get_group_unreconcilled_withdrawals`
- `transaction_alerts/reconcile_deposits` (deposit_for_type 1 contribution, 2 fine,
  3 miscellaneous, 4 income, 5 loan repayment) and `reconcile_withdrawals`
  (withdrawal_for_type 1 expense, 7 contribution refund). Bank loans, transfers,
  stocks, assets and money market lines answer "not available in the app yet".
- `transaction_alerts/payment_request`, `deposits/make_new_group_payment`,
  `deposits/make_payment_to_group_bank_express`, `deposits/make_group_arrears_payment`

Unlike the legacy app, reconciliation and the alert lists are for officials only,
as on the web.

## Legacy import

- `online_payment_requests` → `payment_requests`, with the PHP-serialized item
  arrays turned into lines. Status 3 is `paid`; everything else `failed`.
- `transaction_alerts` keep their ids; deposits and withdrawals that name one in
  `refs.transaction_alert_id` are linked to it. Rows with no amount or date are
  skipped (the anonymized dump blanks every one, so it imports none).
- `user_bank_pairings` → `bank_users`; `bank_user_role_pairings` role 1 is admin.
