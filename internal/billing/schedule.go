// Package billing raises the invoices and fines that contribution settings
// call for: contribution invoices on each invoice date and late-payment fines
// on each fine date. It ports the legacy Contribution_invoices library
// (queue_* and process_*_queue cron jobs) without the intermediate queue
// tables; see docs/schema/billing.md for where it deliberately differs.
package billing

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// Rule is the day picker of a contribution schedule, in the legacy
// regular_contribution_settings encoding (kept verbatim in
// contribution_schedules.rule):
//
//	month_day_monthly / month_day_multiple: 1-31 = that day of the month,
//	    1-4 with a weekday = that occurrence of the weekday (2nd Tuesday),
//	    32 = the last day (or last weekday), 0 = the last day.
//	week_day_*: 0 = none, 1 = Sunday ... 7 = Saturday.
//	start_month_multiple: first month (1-12) of a bimonthly, quarterly,
//	    half-yearly or yearly cycle.
//	after_first_* / after_second_*: the two dates of a twice-monthly
//	    contribution, each "the Nth <weekday> after day D".
type Rule struct {
	MonthDayMonthly     int `json:"month_day_monthly"`
	WeekDayMonthly      int `json:"week_day_monthly"`
	WeekDayWeekly       int `json:"week_day_weekly"`
	WeekDayFortnight    int `json:"week_day_fortnight"`
	WeekNumberFortnight int `json:"week_number_fortnight"`
	MonthDayMultiple    int `json:"month_day_multiple"`
	WeekDayMultiple     int `json:"week_day_multiple"`
	StartMonthMultiple  int `json:"start_month_multiple"`

	AfterFirstStartingDay  int `json:"after_first_starting_day"`
	AfterFirstOccurrence   int `json:"after_first_contribution_day_option"`
	AfterFirstWeekday      int `json:"after_first_day_week_multiple"`
	AfterSecondStartingDay int `json:"after_second_starting_day"`
	AfterSecondOccurrence  int `json:"after_second_contribution_day_option"`
	AfterSecondWeekday     int `json:"after_second_day_week_multiple"`
}

// ParseRule reads the jsonb rule. Legacy values are strings ("5"); numbers
// are accepted too. Unknown keys are ignored.
func ParseRule(raw []byte) (Rule, error) {
	var m map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &m); err != nil {
			return Rule{}, fmt.Errorf("schedule rule: %w", err)
		}
	}
	get := func(k string) int {
		switch v := m[k].(type) {
		case string:
			n, _ := strconv.Atoi(v)
			return n
		case float64:
			return int(v)
		}
		return 0
	}
	return Rule{
		MonthDayMonthly:        get("month_day_monthly"),
		WeekDayMonthly:         get("week_day_monthly"),
		WeekDayWeekly:          get("week_day_weekly"),
		WeekDayFortnight:       get("week_day_fortnight"),
		WeekNumberFortnight:    get("week_number_fortnight"),
		MonthDayMultiple:       get("month_day_multiple"),
		WeekDayMultiple:        get("week_day_multiple"),
		StartMonthMultiple:     get("start_month_multiple"),
		AfterFirstStartingDay:  get("after_first_starting_day"),
		AfterFirstOccurrence:   get("after_first_contribution_day_option"),
		AfterFirstWeekday:      get("after_first_day_week_multiple"),
		AfterSecondStartingDay: get("after_second_starting_day"),
		AfterSecondOccurrence:  get("after_second_contribution_day_option"),
		AfterSecondWeekday:     get("after_second_day_week_multiple"),
	}, nil
}

// JSON encodes the rule in the legacy string form, leaving out zero values.
func (r Rule) JSON() []byte {
	out := map[string]string{}
	add := func(k string, v int) {
		if v != 0 {
			out[k] = strconv.Itoa(v)
		}
	}
	add("month_day_monthly", r.MonthDayMonthly)
	add("week_day_monthly", r.WeekDayMonthly)
	add("week_day_weekly", r.WeekDayWeekly)
	add("week_day_fortnight", r.WeekDayFortnight)
	add("week_number_fortnight", r.WeekNumberFortnight)
	add("month_day_multiple", r.MonthDayMultiple)
	add("week_day_multiple", r.WeekDayMultiple)
	add("start_month_multiple", r.StartMonthMultiple)
	add("after_first_starting_day", r.AfterFirstStartingDay)
	add("after_first_contribution_day_option", r.AfterFirstOccurrence)
	add("after_first_day_week_multiple", r.AfterFirstWeekday)
	add("after_second_starting_day", r.AfterSecondStartingDay)
	add("after_second_contribution_day_option", r.AfterSecondOccurrence)
	add("after_second_day_week_multiple", r.AfterSecondWeekday)
	b, _ := json.Marshal(out)
	return b
}

// Date is a calendar day at midnight UTC. Billing works on whole days in the
// group's local calendar, so time zones never enter the arithmetic.
func Date(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

// Day truncates t to its calendar day (in t's own location) as a Date.
func Day(t time.Time) time.Time { return Date(t.Year(), t.Month(), t.Day()) }

func daysIn(y int, m time.Month) int { return Date(y, m+1, 0).Day() }

// monthStart normalises (y, m) with m possibly outside 1-12.
func monthStart(y int, m time.Month) time.Time { return Date(y, m, 1) }

// legacyWeekday converts 1 = Sunday ... 7 = Saturday.
func legacyWeekday(n int) (time.Weekday, bool) {
	if n < 1 || n > 7 {
		return 0, false
	}
	return time.Weekday(n - 1), true
}

// dayInMonth resolves a month-day picker in the month that starts at ms.
func dayInMonth(ms time.Time, monthDay, weekday int) time.Time {
	y, m := ms.Year(), ms.Month()
	last := daysIn(y, m)
	wd, hasWD := legacyWeekday(weekday)
	switch {
	case hasWD && monthDay >= 1 && monthDay <= 4:
		d := ms
		for d.Weekday() != wd {
			d = d.AddDate(0, 0, 1)
		}
		return d.AddDate(0, 0, 7*(monthDay-1))
	case monthDay == 32 && hasWD:
		d := Date(y, m, last)
		for d.Weekday() != wd {
			d = d.AddDate(0, 0, -1)
		}
		return d
	case monthDay <= 0 || monthDay >= last:
		// 0 and 32 mean the last day; 29-31 fall back to it in short months
		// (legacy mktime rolled them into the next month instead).
		return Date(y, m, last)
	default:
		return Date(y, m, monthDay)
	}
}

// afterDay is "the nth <weekday> after day `start`" in the month at ms.
// Without a weekday it is day `start` itself.
func afterDay(ms time.Time, start, nth, weekday int) (time.Time, bool) {
	y, m := ms.Year(), ms.Month()
	if start < 1 {
		start = 1
	}
	if start > daysIn(y, m) {
		return time.Time{}, false
	}
	d := Date(y, m, start)
	wd, ok := legacyWeekday(weekday)
	if !ok || nth < 1 {
		return d, true
	}
	for d.Weekday() != wd {
		d = d.AddDate(0, 0, 1)
	}
	d = d.AddDate(0, 0, 7*(nth-1))
	if d.Month() != m {
		return time.Time{}, false
	}
	return d, true
}

// Next returns the first contribution date on or after `from` for a
// schedule. `anchor` is the schedule's current contribution date; only
// fortnightly schedules need it, since a fortnight has no calendar anchor.
func Next(frequency string, r Rule, anchor, from time.Time) (time.Time, error) {
	from = Day(from)
	switch frequency {
	case "daily":
		return from, nil
	case "weekly":
		wd, ok := legacyWeekday(r.WeekDayWeekly)
		if !ok {
			if anchor.IsZero() {
				return from, nil
			}
			wd = anchor.Weekday()
		}
		d := from
		for d.Weekday() != wd {
			d = d.AddDate(0, 0, 1)
		}
		return d, nil
	case "fortnightly":
		if anchor.IsZero() {
			return time.Time{}, fmt.Errorf("fortnightly schedule needs a contribution date")
		}
		anchor = Day(anchor)
		if !anchor.Before(from) {
			// Step back so a moved-forward anchor still lands on its cycle.
			for !anchor.Before(from) {
				anchor = anchor.AddDate(0, 0, -14)
			}
		}
		days := int(from.Sub(anchor).Hours() / 24)
		return anchor.AddDate(0, 0, ((days+13)/14)*14), nil
	case "monthly":
		for i := 0; i < 2; i++ {
			ms := monthStart(from.Year(), from.Month()+time.Month(i))
			if d := dayInMonth(ms, r.MonthDayMonthly, r.WeekDayMonthly); !d.Before(from) {
				return d, nil
			}
		}
	case "bimonthly", "quarterly", "biannually", "annually":
		step := map[string]int{"bimonthly": 2, "quarterly": 3, "biannually": 6, "annually": 12}[frequency]
		start := r.StartMonthMultiple
		if start < 1 || start > 12 {
			start = 1
		}
		for i := 0; i <= 12+step; i++ {
			ms := monthStart(from.Year(), from.Month()+time.Month(i))
			if ((int(ms.Month())-start)%step+step)%step != 0 {
				continue
			}
			if d := dayInMonth(ms, r.MonthDayMultiple, r.WeekDayMultiple); !d.Before(from) {
				return d, nil
			}
		}
	case "twice_monthly":
		for i := 0; i < 3; i++ {
			ms := monthStart(from.Year(), from.Month()+time.Month(i))
			var best time.Time
			for _, c := range [][3]int{
				{r.AfterFirstStartingDay, r.AfterFirstOccurrence, r.AfterFirstWeekday},
				{r.AfterSecondStartingDay, r.AfterSecondOccurrence, r.AfterSecondWeekday},
			} {
				if c[0] == 0 && c[2] == 0 {
					continue
				}
				if d, ok := afterDay(ms, c[0], c[1], c[2]); ok && !d.Before(from) && (best.IsZero() || d.Before(best)) {
					best = d
				}
			}
			if !best.IsZero() {
				return best, nil
			}
		}
		return time.Time{}, fmt.Errorf("twice-monthly schedule has no usable dates")
	default:
		return time.Time{}, fmt.Errorf("unknown frequency %q", frequency)
	}
	return time.Time{}, fmt.Errorf("no %s date found after %s", frequency, from.Format(time.DateOnly))
}
