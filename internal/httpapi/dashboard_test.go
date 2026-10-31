package httpapi

import (
	"testing"
	"time"

	"github.com/swahome/legion-api/internal/billing"
)

func TestDashboard(t *testing.T) {
	f := newMoneyFixture(t)
	today := billing.LocalToday(time.Now(), nairobi())
	day := today.Format(dateLayout)
	bank := f.account(map[string]any{"kind": "bank", "name": "Equity", "institution_id": f.bank, "initial_balance": "1000"})
	box := f.account(map[string]any{"kind": "petty_cash", "name": "Cash box"})
	expense := f.scalar(`INSERT INTO expense_categories (group_id, name) VALUES ($1, 'Rent') RETURNING id`, f.group)
	expect(t, f.post("/deposits", map[string]any{"kind": "income", "account_id": box, "amount": "250",
		"date": day, "income_category_id": f.incomeCat}), 201)
	expect(t, f.post("/withdrawals", map[string]any{"kind": "expense", "account_id": bank, "amount": "100",
		"date": day, "expense_category_id": expense}), 201)
	expect(t, f.post("/fines", map[string]any{"member_ids": []int64{f.member}, "fine_category_id": f.fineCat,
		"amount": "200", "date": day}), 201)

	expect(t, f.get(f.outTok, "/dashboard"), 404)
	expect(t, f.get(f.officialTok, "/dashboard?months=0"), 400)

	r := f.get(f.officialTok, "/dashboard")
	expect(t, r, 200)
	cash := r.body["cash"].(map[string]any)
	if cash["at_bank"] != "900.00" || cash["at_hand"] != "250.00" || cash["total"] != "1150.00" ||
		cash["by_kind"].(map[string]any)["petty_cash"] != "250.00" || cash["accounts"].(map[string]any)["bank"] != float64(1) {
		t.Errorf("cash: %v", cash)
	}
	// Otieno paid 1500 and 700.50 (a 999 payment was voided).
	totals := r.body["totals"].(map[string]any)
	if totals["contributions"] != "2200.50" || totals["income"] != "250.00" || totals["expenses"] != "100.00" || totals["fines"] != "0.00" {
		t.Errorf("totals: %v", totals)
	}
	months := r.list("months")
	if len(months) != 12 || months[11]["month"] != today.Format("2006-01") ||
		months[11]["income"] != "250.00" || months[11]["expenses"] != "100.00" {
		t.Errorf("months: %v", months)
	}
	var paid2026 any
	for _, y := range r.list("years") {
		if y["year"] == float64(2026) {
			paid2026 = y["contributions"]
		}
	}
	if paid2026 != "2200.50" {
		t.Errorf("years: %v", r.body["years"])
	}
	// Otieno owes 3000 + 300 invoiced less 2200.50 paid, and the 200 fine.
	owed := r.body["owed"].(map[string]any)
	if c := owed["contributions"].(map[string]any); c["owed"] != "1099.50" || c["members"] != float64(1) {
		t.Errorf("owed contributions: %v", c)
	}
	if c := owed["fines"].(map[string]any); c["owed"] != "200.00" || c["members"] != float64(1) {
		t.Errorf("owed fines: %v", c)
	}
	if r.body["members_behind"] != float64(1) || r.body["pending_withdrawal_requests"] != float64(0) || r.body["loans"].(map[string]any)["balance"] != "0.00" {
		t.Errorf("behind/pending: %s", r.raw)
	}
	recent := r.list("recent")
	if len(recent) != 2 || recent[0]["direction"] != "out" || recent[0]["kind"] != "expense" ||
		recent[1]["kind"] != "income" || recent[1]["account"] != "Cash box" {
		t.Errorf("recent: %v", recent)
	}

	// Members see the group totals but not the cash book.
	r = f.get(f.memberTok, "/dashboard?months=3")
	expect(t, r, 200)
	if len(r.list("months")) != 3 || r.body["recent"] != nil || r.body["cash"].(map[string]any)["total"] != "1150.00" {
		t.Errorf("member view: %s", r.raw)
	}
}
