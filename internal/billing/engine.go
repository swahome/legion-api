package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/swahome/legion-api/internal/store"
)

// JobName is the job_runs.job of the nightly run.
const JobName = "billing"

// CatchUpDays is how far back a run still raises invoices and fines that
// were due while the job didn't run (the server was down, say). Anything
// older is not charged: the schedule just moves on to its next date, the
// way the legacy update_next_invoice_dates job did every night.
const CatchUpDays = 7

// lockKey is the Postgres advisory lock that keeps two processes from
// billing at the same time ("legion-billing" folded into an int64).
const lockKey int64 = 0x6c6567696f6e6231

// ErrLocked means another process is running the job.
var ErrLocked = errors.New("billing is already running in another process")

// Summary is what one run did. It is stored in job_runs.summary.
type Summary struct {
	Date              string   `json:"date"`
	SchedulesInvoiced int      `json:"schedules_invoiced"`
	InvoicesRaised    int64    `json:"invoices_raised"`
	SchedulesSkipped  int      `json:"schedules_skipped"` // too far behind, moved on
	FineRulesRun      int      `json:"fine_rules_run"`
	FinesRaised       int64    `json:"fines_raised"`
	FineRulesSkipped  int      `json:"fine_rules_skipped"` // too far behind, moved on
	Problems          []string `json:"problems,omitempty"`
}

func (s *Summary) problem(format string, args ...any) {
	if len(s.Problems) < 50 {
		s.Problems = append(s.Problems, fmt.Sprintf(format, args...))
	}
}

// Engine runs billing against the database.
type Engine struct {
	pool *pgxpool.Pool
	log  *slog.Logger
}

func New(pool *pgxpool.Pool, log *slog.Logger) *Engine {
	if log == nil {
		log = slog.Default()
	}
	return &Engine{pool: pool, log: log}
}

// RunOptions say who started a run and for which day.
type RunOptions struct {
	Today     time.Time // the group calendar day being billed
	Trigger   string    // schedule | manual | cli
	StartedBy *int64
}

// Run bills everything due on or before opts.Today. It holds an advisory
// lock for the whole run, records itself in job_runs, and is safe to repeat:
// entries the job already raised for a date are not raised again.
func (e *Engine) Run(ctx context.Context, opts RunOptions) (Summary, error) {
	today := Day(opts.Today)
	sum := Summary{Date: today.Format(time.DateOnly)}

	conn, err := e.pool.Acquire(ctx)
	if err != nil {
		return sum, err
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", lockKey).Scan(&locked); err != nil {
		return sum, err
	}
	if !locked {
		return sum, ErrLocked
	}
	defer func() {
		// Unlock on a fresh context so a cancelled run still lets go.
		if _, err := conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", lockKey); err != nil {
			e.log.Error("billing unlock", "err", err)
		}
	}()

	q := store.New(e.pool)
	_ = q.FailStaleJobRuns(ctx)
	runID, err := q.StartJobRun(ctx, store.StartJobRunParams{Job: JobName, RunDate: today, Trigger: opts.Trigger, StartedBy: opts.StartedBy})
	if err != nil {
		return sum, err
	}
	runErr := e.invoice(ctx, today, &sum)
	if runErr == nil {
		runErr = e.fine(ctx, today, &sum)
	}
	status, msg := "succeeded", ""
	if runErr != nil {
		status, msg = "failed", runErr.Error()
	}
	js, _ := json.Marshal(sum)
	if err := q.FinishJobRun(context.WithoutCancel(ctx), store.FinishJobRunParams{ID: runID, Status: status, Summary: js, Error: msg}); err != nil && runErr == nil {
		runErr = err
	}
	e.log.Info("billing run", "date", sum.Date, "status", status, "invoices", sum.InvoicesRaised, "fines", sum.FinesRaised, "problems", len(sum.Problems))
	return sum, runErr
}

func (e *Engine) tx(ctx context.Context, fn func(q *store.Queries) error) error {
	return pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error { return fn(store.New(tx)) })
}

func freq(f store.NullContributionFrequency) string {
	if !f.Valid {
		return ""
	}
	return string(f.ContributionFrequency)
}

// invoice raises contribution invoices for every schedule that is due.
func (e *Engine) invoice(ctx context.Context, today time.Time, sum *Summary) error {
	oldest := today.AddDate(0, 0, -CatchUpDays)
	rows, err := store.New(e.pool).DueSchedules(ctx, store.DueSchedulesParams{Today: today, Oldest: oldest})
	if err != nil {
		return err
	}
	for _, s := range rows {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := e.invoiceSchedule(ctx, s, today, oldest, sum); err != nil {
			sum.problem("contribution %d (%s): %v", s.ContributionID, s.Name, err)
		}
	}
	return nil
}

func (e *Engine) invoiceSchedule(ctx context.Context, s store.DueSchedulesRow, today, oldest time.Time, sum *Summary) error {
	invoiceDate, due := Day(*s.InvoiceDate), Day(*s.ContributionDate)
	if s.Kind == store.ContributionKindOneTime {
		return e.tx(ctx, func(q *store.Queries) error {
			n, err := q.InvoiceContributionMembers(ctx, store.InvoiceContributionMembersParams{
				InvoiceDate: invoiceDate, DueDate: due, ContributionID: s.ContributionID,
			})
			if err != nil {
				return err
			}
			sum.SchedulesInvoiced++
			sum.InvoicesRaised += n
			return q.AdvanceSchedule(ctx, store.AdvanceScheduleParams{
				InvoiceDate: &invoiceDate, ContributionDate: &due, LastInvoicedOn: &invoiceDate, ContributionID: s.ContributionID,
			})
		})
	}

	frequency := freq(s.Frequency)
	rule, err := ParseRule(s.Rule)
	if err != nil {
		return err
	}
	daysBefore := 0
	if s.InvoiceDaysBefore != nil && *s.InvoiceDaysBefore > 0 {
		daysBefore = int(*s.InvoiceDaysBefore)
	}
	nextCycle := func(c time.Time) (time.Time, time.Time, error) {
		n, err := Next(frequency, rule, c, c.AddDate(0, 0, 1))
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
		return n.AddDate(0, 0, -daysBefore), n, nil
	}

	if invoiceDate.Before(oldest) {
		// Too far behind: skip to the first cycle whose invoice date is
		// inside the catch-up window and bill from there.
		c, err := Next(frequency, rule, due, oldest)
		if err != nil {
			return err
		}
		inv := c.AddDate(0, 0, -daysBefore)
		for inv.Before(oldest) {
			if inv, c, err = nextCycle(c); err != nil {
				return err
			}
		}
		sum.SchedulesSkipped++
		invoiceDate, due = inv, c
		if invoiceDate.After(today) {
			return store.New(e.pool).AdvanceSchedule(ctx, store.AdvanceScheduleParams{
				InvoiceDate: &invoiceDate, ContributionDate: &due, ContributionID: s.ContributionID,
			})
		}
	}

	return e.tx(ctx, func(q *store.Queries) error {
		var last *time.Time
		for i := 0; !invoiceDate.After(today); i++ {
			if i > 400 {
				return fmt.Errorf("schedule does not advance past %s", invoiceDate.Format(time.DateOnly))
			}
			n, err := q.InvoiceContributionMembers(ctx, store.InvoiceContributionMembersParams{
				InvoiceDate: invoiceDate, DueDate: due, ContributionID: s.ContributionID,
			})
			if err != nil {
				return err
			}
			sum.InvoicesRaised += n
			d := invoiceDate
			last = &d
			if invoiceDate, due, err = nextCycle(due); err != nil {
				return err
			}
		}
		sum.SchedulesInvoiced++
		return q.AdvanceSchedule(ctx, store.AdvanceScheduleParams{
			InvoiceDate: &invoiceDate, ContributionDate: &due, LastInvoicedOn: last, ContributionID: s.ContributionID,
		})
	})
}

// fine raises late-payment fines for every fine rule that is due.
func (e *Engine) fine(ctx context.Context, today time.Time, sum *Summary) error {
	rows, err := store.New(e.pool).DueFineRules(ctx, today)
	if err != nil {
		return err
	}
	oldest := today.AddDate(0, 0, -CatchUpDays)
	for _, r := range rows {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := e.fineRule(ctx, r, today, oldest, sum); err != nil {
			sum.problem("fine rule %d on contribution %d (%s): %v", r.ID, r.ContributionID, r.Name, err)
		}
	}
	return nil
}

// RuleFromRow builds a FineRule from its stored columns.
func RuleFromRow(kind store.FineKind, fixed, rate string, percentageOf, chargeableOn string, mode, frequency *string, limit *int32) (FineRule, error) {
	f := FineRule{Kind: string(kind), ChargeableOn: chargeableOn, Mode: ModeBalance}
	if percentageOf != "" {
		f.PercentageOf = percentageOf
	} else {
		f.PercentageOf = "2"
	}
	if mode != nil && *mode != "" {
		f.Mode = *mode
	}
	if frequency != nil {
		f.Frequency = *frequency
	}
	if limit != nil {
		f.Limit = int(*limit)
	}
	var err error
	if f.Kind == "fixed" {
		f.FixedCents, err = ParseCents(fixed)
	} else {
		f.Rate, err = ParseRate(rate)
	}
	return f, err
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (e *Engine) fineRule(ctx context.Context, r store.DueFineRulesRow, today, oldest time.Time, sum *Summary) error {
	rule, err := RuleFromRow(r.Kind, r.FixedAmount, r.PercentageRate, deref(r.PercentageOf), r.ChargeableOn, r.Mode, r.Frequency, r.FineLimit)
	if err != nil {
		return err
	}
	sched, err := ParseRule(r.Rule)
	if err != nil {
		return err
	}
	frequency := freq(r.ScheduleFrequency)
	var anchor time.Time
	if r.ContributionDate != nil {
		anchor = *r.ContributionDate
	}
	contributionCents, err := ParseCents(r.ContributionAmount)
	if err != nil {
		return err
	}
	fineDate := Day(*r.NextFineDate)
	sum.FineRulesRun++

	if fineDate.Before(oldest) {
		// Too far behind: fines that old are not charged; move to the first
		// fine date inside the catch-up window.
		sum.FineRulesSkipped++
		for i := 0; fineDate.Before(oldest); i++ {
			if i > 20000 {
				return fmt.Errorf("fine date does not advance past %s", fineDate.Format(time.DateOnly))
			}
			if fineDate, err = rule.NextFineDate(fineDate, frequency, sched, anchor); err != nil {
				return err
			}
		}
	}
	return e.tx(ctx, func(q *store.Queries) error {
		for i := 0; !fineDate.After(today); i++ {
			if i > 400 {
				return fmt.Errorf("fine date does not advance past %s", fineDate.Format(time.DateOnly))
			}
			n, err := e.chargeFines(ctx, q, r, rule, contributionCents, fineDate, oldest)
			if err != nil {
				return err
			}
			sum.FinesRaised += n
			if fineDate, err = rule.NextFineDate(fineDate, frequency, sched, anchor); err != nil {
				return err
			}
		}
		return q.SetNextFineDate(ctx, store.SetNextFineDateParams{ID: r.ID, NextFineDate: &fineDate})
	})
}

func (e *Engine) chargeFines(ctx context.Context, q *store.Queries, r store.DueFineRulesRow, rule FineRule, contributionCents int64, on, since time.Time) (int64, error) {
	members, err := q.FinableMembers(ctx, r.ContributionID)
	if err != nil || len(members) == 0 {
		return 0, err
	}
	cid := r.ContributionID
	rows, err := q.ContributionEntriesForFines(ctx, store.ContributionEntriesForFinesParams{
		ContributionID: &cid, MembershipIds: members, OnDate: on,
	})
	if err != nil {
		return 0, err
	}
	entries := map[int64][]Entry{}
	for _, row := range rows {
		c, err := ParseCents(row.Amount)
		if err != nil {
			return 0, err
		}
		entries[*row.MembershipID] = append(entries[*row.MembershipID], Entry{
			ID: row.ID, IsInvoice: row.IsInvoice, Sign: int(row.Sign), Cents: c, Date: row.EntryDate, Due: row.DueDate,
		})
	}
	fineBal := map[int64]int64{}
	if rule.PercentageOf == "3" {
		bals, err := q.ContributionFineBalances(ctx, store.ContributionFineBalancesParams{
			ContributionID: &cid, MembershipIds: members, OnDate: on,
		})
		if err != nil {
			return 0, err
		}
		for _, b := range bals {
			fineBal[b.MembershipID], _ = ParseCents(b.Balance)
		}
	}
	prior := map[int64]PriorFines{}
	if rule.Mode == ModeEachInvoice {
		ps, err := q.PriorInvoiceFines(ctx, &cid)
		if err != nil {
			return 0, err
		}
		for _, p := range ps {
			c, _ := ParseCents(p.Amount)
			prior[p.ParentEntryID] = PriorFines{Count: int(p.Fines), Cents: c}
		}
	}

	var raised int64
	ruleID := r.ID
	for _, m := range members {
		es := entries[m]
		if len(es) == 0 {
			continue
		}
		for _, f := range rule.Assess(on, since, contributionCents, es, fineBal[m], prior) {
			mid := m
			n, err := q.InsertContributionFine(ctx, store.InsertContributionFineParams{
				GroupID: r.GroupID, MembershipID: &mid, ContributionID: &cid, Amount: FormatCents(f.Cents),
				FineDate: on, Description: "Late payment fine: " + r.Name, ParentEntryID: f.Parent, FineRuleID: &ruleID,
			})
			if err != nil {
				return raised, err
			}
			raised += n
		}
	}
	return raised, nil
}
