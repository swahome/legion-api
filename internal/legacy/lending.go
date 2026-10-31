package legacy

import (
	"strconv"
	"time"
)

// Milestone 4b: debtors and bank loans. Debtor loans (debtor_loans and their
// invoices, repayments and guarantors) are counted in the report but not
// imported: the legacy data has none.

var DebtorCols = []string{"id", "legacy_id", "group_id", "name", "phone", "email", "id_number", "description",
	"is_active", "created_by", "created_at", "updated_at"}

// debtors loads people outside the group who borrowed from it, and returns
// debtor id -> group.
func (p *Plan) debtors(rows []Row, ref refSets, now time.Time) map[int64]int64 {
	ids := map[int64]int64{}
	for _, r := range byID(rows) {
		id, gid := r.Int("id"), r.Int("group_id")
		name := clean(r.Str("name"))
		switch {
		case !ref.groups[gid]:
			p.Report.issue("debtors: group missing, skipped")
			continue
		case name == "":
			p.Report.issue("debtors: blank name, skipped")
			continue
		}
		phone := clean(r.Str("phone"))
		if phone == "0" {
			phone = ""
		}
		created := timeOr(epoch(r.Str("created_on")), now)
		updated := timeOr(epoch(r.Str("modified_on")), created)
		p.Debtors = append(p.Debtors, []any{id, id, gid, name, phone, clean(r.Str("email")), clean(r.Str("id_number")),
			clean(r.Str("description")), r.Ptr("active") == nil || r.Flag("active"), p.createdBy(r, ref.users), created, updated})
		ids[id] = gid
	}
	return ids
}

var BankLoanCols = []string{"id", "legacy_id", "group_id", "description", "amount", "total_payable", "opening_balance",
	"starts_on", "ends_on", "account_id", "created_by", "created_at", "updated_at", "voided_at"}

// bankLoans loads the loans groups took, and returns bank loan id -> group.
// Legacy loan_balance is what is owed now; the opening balance adds back
// the live repayments, which are imported as withdrawals.
func (p *Plan) bankLoans(rows, withdrawals []Row, ref refSets, ix *accountIndex, now time.Time) map[int64]int64 {
	repaid := map[int64]int64{}
	for _, w := range withdrawals {
		if kind, _, ok := WithdrawalType(w.Int("type")); ok && kind == "bank_loan_repayment" && w.Flag("active") {
			repaid[w.Int("bank_loan_id")] += cents(w.Str("amount"))
		}
	}
	ids := map[int64]int64{}
	for _, r := range byID(rows) {
		id, gid := r.Int("id"), r.Int("group_id")
		if !ref.groups[gid] {
			p.Report.issue("bank_loans: group missing, skipped")
			continue
		}
		amount, total := cents(r.Str("amount_loaned")), cents(r.Str("total_loan_amount_payable"))
		if amount <= 0 {
			p.Report.issue("bank_loans: zero or bad amount, skipped")
			continue
		}
		if total < amount {
			p.Report.issue("bank_loans: total payable under the amount received, taken as the amount")
			total = amount
		}
		opening := max(cents(r.Str("loan_balance")), 0) + repaid[id]
		if opening > total {
			p.Report.issue("bank_loans: balance and repayments over the total payable, capped")
			opening = total
		}
		starts := epochDate(r.Str("loan_start_date"))
		if starts == nil {
			starts = epochDate(r.Str("created_on"))
		}
		if starts == nil {
			p.Report.issue("bank_loans: no start date, skipped")
			continue
		}
		ends := epochDate(r.Str("loan_end_date"))
		if ends == nil || ends.Before(*starts) {
			p.Report.issue("bank_loans: end date missing or before the start, taken as the start")
			ends = starts
		}
		created := timeOr(epoch(r.Str("created_on")), now)
		updated := timeOr(epoch(r.Str("modified_on")), created)
		var voided *time.Time
		if r.Ptr("active") != nil && !r.Flag("active") {
			voided = &updated
		}
		p.BankLoans = append(p.BankLoans, []any{id, id, gid, clean(r.Str("description")), centsText(amount),
			centsText(total), centsText(opening), *starts, *ends,
			p.accountFor("bank_loans", ix, gid, "bank", r.Str("account_id")), p.createdBy(r, ref.users),
			created, updated, voided})
		ids[id] = gid
	}
	return ids
}

// bankLoanLink is the imported bank loan a deposit or withdrawal names.
func (p *Plan) bankLoanLink(table string, r Row, gid int64, bankLoans map[int64]int64) *int64 {
	v := r.Int("bank_loan_id")
	if v <= 0 {
		return nil
	}
	if bankLoans[v] == gid {
		return &v
	}
	p.Report.issue("%s: bank loan missing or in another group, link kept in refs only", table)
	return nil
}

// cents reads a legacy amount; anything unreadable is 0.
func cents(s string) int64 {
	f, err := strconv.ParseFloat(clean(s), 64)
	if err != nil {
		return 0
	}
	if f < 0 {
		return int64(f*100 - 0.5)
	}
	return int64(f*100 + 0.5)
}

func centsText(c int64) string {
	sign := ""
	if c < 0 {
		sign, c = "-", -c
	}
	return sign + strconv.FormatInt(c/100, 10) + "." + strconv.FormatInt(c%100/10, 10) + strconv.FormatInt(c%10, 10)
}

// notImported counts legacy rows of tables the new system does not take yet.
func (p *Plan) notImported(t Tables, tables ...string) {
	for _, name := range tables {
		for range t[name] {
			p.Report.issue("%s: not imported (no debtor loans in the legacy data when this was written)", name)
		}
	}
}
