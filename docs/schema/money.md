# Money: accounts, deposits, withdrawals and the cash book

Milestone 3, part 1. Migration `db/migrations/00006_money.sql`.

## Tables

| New table | Legacy table(s) | Notes |
|---|---|---|
| `saccos`, `sacco_branches`, `mobile_money_providers` | same names | Reference lists, like `banks`. Ids kept |
| `group_accounts` | `bank_accounts`, `sacco_accounts`, `mobile_money_accounts`, `petty_cash_accounts` | One table with `kind`. The four legacy tables share an id space, so new ids are assigned and `(kind, legacy_id)` is unique. Signatory, alert and other legacy columns go to `settings` |
| `income_categories`, `expense_categories`, `fine_categories` | same names | Per group. Ids kept. `fine_categories.amount` is the default fine |
| `depositors` | `depositors` | Outside payers, for income |
| `deposits` | `deposits` | `kind` is what the money is for, `account_kind` where it went. Links to loans, stocks, assets and the like stay in `refs` until those milestones add tables |
| `withdrawals` | `withdrawals` | Imported now so the cash book is whole. Recording and approvals come in part 2 |
| `account_transfers` | `account_transfers` | |
| `account_entries` | `transaction_statements` | The cash book: one row per movement in (`direction` 1) or out (-1) of an account |

`ledger_entries` gains `deposit_id` (the deposit a payment came from) and
`fine_category_id` (legacy `statements.fine_category_id`).

## Balances

An account's balance is `initial_balance + sum(direction * amount)` over its live
`account_entries`. Nothing is stored. The legacy app kept a running
`current_balance` on each account and added `initial_balance` to it for display,
so the two agree wherever the legacy total is correct.

## Legacy type codes

The legacy type numbers encode both the purpose and the account kind, in blocks of
four: bank, sacco, mobile money, petty cash (see `Transactions.php`).

| Legacy | Codes | New |
|---|---|---|
| `deposits.type` | 1, 2, 3, 7 | `contribution` (bank, sacco, mobile, petty) |
| | 4, 5, 6, 8 | `fine` |
| | 9–12, 13–16, 17–20, 21–24, 25–28, 29–32, 33–36, 37–40, 41–44, 45–48, 49–52 | `misc`, `income`, `loan_repayment`, `bank_loan`, `stock_sale`, `money_market`, `asset_sale`, `funds_transfer`, `loan_processing_income`, `external_lending_processing_income`, `external_lending_repayment` |
| `withdrawals.type` | 1–4, 5–8, ... 37–40 | `expense`, `asset_purchase`, `loan_disbursement`, `stock_purchase`, `money_market_investment`, `contribution_refund`, `bank_loan_repayment`, `funds_transfer`, `external_loan_disbursement`, `dividend` |
| `transaction_statements.transaction_type` | 1–40 | money in, same codes as deposits |
| | 41–72 | money out, withdrawal codes 1–32 (plus 40) |
| | 73–76, 81–88 | money in: loan processing income, external lending income and repayments |
| | 77–80, 89–92 | money out: external lending, dividends |

Deposit methods: 1 MPesa, 2 cash, 3 cheque, 4 funds transfer, 5 Equitel, 6 standing
order. Withdrawal methods: 1 cash, 2 MPesa, 3 cheque, 4 account to account, 5 Equitel.

## Which account a legacy row used

A production export stores `account_id` as `bank-12`, `sacco-3`, `mobile-7` or
`petty-2`, and the import resolves that directly. The anonymized dump lost it: the
column was typed as an integer and every value is 0. For those rows the import uses
the group's only account of the right kind. When the group has several, it leaves
the account unknown instead of guessing. The row still has its `account_kind`, so
group totals stay right, and `/accounts` shows the unknown part as `unassigned`.

On the anonymized dump, 5,891 of 24,218 cash book rows get an account this way and
18,327 stay unassigned. Of the 31 accounts whose rows are all attributed and that
have a legacy `current_balance`, 30 match it to the shilling.

## Recording a deposit

`POST /v1/groups/{id}/deposits` writes three rows in one transaction:

1. `deposits`
2. `account_entries` (money in to the chosen account)
3. for member payments, `ledger_entries` (`contribution_payment`, `fine_payment` or
   `misc_payment`) with `deposit_id` set

Voiding sets `voided_at` on all three, so the account and the member's statement
both go back. Income has no member and writes no ledger row.
