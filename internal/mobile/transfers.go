package mobile

import (
	"context"

	"github.com/swahome/legion-api/internal/books"
	"github.com/swahome/legion-api/internal/store"
)

// Contribution transfers: deposits/record_contribution_transfer,
// void_contribution_transfer and get_group_contribution_transfers. The
// legacy checks run first, with their messages; books then checks that the
// money is there (what was paid into the contribution, or repaid on the
// loan) and that every id belongs to the group.

var transferToNames = map[store.TransferKind]string{"contribution": "Contribution share", "fine": "Fine payment", "loan": "Loan share"}

var transferKindCodes = map[string]store.TransferKind{"1": "contribution", "2": "fine", "3": "loan"}

func numeric(v string) bool { return appID(v) != nil }

// appTransfer reads the request, or returns the legacy error message.
func appTransfer(c *call, g *member) (books.Transfer, string) {
	r := c.req
	t := books.Transfer{Date: appDate(r.Str("transfer_date"), g.loc), Amount: appAmount(r.Str("amount")),
		Description: r.Str("description")}
	from, to := r.Str("contribution_from_id"), r.Str("transfer_to")
	switch {
	case r.Str("transfer_date") == "" || t.Date == nil:
		return t, "Transafer date is missing"
	case from == "":
		return t, "Contribution from is required"
	case to == "1" && from == r.Str("contribution_to_id"):
		return t, "Contribution from should not be the same as contribution to"
	case from != "loan" && !numeric(from):
		return t, "Contribution from id should be numeric"
	case r.Str("member_id") == "":
		return t, "Member id should be numeric"
	case !numeric(r.Str("member_id")):
		return t, "Member id should be numeric"
	case r.Str("amount") == "":
		return t, "Amount is required"
	case t.Amount == "":
		return t, "Valid amount is required"
	}
	t.MemberID = r.Int("member_id")
	if from == "loan" {
		switch {
		case r.Str("loan_from_id") == "":
			return t, "Loan from is required"
		case !numeric(r.Str("loan_from_id")):
			return t, "Loan id should be numeric"
		}
		t.FromLoanID = appID(r.Str("loan_from_id"))
	} else {
		t.FromContributionID = appID(from)
	}
	kindCode, contributionKey, fineKey, loanKey := to, "contribution_to_id", "fine_category_to_id", "loan_to_id"
	switch to {
	case "1", "2", "3":
	case "4":
		switch {
		case r.Str("member_to_id") == "":
			return t, "Memebr to id is required"
		case !numeric(r.Str("member_to_id")):
			return t, "Member to id should be numeric"
		case r.Str("member_to_id") == r.Str("member_id"):
			return t, "member to id should not be the same as member id"
		case r.Str("member_transfer_to") == "":
			return t, "Member transfer to is required"
		}
		t.ToMemberID = appID(r.Str("member_to_id"))
		kindCode = r.Str("member_transfer_to")
		contributionKey, fineKey, loanKey = "member_contribution_to_id", "member_fine_category_to_id", "member_loan_to_id"
	default:
		return t, "Transfer to is required"
	}
	t.ToKind = transferKindCodes[kindCode]
	switch t.ToKind {
	case "contribution":
		if r.Str(contributionKey) == "" {
			return t, "Contribution to id is required"
		}
		if t.ToContributionID = appID(r.Str(contributionKey)); t.ToContributionID == nil {
			return t, "Contribution to id should be numeric"
		}
	case "fine":
		if r.Str(fineKey) == "" {
			return t, "Fine category id is required"
		}
		t.ToContributionID, t.ToFineCategoryID = appFineRef(r.Str(fineKey))
	case "loan":
		if r.Str(loanKey) == "" {
			return t, "Loan to id is required"
		}
		if t.ToLoanID = appID(r.Str(loanKey)); t.ToLoanID == nil {
			return t, "Loan to id should be numeric"
		}
	default:
		return t, "Member transfer to should be numeric"
	}
	return t, ""
}

func (s *Server) recordContributionTransfer(ctx context.Context, c *call, g *member) (any, error) {
	t, msg := appTransfer(c, g)
	if msg != "" {
		return timed(status(0, "Entries are invalid: "+msg), s.now()), nil
	}
	err := s.inTx(ctx, func(q *store.Queries) error {
		_, err := books.RecordContributionTransfer(ctx, q, g.group.ID, g.user.ID, t)
		return err
	})
	if be, ok := asBooksError(err); ok {
		return timed(status(0, "Entries are invalid: "+be.Msg), s.now()), nil
	}
	if err != nil {
		return nil, err
	}
	return timed(status(1, "Contribution transfer recorded successfully"), s.now()), nil
}

func (s *Server) voidContributionTransfer(ctx context.Context, c *call, g *member) (any, error) {
	id := appID(c.req.Str("id"))
	if id == nil {
		return timed(status(0, "Contribution transfer details are missing"), s.now()), nil
	}
	err := s.inTx(ctx, func(q *store.Queries) error { return books.VoidContributionTransfer(ctx, q, g.group.ID, g.user.ID, *id) })
	if be, ok := asBooksError(err); ok {
		if be.Code == "not_found" || be.Code == "already_voided" {
			return timed(status(0, "Could not find contribution transfer details"), s.now()), nil
		}
		return timed(status(0, be.Msg), s.now()), nil
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"status": 1, "time": s.now().Unix()}, nil
}

func (s *Server) getGroupContributionTransfers(ctx context.Context, c *call, g *member) (any, error) {
	limit, offset := pageOf(c, 20)
	p := store.ListContributionTransfersParams{GroupID: g.group.ID, Lim: limit, Off: offset}
	if !g.official() && g.flag("enable_member_information_privacy") {
		p.MembershipID = &g.m.ID
	}
	rows, err := s.q.ListContributionTransfers(ctx, p)
	if err != nil {
		return nil, err
	}
	out := []any{}
	for _, r := range rows {
		desc := "Contribution transfer to "
		if r.FromLoanID != nil {
			desc = "Loan payment transfer to "
		}
		if r.ToMember {
			desc += "Another member"
		} else {
			desc += transferToNames[r.ToKind]
		}
		out = append(out, map[string]any{"id": r.ID, "date": mobileShortTime(r.TransferDate), "member": r.MemberName,
			"description": desc, "amount": r.Amount})
	}
	return timed(map[string]any{"status": 1, "message": "deposit list", "deposits": out}, s.now()), nil
}
