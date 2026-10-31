package billing

import (
	"sort"
	"time"
)

// Entry is one live contribution-bucket ledger row of a member for the
// contribution being fined.
type Entry struct {
	ID        int64
	IsInvoice bool // contribution_invoice
	Sign      int  // +1 raises what the member owes, -1 lowers it
	Cents     int64
	Date      time.Time
	Due       *time.Time
}

// PriorFines describes the fines one invoice has already collected.
type PriorFines struct {
	Count int
	Cents int64
}

// Fine is one fine to charge. Parent is the invoice it fines (mode 1).
type Fine struct {
	Parent *int64
	Cents  int64
}

// maxInvoicesPerRun mirrors the legacy cap of 12 invoices fined per member
// in one run (the most recent ones).
const maxInvoicesPerRun = 12

// Assess works out the fines a member owes on fine date `on`.
//
// An invoice counts once it is overdue: its due date is before `on`.
// Payments, refunds and transfers count from their entry date up to and
// including `on`. Payments settle the oldest overdue invoices first, so
// "unpaid invoices" are the ones the member's payments don't cover.
// fineBalance is the member's unpaid fines on this contribution, used by
// percentage_of "3".
//
// In mode 1 an invoice that has no fines yet is fined only when its own
// first fine date is on or after `since` (the start of the catch-up
// window). Older unpaid invoices were the legacy system's to fine, and the
// dump doesn't say which fines belong to which invoice, so charging them
// again would double-fine members at cutover.
func (f FineRule) Assess(on, since time.Time, contributionCents int64, entries []Entry, fineBalance int64, prior map[int64]PriorFines) []Fine {
	on = Day(on)
	var overdue []Entry
	var credit int64 // payments minus refunds and transfers out
	for _, e := range entries {
		if e.IsInvoice {
			due := e.Date
			if e.Due != nil {
				due = Day(*e.Due)
			}
			if due.Before(on) {
				e.Due = &due
				overdue = append(overdue, e)
			}
			continue
		}
		if Day(e.Date).After(on) {
			continue
		}
		credit -= int64(e.Sign) * e.Cents
	}
	var owed int64
	for _, e := range overdue {
		owed += e.Cents
	}
	balance := owed - credit
	if balance <= 0 {
		return nil
	}
	if fineBalance < 0 {
		fineBalance = 0
	}

	if f.Mode != ModeEachInvoice {
		base := balance
		switch f.PercentageOf {
		case "1":
			base = contributionCents
		case "3":
			base = balance + fineBalance
		}
		if c := f.Amount(base); c > 0 {
			return []Fine{{Cents: c}}
		}
		return nil
	}

	sort.SliceStable(overdue, func(i, j int) bool {
		if !overdue[i].Due.Equal(*overdue[j].Due) {
			return overdue[i].Due.Before(*overdue[j].Due)
		}
		return overdue[i].ID < overdue[j].ID
	})
	type unpaid struct {
		e    Entry
		left int64
	}
	var open []unpaid
	avail := credit
	for _, e := range overdue {
		left := e.Cents
		if avail > 0 {
			use := min(avail, left)
			left -= use
			avail -= use
		}
		if left > 0 {
			open = append(open, unpaid{e, left})
		}
	}
	if len(open) > maxInvoicesPerRun {
		open = open[len(open)-maxInvoicesPerRun:]
	}
	var out []Fine
	for _, u := range open {
		p := prior[u.e.ID]
		if f.Limit > 0 && p.Count >= f.Limit {
			continue
		}
		if first := f.FirstFineDate(*u.e.Due); first.After(on) || (p.Count == 0 && first.Before(Day(since))) {
			continue
		}
		base := u.left
		switch f.PercentageOf {
		case "1":
			base = u.e.Cents
		case "3":
			base = u.left + min(p.Cents, fineBalance)
		}
		if c := f.Amount(base); c > 0 {
			id := u.e.ID
			out = append(out, Fine{Parent: &id, Cents: c})
		}
	}
	return out
}
