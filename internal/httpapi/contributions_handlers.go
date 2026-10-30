package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/swahome/legion-api/internal/store"
)

type scheduleResponse struct {
	Frequency          *string `json:"frequency"`
	InvoiceDate        *Date   `json:"invoice_date"`
	ContributionDate   *Date   `json:"contribution_date"`
	InvoiceDaysBefore  *int32  `json:"invoice_days_before,omitempty"`
	InvoicingActive    *bool   `json:"invoicing_active"`
	SMSNotifications   *bool   `json:"sms_notifications,omitempty"`
	EmailNotifications *bool   `json:"email_notifications,omitempty"`
	FinesEnabled       *bool   `json:"fines_enabled"`
	// Rule holds the legacy day/week picker settings until invoicing is ported.
	Rule []byte `json:"rule,omitempty"`
}

type fineRuleResponse struct {
	ID             int64   `json:"id"`
	Kind           string  `json:"kind"`
	FixedAmount    *string `json:"fixed_amount"`
	PercentageRate *string `json:"percentage_rate"`
	PercentageOf   *string `json:"percentage_of"`
	ChargeableOn   string  `json:"chargeable_on"`
	Mode           *string `json:"mode"`
	Frequency      *string `json:"frequency"`
	FineLimit      *int32  `json:"fine_limit"`
	IsActive       bool    `json:"is_active"`
}

type contributionResponse struct {
	ID                  int64             `json:"id"`
	Name                string            `json:"name"`
	Kind                string            `json:"kind"`
	Category            *string           `json:"category"`
	Amount              string            `json:"amount"`
	IsActive            bool              `json:"is_active"`
	IsHidden            bool              `json:"is_hidden"`
	IsClosed            bool              `json:"is_closed"`
	AppliesToAllMembers bool              `json:"applies_to_all_members"`
	Schedule            *scheduleResponse `json:"schedule"`

	// Detail view only.
	IsNonRefundable *bool              `json:"is_non_refundable,omitempty"`
	IsEquity        *bool              `json:"is_equity,omitempty"`
	MemberIDs       []int64            `json:"member_ids,omitempty"`
	FineRules       []fineRuleResponse `json:"fine_rules,omitempty"`
}

func nullEnum[T ~string](valid bool, v T) *string {
	if !valid {
		return nil
	}
	s := string(v)
	return &s
}

func (s *Server) listContributions(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	rows, err := s.q.ListContributions(r.Context(), store.ListContributionsParams{GroupID: a.Group.ID, IncludeHidden: a.IsOfficial})
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	out := make([]contributionResponse, 0, len(rows))
	for _, c := range rows {
		cr := contributionResponse{
			ID: c.ID, Name: c.Name, Kind: string(c.Kind), Category: nullEnum(c.Category.Valid, c.Category.ContributionCategory),
			Amount: money(c.Amount), IsActive: c.IsActive, IsHidden: c.IsHidden, IsClosed: c.IsClosed,
			AppliesToAllMembers: c.AppliesToAllMembers,
		}
		if c.InvoicingActive != nil {
			cr.Schedule = &scheduleResponse{
				Frequency:        nullEnum(c.Frequency.Valid, c.Frequency.ContributionFrequency),
				InvoiceDate:      datePtr(c.InvoiceDate),
				ContributionDate: datePtr(c.ContributionDate),
				InvoicingActive:  c.InvoicingActive,
				FinesEnabled:     c.FinesEnabled,
			}
		}
		out = append(out, cr)
	}
	writeJSON(w, http.StatusOK, map[string]any{"contributions": out})
}

func (s *Server) getContribution(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	cid, ok := pathID(r, "contributionID")
	if !ok {
		writeError(w, http.StatusNotFound, "contribution_not_found", "Contribution not found.")
		return
	}
	c, err := s.loadContribution(r.Context(), a, cid)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "contribution_not_found", "Contribution not found.")
		return
	}
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) loadContribution(ctx context.Context, a groupAccess, id int64) (contributionResponse, error) {
	c, err := s.q.GetContribution(ctx, store.GetContributionParams{GroupID: a.Group.ID, ID: id})
	if err != nil {
		return contributionResponse{}, err
	}
	if c.IsHidden && !a.IsOfficial {
		return contributionResponse{}, pgx.ErrNoRows
	}
	cr := contributionResponse{
		ID: c.ID, Name: c.Name, Kind: string(c.Kind), Category: nullEnum(c.Category.Valid, c.Category.ContributionCategory),
		Amount: money(c.Amount), IsActive: c.IsActive, IsHidden: c.IsHidden, IsClosed: c.IsClosed,
		AppliesToAllMembers: c.AppliesToAllMembers, IsNonRefundable: &c.IsNonRefundable, IsEquity: &c.IsEquity,
		FineRules: []fineRuleResponse{},
	}
	sch, err := s.q.GetContributionSchedule(ctx, id)
	switch {
	case err == nil:
		cr.Schedule = &scheduleResponse{
			Frequency:          nullEnum(sch.Frequency.Valid, sch.Frequency.ContributionFrequency),
			InvoiceDate:        datePtr(sch.InvoiceDate),
			ContributionDate:   datePtr(sch.ContributionDate),
			InvoiceDaysBefore:  sch.InvoiceDaysBefore,
			InvoicingActive:    &sch.InvoicingActive,
			SMSNotifications:   &sch.SmsNotifications,
			EmailNotifications: &sch.EmailNotifications,
			FinesEnabled:       &sch.FinesEnabled,
		}
		if string(sch.Rule) != "{}" {
			cr.Schedule.Rule = sch.Rule
		}
	case !errors.Is(err, pgx.ErrNoRows):
		return cr, err
	}
	if !c.AppliesToAllMembers {
		if cr.MemberIDs, err = s.q.ListContributionMemberIDs(ctx, id); err != nil {
			return cr, err
		}
	}
	rules, err := s.q.ListContributionFineRules(ctx, id)
	if err != nil {
		return cr, err
	}
	for _, f := range rules {
		cr.FineRules = append(cr.FineRules, fineRuleResponse{
			ID: f.ID, Kind: string(f.Kind), FixedAmount: numericMoney(f.FixedAmount),
			PercentageRate: numericString(f.PercentageRate), PercentageOf: f.PercentageOf,
			ChargeableOn: f.ChargeableOn, Mode: f.Mode, Frequency: f.Frequency, FineLimit: f.FineLimit,
			IsActive: f.IsActive,
		})
	}
	return cr, nil
}

type scheduleRequest struct {
	Frequency          *string `json:"frequency"`
	InvoiceDate        *Date   `json:"invoice_date"`
	ContributionDate   *Date   `json:"contribution_date"`
	InvoiceDaysBefore  *int32  `json:"invoice_days_before"`
	InvoicingActive    *bool   `json:"invoicing_active"`
	SMSNotifications   bool    `json:"sms_notifications"`
	EmailNotifications bool    `json:"email_notifications"`
	FinesEnabled       bool    `json:"fines_enabled"`
}

type contributionRequest struct {
	Name                *string          `json:"name"`
	Kind                *string          `json:"kind"`
	Category            *string          `json:"category"`
	Amount              *Amount          `json:"amount"`
	IsActive            *bool            `json:"is_active"`
	IsHidden            *bool            `json:"is_hidden"`
	AppliesToAllMembers *bool            `json:"applies_to_all_members"`
	MemberIDs           []int64          `json:"member_ids"`
	Schedule            *scheduleRequest `json:"schedule"`
}

var validFrequencies = map[string]bool{}

func init() {
	for _, f := range store.AllContributionFrequencyValues() {
		validFrequencies[string(f)] = true
	}
}

// validate checks a create (full) or update (partial) request.
func (req *contributionRequest) validate(kind string, create bool) (code, msg string) {
	if req.Name != nil {
		n := strings.TrimSpace(*req.Name)
		req.Name = &n
	}
	if create && (req.Name == nil || *req.Name == "") {
		return "missing_fields", "Name is required."
	}
	if !create && req.Name != nil && *req.Name == "" {
		return "invalid_name", "Name cannot be blank."
	}
	if create && req.Amount == nil {
		return "missing_fields", "Amount is required."
	}
	if req.Category != nil && !store.ContributionCategory(*req.Category).Valid() {
		return "invalid_category", "Unknown category."
	}
	if sch := req.Schedule; sch != nil {
		if kind == "non_scheduled" {
			return "invalid_schedule", "Non-scheduled contributions have no schedule."
		}
		if kind == "regular" && (sch.Frequency == nil || !validFrequencies[*sch.Frequency]) {
			return "invalid_schedule", "Regular contributions need a valid frequency."
		}
		if kind == "one_time" && sch.Frequency != nil {
			return "invalid_schedule", "One-time contributions have no frequency."
		}
		if sch.ContributionDate == nil {
			return "invalid_schedule", "The schedule needs a contribution date."
		}
		if sch.InvoiceDate != nil && sch.InvoiceDate.After(sch.ContributionDate.Time) {
			return "invalid_schedule", "The invoice date must not be after the contribution date."
		}
	} else if create && kind == "regular" {
		return "invalid_schedule", "Regular contributions need a schedule."
	}
	return "", ""
}

func (s *Server) createContribution(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	var req contributionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Kind == nil || !store.ContributionKind(*req.Kind).Valid() {
		writeError(w, http.StatusBadRequest, "invalid_kind", "kind must be regular, one_time or non_scheduled.")
		return
	}
	if code, msg := req.validate(*req.Kind, true); code != "" {
		writeError(w, http.StatusBadRequest, code, msg)
		return
	}
	all := req.AppliesToAllMembers == nil || *req.AppliesToAllMembers
	var id int64
	err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		p := store.CreateContributionParams{
			GroupID: a.Group.ID, Name: *req.Name, Kind: store.ContributionKind(*req.Kind),
			Amount: req.Amount.Numeric(), AppliesToAllMembers: all,
		}
		if req.Category != nil {
			p.Category = store.NullContributionCategory{ContributionCategory: store.ContributionCategory(*req.Category), Valid: true}
		}
		var err error
		if id, err = q.CreateContribution(r.Context(), p); err != nil {
			return err
		}
		return s.writeContributionExtras(r.Context(), q, a.Group.ID, id, &req)
	})
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	c, err := s.loadContribution(r.Context(), a, id)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func (s *Server) updateContribution(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	cid, ok := pathID(r, "contributionID")
	if !ok {
		writeError(w, http.StatusNotFound, "contribution_not_found", "Contribution not found.")
		return
	}
	var req contributionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Kind != nil {
		writeError(w, http.StatusBadRequest, "kind_immutable", "A contribution's kind cannot be changed.")
		return
	}
	existing, err := s.q.GetContribution(r.Context(), store.GetContributionParams{GroupID: a.Group.ID, ID: cid})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "contribution_not_found", "Contribution not found.")
		return
	}
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	if code, msg := req.validate(string(existing.Kind), false); code != "" {
		writeError(w, http.StatusBadRequest, code, msg)
		return
	}
	err = pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		p := store.UpdateContributionParams{
			GroupID: a.Group.ID, ID: cid, Name: req.Name, IsActive: req.IsActive, IsHidden: req.IsHidden,
			AppliesToAllMembers: req.AppliesToAllMembers,
		}
		if req.Amount != nil {
			p.Amount = req.Amount.Numeric()
		}
		if req.Category != nil {
			p.Category = store.NullContributionCategory{ContributionCategory: store.ContributionCategory(*req.Category), Valid: true}
		}
		if _, err := q.UpdateContribution(r.Context(), p); err != nil {
			return err
		}
		return s.writeContributionExtras(r.Context(), q, a.Group.ID, cid, &req)
	})
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	c, err := s.loadContribution(r.Context(), a, cid)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// writeContributionExtras replaces the schedule and member list when the
// request includes them.
func (s *Server) writeContributionExtras(ctx context.Context, q *store.Queries, groupID, id int64, req *contributionRequest) error {
	if sch := req.Schedule; sch != nil {
		p := store.UpsertContributionScheduleParams{
			ContributionID:     id,
			InvoiceDate:        sch.InvoiceDate.timePtr(),
			ContributionDate:   sch.ContributionDate.timePtr(),
			InvoiceDaysBefore:  sch.InvoiceDaysBefore,
			InvoicingActive:    sch.InvoicingActive == nil || *sch.InvoicingActive,
			SmsNotifications:   sch.SMSNotifications,
			EmailNotifications: sch.EmailNotifications,
			FinesEnabled:       sch.FinesEnabled,
		}
		if sch.Frequency != nil {
			p.Frequency = store.NullContributionFrequency{ContributionFrequency: store.ContributionFrequency(*sch.Frequency), Valid: true}
		}
		if err := q.UpsertContributionSchedule(ctx, p); err != nil {
			return err
		}
	}
	if req.MemberIDs != nil {
		if err := q.DeleteContributionMembers(ctx, id); err != nil {
			return err
		}
		if len(req.MemberIDs) > 0 {
			// Ids from other groups are silently ignored by the query.
			return q.AddContributionMembers(ctx, store.AddContributionMembersParams{
				ContributionID: id, GroupID: groupID, MembershipIds: req.MemberIDs,
			})
		}
	}
	return nil
}
