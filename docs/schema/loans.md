# Loans

Milestone 4, part 1. Migration `00010_loans.sql`, schedule maths in
`internal/loans`, writes in `internal/books/loans.go`, the view in
`internal/books/loan_view.go`, import in `internal/legacy/loans.go`.

## Tables

| Table | Legacy | What it holds |
|---|---|---|
| `loan_types` | `loan_types` | The terms a group offers: interest method and rate, term limits, amount limits, guarantor rule, late fine, outstanding-balance fine, processing fee |
| `loans` | `loans` | One loan to one member. The terms are copied from the type when the loan is recorded, so editing a type never changes an existing loan |
| `loan_charges` | `loan_invoices` | What the member owes: one `installment` per scheduled payment, plus `late_fine`, `balance_fine` and `transfer_out` rows |
| `loan_repayments` | `loan_repayments` | Money paid towards the loan. Each one points at the `loan_repayment` deposit that put the money in an account |
| `loan_guarantors` | `loan_guarantors` | Members standing behind the loan, optionally with an amount |

`deposits.loan_id` and `withdrawals.loan_id` tie the loan's money to the cash
book: the disbursement is a `loan_disbursement` withdrawal, the processing fee a
`loan_processing_income` deposit, each repayment a `loan_repayment` deposit.

**No stored balance.** A loan's balance is its live charges less its live
repayments. Repayments settle charges oldest first by due date; with
`fine_deferment` installments are settled before fines. A charge is `paid`,
`partly_paid`, `overdue` (due date passed, not paid) or `due`. A loan is
`voided`, `paid` (balance at or below zero), `bad_debt`, `arrears` (anything
overdue) or `active`.

## Legacy codes

| Field | Legacy value → new |
|---|---|
| `interest_type` | 1 `fixed_balance`, 2 `reducing_balance`, 3 `custom` (imported, not offered for new loans), 4 `fixed_amount` |
| `loan_interest_rate_per` | 1 `day`, 2 `week`, 3 `month`, 4 `year`, 5 `term` |
| `loan_repayment_period_type` | 1 `daily`, 2 `weekly`, 3 or NULL `monthly` |
| `loan_invoices.type` | 1 `installment`, 2 `late_fine`, 3 `balance_fine`, 5 `transfer_out` (4 was a payment line and is not a charge) |
| `loan_amount_type` | 1 `range`, 2 `savings` (savings × `loan_times_number`) |
| `loan_guarantors_type` | 1 `always`, 2 `over_max` |

Fine settings become a JSON rule: `kind` fixed, percentage or one-off;
`rate_on` installment_balance, loan_amount, unpaid or installment_interest;
`frequency` once, daily, weekly, monthly, yearly; `scope` installment or loan.

## Schedule

Matches legacy `loan_invoices` generation:

- Installment *i* falls due `grace_periods − 1 + i` periods after disbursement,
  or from `first_due_on` when given. Month arithmetic overflows like PHP
  `strtotime` (31 January + 1 month = 2 or 3 March).
- The invoice date is 7 days before the due date; fines start the day after it.
- Rates convert to the repayment period: a month is 30 days or 4 weeks, a year
  365 days, 52 weeks or 12 months; a `term` rate is spread over the installments.
- Amounts are floored to the cent. One difference from legacy, on purpose: the
  last installment takes the rounding remainder so principal adds up exactly.

Checked against the dump: 470 of 553 standard loans reproduce to the cent. The
others differ because legacy rewrote reducing-balance invoices after each
repayment, or the loan was edited after it was made. Imported loans keep their
legacy invoices as they are; only new loans use the engine.

## Import

Loan types, loans, invoices, guarantors and repayments come across with their
ids. A repayment keeps its deposit link only when the deposit was imported into
the same group. `import-legacy` reconciles each live loan's balance (charges
less repayments) against the legacy invoices less repayments and reports any
difference; on the current dump there are none.
