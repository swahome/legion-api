package mobile

import (
	"fmt"
	"math"
	"strconv"
	"time"
)

// Dates and amounts the way the legacy PHP helpers printed them.

// ordinal is PHP's "jS": 1st, 2nd, 3rd, 11th, 22nd.
func ordinal(d int) string {
	suffix := "th"
	if d%100 < 11 || d%100 > 13 {
		switch d % 10 {
		case 1:
			suffix = "st"
		case 2:
			suffix = "nd"
		case 3:
			suffix = "rd"
		}
	}
	return fmt.Sprintf("%d%s", d, suffix)
}

// mobileTime is timestamp_to_mobile_time: "Monday, 5th February 2024".
func mobileTime(t time.Time) string {
	return t.Format("Monday, ") + ordinal(t.Day()) + t.Format(" January 2006")
}

// mobileShortTime is timestamp_to_mobile_shorttime: "Mon, 5th Feb 2024".
func mobileShortTime(t time.Time) string {
	return t.Format("Mon, ") + ordinal(t.Day()) + t.Format(" Jan 2006")
}

// shortDate is timestamp_to_date(t, TRUE): "05-02-2024".
func shortDate(t time.Time) string { return t.Format("02-01-2006") }

// dateOrEpoch places a date at midnight in loc. A missing date is the Unix
// epoch, which is what PHP printed for an empty timestamp.
func dateOrEpoch(d *time.Time, loc *time.Location) time.Time {
	if d == nil {
		return time.Unix(0, 0).In(loc)
	}
	return time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, loc)
}

// elapsed is the legacy daysAgo: "3 days", "2 months", "0 days".
func elapsed(from, to time.Time) string {
	secs := math.Abs(math.Round(to.Sub(from).Seconds()))
	units := []struct {
		secs float64
		name string
	}{{365 * 86400, "year"}, {30 * 86400, "month"}, {86400, "day"}, {3600, "hour"}, {60, "minute"}, {1, "second"}}
	for _, u := range units {
		if n := secs / u.secs; n >= 1 {
			r := math.Round(n)
			if r > 1 {
				return fmt.Sprintf("%.0f %ss", r, u.name)
			}
			return fmt.Sprintf("%.0f %s", r, u.name)
		}
	}
	return "0 days"
}

// timeAgo is timestamp_to_time_elapsed: "5 minutes ago".
func timeAgo(t, now time.Time) string {
	if !now.After(t) {
		return "0 seconds"
	}
	return elapsed(t, now) + " ago"
}

// amount turns a numeric column read as text into a JSON number.
func amount(s string) float64 {
	f, _ := strconv.ParseFloat(s, 64)
	return f
}
