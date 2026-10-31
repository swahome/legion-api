package billing

import (
	"math/big"
	"testing"
	"time"
)

func d(s string) time.Time {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestNext(t *testing.T) {
	cases := []struct {
		name, freq string
		rule       Rule
		anchor     string
		from, want string
	}{
		{"day of month, this month", "monthly", Rule{MonthDayMonthly: 10}, "", "2026-10-05", "2026-10-10"},
		{"day of month, on the day", "monthly", Rule{MonthDayMonthly: 10}, "", "2026-10-10", "2026-10-10"},
		{"day of month, next month", "monthly", Rule{MonthDayMonthly: 10}, "", "2026-10-11", "2026-11-10"},
		{"31st in a 30-day month", "monthly", Rule{MonthDayMonthly: 31}, "", "2026-11-02", "2026-11-30"},
		{"0 means last day", "monthly", Rule{}, "", "2026-02-02", "2026-02-28"},
		{"32 means last day", "monthly", Rule{MonthDayMonthly: 32}, "", "2028-02-02", "2028-02-29"},
		{"2nd Monday", "monthly", Rule{MonthDayMonthly: 2, WeekDayMonthly: 2}, "", "2018-09-01", "2018-09-10"},
		{"3rd Saturday", "monthly", Rule{MonthDayMonthly: 3, WeekDayMonthly: 7}, "", "2023-02-01", "2023-02-18"},
		{"1st Sunday, rolls over", "monthly", Rule{MonthDayMonthly: 1, WeekDayMonthly: 1}, "", "2023-05-08", "2023-06-04"},
		{"last Friday", "monthly", Rule{MonthDayMonthly: 32, WeekDayMonthly: 6}, "", "2026-10-01", "2026-10-30"},
		{"weekly Tuesday", "weekly", Rule{WeekDayWeekly: 3}, "", "2026-10-31", "2026-11-03"},
		{"weekly on the day", "weekly", Rule{WeekDayWeekly: 7}, "", "2026-10-31", "2026-10-31"},
		{"fortnightly from anchor", "fortnightly", Rule{}, "2026-10-01", "2026-10-02", "2026-10-15"},
		{"fortnightly far ahead", "fortnightly", Rule{}, "2026-10-01", "2026-12-01", "2026-12-10"},
		{"fortnightly anchor in future", "fortnightly", Rule{}, "2026-12-10", "2026-10-02", "2026-10-15"},
		{"daily", "daily", Rule{}, "", "2026-10-31", "2026-10-31"},
		{"quarterly from Feb", "quarterly", Rule{MonthDayMultiple: 15, StartMonthMultiple: 2}, "", "2026-05-16", "2026-08-15"},
		{"quarterly same month", "quarterly", Rule{MonthDayMultiple: 15, StartMonthMultiple: 2}, "", "2026-05-01", "2026-05-15"},
		{"bimonthly odd months", "bimonthly", Rule{MonthDayMultiple: 1, StartMonthMultiple: 1}, "", "2026-10-02", "2026-11-01"},
		{"half-yearly", "biannually", Rule{MonthDayMultiple: 30, StartMonthMultiple: 6}, "", "2026-07-01", "2026-12-30"},
		{"yearly next year", "annually", Rule{MonthDayMultiple: 10, StartMonthMultiple: 1}, "", "2026-01-11", "2027-01-10"},
		{"yearly 2nd Wednesday of March", "annually", Rule{MonthDayMultiple: 2, WeekDayMultiple: 4, StartMonthMultiple: 3}, "", "2026-01-01", "2026-03-11"},
		{"twice monthly, first", "twice_monthly", Rule{
			AfterFirstStartingDay: 1, AfterFirstOccurrence: 1, AfterFirstWeekday: 6,
			AfterSecondStartingDay: 15, AfterSecondOccurrence: 1, AfterSecondWeekday: 6,
		}, "", "2026-10-01", "2026-10-02"},
		{"twice monthly, second", "twice_monthly", Rule{
			AfterFirstStartingDay: 1, AfterFirstOccurrence: 1, AfterFirstWeekday: 6,
			AfterSecondStartingDay: 15, AfterSecondOccurrence: 1, AfterSecondWeekday: 6,
		}, "", "2026-10-03", "2026-10-16"},
		{"twice monthly, next month", "twice_monthly", Rule{
			AfterFirstStartingDay: 1, AfterFirstOccurrence: 1, AfterFirstWeekday: 6,
			AfterSecondStartingDay: 15, AfterSecondOccurrence: 1, AfterSecondWeekday: 6,
		}, "", "2026-10-17", "2026-11-06"},
		{"twice monthly, plain days", "twice_monthly", Rule{AfterFirstStartingDay: 10, AfterSecondStartingDay: 20}, "", "2026-10-11", "2026-10-20"},
	}
	for _, c := range cases {
		var anchor time.Time
		if c.anchor != "" {
			anchor = d(c.anchor)
		}
		got, err := Next(c.freq, c.rule, anchor, d(c.from))
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if !got.Equal(d(c.want)) {
			t.Errorf("%s: got %s, want %s", c.name, got.Format(time.DateOnly), c.want)
		}
	}
}

func TestRuleRoundTrip(t *testing.T) {
	r, err := ParseRule([]byte(`{"month_day_monthly":"2","week_day_monthly":"7","junk":"x","start_month_multiple":3}`))
	if err != nil {
		t.Fatal(err)
	}
	if r.MonthDayMonthly != 2 || r.WeekDayMonthly != 7 || r.StartMonthMultiple != 3 {
		t.Fatalf("parsed %+v", r)
	}
	back, _ := ParseRule(r.JSON())
	if back != r {
		t.Fatalf("round trip %+v != %+v", back, r)
	}
}

func TestFineDates(t *testing.T) {
	monthly := Rule{MonthDayMonthly: 5}
	cases := []struct {
		name        string
		rule        FineRule
		due, first  string
		after, next string
	}{
		{"once, 14 days after", FineRule{ChargeableOn: "14", Frequency: "0"}, "2026-10-05", "2026-10-19", "2026-10-19", "2026-11-19"},
		{"once, last day of month", FineRule{ChargeableOn: "last_day_of_the_month", Frequency: "0"}, "2026-10-05", "2026-10-31", "2026-10-31", "2026-11-30"},
		{"once, first day of next month", FineRule{ChargeableOn: "first_day_of_the_month", Frequency: "0"}, "2026-10-05", "2026-11-01", "2026-11-01", "2026-12-01"},
		{"daily", FineRule{ChargeableOn: "1", Frequency: "1"}, "2026-10-05", "2026-10-06", "2026-10-06", "2026-10-07"},
		{"weekly", FineRule{ChargeableOn: "1", Frequency: "2"}, "2026-10-05", "2026-10-06", "2026-10-06", "2026-10-13"},
		{"monthly", FineRule{ChargeableOn: "1", Frequency: "3"}, "2026-10-05", "2026-10-06", "2026-10-31", "2026-11-30"},
		{"once, 35 days (longer than the cycle)", FineRule{ChargeableOn: "35", Frequency: "0"}, "2026-10-05", "2026-11-09", "2026-11-09", "2026-12-10"},
	}
	for _, c := range cases {
		if got := c.rule.FirstFineDate(d(c.due)); !got.Equal(d(c.first)) {
			t.Errorf("%s: first %s, want %s", c.name, got.Format(time.DateOnly), c.first)
		}
		got, err := c.rule.NextFineDate(d(c.after), "monthly", monthly, time.Time{})
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
		} else if !got.Equal(d(c.next)) {
			t.Errorf("%s: next %s, want %s", c.name, got.Format(time.DateOnly), c.next)
		}
	}
}

func TestAmount(t *testing.T) {
	f := FineRule{Kind: "percentage", Rate: big.NewRat(25, 10)} // 2.5%
	if got := f.Amount(100_00); got != 2_50 {
		t.Errorf("2.5%% of 100.00 = %d", got)
	}
	if got := f.Amount(33); got != 1 { // 0.825 cents rounds to 1
		t.Errorf("2.5%% of 0.33 = %d", got)
	}
	if got := (FineRule{Kind: "fixed", FixedCents: 200_00}).Amount(0); got != 200_00 {
		t.Errorf("fixed = %d", got)
	}
	c, _ := ParseCents("1500.505")
	if c != 1500_51 || FormatCents(c) != "1500.51" || FormatCents(-5) != "-0.05" {
		t.Errorf("cents %d %s", c, FormatCents(c))
	}
}

func TestAssess(t *testing.T) {
	due := func(s string) *time.Time { v := d(s); return &v }
	entries := []Entry{
		{ID: 1, IsInvoice: true, Sign: 1, Cents: 1000_00, Date: d("2026-08-01"), Due: due("2026-08-05")},
		{ID: 2, IsInvoice: true, Sign: 1, Cents: 1000_00, Date: d("2026-09-01"), Due: due("2026-09-05")},
		{ID: 3, IsInvoice: true, Sign: 1, Cents: 1000_00, Date: d("2026-10-01"), Due: due("2026-10-05")},
		{ID: 4, IsInvoice: true, Sign: 1, Cents: 1000_00, Date: d("2026-10-29"), Due: due("2026-11-05")}, // not due yet
		{ID: 5, Sign: -1, Cents: 1500_00, Date: d("2026-09-10")},
		{ID: 6, Sign: -1, Cents: 5000_00, Date: d("2026-10-20")}, // after the fine date
	}
	on := d("2026-10-06")

	fixed := FineRule{Kind: "fixed", FixedCents: 100_00, Mode: ModeEachInvoice}
	got := fixed.Assess(on, d("2026-01-01"), 1000_00, entries, 0, nil)
	if len(got) != 2 || *got[0].Parent != 2 || *got[1].Parent != 3 {
		t.Fatalf("each invoice: %+v", got)
	}

	limited := fixed
	limited.Limit = 1
	got = limited.Assess(on, d("2026-01-01"), 1000_00, entries, 0, map[int64]PriorFines{2: {Count: 1, Cents: 100_00}})
	if len(got) != 1 || *got[0].Parent != 3 {
		t.Fatalf("limit: %+v", got)
	}

	// Invoices that became finable before the catch-up window are left
	// alone until they have a fine of the job's own.
	got = fixed.Assess(on, d("2026-10-01"), 1000_00, entries, 0, nil)
	if len(got) != 1 || *got[0].Parent != 3 {
		t.Fatalf("since: %+v", got)
	}
	got = fixed.Assess(on, d("2026-10-01"), 1000_00, entries, 0, map[int64]PriorFines{2: {Count: 1}})
	if len(got) != 2 {
		t.Fatalf("since with a prior fine: %+v", got)
	}
	// A rule charged 14 days after the due date doesn't touch invoice 3 yet.
	late := fixed
	late.ChargeableOn = "14"
	if got := late.Assess(on, d("2026-01-01"), 1000_00, entries, 0, nil); len(got) != 1 || *got[0].Parent != 2 {
		t.Fatalf("not finable yet: %+v", got)
	}

	pct := FineRule{Kind: "percentage", Rate: big.NewRat(10, 1), Mode: ModeEachInvoice, PercentageOf: "2"}
	got = pct.Assess(on, d("2026-01-01"), 1000_00, entries, 0, nil)
	if len(got) != 2 || got[0].Cents != 50_00 || got[1].Cents != 100_00 {
		t.Fatalf("percent of unpaid part: %+v", got)
	}

	bal := FineRule{Kind: "percentage", Rate: big.NewRat(10, 1), Mode: ModeBalance, PercentageOf: "3"}
	got = bal.Assess(on, d("2026-01-01"), 1000_00, entries, 200_00, nil)
	if len(got) != 1 || got[0].Parent != nil || got[0].Cents != 170_00 { // 10% of (1500 + 200)
		t.Fatalf("balance plus fines: %+v", got)
	}

	if got := fixed.Assess(d("2026-10-21"), d("2026-01-01"), 1000_00, entries, 0, nil); got != nil {
		t.Fatalf("paid up: %+v", got)
	}
}
