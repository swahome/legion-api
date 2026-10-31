package legacy

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Money: saccos, mobile money providers, group accounts, categories,
// depositors, deposits, withdrawals, account transfers and the cash book
// (transaction_statements).

// Legacy type codes encode both what a transaction is for and which kind
// of account it touched (see Transactions.php: *_deposit_transaction_types,
// *_withdrawal_transaction_types and $transaction_names).
var accountKinds = [4]string{"bank", "sacco", "mobile_money", "petty_cash"}

var depositKinds = []string{"misc", "income", "loan_repayment", "bank_loan", "stock_sale", "money_market",
	"asset_sale", "funds_transfer", "loan_processing_income", "external_lending_processing_income",
	"external_lending_repayment"}

var withdrawalKinds = []string{"expense", "asset_purchase", "loan_disbursement", "stock_purchase",
	"money_market_investment", "contribution_refund", "bank_loan_repayment", "funds_transfer",
	"external_loan_disbursement", "dividend"}

// DepositType maps a legacy deposits.type to (kind, account kind).
func DepositType(t int64) (kind, account string, ok bool) {
	switch t {
	case 1, 2, 3:
		return "contribution", accountKinds[t-1], true
	case 7:
		return "contribution", "petty_cash", true
	case 4, 5, 6:
		return "fine", accountKinds[t-4], true
	case 8:
		return "fine", "petty_cash", true
	}
	if t >= 9 && t <= 52 {
		return depositKinds[(t-9)/4], accountKinds[(t-9)%4], true
	}
	return "", "", false
}

// WithdrawalType maps a legacy withdrawals.type to (kind, account kind).
func WithdrawalType(t int64) (kind, account string, ok bool) {
	if t >= 1 && t <= 40 {
		return withdrawalKinds[(t-1)/4], accountKinds[(t-1)%4], true
	}
	return "", "", false
}

// EntryType maps a legacy transaction_statements.transaction_type to the
// direction of the money (+1 in, -1 out) and the account kind. Types 1-40
// are deposits with the deposit codes; 41 and up run in blocks of four
// (bank, sacco, mobile, petty) and are withdrawals except loan processing
// income (73-76) and external lending income and repayments (81-88).
func EntryType(t int64) (direction int16, account string, ok bool) {
	switch {
	case t >= 1 && t <= 40:
		_, account, ok = DepositType(t)
		return 1, account, ok
	case t >= 41 && t <= 92:
		account = accountKinds[(t-41)%4]
		if (t >= 73 && t <= 76) || (t >= 81 && t <= 88) {
			return 1, account, true
		}
		return -1, account, true
	}
	return 0, "", false
}

var depositMethods = map[int64]string{1: "mpesa", 2: "cash", 3: "cheque", 4: "funds_transfer", 5: "equitel", 6: "standing_order"}
var withdrawalMethods = map[int64]string{1: "cash", 2: "mpesa", 3: "cheque", 4: "funds_transfer", 5: "equitel"}

// legacyAccountPrefixes are the prefixes of account_id values in a
// production export ("bank-12", "sacco-3", "mobile-7", "petty-2").
var legacyAccountPrefixes = map[string]string{"bank": "bank", "sacco": "sacco", "mobile": "mobile_money", "petty": "petty_cash"}

type accountKey struct {
	kind string
	id   int64
}

// accountIndex finds the new id of a legacy account reference.
type accountIndex struct {
	byLegacy map[accountKey]int64         // (kind, legacy id) -> new id
	groupOf  map[int64]int64              // new id -> group
	byGroup  map[int64]map[string][]int64 // group -> kind -> new ids
}

// resolve returns the account a legacy row used. A production export holds
// account_id as "bank-12"; the anonymized dump lost it (every value is 0),
// so a row falls back to the group's only account of the right kind. With
// several candidates the account stays unknown rather than guessed.
func (ix *accountIndex) resolve(groupID int64, kind, raw string) (id *int64, how string) {
	raw = clean(raw)
	// Some legacy rows repeat the prefix ("bank-bank-12").
	for {
		prefix, rest, ok := strings.Cut(raw, "-")
		if _, known := legacyAccountPrefixes[prefix]; !ok || !known || !strings.HasPrefix(rest, prefix+"-") {
			break
		}
		raw = rest
	}
	if prefix, num, ok := strings.Cut(raw, "-"); ok {
		if k, known := legacyAccountPrefixes[prefix]; known {
			n, _ := strconv.ParseInt(num, 10, 64)
			if v, found := ix.byLegacy[accountKey{k, n}]; found && ix.groupOf[v] == groupID {
				return &v, "by id"
			}
		}
	}
	switch c := ix.byGroup[groupID][kind]; len(c) {
	case 0:
		return nil, "no account of that kind"
	case 1:
		v := c[0]
		return &v, "only account of that kind"
	default:
		return nil, "several accounts of that kind"
	}
}

var SaccoCols = []string{"id", "legacy_id", "name", "slug", "is_active", "created_at", "updated_at"}
var SaccoBranchCols = []string{"id", "legacy_id", "sacco_id", "name", "is_active", "created_at", "updated_at"}
var ProviderCols = SaccoCols

// institutions loads saccos, sacco branches and mobile money providers.
func (p *Plan) institutions(t Tables, now time.Time) (saccos, saccoBranches, providers map[int64]bool) {
	named := func(table string, rows []Row, out *[][]any) map[int64]bool {
		ids := map[int64]bool{}
		slugs := map[string]bool{}
		for _, r := range byID(rows) {
			id := r.Int("id")
			name := clean(r.Str("name"))
			if name == "" {
				p.Report.issue("%s: blank name, skipped", table)
				continue
			}
			slug := strings.Trim(reSlug.ReplaceAllString(strings.ToLower(clean(r.Str("slug"))), "-"), "-")
			if slug == "" || slugs[slug] {
				slug = fmt.Sprintf("%s-%d", strings.TrimSuffix(table, "s"), id)
			}
			slugs[slug] = true
			created := timeOr(epoch(r.Str("created_on")), now)
			*out = append(*out, []any{id, id, name, slug, r.Ptr("active") == nil || r.Flag("active"),
				created, timeOr(epoch(r.Str("modified_on")), created)})
			ids[id] = true
		}
		return ids
	}
	saccos = named("saccos", t["saccos"], &p.Saccos)
	providers = named("mobile_money_providers", t["mobile_money_providers"], &p.Providers)

	saccoBranches = map[int64]bool{}
	for _, r := range byID(t["sacco_branches"]) {
		id := r.Int("id")
		name := clean(r.Str("name"))
		if !saccos[r.Int("sacco_id")] || name == "" {
			p.Report.issue("sacco_branches: sacco missing or blank name, skipped")
			continue
		}
		created := timeOr(epoch(r.Str("created_on")), now)
		p.SaccoBranches = append(p.SaccoBranches, []any{id, id, r.Int("sacco_id"), name,
			r.Ptr("active") == nil || r.Flag("active"), created, timeOr(epoch(r.Str("modified_on")), created)})
		saccoBranches[id] = true
	}
	return saccos, saccoBranches, providers
}

var AccountCols = []string{"id", "legacy_id", "group_id", "kind", "name", "account_number", "bank_id",
	"bank_branch_id", "sacco_id", "sacco_branch_id", "provider_id", "initial_balance", "is_default",
	"is_verified", "is_closed", "settings", "deleted_at", "created_at", "updated_at"}

var accountColumnsUsed = set("id", "group_id", "account_name", "account_number", "bank_id", "bank_branch_id",
	"sacco_id", "sacco_branch_id", "mobile_money_provider_id", "initial_balance", "current_balance", "is_default",
	"is_verified", "is_closed", "is_deleted", "active", "created_by", "created_on", "modified_by", "modified_on",
	"account_password", "account_slug", "old_id", "is_system")

type refSets struct {
	groups, users, banks, branches, saccos, saccoBranches, providers map[int64]bool
	members                                                          map[int64]int64 // membership -> group
	contribs                                                         map[int64]*contribution
}

// accounts gives every legacy account a new id (the four legacy tables
// share an id space), bank first, then sacco, mobile money and petty cash.
func (p *Plan) accounts(t Tables, ref refSets, now time.Time) *accountIndex {
	ix := &accountIndex{byLegacy: map[accountKey]int64{}, groupOf: map[int64]int64{}, byGroup: map[int64]map[string][]int64{}}
	var next int64
	for _, src := range []struct{ table, kind string }{
		{"bank_accounts", "bank"}, {"sacco_accounts", "sacco"},
		{"mobile_money_accounts", "mobile_money"}, {"petty_cash_accounts", "petty_cash"},
	} {
		for _, r := range byID(t[src.table]) {
			lid, gid := r.Int("id"), r.Int("group_id")
			if !ref.groups[gid] {
				p.Report.issue("%s: group missing, skipped", src.table)
				continue
			}
			var bank, branch, sacco, saccoBranch, provider *int64
			optional := func(col string, known map[int64]bool) *int64 {
				if v := r.Int(col); v > 0 {
					if known[v] {
						return &v
					}
					p.Report.issue("%s: %s missing, cleared", src.table, col)
				}
				return nil
			}
			switch src.kind {
			case "bank":
				bank, branch = optional("bank_id", ref.banks), optional("bank_branch_id", ref.branches)
			case "sacco":
				sacco, saccoBranch = optional("sacco_id", ref.saccos), optional("sacco_branch_id", ref.saccoBranches)
			case "mobile_money":
				provider = optional("mobile_money_provider_id", ref.providers)
			}
			name := clean(r.Str("account_name"))
			number := clean(r.Str("account_number"))
			if name == "" || (src.kind == "mobile_money" && name == number) {
				// The anonymized dump turned mobile money account names into numbers.
				name = map[string]string{"bank": "Bank account", "sacco": "Sacco account",
					"mobile_money": "Mobile money account", "petty_cash": "Petty cash"}[src.kind]
				if number != "" {
					name += " " + number
				}
			}
			initial := "0"
			if f, err := strconv.ParseFloat(clean(r.Str("initial_balance")), 64); err == nil {
				initial = strconv.FormatFloat(f, 'f', 2, 64)
			}
			created := timeOr(epoch(r.Str("created_on")), now)
			updated := timeOr(epoch(r.Str("modified_on")), created)
			var deleted *time.Time
			if r.Flag("is_deleted") {
				deleted = &updated
			}
			next++
			id := next
			p.Accounts = append(p.Accounts, []any{id, lid, gid, src.kind, name, number, bank, branch, sacco,
				saccoBranch, provider, initial, r.Flag("is_default"), r.Flag("is_verified"), r.Flag("is_closed"),
				extras(r, accountColumnsUsed), deleted, created, updated})
			ix.byLegacy[accountKey{src.kind, lid}] = id
			ix.groupOf[id] = gid
			if ix.byGroup[gid] == nil {
				ix.byGroup[gid] = map[string][]int64{}
			}
			ix.byGroup[gid][src.kind] = append(ix.byGroup[gid][src.kind], id)
		}
	}
	return ix
}

var IncomeCategoryCols = []string{"id", "legacy_id", "group_id", "name", "description", "is_hidden", "is_active", "deleted_at", "created_at", "updated_at"}
var ExpenseCategoryCols = []string{"id", "legacy_id", "group_id", "name", "description", "is_administrative", "is_hidden", "is_active", "deleted_at", "created_at", "updated_at"}
var FineCategoryCols = []string{"id", "legacy_id", "group_id", "name", "description", "amount", "is_hidden", "is_active", "deleted_at", "created_at", "updated_at"}
var DepositorCols = []string{"id", "legacy_id", "group_id", "name", "email", "phone", "description", "is_active", "created_at", "updated_at"}

// categories loads the per-group lists and returns category id -> group.
func (p *Plan) categories(t Tables, groups map[int64]bool, now time.Time) (income, expense, fine, depositors map[int64]int64) {
	load := func(table string, extra func(r Row) []any, out *[][]any) map[int64]int64 {
		ids := map[int64]int64{}
		for _, r := range byID(t[table]) {
			id, gid := r.Int("id"), r.Int("group_id")
			name := clean(r.Str("name"))
			switch {
			case !groups[gid]:
				p.Report.issue("%s: group missing, skipped", table)
				continue
			case name == "":
				p.Report.issue("%s: blank name, skipped", table)
				continue
			}
			created := timeOr(epoch(r.Str("created_on")), now)
			updated := timeOr(epoch(r.Str("modified_on")), created)
			row := []any{id, id, gid, name}
			row = append(row, extra(r)...)
			if table == "depositors" {
				row = append(row, r.Ptr("active") == nil || r.Flag("active"), created, updated)
			} else {
				var deleted *time.Time
				if r.Flag("is_deleted") {
					deleted = &updated
				}
				row = append(row, r.Flag("is_hidden"), r.Ptr("active") == nil || r.Flag("active"), deleted, created, updated)
			}
			*out = append(*out, row)
			ids[id] = gid
		}
		return ids
	}
	desc := func(r Row) []any { return []any{clean(r.Str("description"))} }
	income = load("income_categories", desc, &p.IncomeCategories)
	expense = load("expense_categories", func(r Row) []any {
		return []any{clean(r.Str("description")), r.Flag("is_an_administrative_expense_category")}
	}, &p.ExpenseCategories)
	fine = load("fine_categories", func(r Row) []any {
		amount := "0"
		if f, err := strconv.ParseFloat(clean(r.Str("amount")), 64); err == nil && f >= 0 {
			amount = strconv.FormatFloat(f, 'f', 2, 64)
		}
		return []any{clean(r.Str("description")), amount}
	}, &p.FineCategories)
	depositors = load("depositors", func(r Row) []any {
		return []any{cleanPtr(r["email"]), cleanPtr(r["phone"]), clean(r.Str("description"))}
	}, &p.Depositors)
	return income, expense, fine, depositors
}

// moneyRow holds what deposits, withdrawals and entries share.
type moneyRow struct {
	gid     int64
	amount  string
	date    time.Time
	created time.Time
	voided  *time.Time
	member  *int64
	contrib *int64
}

// common validates the group, amount and date and resolves member and
// contribution, reporting under table.
func (p *Plan) common(table string, r Row, dateCol string, ref refSets, now time.Time) (moneyRow, bool) {
	m := moneyRow{gid: r.Int("group_id")}
	if !ref.groups[m.gid] {
		p.Report.issue("%s: group missing, skipped", table)
		return m, false
	}
	f, err := strconv.ParseFloat(clean(r.Str("amount")), 64)
	if err != nil || f <= 0 {
		p.Report.issue("%s: zero or bad amount, skipped", table)
		return m, false
	}
	m.amount = clean(r.Str("amount"))
	d := epochDate(r.Str(dateCol))
	if d == nil {
		d = epochDate(r.Str("created_on"))
	}
	if d == nil {
		p.Report.issue("%s: no date, skipped", table)
		return m, false
	}
	m.date = *d
	m.created = timeOr(epoch(r.Str("created_on")), now)
	if !r.Flag("active") {
		v := timeOr(epoch(r.Str("modified_on")), m.created)
		m.voided = &v
	}
	if mid := r.Int("member_id"); mid > 0 {
		if ref.members[mid] == m.gid {
			m.member = &mid
		} else {
			p.Report.issue("%s: member missing or in another group, kept without member", table)
		}
	}
	if cid := r.Int("contribution_id"); cid > 0 {
		if c := ref.contribs[cid]; c != nil && c.groupID == m.gid {
			m.contrib = &cid
		} else {
			p.Report.issue("%s: contribution missing or in another group, kept without contribution", table)
		}
	}
	return m, true
}

// inGroup returns &id when the legacy id names a row of the same group.
func inGroup(p *Plan, table, col string, r Row, gid int64, known map[int64]int64) *int64 {
	if v := r.Int(col); v > 0 {
		if known[v] == gid {
			return &v
		}
		p.Report.issue("%s: %s missing or in another group, cleared", table, col)
	}
	return nil
}

// refsJSON keeps the legacy links to tables later milestones bring over.
func refsJSON(r Row, cols ...string) []byte {
	m := map[string]int64{}
	for _, c := range cols {
		if v := r.Int(c); v > 0 {
			m[c] = v
		}
	}
	b, _ := json.Marshal(m)
	return b
}

func (p *Plan) accountFor(table string, ix *accountIndex, gid int64, kind, raw string) *int64 {
	id, how := ix.resolve(gid, kind, raw)
	if id == nil {
		p.Report.issue("%s: account unknown (%s), kept without account", table, how)
	} else if how != "by id" {
		p.Report.issue("%s: account taken as the group's %s", table, how)
	}
	return id
}

func (p *Plan) createdBy(r Row, users map[int64]bool) *int64 {
	if v := r.Int("created_by"); users[v] {
		return &v
	}
	return nil
}

var DepositCols = []string{"id", "legacy_id", "legacy_type", "group_id", "account_id", "account_kind", "kind",
	"method", "membership_id", "contribution_id", "fine_category_id", "income_category_id", "depositor_id",
	"amount", "deposit_date", "description", "is_backdated", "refs", "created_by", "created_at", "voided_at", "loan_id",
	"bank_loan_id"}

var depositRefs = []string{"loan_id", "loan_repayment_id", "stock_id", "stock_sale_id", "asset_id",
	"money_market_investment_id", "bank_loan_id", "account_transfer_id", "transaction_alert_id", "debtor_id",
	"debtor_loan_id", "debtor_loan_repayment_id", "meeting_id"}

// deposits also returns, for loan repayments, legacy loan_repayment id ->
// deposit id.
func (p *Plan) deposits(rows []Row, ref refSets, ix *accountIndex, income, fines, depositors map[int64]int64,
	loanSet loanSet, bankLoans map[int64]int64, now time.Time) (ids map[int64]int64, repaymentDeposit map[int64]int64) {
	ids, repaymentDeposit = map[int64]int64{}, map[int64]int64{}
	for _, r := range byID(rows) {
		id, typ := r.Int("id"), r.Int("type")
		kind, acct, ok := DepositType(typ)
		if !ok {
			p.Report.issue("deposits: unknown type %d, skipped", typ)
			continue
		}
		m, ok := p.common("deposits", r, "deposit_date", ref, now)
		if !ok {
			continue
		}
		var method *string
		if v, ok := depositMethods[r.Int("deposit_method")]; ok {
			method = &v
		}
		p.Deposits = append(p.Deposits, []any{id, id, typ, m.gid,
			p.accountFor("deposits", ix, m.gid, acct, r.Str("account_id")), acct, kind, method, m.member, m.contrib,
			inGroup(p, "deposits", "fine_category_id", r, m.gid, fines),
			inGroup(p, "deposits", "income_category_id", r, m.gid, income),
			inGroup(p, "deposits", "depositor_id", r, m.gid, depositors),
			m.amount, m.date, clean(r.Str("description")), r.Flag("is_a_back_dating_record"),
			refsJSON(r, depositRefs...), p.createdBy(r, ref.users), m.created, m.voided, p.loanLink("deposits", r, m.gid, loanSet),
			p.bankLoanLink("deposits", r, m.gid, bankLoans)})
		ids[id] = m.gid
		if rid := r.Int("loan_repayment_id"); rid > 0 && kind == "loan_repayment" {
			if _, dup := repaymentDeposit[rid]; dup {
				p.Report.issue("deposits: two deposits for one loan repayment, second not linked")
			} else {
				repaymentDeposit[rid] = id
			}
		}
	}
	return ids, repaymentDeposit
}

// loanLink is the imported loan a deposit or withdrawal names, if any.
func (p *Plan) loanLink(table string, r Row, gid int64, loanSet loanSet) *int64 {
	v := r.Int("loan_id")
	if v <= 0 {
		return nil
	}
	if id := loanSet.in(v, gid); id != nil {
		return id
	}
	p.Report.issue("%s: loan missing or in another group, link kept in refs only", table)
	return nil
}

var WithdrawalCols = []string{"id", "legacy_id", "legacy_type", "group_id", "account_id", "account_kind", "kind",
	"method", "membership_id", "contribution_id", "expense_category_id", "amount", "withdrawal_date",
	"description", "is_backdated", "refs", "created_by", "created_at", "voided_at", "loan_id", "bank_loan_id"}

var withdrawalRefs = []string{"loan_id", "stock_id", "asset_id", "money_market_investment_id", "contribution_refund_id",
	"bank_loan_id", "bank_loan_repayment_id", "account_transfer_id", "transaction_alert_id", "debtor_id",
	"debtor_loan_id", "meeting_id"}

func (p *Plan) withdrawals(rows []Row, ref refSets, ix *accountIndex, expense map[int64]int64, loanSet loanSet,
	bankLoans map[int64]int64, now time.Time) map[int64]int64 {
	ids := map[int64]int64{}
	for _, r := range byID(rows) {
		id, typ := r.Int("id"), r.Int("type")
		kind, acct, ok := WithdrawalType(typ)
		if !ok {
			p.Report.issue("withdrawals: unknown type %d, skipped", typ)
			continue
		}
		m, ok := p.common("withdrawals", r, "withdrawal_date", ref, now)
		if !ok {
			continue
		}
		var method *string
		if v, ok := withdrawalMethods[r.Int("withdrawal_method")]; ok {
			method = &v
		}
		p.Withdrawals = append(p.Withdrawals, []any{id, id, typ, m.gid,
			p.accountFor("withdrawals", ix, m.gid, acct, r.Str("account_id")), acct, kind, method, m.member, m.contrib,
			inGroup(p, "withdrawals", "expense_category_id", r, m.gid, expense),
			m.amount, m.date, clean(r.Str("description")), r.Flag("is_a_back_dating_record"),
			refsJSON(r, withdrawalRefs...), p.createdBy(r, ref.users), m.created, m.voided, p.loanLink("withdrawals", r, m.gid, loanSet),
			p.bankLoanLink("withdrawals", r, m.gid, bankLoans)})
		ids[id] = m.gid
	}
	return ids
}

var AccountTransferCols = []string{"id", "legacy_id", "group_id", "from_account_id", "to_account_id", "amount",
	"transfer_date", "description", "created_by", "created_at", "voided_at"}

// accountTransfers keeps transfers whose accounts are given by id; the
// anonymized dump has none, so both sides stay unknown there.
func (p *Plan) accountTransfers(rows []Row, ref refSets, ix *accountIndex, now time.Time) map[int64]int64 {
	ids := map[int64]int64{}
	side := func(gid int64, raw string) *int64 {
		id, how := ix.resolve(gid, "", raw)
		if how != "by id" {
			return nil
		}
		return id
	}
	for _, r := range byID(rows) {
		id := r.Int("id")
		m, ok := p.common("account_transfers", r, "transfer_date", ref, now)
		if !ok {
			continue
		}
		from, to := side(m.gid, r.Str("from_account_id")), side(m.gid, r.Str("to_account_id"))
		if from == nil || to == nil {
			p.Report.issue("account_transfers: account unknown, kept without it")
		}
		p.AccountTransfers = append(p.AccountTransfers, []any{id, id, m.gid, from, to, m.amount, m.date,
			clean(r.Str("description")), p.createdBy(r, ref.users), m.created, m.voided})
		ids[id] = m.gid
	}
	return ids
}

var AccountEntryCols = []string{"id", "legacy_id", "legacy_type", "group_id", "account_id", "account_kind",
	"direction", "amount", "entry_date", "description", "deposit_id", "withdrawal_id", "account_transfer_id",
	"membership_id", "is_backdated", "created_at", "voided_at"}

func (p *Plan) accountEntries(rows []Row, ref refSets, ix *accountIndex, deposits, withdrawals, transfers map[int64]int64, now time.Time) {
	link := func(col string, r Row, gid int64, known map[int64]int64) *int64 {
		if v := r.Int(col); v > 0 {
			if known[v] == gid {
				return &v
			}
			p.Report.issue("account_entries: %s missing or in another group, cleared", col)
		}
		return nil
	}
	for _, r := range byID(rows) {
		id, typ := r.Int("id"), r.Int("transaction_type")
		dir, acct, ok := EntryType(typ)
		if !ok {
			p.Report.issue("account_entries: unknown transaction_type %d, skipped", typ)
			continue
		}
		m, ok := p.common("account_entries", r, "transaction_date", ref, now)
		if !ok {
			continue
		}
		p.AccountEntries = append(p.AccountEntries, []any{id, id, typ, m.gid,
			p.accountFor("account_entries", ix, m.gid, acct, r.Str("account_id")), acct, dir, m.amount, m.date,
			clean(r.Str("description")), link("deposit_id", r, m.gid, deposits),
			link("withdrawal_id", r, m.gid, withdrawals), link("account_transfer_id", r, m.gid, transfers),
			m.member, r.Flag("is_a_back_dating_record"), m.created, m.voided})
	}
}
