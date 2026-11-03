package legacy

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// Milestone 8: assets, stocks, money market investments, meetings,
// contribution transfers, notifications and the subscription billing
// history. All keep legacy ids. Links from deposits, withdrawals and loan
// rows are set after the copy from the legacy ids kept in their refs.

var AssetCategoryCols = []string{"id", "legacy_id", "group_id", "name", "slug", "description", "is_hidden",
	"is_active", "created_by", "created_at", "updated_at"}
var AssetCols = []string{"id", "legacy_id", "group_id", "asset_category_id", "name", "cost", "description",
	"is_backdated", "created_by", "created_at", "voided_at"}
var StockCols = []string{"id", "legacy_id", "group_id", "name", "purchase_date", "shares", "shares_sold",
	"purchase_price", "current_price", "is_backdated", "created_by", "created_at", "voided_at"}
var StockSaleCols = []string{"id", "legacy_id", "group_id", "stock_id", "sale_date", "shares", "price_per_share",
	"created_by", "created_at", "voided_at"}
var MoneyMarketCols = []string{"id", "legacy_id", "group_id", "institution", "investment_date", "amount",
	"description", "is_closed", "cash_in_amount", "cash_in_date", "is_backdated", "created_by", "created_at", "voided_at"}
var MeetingCols = []string{"id", "legacy_id", "group_id", "title", "venue", "purpose", "meeting_at", "created_by", "created_at"}
var MeetingAttendanceCols = []string{"meeting_id", "group_id", "membership_id", "status"}
var MeetingItemCols = []string{"meeting_id", "kind", "name", "position"}
var ContributionTransferCols = []string{"id", "legacy_id", "group_id", "membership_id", "from_contribution_id",
	"from_loan_id", "to_member", "to_kind", "to_membership_id", "to_contribution_id", "to_fine_category_id",
	"to_loan_id", "amount", "transfer_date", "description", "created_by", "created_at", "voided_at"}
var NotificationCols = []string{"id", "legacy_id", "group_id", "membership_id", "user_id", "from_user_id", "subject",
	"message", "category", "call_to_action", "link", "file_path", "file_type", "file_size", "refs", "is_read", "created_at"}
var BillingPackageCols = []string{"id", "legacy_id", "name", "slug", "billing_type", "monthly_amount",
	"quarterly_amount", "annual_amount", "percentage_tax", "is_default", "is_active", "settings"}
var BillingInvoiceCols = []string{"id", "legacy_id", "group_id", "number", "billing_date", "due_date", "amount", "tax",
	"prorated_amount", "amount_paid", "billing_cycle", "package_id", "is_paid", "created_at"}
var BillingPaymentCols = []string{"id", "legacy_id", "group_id", "receipt_number", "receipt_date", "amount", "tax",
	"payment_method", "transaction_code", "invoice_id", "package_id", "description", "created_at"}

// voidedAt is when an inactive legacy row was voided.
func voidedAt(r Row, created time.Time) *time.Time {
	if r.Ptr("active") == nil || r.Flag("active") {
		return nil
	}
	v := timeOr(epoch(r.Str("modified_on")), created)
	return &v
}

// positiveMoney is a legacy amount as a decimal string, or "" when it is
// missing, unparseable or not above zero.
func positiveMoney(s string) string {
	c := cents(strings.ReplaceAll(s, ",", ""))
	if c <= 0 {
		return ""
	}
	return centsText(c)
}

// decimal4 keeps up to four places (share counts and prices).
func decimal4(s string) (string, bool) {
	f, err := strconv.ParseFloat(strings.ReplaceAll(clean(s), ",", ""), 64)
	if err != nil || f < 0 {
		return "0", false
	}
	return strconv.FormatFloat(f, 'f', 4, 64), true
}

func (p *Plan) investments(t Tables, ref refSets, now time.Time) {
	cats := map[int64]int64{}
	slugs := map[int64]map[string]bool{}
	for _, r := range byID(t["asset_categories"]) {
		id, gid := r.Int("id"), r.Int("group_id")
		name := clean(r.Str("name"))
		if !ref.groups[gid] || name == "" {
			p.Report.issue("asset_categories: group missing or blank name, skipped")
			continue
		}
		slug := clean(r.Str("slug"))
		if slug == "" {
			slug = strings.ToLower(strings.Join(strings.Fields(name), "-"))
		}
		if slugs[gid] == nil {
			slugs[gid] = map[string]bool{}
		}
		if slugs[gid][slug] {
			p.Report.issue("asset_categories: repeated slug in a group, made unique")
			slug += "-" + strconv.FormatInt(id, 10)
		}
		slugs[gid][slug] = true
		created := timeOr(epoch(r.Str("created_on")), now)
		p.AssetCategories = append(p.AssetCategories, []any{id, id, gid, name, slug, clean(r.Str("description")),
			r.Flag("is_hidden"), r.Ptr("active") == nil || r.Flag("active"), p.createdBy(r, ref.users), created,
			timeOr(epoch(r.Str("modified_on")), created)})
		cats[id] = gid
	}
	for _, r := range byID(t["assets"]) {
		id, gid := r.Int("id"), r.Int("group_id")
		name := clean(r.Str("name"))
		if !ref.groups[gid] || name == "" {
			p.Report.issue("assets: group missing or blank name, skipped")
			continue
		}
		created := timeOr(epoch(r.Str("created_on")), now)
		p.Assets = append(p.Assets, []any{id, id, gid, inGroup(p, "assets", "asset_category_id", r, gid, cats), name,
			moneyOrZero(r.Str("cost")), clean(r.Str("description")), r.Flag("is_a_back_dating_record"),
			p.createdBy(r, ref.users), created, voidedAt(r, created)})
	}
	stocks := map[int64]int64{}
	for _, r := range byID(t["stocks"]) {
		id, gid := r.Int("id"), r.Int("group_id")
		name := clean(r.Str("name"))
		shares, ok := decimal4(r.Str("number_of_shares"))
		date := epochDate(r.Str("purchase_date"))
		if !ref.groups[gid] || name == "" || !ok || shares == "0.0000" || date == nil {
			p.Report.issue("stocks: group, name, shares or date missing, skipped")
			continue
		}
		sold, _ := decimal4(r.Str("number_of_shares_sold"))
		price, _ := decimal4(r.Str("purchase_price"))
		current, ok := decimal4(r.Str("current_price"))
		if !ok || current == "0.0000" {
			current = price
		}
		created := timeOr(epoch(r.Str("created_on")), now)
		p.Stocks = append(p.Stocks, []any{id, id, gid, name, *date, shares, sold, price, current,
			r.Flag("is_a_back_dating_record"), p.createdBy(r, ref.users), created, voidedAt(r, created)})
		stocks[id] = gid
	}
	for _, r := range byID(t["stock_sales"]) {
		id, gid := r.Int("id"), r.Int("group_id")
		shares, ok := decimal4(r.Str("number_of_shares_sold"))
		date := epochDate(r.Str("sale_date"))
		stock := inGroup(p, "stock_sales", "stock_id", r, gid, stocks)
		if stock == nil || !ok || shares == "0.0000" || date == nil {
			p.Report.issue("stock_sales: stock, shares or date missing, skipped")
			continue
		}
		price, _ := decimal4(r.Str("sale_price_per_share"))
		created := timeOr(epoch(r.Str("created_on")), now)
		p.StockSales = append(p.StockSales, []any{id, id, gid, *stock, *date, shares, price,
			p.createdBy(r, ref.users), created, voidedAt(r, created)})
	}
	for _, r := range byID(t["money_market_investments"]) {
		id, gid := r.Int("id"), r.Int("group_id")
		inst := clean(r.Str("investment_institution_name"))
		date := epochDate(r.Str("investment_date"))
		if !ref.groups[gid] || date == nil {
			p.Report.issue("money_market_investments: group or date missing, skipped")
			continue
		}
		if inst == "" {
			inst = "Money market"
		}
		closed := r.Flag("is_closed")
		var cashIn any
		var cashInDate *time.Time
		if closed {
			cashIn = moneyOrZero(r.Str("cash_in_amount"))
			cashInDate = epochDate(r.Str("cash_in_date"))
		}
		amount := cents(r.Str("investment_amount")) + max(cents(r.Str("top_up_amount")), 0)
		created := timeOr(epoch(r.Str("created_on")), now)
		p.MoneyMarket = append(p.MoneyMarket, []any{id, id, gid, inst, *date, centsText(max(amount, 0)),
			clean(r.Str("description")), closed, cashIn, cashInDate, r.Flag("is_a_back_dating_record"),
			p.createdBy(r, ref.users), created, voidedAt(r, created)})
	}
}

func (p *Plan) meetings(t Tables, ref refSets, now time.Time) {
	meetings := map[int64]int64{}
	for _, r := range byID(t["meetings"]) {
		id, gid := r.Int("id"), r.Int("group_id")
		at := epoch(r.Str("date"))
		if !ref.groups[gid] || at == nil {
			p.Report.issue("meetings: group or date missing, skipped")
			continue
		}
		if !r.Flag("active") {
			p.Report.issue("meetings: deleted in legacy, skipped")
			continue
		}
		title := clean(r.Str("title"))
		if title == "" {
			title = "Meeting"
		}
		p.Meetings = append(p.Meetings, []any{id, id, gid, title, clean(r.Str("venue")), clean(r.Str("purpose")), *at,
			p.createdBy(r, ref.users), timeOr(epoch(r.Str("created_on")), *at)})
		meetings[id] = gid
	}
	seen := map[[2]int64]bool{}
	for _, r := range byID(t["atttendance"]) {
		mid, gid, member := r.Int("meeting_id"), r.Int("group_id"), r.Int("member_id")
		status := r.Int("atttendance_status")
		switch {
		case meetings[mid] != gid || ref.members[member] != gid:
			p.Report.issue("meeting_attendance: meeting or member missing, skipped")
			continue
		case status < 1 || status > 4:
			p.Report.issue("meeting_attendance: unknown status, skipped")
			continue
		case !r.Flag("active"), seen[[2]int64{mid, member}]:
			continue
		}
		seen[[2]int64{mid, member}] = true
		p.MeetingAttendance = append(p.MeetingAttendance, []any{mid, gid, member, int16(status)})
	}
	for kind, table := range map[string]string{"agenda": "agenda", "aob": "any_other_business"} {
		pos := map[int64]int32{}
		for _, r := range byID(t[table]) {
			mid := r.Int("meeting_id")
			name := clean(r.Str("name"))
			if meetings[mid] == 0 || meetings[mid] != r.Int("group_id") || name == "" || !r.Flag("active") {
				p.Report.issue("meeting_items: %s meeting missing, blank or deleted, skipped", table)
				continue
			}
			p.MeetingItems = append(p.MeetingItems, []any{mid, kind, name, pos[mid]})
			pos[mid]++
		}
	}
}

var transferKinds = map[int64]string{1: "contribution", 2: "fine", 3: "loan"}

// contributionTransfers returns transfer id -> group for the ledger link.
func (p *Plan) contributionTransfers(rows []Row, ref refSets, fines map[int64]int64, loans loanSet, now time.Time) map[int64]int64 {
	ids := map[int64]int64{}
	for _, r := range byID(rows) {
		id, gid, member := r.Int("id"), r.Int("group_id"), r.Int("member_id")
		if !ref.groups[gid] || ref.members[member] != gid {
			p.Report.issue("contribution_transfers: group or member missing, skipped")
			continue
		}
		amount := positiveMoney(r.Str("amount"))
		date := epochDate(r.Str("transfer_date"))
		if amount == "" || date == nil {
			p.Report.issue("contribution_transfers: bad amount or date, skipped")
			continue
		}
		contrib := func(col string) *int64 {
			if v := r.Int(col); v > 0 {
				if c := ref.contribs[v]; c != nil && c.groupID == gid {
					return &v
				}
			}
			return nil
		}
		var fromContrib, fromLoan *int64
		if v := r.Int("loan_from_id"); v > 0 {
			fromLoan = loans.in(v, gid)
		} else {
			fromContrib = contrib("contribution_from_id")
		}
		if fromContrib == nil && fromLoan == nil {
			p.Report.issue("contribution_transfers: source contribution or loan missing, skipped")
			continue
		}
		toMember := r.Int("transfer_to") == 4
		kindCode, toMembership := r.Int("transfer_to"), member
		if toMember {
			kindCode, toMembership = r.Int("member_transfer_to"), r.Int("member_to_id")
		}
		kind, ok := transferKinds[kindCode]
		if !ok || ref.members[toMembership] != gid {
			p.Report.issue("contribution_transfers: destination unknown, skipped")
			continue
		}
		var toContrib, toFine, toLoan *int64
		switch kind {
		case "contribution":
			toContrib = contrib("contribution_to_id")
		case "fine":
			toContrib = contrib("contribution_to_id")
			toFine = inGroup(p, "contribution_transfers", "fine_category_to_id", r, gid, fines)
		case "loan":
			toLoan = loans.in(r.Int("loan_to_id"), gid)
		}
		created := timeOr(epoch(r.Str("created_on")), now)
		p.ContributionTransfers = append(p.ContributionTransfers, []any{id, id, gid, member, fromContrib, fromLoan,
			toMember, kind, toMembership, toContrib, toFine, toLoan, amount, *date, clean(r.Str("description")),
			p.createdBy(r, ref.users), created, voidedAt(r, created)})
		ids[id] = gid
	}
	return ids
}

var notificationRefs = []string{"invoice_id", "deposit_id", "transaction_alert_id", "withdrawal_request_id",
	"withdrawal_approval_request_id", "loan_id", "withdrawal_id", "from_member_id"}

func (p *Plan) notifications(rows []Row, ref refSets, now time.Time) {
	for _, r := range byID(rows) {
		id, gid, member := r.Int("id"), r.Int("group_id"), r.Int("to_member_id")
		if !ref.groups[gid] || ref.members[member] != gid {
			p.Report.issue("notifications: group or member missing, skipped")
			continue
		}
		if r.Ptr("active") != nil && clean(r.Str("active")) == "0" {
			p.Report.issue("notifications: deleted in legacy, skipped")
			continue
		}
		user := func(col string) *int64 {
			if v := r.Int(col); ref.users[v] {
				return &v
			}
			return nil
		}
		cat, _ := strconv.ParseInt(clean(r.Str("category")), 10, 16)
		var size *int64
		if v, err := strconv.ParseInt(clean(r.Str("file_size")), 10, 64); err == nil {
			size = &v
		}
		p.Notifications = append(p.Notifications, []any{id, id, gid, member, user("to_user_id"), user("from_user_id"),
			clean(r.Str("subject")), clean(r.Str("message")), int16(cat), clean(r.Str("call_to_action")),
			clean(r.Str("call_to_action_link")), clean(r.Str("file_path")), clean(r.Str("file_type")), size,
			refsJSON(r, notificationRefs...), r.Flag("is_read"), timeOr(epoch(r.Str("created_on")), now)})
	}
}

func (p *Plan) billing(t Tables, ref refSets, now time.Time) {
	packages := map[int64]bool{}
	for _, r := range byID(t["billing_packages"]) {
		id := r.Int("id")
		name := clean(r.Str("name"))
		if name == "" {
			p.Report.issue("billing_packages: blank name, skipped")
			continue
		}
		settings := map[string]string{}
		for _, c := range []string{"rate", "rate_on", "monthly_smses", "quarterly_smses", "annual_smses", "enable_tax",
			"enable_extra_member_charge", "monthly_pay_over", "quarterly_pay_over", "annual_pay_over",
			"display_reports", "number_of_plans"} {
			if v := clean(r.Str(c)); v != "" {
				settings[c] = v
			}
		}
		b, _ := json.Marshal(settings)
		p.BillingPackages = append(p.BillingPackages, []any{id, id, name, clean(r.Str("slug")), int16(r.Int("billing_type")),
			moneyOrZero(r.Str("monthly_amount")), moneyOrZero(r.Str("quarterly_amount")), moneyOrZero(r.Str("annual_amount")),
			moneyOrZero(r.Str("percentage_tax")), r.Flag("is_default"), r.Ptr("active") == nil || r.Flag("active"), b})
		packages[id] = true
	}
	pkg := func(r Row) *int64 {
		if v := r.Int("billing_package_id"); packages[v] {
			return &v
		}
		return nil
	}
	invoices := map[int64]int64{}
	for _, r := range byID(t["billing_invoices"]) {
		id, gid := r.Int("id"), r.Int("group_id")
		if !ref.groups[gid] || !r.Flag("active") {
			p.Report.issue("billing_invoices: group missing or deleted, skipped")
			continue
		}
		amount, paid := cents(r.Str("amount")), cents(r.Str("amount_paid"))
		p.BillingInvoices = append(p.BillingInvoices, []any{id, id, gid, clean(r.Str("billing_invoice_number")),
			epochDate(r.Str("billing_date")), epochDate(r.Str("due_date")), moneyOrZero(r.Str("amount")),
			moneyOrZero(r.Str("tax")), moneyOrZero(r.Str("prorated_amount")), moneyOrZero(r.Str("amount_paid")),
			int16(r.Int("billing_cycle")), pkg(r), amount > 0 && paid >= amount, timeOr(epoch(r.Str("created_on")), now)})
		invoices[id] = gid
	}
	for _, r := range byID(t["billing_payments"]) {
		id, gid := r.Int("id"), r.Int("group_id")
		if !ref.groups[gid] || !r.Flag("active") {
			p.Report.issue("billing_payments: group missing or deleted, skipped")
			continue
		}
		var invoice *int64
		if v := r.Int("billing_invoice_id"); invoices[v] == gid && v > 0 {
			invoice = &v
		}
		p.BillingPayments = append(p.BillingPayments, []any{id, id, gid, clean(r.Str("billing_receipt_number")),
			epochDate(r.Str("receipt_date")), moneyOrZero(r.Str("amount")), moneyOrZero(r.Str("tax")),
			int16(r.Int("payment_method")), clean(r.Str("ipn_transaction_code")), invoice, pkg(r),
			clean(r.Str("description")), timeOr(epoch(r.Str("created_on")), now)})
	}
}

// sweepLinks run after the copy: money rows and loan rows point at the
// milestone 8 tables through the legacy ids kept in refs. Stocks and money
// market investments take their account from the purchase withdrawal.
var sweepLinks = []string{
	`UPDATE deposits x SET asset_id = a.id FROM assets a WHERE a.id = (x.refs->>'asset_id')::bigint AND a.group_id = x.group_id`,
	`UPDATE deposits x SET stock_id = a.id FROM stocks a WHERE a.id = (x.refs->>'stock_id')::bigint AND a.group_id = x.group_id`,
	`UPDATE deposits x SET stock_sale_id = a.id FROM stock_sales a WHERE a.id = (x.refs->>'stock_sale_id')::bigint AND a.group_id = x.group_id`,
	`UPDATE deposits x SET money_market_investment_id = a.id FROM money_market_investments a
		WHERE a.id = (x.refs->>'money_market_investment_id')::bigint AND a.group_id = x.group_id`,
	`UPDATE deposits x SET meeting_id = a.id FROM meetings a WHERE a.id = (x.refs->>'meeting_id')::bigint AND a.group_id = x.group_id`,
	`UPDATE withdrawals x SET asset_id = a.id FROM assets a WHERE a.id = (x.refs->>'asset_id')::bigint AND a.group_id = x.group_id`,
	`UPDATE withdrawals x SET stock_id = a.id FROM stocks a WHERE a.id = (x.refs->>'stock_id')::bigint AND a.group_id = x.group_id`,
	`UPDATE withdrawals x SET money_market_investment_id = a.id FROM money_market_investments a
		WHERE a.id = (x.refs->>'money_market_investment_id')::bigint AND a.group_id = x.group_id`,
	`UPDATE withdrawals x SET meeting_id = a.id FROM meetings a WHERE a.id = (x.refs->>'meeting_id')::bigint AND a.group_id = x.group_id`,
	`UPDATE loan_charges x SET contribution_transfer_id = t.id FROM contribution_transfers t
		WHERE t.id = (x.refs->>'contribution_transfer_id')::bigint AND t.group_id = x.group_id`,
	`UPDATE loan_repayments x SET contribution_transfer_id = t.id FROM contribution_transfers t
		WHERE t.id = (x.refs->>'incoming_contribution_transfer_id')::bigint AND t.group_id = x.group_id`,
	`UPDATE stocks s SET account_id = w.account_id FROM withdrawals w
		WHERE w.stock_id = s.id AND w.kind = 'stock_purchase' AND w.account_id IS NOT NULL`,
	`UPDATE stock_sales s SET account_id = d.account_id FROM deposits d WHERE d.stock_sale_id = s.id AND d.account_id IS NOT NULL`,
	`UPDATE money_market_investments m SET account_id = w.account_id FROM withdrawals w
		WHERE w.money_market_investment_id = m.id AND w.kind = 'money_market_investment' AND w.account_id IS NOT NULL`,
}
