package billing

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/swahome/legion-api/internal/store"
)

// Clock is a daily time of day such as 00:30.
type Clock struct{ Hour, Minute int }

// ParseClock reads "HH:MM".
func ParseClock(s string) (Clock, error) {
	h, m, ok := strings.Cut(strings.TrimSpace(s), ":")
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if !ok || err1 != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return Clock{}, fmt.Errorf("want HH:MM, got %q", s)
	}
	return Clock{hh, mm}, nil
}

// LocalToday is the calendar day it is now in loc, as a Date.
func LocalToday(now time.Time, loc *time.Location) time.Time { return Day(now.In(loc)) }

// nextRun is the first moment at the clock time strictly after now.
func (c Clock) nextRun(now time.Time, loc *time.Location) time.Time {
	l := now.In(loc)
	t := time.Date(l.Year(), l.Month(), l.Day(), c.Hour, c.Minute, 0, 0, loc)
	if !t.After(now) {
		t = t.AddDate(0, 0, 1)
	}
	return t
}

// Schedule runs the job once a day at `at` in loc until ctx ends. When the
// process starts after today's run time and today hasn't been billed yet,
// it runs straight away, so a restart never loses a day. Several servers
// can run the scheduler: the advisory lock lets one bill at a time, and a
// second run for the same day adds nothing.
func (e *Engine) Schedule(ctx context.Context, at Clock, loc *time.Location) {
	if e.dueNow(ctx, at, loc) {
		e.scheduledRun(ctx, loc)
	}
	for {
		wait := time.Until(at.nextRun(time.Now(), loc))
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
			e.scheduledRun(ctx, loc)
		}
	}
}

func (e *Engine) dueNow(ctx context.Context, at Clock, loc *time.Location) bool {
	now := time.Now().In(loc)
	if now.Hour()*60+now.Minute() < at.Hour*60+at.Minute {
		return false
	}
	last, err := store.New(e.pool).LastSucceededRun(ctx, JobName)
	if errors.Is(err, pgx.ErrNoRows) {
		return true
	}
	if err != nil {
		e.log.Error("billing: last run", "err", err)
		return false
	}
	return Day(last).Before(LocalToday(now, loc))
}

func (e *Engine) scheduledRun(ctx context.Context, loc *time.Location) {
	_, err := e.Run(ctx, RunOptions{Today: LocalToday(time.Now(), loc), Trigger: "schedule"})
	switch {
	case errors.Is(err, ErrLocked):
		e.log.Info("billing: another process is running it")
	case err != nil && ctx.Err() == nil:
		e.log.Error("billing run failed", "err", err)
	}
}
