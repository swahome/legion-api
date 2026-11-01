package legacy

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/swahome/legion-api/internal/auth"
)

// Bank and MPesa transaction alerts, member payment requests (MPesa
// prompts) and bank staff.

var TransactionAlertCols = []string{"id", "legacy_id", "source", "provider_ref", "bank_id", "account_number",
	"group_id", "account_id", "direction", "amount", "currency", "transaction_at", "reference", "particulars",
	"status", "note", "raw", "created_at", "updated_at"}

// alertSource names the feed an alert came from by which of the legacy
// per-feed ids it carries.
func alertSource(r Row) string {
	switch {
	case r.Int("coop_bank_transaction_alert_id") > 0:
		return "coop"
	case r.Int("equity_bank_transaction_alert_id") > 0:
		return "equity"
	case r.Int("safaricom_transaction_alert_id") > 0:
		return "mpesa"
	case r.Int("online_banking_transaction_alert_id") > 0:
		return "chamasoft_direct"
	default:
		return "legacy"
	}
}

// transactionAlerts keeps legacy ids. Rows without an amount or a date
// (the anonymized dump blanks them all) are skipped and counted.
func (p *Plan) transactionAlerts(rows []Row, ref refSets, ix *accountIndex, now time.Time) map[int64]int64 {
	ids := map[int64]int64{}
	seen := map[string]bool{}
	for _, r := range byID(rows) {
		id := r.Int("id")
		amount, ok := normAmount(r.Str("amount"))
		if !ok {
			p.Report.issue("transaction_alerts: no amount, skipped")
			continue
		}
		at := epoch(r.Str("transaction_date"))
		if at == nil {
			at = epoch(r.Str("created_on"))
		}
		if at == nil {
			p.Report.issue("transaction_alerts: no date, skipped")
			continue
		}
		direction := int16(1)
		if r.Int("type") == 2 {
			direction = -1
		}
		source := alertSource(r)
		pref := clean(r.Str("transaction_id"))
		if pref == "" {
			pref = "legacy-" + strconv.FormatInt(id, 10)
		}
		key := fmt.Sprintf("%s|%s|%d", source, pref, direction)
		if seen[key] {
			p.Report.issue("transaction_alerts: duplicate transaction id, kept with the legacy id appended")
			pref += "-" + strconv.FormatInt(id, 10)
			key = fmt.Sprintf("%s|%s|%d", source, pref, direction)
		}
		seen[key] = true

		var bank, group, account *int64
		if v := r.Int("bank_id"); v > 0 && ref.banks[v] {
			bank = &v
		}
		if v := r.Int("group_id"); v > 0 {
			if ref.groups[v] {
				group = &v
				if raw := clean(r.Str("account_id")); raw != "" {
					account, _ = ix.resolve(v, "bank", raw)
				}
			} else {
				p.Report.issue("transaction_alerts: group missing, kept unassigned")
			}
		}
		status, note := "unreconciled", ""
		switch {
		case !r.Flag("active"):
			status, note = "ignored", "Inactive in the old system."
		case r.Flag("reconciled") || r.Flag("marked_as_reconciled") || r.Flag("is_merged"):
			status = "reconciled"
		}
		particulars := clean(r.Str("particulars"))
		if d := clean(r.Str("description")); d != "" && d != particulars {
			particulars = strings.TrimSpace(particulars + " " + d)
		}
		currency := strings.ToUpper(clean(r.Str("currency")))
		if currency == "" {
			currency = "KES"
		}
		created := timeOr(epoch(r.Str("created_on")), *at)
		updated := timeOr(epoch(r.Str("modified_on")), created)
		raw, _ := json.Marshal(map[string]any{"legacy": json.RawMessage(extras(r, set()))})
		p.TransactionAlerts = append(p.TransactionAlerts, []any{id, id, source, pref, bank,
			clean(r.Str("account_number")), group, account, direction, amount, currency, *at,
			clean(r.Str("reference_number")), particulars, status, note, raw, created, updated})
		gid := int64(0)
		if group != nil {
			gid = *group
		}
		ids[id] = gid
	}
	return ids
}

func normAmount(s string) (string, bool) {
	f, err := strconv.ParseFloat(strings.ReplaceAll(clean(s), ",", ""), 64)
	if err != nil || f == 0 {
		return "", false
	}
	if f < 0 {
		f = -f
	}
	return strconv.FormatFloat(f, 'f', 2, 64), true
}

var PaymentRequestCols = []string{"id", "legacy_id", "group_id", "membership_id", "user_id", "account_id",
	"account_number", "phone", "amount", "reference", "lines", "channel", "status", "response_code",
	"response_description", "result_code", "result_description", "provider_ref", "transaction_alert_id",
	"created_at", "updated_at"}

// paymentLine is the shape payments.Line stores.
type paymentLine struct {
	Kind           string `json:"kind"`
	ContributionID *int64 `json:"contribution_id,omitempty"`
	FineCategoryID *int64 `json:"fine_category_id,omitempty"`
	LoanID         *int64 `json:"loan_id,omitempty"`
	Amount         string `json:"amount"`
	Description    string `json:"description,omitempty"`
}

// paymentRequests imports online_payment_requests. The legacy row keeps
// its items as PHP-serialized arrays that share keys: payment_for[k]
// (1 contribution, 2 fine, 3 loan, 4 other) with contribution_ids[k],
// fine_category_ids[k] ("contribution-ID" for a late fine or
// "fine_category-ID"), loan_ids[k], descriptions[k] and amounts[k].
func (p *Plan) paymentRequests(rows []Row, ref refSets, ix *accountIndex, fines map[int64]int64, loanSet loanSet,
	alerts map[int64]int64, opt Options, now time.Time) {
	refs := map[string]bool{}
	var next int64
	for _, r := range byID(rows) {
		lid, gid := r.Int("id"), r.Int("group_id")
		if !ref.groups[gid] {
			p.Report.issue("online_payment_requests: group missing, skipped")
			continue
		}
		amount, ok := normAmount(r.Str("amount"))
		if !ok {
			p.Report.issue("online_payment_requests: no amount, skipped")
			continue
		}
		var member, user, account, alert *int64
		if v := r.Int("member_id"); v > 0 && ref.members[v] == gid {
			member = &v
		}
		if v := r.Int("user_id"); v > 0 && ref.users[v] {
			user = &v
		}
		if raw := clean(r.Str("account_id")); raw != "" {
			account, _ = ix.resolve(gid, "bank", raw)
		}
		if v := r.Int("transaction_alert_id"); v > 0 {
			if g, ok := alerts[v]; ok && (g == gid || g == 0) {
				alert = &v
			}
		}
		reference := clean(r.Str("reference_number"))
		if reference == "" {
			reference = "legacy-" + strconv.FormatInt(lid, 10)
		}
		if refs[reference] {
			p.Report.issue("online_payment_requests: duplicate reference, legacy id appended")
			reference += "-" + strconv.FormatInt(lid, 10)
		}
		refs[reference] = true

		phone := clean(r.Str("phone"))
		if e, err := auth.NormalizePhone(phone, opt.DefaultCallingCode); err == nil {
			phone = e
		}
		status := "failed"
		switch r.Int("status") {
		case 3:
			status = "paid"
		case 2:
		default:
			p.Report.issue("online_payment_requests: never answered, imported as failed")
		}
		result := clean(r.Str("result_description"))
		if status == "failed" && result == "" {
			result = "No answer from MPesa."
		}
		var receipt *string
		if v := clean(r.Str("transaction_id")); v != "" && v != "0" {
			receipt = &v
		}
		lines := p.paymentLines(r, gid, ref, fines, loanSet)
		created := timeOr(epoch(r.Str("created_on")), now)
		updated := timeOr(epoch(r.Str("modified_on")), created)
		next++
		p.PaymentRequests = append(p.PaymentRequests, []any{next, lid, gid, member, user, account,
			clean(r.Str("account_number")), phone, amount, reference, lines, "legacy", status,
			clean(r.Str("response_code")), clean(r.Str("response_description")), clean(r.Str("result_code")),
			result, receipt, alert, created, updated})
	}
}

func (p *Plan) paymentLines(r Row, gid int64, ref refSets, fines map[int64]int64, loanSet loanSet) []byte {
	kinds, amounts := phpArray(r.Str("payment_for")), phpArray(r.Str("amounts"))
	contribs, fineIDs := phpArray(r.Str("contribution_ids")), phpArray(r.Str("fine_category_ids"))
	loanIDs, descs := phpArray(r.Str("loan_ids")), phpArray(r.Str("descriptions"))
	lines := []paymentLine{}
	for _, k := range phpKeys(r.Str("amounts")) {
		amt, ok := normAmount(amounts[k])
		if !ok {
			continue
		}
		l := paymentLine{Amount: amt, Description: descs[k]}
		num := func(s string) int64 { n, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64); return n }
		switch kinds[k] {
		case "1":
			l.Kind = "contribution"
			if c := ref.contribs[num(contribs[k])]; c != nil && c.groupID == gid {
				v := num(contribs[k])
				l.ContributionID = &v
			}
		case "2":
			l.Kind = "fine"
			what, id, _ := strings.Cut(fineIDs[k], "-")
			v := num(id)
			if what == "contribution" {
				if c := ref.contribs[v]; c != nil && c.groupID == gid {
					l.ContributionID = &v
				}
			} else if fines[v] == gid && v > 0 {
				l.FineCategoryID = &v
			}
		case "3":
			l.Kind = "loan"
			l.LoanID = loanSet.in(num(loanIDs[k]), gid)
		default:
			l.Kind = "misc"
		}
		lines = append(lines, l)
	}
	b, _ := json.Marshal(lines)
	return b
}

// phpArray reads a PHP-serialized flat array (a:2:{i:0;s:2:"10";i:1;i:5;})
// into key -> value text. Anything else gives an empty map.
func phpArray(s string) map[string]string {
	out := map[string]string{}
	vals := phpTokens(s)
	for i := 0; i+1 < len(vals); i += 2 {
		out[vals[i]] = vals[i+1]
	}
	return out
}

// phpKeys gives the array's keys in order.
func phpKeys(s string) []string {
	var keys []string
	vals := phpTokens(s)
	for i := 0; i+1 < len(vals); i += 2 {
		keys = append(keys, vals[i])
	}
	return keys
}

func phpTokens(s string) []string {
	s = strings.TrimSpace(s)
	open := strings.Index(s, "{")
	if !strings.HasPrefix(s, "a:") || open < 0 || !strings.HasSuffix(s, "}") {
		return nil
	}
	body := s[open+1 : len(s)-1]
	var out []string
	for body != "" {
		switch {
		case strings.HasPrefix(body, "s:"):
			colon := strings.Index(body[2:], ":")
			if colon < 0 {
				return nil
			}
			n, err := strconv.Atoi(body[2 : 2+colon])
			start := 2 + colon + 2 // past :"
			if err != nil || start+n+2 > len(body) {
				return nil
			}
			out = append(out, body[start:start+n])
			body = body[start+n+2:] // past ";
		case strings.HasPrefix(body, "i:"), strings.HasPrefix(body, "d:"), strings.HasPrefix(body, "b:"):
			semi := strings.Index(body, ";")
			if semi < 0 {
				return nil
			}
			out = append(out, body[2:semi])
			body = body[semi+1:]
		case strings.HasPrefix(body, "N;"):
			out = append(out, "")
			body = body[2:]
		default:
			return nil // nested arrays and objects are not used here
		}
	}
	return out
}

var BankUserCols = []string{"bank_id", "user_id", "role", "created_by", "created_at"}

// bankUsers pairs users with banks. bank_user_role_pairings gives the
// role: 1 Bank Admin, anything else staff (2 is Customer Care).
func (p *Plan) bankUsers(pairings, roles []Row, ref refSets, now time.Time) {
	admin := map[int64]bool{}
	for _, r := range roles {
		if r.Int("user_role_id") == 1 {
			admin[r.Int("user_id")] = true
		}
	}
	seen := map[[2]int64]bool{}
	for _, r := range byID(pairings) {
		uid, bid := r.Int("user_id"), r.Int("bank_id")
		if !ref.users[uid] || !ref.banks[bid] {
			p.Report.issue("user_bank_pairings: user or bank missing, skipped")
			continue
		}
		if seen[[2]int64{bid, uid}] {
			continue
		}
		seen[[2]int64{bid, uid}] = true
		role := "staff"
		if admin[uid] {
			role = "admin"
		}
		p.BankUsers = append(p.BankUsers, []any{bid, uid, role, p.createdBy(r, ref.users),
			timeOr(epoch(r.Str("created_on")), now)})
	}
}
