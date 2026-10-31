# Billing: contribution invoices and fines

Milestone 3, part 3. Migration `00008_billing.sql`, code in `internal/billing`.

## What runs

One job, `billing`, raises everything that falls due on a calendar day in the
groups' time zone (`LEGION_TIMEZONE`, default Africa/Nairobi):

1. **Contribution invoices.** For each schedule whose `invoice_date` has come, one
   `contribution_invoice` per active member who pays the contribution (all members,
   or the `contribution_members` list). The entry is dated the invoice date and due
   on the contribution date. A regular schedule then moves to its next cycle: the
   next contribution date from the day picker, and the invoice date
   `invoice_days_before` days earlier. A one-time schedule is invoiced once
   (`last_invoiced_on`).
2. **Late-payment fines.** For each active fine rule whose `next_fine_date` has
   come (regular contributions with `fines_enabled`), a `contribution_fine_invoice`
   per member who is behind. The rule then moves to its next fine date.

The scheduler runs it inside the API process every day at `LEGION_BILLING_AT`
(default `00:30`). If the process starts after that time and today hasn't been
billed, it runs at once. `legion-api billing-run [-date YYYY-MM-DD]` and
`POST /v1/admin/jobs/billing/run` run it by hand. Every run is a row in `job_runs`
with a summary.

**Safe to repeat.** Entries the job raises carry `generated_by` (`invoicing` or
`fines`). A unique index on member, contribution, type, date, fined invoice and
fine rule means a second run for the same day adds nothing, even if an entry was
voided since. A Postgres advisory lock lets only one process bill at a time, so
several servers can run the scheduler.

**Catching up.** If the job didn't run for a few days, the next run raises what
fell due in the last `CatchUpDays` (7) days. Anything older is not charged; the
schedule or rule moves on to its next date. That is also what happens to the stale
dates in an old dump.

## Fines

A fine rule (legacy `contribution_fine_settings`) keeps the legacy codes:

| Setting | Values |
|---|---|
| `chargeable_on` | `1`–`56` days after the contribution date, `last_day_of_the_month`, `first_day_of_the_month` (of the next month) |
| `frequency` | `0` once per contribution; `1`–`6` again every day, week, month, quarter, half year or year while unpaid |
| `mode` | `1` a fine for each unpaid invoice; `2` one fine on the outstanding balance |
| `percentage_of` | `1` contribution amount; `2` unpaid balance; `3` unpaid balance plus unpaid fines of the contribution |
| `fine_limit` | most fines one invoice can collect (mode 1); `0` no limit |

The first fine date is worked out from the schedule's contribution date when the
rule is saved, and again when the contribution date is changed by hand.

**Who is behind.** An invoice counts once it is overdue (due before the fine date).
Payments, refunds and transfers count up to and including the fine date. Payments
settle the oldest invoices first, so in mode 1 the "unpaid invoices" are the ones
payments don't cover. At most the 12 most recent are fined in one run, like the
legacy job.

Each mode 1 fine points at the invoice it fines (`parent_entry_id`); that is how
`fine_limit` is counted. Percentage fines round half up to the cent.

## Where it differs from the legacy jobs

The legacy app queued work in `contribution_invoicing_queue` and
`contribution_fine_invoicing_queue` and processed the queues on later cron calls.
This job writes the ledger directly, in one transaction per schedule or rule.

- **Day picker.** Same settings, computed from the schedule's own date rather than
  "now". A day that doesn't exist in a month (the 31st in November) falls on the
  month's last day instead of rolling into the next month. Fortnightly schedules
  step 14 days from their contribution date. Daily schedules invoice every day
  (the legacy code jumped two days).
- **Fine limit.** The legacy job capped how many invoices it fined per run at
  `fine_limit`. Here the limit is per invoice, as the setting's label says
  ("limit to N fines per unpaid contribution").
- **Old arrears at cutover.** The dump doesn't link legacy fines to the invoices
  they fined (`invoices.parent_id` is empty), so in mode 1 an invoice that has no
  fine from this job yet is only fined when its own first fine date is inside the
  catch-up window. Invoices that fell behind before cutover are not fined again.
  Mode 2 (balance) fines the whole balance on every fine date, as before.
- **Monthly and longer fine periods** keep to the month's last day (31 October + 1
  month = 30 November) and count from the fine date, not from the day the job ran.
- **Notifications.** The legacy job sent invoice and fine SMS and email. Sending
  arrives with the SMS work in milestone 5; the `sms_notifications` and
  `email_notifications` flags are kept and will be honoured then.

## Fines raised by hand

`POST /v1/groups/{id}/fines` charges one fine (a `fine_category`, amount and date)
to several members, as the legacy "fine members" screen did: one `fine_invoice`
per member, with `created_by`. Voiding sets `voided_at` and `voided_by`. Paying a
fine is a fine deposit.
