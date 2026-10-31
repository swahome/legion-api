package mobile

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/swahome/legion-api/internal/store"
)

// defaultLegacyType stands in for entries created after the import, which
// have no legacy transaction type.
var defaultLegacyType = map[store.LedgerEntryType]int16{
	"contribution_invoice": 1, "contribution_payment": 9, "contribution_refund": 21,
	"contribution_transfer_out": 25, "contribution_transfer_in": 26, "contribution_to_loan_transfer": 30,
}

// contributionStatement is the last six months of a member's contribution
// statement: balance brought forward, each entry, and totals.
//
// Groups with arrears disabled got a deposits-only "savings" statement in
// the legacy app; that view needs deposits (milestone 3), so they get the
// regular statement for now.
func (s *Server) contributionStatement(ctx context.Context, c *call, g *member) (any, error) {
	memberID := g.m.ID
	if !g.flag("enable_member_information_privacy") {
		if id := c.req.Int("member_id"); id != 0 {
			memberID = id
		}
	}
	// The legacy code looked the member up in any group; this one only
	// finds members of the caller's group.
	rows, err := s.q.MobileGroupMembers(ctx, store.MobileGroupMembersParams{GroupID: g.group.ID, MembershipID: &memberID, Lim: 1})
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return timed(status(0, "Could not find group member details"), s.now()), nil
	}
	m := rows[0]

	now := s.now().In(g.loc)
	from := now.AddDate(0, -6, 0)
	fromDay := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.UTC)
	entries, err := s.q.MobileStatementEntries(ctx, store.MobileStatementEntriesParams{GroupID: g.group.ID, MembershipID: &memberID})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	// Invoices and transfers out add to "payable"; payments and transfers
	// in add to "paid", and refunds and moves to loans or other
	// contributions take from it, as the legacy totals did.
	var payable, paid, balance float64
	var openPayable, openPaid, openBalance float64
	body := []any{}
	for _, e := range entries {
		amt := amount(e.Amount)
		t := defaultLegacyType[e.EntryType]
		if e.LegacyType != nil {
			t = *e.LegacyType
		}
		var desc string
		var dPayable, dPaid float64
		switch {
		case t == 1:
			desc, dPayable = "Contribution invoice "+e.ContributionName, amt
		case t >= 21 && t <= 24:
			desc, dPaid = "Contribution refund "+e.ContributionName+" refund", -amt
		case t == 30:
			desc, dPaid = "Contribution transfer to loan"+e.ContributionName+" to loan", -amt
		case t == 9 || t == 10 || t == 11 || t == 15:
			desc, dPaid = "Contribution payment "+e.ContributionName, amt
		case t == 25:
			desc, dPayable = "Contribution transfer from  "+e.ContributionName, amt
		case t == 26:
			desc, dPaid = "Contribution transfer to", amt
		case t == 27:
			desc, dPaid = "Contribution transfer to contribution", -amt
		default:
			continue
		}
		payable += dPayable
		paid += dPaid
		balance += float64(e.Sign) * amt
		if e.EntryDate.Before(fromDay) {
			openPayable, openPaid, openBalance = payable, paid, balance
			continue
		}
		row := map[string]any{"description": desc, "date": shortDate(e.EntryDate), "payable": 0, "paid": amt,
			"balance": balance, "invoice_id": nil, "deposit_id": nil}
		if t == 1 {
			row["payable"], row["paid"] = amt, 0
		}
		body = append(body, row)
	}

	name := m.FirstName + " "
	if m.MiddleName != "" {
		name += m.MiddleName + " "
	}
	name += m.LastName
	role := "Member"
	if m.IsOfficial {
		role = "Admin"
	} else if m.RoleName != nil {
		role = *m.RoleName
	}
	return map[string]any{
		"status":  1,
		"message": "Contribution statement",
		"time":    s.now().Unix(),
		"member_datails": map[string]any{
			"name":   name,
			"role":   role,
			"email":  deref(m.Email),
			"phone":  legacyPhone(m.Phone),
			"avatar": nullable(m.Avatar),
			"group":  g.group.Name,
		},
		"statement_details": map[string]any{
			"statement_as_at":       shortDate(now),
			"statement_period_from": shortDate(from),
			"statement_period_to":   shortDate(now),
		},
		"statement_header": map[string]any{
			"description": "Balance B/F",
			"date":        shortDate(from),
			"payable":     openPayable,
			"paid":        openPaid,
			"balance":     openBalance,
		},
		"statement_body": body,
		"statement_footer": map[string]any{
			"description": "Totals",
			"payable":     payable,
			"paid":        paid,
			"balance":     balance,
		},
	}, nil
}
