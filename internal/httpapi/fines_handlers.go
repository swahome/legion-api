package httpapi

import (
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/swahome/legion-api/internal/store"
)

// Fines: what members are charged for breaking group rules (legacy "fine
// members", fine_invoice with a fine category) and the late-payment fines
// the billing job raises on contributions (contribution_fine_invoice).
// Paying a fine is a fine deposit; see deposits_handlers.go.

type fineResponse struct {
	ID           int64     `json:"id"`
	Kind         string    `json:"kind"` // fine | contribution_fine
	Member       *namedRef `json:"member"`
	Amount       string    `json:"amount"`
	Date         Date      `json:"date"`
	Description  string    `json:"description"`
	FineCategory *namedRef `json:"fine_category"`
	Contribution *namedRef `json:"contribution"`
	// For a late-payment fine on one invoice: that invoice's due date.
	InvoiceDueDate *Date      `json:"invoice_due_date"`
	Automatic      bool       `json:"automatic"`
	CreatedAt      time.Time  `json:"created_at"`
	VoidedAt       *time.Time `json:"voided_at"`
}

var fineKinds = map[string]store.LedgerEntryType{
	"fine":              store.LedgerEntryTypeFineInvoice,
	"contribution_fine": store.LedgerEntryTypeContributionFineInvoice,
}

func toFine(f store.ListFinesRow) fineResponse {
	kind := "fine"
	if f.EntryType == store.LedgerEntryTypeContributionFineInvoice {
		kind = "contribution_fine"
	}
	out := fineResponse{
		ID: f.ID, Kind: kind, Amount: money(f.Amount), Date: Date{f.EntryDate}, Description: f.Description,
		FineCategory: ref(f.FineCategoryID, f.FineCategoryName), Contribution: ref(f.ContributionID, f.ContributionName),
		InvoiceDueDate: datePtr(f.ParentDueDate), Automatic: f.GeneratedBy != nil,
		CreatedAt: f.CreatedAt, VoidedAt: f.VoidedAt,
	}
	if f.MembershipID != nil {
		out.Member = &namedRef{ID: *f.MembershipID, Name: f.MemberName}
	}
	return out
}

// listFines pages through fines, newest first. Members see only their own.
// Filters: member_id, kind (fine | contribution_fine), from, to,
// include_voided=true (officials).
func (s *Server) listFines(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	pg := pageParams(r)
	p := store.ListFinesParams{
		GroupID: a.Group.ID, IncludeVoided: a.IsOfficial && r.URL.Query().Get("include_voided") == "true",
		PageSize: pg.size, PageOffset: pg.offset,
	}
	var ok1, ok2, ok3 bool
	p.MembershipID, ok1 = optionalID(r, "member_id")
	p.FromDate, ok2 = optionalDate(r, "from")
	p.ToDate, ok3 = optionalDate(r, "to")
	if !ok1 || !ok2 || !ok3 {
		writeError(w, http.StatusBadRequest, "invalid_filter", "Filters must be ids or dates like 2026-11-05.")
		return
	}
	if k := r.URL.Query().Get("kind"); k != "" {
		t, ok := fineKinds[k]
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid_kind", "kind must be fine or contribution_fine.")
			return
		}
		p.EntryType = store.NullLedgerEntryType{LedgerEntryType: t, Valid: true}
	}
	if !a.IsOfficial {
		if a.MembershipID == nil || (p.MembershipID != nil && *p.MembershipID != *a.MembershipID) {
			writeError(w, http.StatusForbidden, "officials_only", "Members can only see their own fines.")
			return
		}
		p.MembershipID = a.MembershipID
	}
	rows, err := s.q.ListFines(r.Context(), p)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	items := make([]fineResponse, 0, len(rows))
	var total int64
	for _, row := range rows {
		items = append(items, toFine(row))
		total = row.Total
	}
	writeJSON(w, http.StatusOK, newPaged(items, total, pg))
}

type fineBalance struct {
	Member  namedRef `json:"member"`
	Fined   string   `json:"fined"`
	Paid    string   `json:"paid"`
	Balance string   `json:"balance"`
}

// fineBalances is every active member's fines charged, paid and owed.
// A member gets only their own row.
func (s *Server) fineBalances(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	p := store.FineBalancesParams{GroupID: a.Group.ID}
	if !a.IsOfficial {
		if a.MembershipID == nil {
			writeError(w, http.StatusForbidden, "officials_only", "Only group officials can do this.")
			return
		}
		p.MembershipID = a.MembershipID
	}
	rows, err := s.q.FineBalances(r.Context(), p)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	out := make([]fineBalance, 0, len(rows))
	var owed int64
	for _, row := range rows {
		out = append(out, fineBalance{
			Member: namedRef{ID: row.MembershipID, Name: row.MemberName},
			Fined:  money(row.Fined), Paid: money(row.Paid), Balance: money(row.Balance),
		})
		owed += cents(row.Balance)
	}
	writeJSON(w, http.StatusOK, map[string]any{"members": out, "total_owed": formatCents(owed)})
}

type fineMembersRequest struct {
	MemberIDs      []int64 `json:"member_ids"`
	FineCategoryID int64   `json:"fine_category_id"`
	Amount         *Amount `json:"amount"`
	Date           *Date   `json:"date"`
	Description    string  `json:"description"`
}

// fineMembers charges the same fine to one or more members (officials).
func (s *Server) fineMembers(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	var req fineMembersRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Description = strings.TrimSpace(req.Description)
	if err := checkAmountDate(req.Amount, req.Date, nil, "fine"); writeMoneyError(w, r, err) {
		return
	}
	if len(req.MemberIDs) == 0 || len(req.MemberIDs) > 500 {
		writeError(w, http.StatusBadRequest, "missing_fields", "Pick the members to fine (up to 500 at a time).")
		return
	}
	userID := claimsFrom(r.Context()).UserID
	var ids []int64
	err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		ctx := r.Context()
		if err := found("invalid_fine_category", "Pick one of the group's fine categories.")(
			q.FineCategoryInGroup(ctx, store.FineCategoryInGroupParams{GroupID: a.Group.ID, ID: req.FineCategoryID})); err != nil {
			return err
		}
		seen := map[int64]bool{}
		for _, mid := range req.MemberIDs {
			if seen[mid] {
				continue
			}
			seen[mid] = true
			if err := found("invalid_member", "One of those members is not in this group.")(
				q.ActiveMembershipInGroup(ctx, store.ActiveMembershipInGroupParams{GroupID: a.Group.ID, ID: mid})); err != nil {
				return err
			}
			m, cat := mid, req.FineCategoryID
			id, err := q.CreateManualFine(ctx, store.CreateManualFineParams{
				GroupID: a.Group.ID, MembershipID: &m, Amount: string(*req.Amount), FineDate: req.Date.Time,
				Description: req.Description, FineCategoryID: &cat, CreatedBy: &userID,
			})
			if err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return nil
	})
	if writeMoneyError(w, r, err) {
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"fine_ids": ids})
}

// voidFine cancels a fine (officials). Payments already made against fines
// stay; the member's fine balance just drops.
func (s *Server) voidFine(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	id, ok := pathID(r, "fineID")
	if !ok {
		writeError(w, http.StatusNotFound, "fine_not_found", "Fine not found.")
		return
	}
	userID := claimsFrom(r.Context()).UserID
	n, err := s.q.VoidFine(r.Context(), store.VoidFineParams{GroupID: a.Group.ID, ID: id, VoidedBy: &userID})
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	if n == 0 {
		writeError(w, http.StatusNotFound, "fine_not_found", "Fine not found, or already voided.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
