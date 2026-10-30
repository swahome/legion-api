package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/swahome/legion-api/internal/store"
)

// Labels shown on statements, from the legacy statement_transaction_names.
var entryLabels = map[store.LedgerEntryType]string{
	store.LedgerEntryTypeContributionInvoice:        "Contribution invoice",
	store.LedgerEntryTypeContributionPayment:        "Contribution payment",
	store.LedgerEntryTypeContributionRefund:         "Contribution refund",
	store.LedgerEntryTypeContributionTransferIn:     "Contribution transfer in",
	store.LedgerEntryTypeContributionTransferOut:    "Contribution transfer out",
	store.LedgerEntryTypeContributionToLoanTransfer: "Contribution transfer to loan",
	store.LedgerEntryTypeContributionFineInvoice:    "Contribution fine",
	store.LedgerEntryTypeFineInvoice:                "Fine",
	store.LedgerEntryTypeMiscInvoice:                "Miscellaneous invoice",
	store.LedgerEntryTypeFinePayment:                "Fine payment",
	store.LedgerEntryTypeMiscPayment:                "Miscellaneous payment",
	store.LedgerEntryTypeContributionToFineTransfer: "Contribution transfer to fine",
	store.LedgerEntryTypeLoanToContributionTransfer: "Loan transfer to contribution",
	store.LedgerEntryTypeLoanToFineTransfer:         "Loan transfer to fine",
}

type statementEntry struct {
	ID               int64   `json:"id"`
	Date             Date    `json:"date"`
	Type             string  `json:"type"`
	Label            string  `json:"label"`
	ContributionID   *int64  `json:"contribution_id"`
	ContributionName *string `json:"contribution_name"`
	Description      string  `json:"description"`
	// Debit increases what the member owes; credit reduces it.
	Debit       *string `json:"debit"`
	Credit      *string `json:"credit"`
	Balance     string  `json:"balance"`
	IsBackdated bool    `json:"is_backdated"`
	DueDate     *Date   `json:"due_date,omitempty"`
}

type contributionTotals struct {
	ContributionID   *int64  `json:"contribution_id"`
	ContributionName *string `json:"contribution_name"`
	Payable          string  `json:"payable"`
	Paid             string  `json:"paid"`
	Balance          string  `json:"balance"`
}

type statementResponse struct {
	Member         memberResponse       `json:"member"`
	Currency       string               `json:"currency"`
	From           *Date                `json:"from"`
	To             *Date                `json:"to"`
	OpeningBalance string               `json:"opening_balance"`
	ClosingBalance string               `json:"closing_balance"`
	Entries        []statementEntry     `json:"entries"`
	Totals         []contributionTotals `json:"totals"`
}

// memberStatement is the contribution statement for one member, with a
// running balance (positive means the member owes the group). Query
// parameters: from, to (YYYY-MM-DD) and contribution_id.
func (s *Server) memberStatement(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a := accessFrom(ctx)
	mid, ok := pathID(r, "memberID")
	if !ok || !a.canSeeMember(mid) {
		writeError(w, http.StatusNotFound, "member_not_found", "Member not found.")
		return
	}
	member, err := s.loadMember(ctx, a.Group.ID, mid)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "member_not_found", "Member not found.")
		return
	}
	if err != nil {
		writeInternal(w, r, err)
		return
	}

	p := store.MemberContributionStatementParams{GroupID: a.Group.ID, MembershipID: &mid, IncludeHidden: a.IsOfficial}
	qs := r.URL.Query()
	for _, f := range []struct {
		name string
		dst  **time.Time
	}{{"from", &p.FromDate}, {"to", &p.ToDate}} {
		if v := qs.Get(f.name); v != "" {
			t, err := time.Parse(dateLayout, v)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid_date", f.name+" must look like 2026-11-05.")
				return
			}
			*f.dst = &t
		}
	}
	if p.FromDate != nil && p.ToDate != nil && p.FromDate.After(*p.ToDate) {
		writeError(w, http.StatusBadRequest, "invalid_date", "from must not be after to.")
		return
	}
	if v := qs.Get("contribution_id"); v != "" {
		cid, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_contribution", "contribution_id must be a number.")
			return
		}
		p.ContributionID = &cid
	}

	rows, err := s.q.MemberContributionStatement(ctx, p)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	resp := statementResponse{
		Member: member, Currency: a.Group.Currency, From: datePtr(p.FromDate), To: datePtr(p.ToDate),
		OpeningBalance: "0.00", Entries: make([]statementEntry, 0, len(rows)),
	}
	if p.FromDate != nil {
		ob, err := s.q.OpeningBalance(ctx, store.OpeningBalanceParams{
			GroupID: a.Group.ID, MembershipID: &mid, ContributionID: p.ContributionID,
			IncludeHidden: a.IsOfficial, Before: *p.FromDate,
		})
		if err != nil {
			writeInternal(w, r, err)
			return
		}
		resp.OpeningBalance = money(ob)
	}
	resp.ClosingBalance = resp.OpeningBalance
	for _, e := range rows {
		amt := money(e.Amount)
		se := statementEntry{
			ID: e.ID, Date: Date{e.EntryDate}, Type: string(e.EntryType), Label: entryLabels[e.EntryType],
			ContributionID: e.ContributionID, ContributionName: e.ContributionName, Description: e.Description,
			Balance: money(e.RunningBalance), IsBackdated: e.IsBackdated, DueDate: datePtr(e.DueDate),
		}
		if e.Sign > 0 {
			se.Debit = &amt
		} else {
			se.Credit = &amt
		}
		resp.Entries = append(resp.Entries, se)
		resp.ClosingBalance = se.Balance
	}

	totals, err := s.q.MemberContributionTotals(ctx, store.MemberContributionTotalsParams{
		GroupID: a.Group.ID, MembershipID: &mid, IncludeHidden: a.IsOfficial,
	})
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	resp.Totals = make([]contributionTotals, 0, len(totals))
	for _, t := range totals {
		resp.Totals = append(resp.Totals, contributionTotals{
			ContributionID: t.ContributionID, ContributionName: t.ContributionName,
			Payable: money(t.Payable), Paid: money(t.Paid), Balance: money(t.Balance),
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

type memberBalance struct {
	MembershipID     int64   `json:"membership_id"`
	FirstName        string  `json:"first_name"`
	LastName         string  `json:"last_name"`
	MembershipNumber *string `json:"membership_number"`
	Payable          string  `json:"payable"`
	Paid             string  `json:"paid"`
	Balance          string  `json:"balance"`
}

// contributionBalances lists every active member's contribution totals,
// most in arrears first. Officials only.
func (s *Server) contributionBalances(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	rows, err := s.q.GroupContributionBalances(r.Context(), a.Group.ID)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	out := make([]memberBalance, 0, len(rows))
	for _, b := range rows {
		out = append(out, memberBalance{
			MembershipID: b.MembershipID, FirstName: b.FirstName, LastName: b.LastName,
			MembershipNumber: b.MembershipNumber, Payable: money(b.Payable), Paid: money(b.Paid), Balance: money(b.Balance),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"currency": a.Group.Currency, "balances": out})
}
