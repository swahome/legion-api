package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/swahome/legion-api/internal/billing"
)

func (f *moneyFixture) bill(day string) billing.Summary {
	f.t.Helper()
	d, _ := time.Parse(time.DateOnly, day)
	sum, err := billing.New(f.pool, nil).Run(context.Background(), billing.RunOptions{Today: d, Trigger: "cli"})
	if err != nil {
		f.t.Fatalf("billing %s: %v", day, err)
	}
	if len(sum.Problems) > 0 {
		f.t.Fatalf("billing %s problems: %v", day, sum.Problems)
	}
	return sum
}

func TestBillingRun(t *testing.T) {
	f := newMoneyFixture(t)

	// Bad schedules and fine rules are refused.
	for name, bad := range map[string]map[string]any{
		"fines on one-time": {"name": "Hall", "kind": "one_time", "amount": "500",
			"schedule":   map[string]any{"contribution_date": "2026-11-05"},
			"fine_rules": []any{map[string]any{"kind": "fixed", "fixed_amount": "50", "chargeable_on": "1"}}},
		"no fine amount": {"name": "X", "kind": "regular", "amount": "500",
			"schedule":   map[string]any{"frequency": "monthly", "contribution_date": "2026-11-05"},
			"fine_rules": []any{map[string]any{"kind": "fixed", "chargeable_on": "1"}}},
		"odd chargeable": {"name": "X", "kind": "regular", "amount": "500",
			"schedule":   map[string]any{"frequency": "monthly", "contribution_date": "2026-11-05"},
			"fine_rules": []any{map[string]any{"kind": "fixed", "fixed_amount": "50", "chargeable_on": "99"}}},
		"rate over 100": {"name": "X", "kind": "regular", "amount": "500",
			"schedule":   map[string]any{"frequency": "monthly", "contribution_date": "2026-11-05"},
			"fine_rules": []any{map[string]any{"kind": "percentage", "percentage_rate": "150", "percentage_of": "2", "chargeable_on": "1"}}},
		"bad twice monthly": {"name": "X", "kind": "regular", "amount": "500",
			"schedule": map[string]any{"frequency": "twice_monthly", "contribution_date": "2026-11-05", "rule": map[string]any{}}},
	} {
		if r := f.post("/contributions", bad); r.code != 400 {
			t.Errorf("%s: status %d, want 400: %s", name, r.code, r.raw)
		}
	}

	// Welfare: 300 on the 5th, invoiced a week before, fined 100 a day
	// after the due date for each unpaid invoice, at most twice.
	r := f.post("/contributions", map[string]any{"name": "Welfare", "kind": "regular", "amount": "300",
		"schedule": map[string]any{"frequency": "monthly", "contribution_date": "2026-11-05", "invoice_days_before": 7,
			"fines_enabled": true, "rule": map[string]any{"month_day_monthly": 5}},
		"fine_rules": []any{map[string]any{"kind": "fixed", "fixed_amount": "100", "chargeable_on": "1",
			"mode": "1", "frequency": "0", "fine_limit": 2}}})
	expect(t, r, 201)
	welfare := f.id(r)
	sch := r.body["schedule"].(map[string]any)
	rules := r.list("fine_rules")
	if sch["invoice_date"] != "2026-10-29" || string(mustJSON(sch["rule"])) != `{"month_day_monthly":"5"}` ||
		len(rules) != 1 || rules[0]["next_fine_date"] != "2026-11-06" {
		t.Fatalf("created: %s", r.raw)
	}

	// Nothing is due the day before the invoice date.
	if s := f.bill("2026-10-28"); s.InvoicesRaised != 0 {
		t.Errorf("early run raised %d", s.InvoicesRaised)
	}
	// Invoice date: the two active members (not the suspended one) are
	// invoiced; Monthly savings from the fixture has no invoice date.
	if s := f.bill("2026-10-29"); s.InvoicesRaised != 2 || s.SchedulesInvoiced != 1 {
		t.Errorf("invoice run: %+v", s)
	}
	if s := f.bill("2026-10-29"); s.InvoicesRaised != 0 {
		t.Errorf("re-run raised %d", s.InvoicesRaised)
	}
	if got := f.scalar(`SELECT count(*) FROM ledger_entries WHERE contribution_id = $1 AND entry_type = 'contribution_invoice'
		AND entry_date = '2026-10-29' AND due_date = '2026-11-05' AND generated_by = 'invoicing'`, welfare); got != 2 {
		t.Errorf("invoices = %d", got)
	}
	r = f.get(f.officialTok, fmt.Sprintf("/contributions/%d", welfare))
	if sch := r.body["schedule"].(map[string]any); sch["contribution_date"] != "2026-12-05" || sch["invoice_date"] != "2026-11-28" {
		t.Errorf("schedule after invoicing: %s", r.raw)
	}

	// Jane pays on time; Otieno doesn't, and is fined the day after.
	f.exec(`INSERT INTO ledger_entries (group_id, membership_id, contribution_id, entry_type, amount, entry_date)
		VALUES ($1, $2, $3, 'contribution_payment', 300, '2026-11-04')`, f.group, f.official, welfare)
	if s := f.bill("2026-11-06"); s.FinesRaised != 1 {
		t.Errorf("fine run: %+v", s)
	}
	if got := f.scalar(`SELECT count(*) FROM ledger_entries WHERE membership_id = $1 AND entry_type = 'contribution_fine_invoice'
		AND amount = 100 AND entry_date = '2026-11-06' AND parent_entry_id IS NOT NULL`, f.member); got != 1 {
		t.Errorf("Otieno's fines = %d", got)
	}
	r = f.get(f.officialTok, fmt.Sprintf("/contributions/%d", welfare))
	if rules := r.list("fine_rules"); rules[0]["next_fine_date"] != "2026-12-06" {
		t.Errorf("next fine date: %s", r.raw)
	}

	// A month later, after the December invoice: Otieno's November invoice
	// collects its second (last) fine and both members' December invoices
	// their first.
	f.bill("2026-11-28")
	if s := f.bill("2026-12-06"); s.FinesRaised != 3 {
		t.Errorf("December fines: %+v", s)
	}
	// Server down for most of December: the January invoices (dated 29
	// December) are still caught up within the week.
	if s := f.bill("2027-01-03"); s.InvoicesRaised != 2 || s.SchedulesSkipped != 0 {
		t.Errorf("catch-up: %+v", s)
	}
	if s := f.bill("2027-01-07"); s.FinesRaised != 4 {
		t.Errorf("January fines: %+v", s)
	}
	// November's invoice has had its two fines; December gets a second,
	// January a first.
	if got := f.scalar(`SELECT count(*) FROM ledger_entries WHERE membership_id = $1 AND entry_type = 'contribution_fine_invoice'`, f.member); got != 5 {
		t.Errorf("Otieno's fines after January = %d", got)
	}

	// Editing the rule without touching its timing keeps its date.
	r = f.get(f.officialTok, fmt.Sprintf("/contributions/%d", welfare))
	rule := r.list("fine_rules")[0]
	next := rule["next_fine_date"]
	r = f.call("PATCH", f.officialTok, fmt.Sprintf("/contributions/%d", welfare), map[string]any{
		"fine_rules": []any{map[string]any{"id": rule["id"], "kind": "fixed", "fixed_amount": "150", "chargeable_on": "1", "mode": "1", "frequency": "0", "fine_limit": 2}}})
	expect(t, r, 200)
	if got := r.list("fine_rules"); len(got) != 1 || got[0]["fixed_amount"] != "150.00" || got[0]["next_fine_date"] != next {
		t.Errorf("edited rule: %s", r.raw)
	}
	// Removing every rule.
	r = f.call("PATCH", f.officialTok, fmt.Sprintf("/contributions/%d", welfare), map[string]any{"fine_rules": []any{}})
	expect(t, r, 200)
	if _, has := r.body["fine_rules"]; has && len(r.list("fine_rules")) != 0 {
		t.Errorf("rules after delete: %s", r.raw)
	}

	// The runs are in the admin job history.
	rec := f.do("GET", "/v1/admin/jobs?job=billing", nil, withBearer(f.adminTok))
	var jobs struct {
		Items []struct {
			Status  string          `json:"status"`
			Summary json.RawMessage `json:"summary"`
		} `json:"items"`
		Total int `json:"total"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &jobs)
	if rec.Code != 200 || jobs.Total != 8 || jobs.Items[0].Status != "succeeded" {
		t.Errorf("job runs: %d %s", rec.Code, rec.Body.String())
	}
	if rec := f.do("GET", "/v1/admin/jobs", nil, withBearer(f.officialTok)); rec.Code != 403 {
		t.Errorf("officials see jobs: %d", rec.Code)
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func TestFineMembers(t *testing.T) {
	f := newMoneyFixture(t)

	for name, bad := range map[string]map[string]any{
		"no members":     {"member_ids": []int64{}, "fine_category_id": f.fineCat, "amount": "200", "date": "2026-04-01"},
		"other category": {"member_ids": []int64{f.member}, "fine_category_id": f.otherFineCat, "amount": "200", "date": "2026-04-01"},
		"zero":           {"member_ids": []int64{f.member}, "fine_category_id": f.fineCat, "amount": "0", "date": "2026-04-01"},
		"future":         {"member_ids": []int64{f.member}, "fine_category_id": f.fineCat, "amount": "200", "date": "2099-04-01"},
		"outsider":       {"member_ids": []int64{999999}, "fine_category_id": f.fineCat, "amount": "200", "date": "2026-04-01"},
	} {
		if r := f.post("/fines", bad); r.code != 400 {
			t.Errorf("%s: status %d, want 400: %s", name, r.code, r.raw)
		}
	}
	expect(t, f.call("POST", f.memberTok, "/fines", map[string]any{}), 403)

	r := f.post("/fines", map[string]any{"member_ids": []int64{f.member, f.official, f.member}, "fine_category_id": f.fineCat,
		"amount": "200", "date": "2026-04-01", "description": "Late to the meeting"})
	expect(t, r, 201)
	ids := r.body["fine_ids"].([]any)
	if len(ids) != 2 {
		t.Fatalf("fines: %s", r.raw)
	}

	r = f.get(f.officialTok, "/fines")
	expect(t, r, 200)
	items := r.list("items")
	if r.body["total"] != float64(2) || items[0]["fine_category"].(map[string]any)["name"] != "Lateness" || items[0]["kind"] != "fine" {
		t.Errorf("official list: %s", r.raw)
	}
	// A member sees only their own.
	r = f.get(f.memberTok, "/fines")
	expect(t, r, 200)
	if r.body["total"] != float64(1) || r.list("items")[0]["member"].(map[string]any)["id"] != float64(f.member) {
		t.Errorf("member list: %s", r.raw)
	}
	expect(t, f.get(f.memberTok, fmt.Sprintf("/fines?member_id=%d", f.official)), 403)

	r = f.get(f.memberTok, "/fine-balances")
	expect(t, r, 200)
	if m := r.list("members"); len(m) != 1 || m[0]["balance"] != "200.00" {
		t.Errorf("member balance: %s", r.raw)
	}

	fine := int64(ids[0].(float64))
	expect(t, f.call("POST", f.memberTok, fmt.Sprintf("/fines/%d/void", fine), nil), 403)
	expect(t, f.post(fmt.Sprintf("/fines/%d/void", fine), nil), 204)
	expect(t, f.post(fmt.Sprintf("/fines/%d/void", fine), nil), 404)
	r = f.get(f.officialTok, "/fine-balances")
	if r.body["total_owed"] != "200.00" {
		t.Errorf("after void: %s", r.raw)
	}
	r = f.get(f.officialTok, "/fines?include_voided=true")
	if r.body["total"] != float64(2) {
		t.Errorf("with voided: %s", r.raw)
	}
}
