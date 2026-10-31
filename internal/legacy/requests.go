package legacy

import (
	"encoding/json"
	"strings"
	"time"
)

// Withdrawal requests and their approvals (legacy e-wallet).

var WithdrawalRequestCols = []string{"id", "legacy_id", "group_id", "reference_number", "kind", "status",
	"requested_by", "account_id", "amount", "description", "expense_category_id", "membership_id",
	"contribution_id", "recipient", "decided_at", "decline_reason", "refs", "created_by", "created_at", "updated_at"}

// withdrawal_for codes. The legacy code disagrees with itself on whether 2
// or 3 is the refund and which is the merry-go-round payout; both pay a
// member out of their contributions.
var withdrawalFor = map[int64]string{
	1: "expense", 2: "contribution_refund", 3: "contribution_refund", 4: "loan_disbursement", 5: "funds_transfer",
}

var withdrawalRequestRefs = []string{"loan_type_id", "loan_application_id"}

// WithdrawalRequestStatus maps the legacy status flags. status 0 is
// waiting for signatories, 1 decided by them (declined unless approved),
// 2 approved and handed to the payment API.
func WithdrawalRequestStatus(r Row) string {
	switch {
	case r.Flag("is_disbursed"):
		return "paid"
	case r.Flag("is_disbursement_declined"):
		return "failed"
	case r.Int("status") == 2 || (r.Int("status") == 1 && r.Flag("is_approved")):
		return "approved"
	case r.Int("status") == 1 && strings.Contains(strings.ToLower(r.Str("decline_reason")), "cancel"):
		return "cancelled"
	case r.Int("status") == 1:
		return "declined"
	case !r.Flag("active"):
		return "cancelled"
	default:
		return "pending"
	}
}

// withdrawalRequests imports requests. The e-wallet always paid out of the
// group's bank account, so the account is the group's only bank account
// when there is one.
func (p *Plan) withdrawalRequests(rows []Row, ref refSets, ix *accountIndex, expense map[int64]int64, now time.Time) map[int64]int64 {
	ids := map[int64]int64{}
	for _, r := range byID(rows) {
		id := r.Int("id")
		kind, ok := withdrawalFor[r.Int("withdrawal_for")]
		if !ok {
			p.Report.issue("withdrawal_requests: unknown withdrawal_for %d, skipped", r.Int("withdrawal_for"))
			continue
		}
		m, ok := p.common("withdrawal_requests", r, "request_date", ref, now)
		if !ok {
			continue
		}
		var member *int64
		if v := r.Int("recipient_member_id"); v > 0 {
			if ref.members[v] == m.gid {
				member = &v
			} else {
				p.Report.issue("withdrawal_requests: recipient member missing or in another group, cleared")
			}
		}
		status := WithdrawalRequestStatus(r)
		var decided *time.Time
		if status != "pending" {
			decided = epoch(r.Str("approved_on"))
			if decided == nil {
				decided = epoch(r.Str("declined_on"))
			}
			if decided == nil {
				decided = epoch(r.Str("modified_on"))
			}
		}
		acct, _ := ix.resolve(m.gid, "bank", "")
		if acct == nil {
			p.Report.issue("withdrawal_requests: group has no single bank account, kept without account")
		}
		p.WithdrawalRequests = append(p.WithdrawalRequests, []any{id, id, m.gid, clean(r.Str("reference_number")),
			kind, status, m.member, acct, m.amount, clean(r.Str("description")),
			inGroup(p, "withdrawal_requests", "expense_category_id", r, m.gid, expense), member, m.contrib,
			requestRecipient(r), decided, clean(r.Str("decline_reason")), refsJSON(r, withdrawalRequestRefs...),
			p.createdBy(r, ref.users), m.created, timeOr(epoch(r.Str("modified_on")), m.created)})
		ids[id] = m.gid
	}
	return ids
}

// requestRecipient keeps who the money was for, as the API stores it.
func requestRecipient(r Row) []byte {
	out := map[string]any{}
	switch r.Int("recipient") {
	case 1:
		out["type"], out["phone"] = "mobile", clean(r.Str("recipient_phone_number"))
	case 2:
		out["type"], out["paybill"] = "paybill", clean(r.Str("recipient_paybill_number"))
		if v := clean(r.Str("recipient_paybill_account_number")); v != "" && v != "0" {
			out["account_number"] = v
		}
	case 3:
		out["type"], out["bank_id"] = "bank", r.Int("recipient_bank_id")
		out["account_number"], out["account_name"] = clean(r.Str("recipient_account_number")), clean(r.Str("recipient_account_name"))
	}
	for k, v := range out {
		if v == "" {
			delete(out, k)
		}
	}
	b, _ := json.Marshal(out)
	return b
}

var WithdrawalApprovalCols = []string{"id", "legacy_id", "group_id", "withdrawal_request_id", "membership_id",
	"decision", "decided_at", "comment", "decided_by", "created_at"}

func (p *Plan) withdrawalApprovals(rows []Row, ref refSets, requests map[int64]int64, now time.Time) {
	seen := map[[2]int64]bool{}
	for _, r := range byID(rows) {
		id, gid, req, mid := r.Int("id"), r.Int("group_id"), r.Int("withdrawal_request_id"), r.Int("member_id")
		if g, ok := requests[req]; !ok || g != gid {
			p.Report.issue("withdrawal_approvals: request missing or in another group, skipped")
			continue
		}
		if ref.members[mid] != gid {
			p.Report.issue("withdrawal_approvals: member missing or in another group, skipped")
			continue
		}
		if seen[[2]int64{req, mid}] {
			p.Report.issue("withdrawal_approvals: duplicate for the same member, skipped")
			continue
		}
		seen[[2]int64{req, mid}] = true
		decision, decided := "pending", (*time.Time)(nil)
		switch {
		case r.Flag("is_declined"):
			decision, decided = "declined", epoch(r.Str("declined_on"))
		case r.Flag("is_approved"):
			decision, decided = "approved", epoch(r.Str("approved_on"))
		}
		created := timeOr(epoch(r.Str("created_on")), now)
		if decision != "pending" && decided == nil {
			decided = &created
		}
		var by *int64
		if decision != "pending" {
			if v := r.Int("modified_by"); ref.users[v] {
				by = &v
			} else {
				by = p.createdBy(r, ref.users)
			}
		}
		p.WithdrawalApprovals = append(p.WithdrawalApprovals, []any{id, id, gid, req, mid, decision, decided,
			clean(r.Str("comments")), by, created})
	}
}
