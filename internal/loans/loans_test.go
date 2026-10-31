package loans

import (
	"testing"
	"time"
)

func day(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestFixedBalanceMatchesLegacy(t *testing.T) {
	// Legacy loan: 10,000 at 10% a month flat over 3 months, disbursed
	// 2022-07-16 with a one-month grace: 4,333.33 a month (floor of each part).
	s, err := Terms{Amount: 1000000, InterestType: FixedBalance, Rate: 10, RatePer: PerMonth, Frequency: Monthly,
		Installments: 3, GracePeriods: 1, DisbursementOn: day("2022-07-16")}.Schedule()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"2022-08-16", "2022-09-16", "2022-10-16"}
	var principal int64
	for i, inst := range s {
		if inst.DueDate.Format("2006-01-02") != want[i] {
			t.Errorf("installment %d due %s", i+1, inst.DueDate.Format("2006-01-02"))
		}
		if inst.Interest != 100000 {
			t.Errorf("installment %d interest %d", i+1, inst.Interest)
		}
		principal += inst.Principal
	}
	if principal != 1000000 || s[0].Principal != 333333 || s[2].Principal != 333334 {
		t.Errorf("principal %d, %v", principal, s)
	}
	if s[0].InvoiceDate.Format("2006-01-02") != "2022-08-09" || s[0].FineDate.Format("2006-01-02") != "2022-08-17" {
		t.Errorf("invoice/fine dates: %v", s[0])
	}
}

func TestReducingBalance(t *testing.T) {
	// 100,000 at 12% a year over 12 months: 8,884.87 a month.
	s, err := Terms{Amount: 10000000, InterestType: ReducingBalance, Rate: 12, RatePer: PerYear, Frequency: Monthly,
		Installments: 12, GracePeriods: 1, DisbursementOn: day("2024-01-31")}.Schedule()
	if err != nil {
		t.Fatal(err)
	}
	if s[0].Amount() != 888487 || s[0].Interest != 100000 {
		t.Errorf("first: %+v", s[0])
	}
	var principal int64
	for _, i := range s {
		principal += i.Principal
	}
	if principal != 10000000 {
		t.Errorf("principal adds to %d", principal)
	}
	// 31 January + 1 month overflows to 2 March, as legacy strtotime did.
	if s[0].DueDate.Format("2006-01-02") != "2024-03-02" {
		t.Errorf("due %s", s[0].DueDate.Format("2006-01-02"))
	}

	ep, _ := Terms{Amount: 1200000, InterestType: ReducingBalance, Reducing: EqualPrincipal, Rate: 1, RatePer: PerMonth,
		Frequency: Monthly, Installments: 3, DisbursementOn: day("2024-01-10")}.Schedule()
	if ep[0].Principal != 400000 || ep[0].Interest != 12000 || ep[2].Interest != 4000 {
		t.Errorf("equal principal: %+v", ep)
	}
}

func TestFirstDueDateAndRates(t *testing.T) {
	first := day("2024-02-05")
	s, _ := Terms{Amount: 100000, InterestType: FixedBalance, Rate: 52, RatePer: PerYear, Frequency: Weekly,
		Installments: 2, FirstDueDate: &first, DisbursementOn: day("2024-01-20")}.Schedule()
	if s[0].DueDate != first || s[1].DueDate.Format("2006-01-02") != "2024-02-12" || s[0].Interest != 1000 {
		t.Errorf("weekly: %+v", s)
	}
	if r := (Terms{Rate: 30, RatePer: PerTerm, Installments: 3}).PeriodRate(); r != 10 {
		t.Errorf("term rate %v", r)
	}
	fa, _ := Terms{Amount: 300000, InterestType: FixedAmount, FixedInterest: 5000, Frequency: Monthly, Installments: 3,
		DisbursementOn: day("2024-01-01")}.Schedule()
	if Interest(fa) != 15000 {
		t.Errorf("fixed amount interest %d", Interest(fa))
	}
	if _, err := (Terms{Amount: 100, InterestType: CustomInterest, Installments: 1, Frequency: Monthly}).Schedule(); err == nil {
		t.Error("custom interest should be refused")
	}
}

func TestAllocate(t *testing.T) {
	charges := []Charge{
		{ID: 1, Kind: "installment", DueOn: day("2024-02-01"), Amount: 1000},
		{ID: 3, Kind: "late_fine", DueOn: day("2024-02-02"), Amount: 100},
		{ID: 2, Kind: "installment", DueOn: day("2024-03-01"), Amount: 1000},
	}
	got, over := Allocate(charges, 1500, false)
	if got[1] != 1000 || got[3] != 100 || got[2] != 400 || over != 0 {
		t.Errorf("in date order: %v", got)
	}
	got, _ = Allocate(charges, 1500, true)
	if got[1] != 1000 || got[2] != 500 || got[3] != 0 {
		t.Errorf("fines deferred: %v", got)
	}
	_, over = Allocate(charges, 2500, false)
	if over != 400 {
		t.Errorf("overpaid %d", over)
	}
}

func TestFineRules(t *testing.T) {
	r := FineRule{Kind: "percentage", Rate: 10, RateOn: "installment_balance", Frequency: "monthly"}
	if err := r.Check(false); err != nil {
		t.Fatal(err)
	}
	if c := r.Charge(Bases{InstallmentBalance: 123456}); c != 12345 {
		t.Errorf("charge %d", c)
	}
	if n := r.Next(day("2024-01-31")); n == nil || n.Format("2006-01-02") != "2024-03-02" {
		t.Errorf("next %v", n)
	}
	once := FineRule{Kind: "fixed", Amount: "50", Frequency: "once"}
	if err := once.Check(true); err != nil || once.Next(day("2024-01-01")) != nil || once.Charge(Bases{}) != 5000 {
		t.Errorf("one-off: %v", err)
	}
	if err := (&FineRule{Kind: "percentage", Rate: 5, RateOn: "installment_interest", Frequency: "daily"}).Check(true); err == nil {
		t.Error("balance fine on installment interest should be refused")
	}
	fee := ProcessingFee{Kind: "percentage", Rate: 1.5, On: "amount_plus_interest"}
	if err := fee.Check(); err != nil || fee.Charge(1000000, 200000) != 18000 {
		t.Errorf("fee %v %d", err, fee.Charge(1000000, 200000))
	}
}
