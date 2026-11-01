package mobile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/swahome/legion-api/internal/billing"
	"github.com/swahome/legion-api/internal/store"
)

// The setup wizard's contribution screens (setup_tasks/*): create or edit
// a contribution, choose who contributes, and its late-payment fines. The
// request shapes and legacy codes are the app's; the rows written are the
// same ones the web API writes.

var (
	kindByCode      = map[string]store.ContributionKind{"1": "regular", "2": "one_time", "3": "non_scheduled"}
	frequencyByCode = map[string]store.ContributionFrequency{}
)

func init() {
	for f, code := range frequencyCode {
		frequencyByCode[code] = f
	}
}

// legacyInvoiceDays is how many days before the due date the wizard
// invoiced; the app has no field for it.
const legacyInvoiceDays = 2

// contributionForm is the app's contribution form, checked.
type contributionForm struct {
	name, amount string
	kind         store.ContributionKind
	schedule     *store.UpsertContributionScheduleParams
	overpay      bool
}

func (s *Server) readContributionForm(c *call, g *member) (*contributionForm, map[string]any) {
	if reply := s.required(c, [2]string{"name", "Contribution Name"}, [2]string{"amount", "Contribution Amount"},
		[2]string{"type", "Contribution Type"}); reply != nil {
		return nil, reply
	}
	f := &contributionForm{name: c.req.Str("name"), amount: appAmount(c.req.Str("amount")), overpay: c.req.Str("disable_overpayments") == "1"}
	if f.amount == "" {
		return nil, s.formFailed(map[string]string{"amount": "The Contribution Amount field must contain a valid amount."})
	}
	kind, ok := kindByCode[c.req.Str("type")]
	if !ok {
		return nil, timed(status(0, "Invalid contribution type"), s.now())
	}
	f.kind = kind
	sms, email := c.req.Str("sms_notifications_enabled") == "1", c.req.Str("email_notifications_enabled") == "1"
	switch kind {
	case "regular":
		freq, ok := frequencyByCode[c.req.Str("contribution_frequency")]
		if !ok {
			return nil, s.formFailed(map[string]string{"contribution_frequency": "The How often do members contribute field is required."})
		}
		picker := map[string]string{}
		for _, k := range ruleKeys {
			if v := c.req.Str(k); v != "" {
				picker[k] = v
			}
		}
		raw, _ := json.Marshal(picker)
		rule, _ := billing.ParseRule(raw)
		today := s.today(g)
		due, err := billing.Next(string(freq), rule, today, today)
		if err != nil {
			return nil, s.formFailed(map[string]string{"contribution_frequency": "Those contribution days give no contribution date."})
		}
		inv := due.AddDate(0, 0, -legacyInvoiceDays)
		days := int32(legacyInvoiceDays)
		f.schedule = &store.UpsertContributionScheduleParams{
			Frequency: store.NullContributionFrequency{ContributionFrequency: freq, Valid: true}, Rule: rule.JSON(),
			InvoiceDate: &inv, ContributionDate: &due, InvoiceDaysBefore: &days,
			InvoicingActive: c.req.Str("regular_invoicing_active") == "1", SmsNotifications: sms, EmailNotifications: email,
		}
	case "one_time":
		inv, due := appDate(c.req.Str("invoice_date"), g.loc), appDate(c.req.Str("contribution_date"), g.loc)
		if due == nil {
			return nil, s.formFailed(map[string]string{"contribution_date": "The Contribution Date/Due Date field is required."})
		}
		if inv == nil || inv.After(*due) {
			inv = due
		}
		f.schedule = &store.UpsertContributionScheduleParams{
			InvoiceDate: inv, ContributionDate: due, Rule: []byte("{}"),
			InvoicingActive: c.req.Str("one_time_invoicing_active") == "1", SmsNotifications: sms, EmailNotifications: email,
		}
	}
	return f, nil
}

func numericOf(s string) pgtype.Numeric {
	var n pgtype.Numeric
	_ = n.Scan(s)
	return n
}

// writeSchedule saves the schedule and keeps any fine rules in step with a
// moved due date.
func writeSchedule(ctx context.Context, q *store.Queries, id int64, f *contributionForm) error {
	if f.schedule == nil {
		return nil
	}
	p := *f.schedule
	p.ContributionID = id
	if old, err := q.GetContributionSchedule(ctx, id); err == nil {
		p.FinesEnabled = old.FinesEnabled
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if err := q.UpsertContributionSchedule(ctx, p); err != nil {
		return err
	}
	if err := q.SetContributionSchedulePicker(ctx, store.SetContributionSchedulePickerParams{ContributionID: id, DisableOverpayments: f.overpay}); err != nil {
		return err
	}
	return restartFineDates(ctx, q, id, p.ContributionDate)
}

func restartFineDates(ctx context.Context, q *store.Queries, id int64, due *time.Time) error {
	rules, err := q.ListContributionFineRules(ctx, id)
	if err != nil {
		return err
	}
	for _, e := range rules {
		fr, err := billing.RuleFromRow(e.Kind, deref(numStr(e.FixedAmount)), deref(numStr(e.PercentageRate)),
			deref(e.PercentageOf), e.ChargeableOn, e.Mode, e.Frequency, e.FineLimit)
		if err != nil || due == nil {
			continue
		}
		next := fr.FirstFineDate(*due)
		if err := q.SetNextFineDate(ctx, store.SetNextFineDateParams{ID: e.ID, NextFineDate: &next}); err != nil {
			return err
		}
	}
	return nil
}

// setupReply is the legacy success answer: the contribution id with the
// group's members and role options for the next screen.
func (s *Server) setupReply(ctx context.Context, g *member, id int64, msg string) (any, error) {
	rows, err := s.q.MobileGroupMembers(ctx, store.MobileGroupMembersParams{GroupID: g.group.ID, ActiveOnly: true, Lim: 100000})
	if err != nil {
		return nil, err
	}
	members := make([]any, 0, len(rows))
	for _, m := range rows {
		members = append(members, map[string]any{"id": fmt.Sprint(m.ID), "user_id": fmt.Sprint(m.UserID),
			"first_name": m.FirstName, "last_name": m.LastName, "phone": legacyPhone(m.Phone), "email": deref(m.Email),
			"group_id": fmt.Sprint(g.group.ID), "active": "1"})
	}
	roles, err := s.q.ListGroupRoles(ctx, g.group.ID)
	if err != nil {
		return nil, err
	}
	options := map[string]string{}
	for _, r := range roles {
		options[fmt.Sprint(r.ID)] = r.Name
	}
	resp := timed(status(1, msg), s.now())
	resp["members"], resp["group_roles"], resp["contribution_id"] = members, options, id
	return resp, nil
}

// createGroupContributionSetting is setup_tasks/create_group_contribution_setting.
func (s *Server) createGroupContributionSetting(ctx context.Context, c *call, g *member) (any, error) {
	f, reply := s.readContributionForm(c, g)
	if reply != nil {
		return reply, nil
	}
	var id int64
	err := s.inTx(ctx, func(q *store.Queries) error {
		var err error
		id, err = q.CreateContribution(ctx, store.CreateContributionParams{GroupID: g.group.ID, Name: f.name, Kind: f.kind,
			Amount: numericOf(f.amount), AppliesToAllMembers: true})
		if err != nil {
			return err
		}
		return writeSchedule(ctx, q, id, f)
	})
	if err != nil {
		return nil, err
	}
	msg := "Contribution changes saved successfully"
	if f.kind == "non_scheduled" {
		msg = "Non scheduled Contribution successfully added"
	}
	return s.setupReply(ctx, g, id, msg)
}

func (s *Server) groupContribution(ctx context.Context, g *member, id int64) (store.GetContributionRow, bool, error) {
	cr, err := s.q.GetContribution(ctx, store.GetContributionParams{GroupID: g.group.ID, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return cr, false, nil
	}
	return cr, err == nil, err
}

// editContributionSetting is setup_tasks/edit_contribution_setting (id).
// A contribution keeps its type.
func (s *Server) editContributionSetting(ctx context.Context, c *call, g *member) (any, error) {
	cr, ok, err := s.groupContribution(ctx, g, c.req.Int("id"))
	if err != nil {
		return nil, err
	}
	if !ok {
		return timed(status(0, "Could not find contribution details"), s.now()), nil
	}
	f, reply := s.readContributionForm(c, g)
	if reply != nil {
		return reply, nil
	}
	if f.kind != cr.Kind {
		return timed(status(0, "A contribution's type cannot be changed"), s.now()), nil
	}
	err = s.inTx(ctx, func(q *store.Queries) error {
		if _, err := q.UpdateContribution(ctx, store.UpdateContributionParams{GroupID: g.group.ID, ID: cr.ID, Name: &f.name,
			Amount: numericOf(f.amount)}); err != nil {
			return err
		}
		return writeSchedule(ctx, q, cr.ID, f)
	})
	if err != nil {
		return nil, err
	}
	return s.setupReply(ctx, g, cr.ID, "Contribution changes saved successfully")
}

// contributingMembers is setup_tasks/contributing_members: all_members, or
// contributing_members as [{member_id}].
func (s *Server) contributingMembers(ctx context.Context, c *call, g *member) (any, error) {
	cr, ok, err := s.groupContribution(ctx, g, c.req.Int("contribution_id"))
	if err != nil {
		return nil, err
	}
	if !ok {
		return timed(status(0, "Error. Kindly first create a contribution"), s.now()), nil
	}
	all := c.req.Str("all_members") == "1" || c.req.Str("all_members") == "true"
	var ids []int64
	if list, ok := c.req["contributing_members"].([]any); ok {
		for _, x := range list {
			if m, ok := x.(map[string]any); ok {
				if id := appID(request(m).Str("member_id")); id != nil {
					ids = append(ids, *id)
				}
			}
		}
	}
	if !all && len(ids) == 0 {
		return s.formFailed(map[string]string{"member_ids": "Kindly select atleast one member"}), nil
	}
	err = s.inTx(ctx, func(q *store.Queries) error {
		if err := q.DeleteContributionMembers(ctx, cr.ID); err != nil {
			return err
		}
		if _, err := q.UpdateContribution(ctx, store.UpdateContributionParams{GroupID: g.group.ID, ID: cr.ID, AppliesToAllMembers: &all}); err != nil {
			return err
		}
		if all {
			return nil
		}
		return q.AddContributionMembers(ctx, store.AddContributionMembersParams{ContributionID: cr.ID, GroupID: g.group.ID, MembershipIds: ids})
	})
	if err != nil {
		return nil, err
	}
	msg := "Contribution changes saved successfully"
	if !all {
		msg = "successfully added members"
	}
	resp := timed(status(1, msg), s.now())
	resp["contribution_id"] = cr.ID
	return resp, nil
}

// contributionFineSettings is setup_tasks/contribution_fine_settings:
// enable_fines and the fine rows as an array of objects in the legacy
// columns (fine_type 1 fixed or 2 percentage, fixed_amount,
// fixed_fine_mode, fixed_fine_chargeable_on, fixed_fine_frequency,
// percentage_rate, percentage_fine_on, percentage_fine_chargeable_on,
// percentage_fine_mode, percentage_fine_frequency, fine_limit). The rows
// replace the contribution's fines.
func (s *Server) contributionFineSettings(ctx context.Context, c *call, g *member) (any, error) {
	cr, ok, err := s.groupContribution(ctx, g, c.req.Int("contribution_id"))
	if err != nil {
		return nil, err
	}
	if !ok {
		return timed(status(0, "Error. Kindly first create a contribution"), s.now()), nil
	}
	enable := c.req.Str("enable_fines") == "1"
	if cr.Kind == "non_scheduled" {
		if enable {
			return timed(status(0, "This contribution type is not allowed to have fines"), s.now()), nil
		}
		return timed(status(1, "Successful."), s.now()), nil
	}
	var rows []request
	for _, v := range c.req {
		if list, ok := v.([]any); ok {
			for _, x := range list {
				if m, ok := x.(map[string]any); ok && m["fine_type"] != nil {
					rows = append(rows, request(m))
				}
			}
		}
	}
	type rule struct {
		p  store.CreateFineRuleParams
		fr billing.FineRule
	}
	var rules []rule
	if enable {
		if cr.Kind != "regular" {
			return timed(status(0, "Only regular contributions have late-payment fines"), s.now()), nil
		}
		for _, r := range rows {
			p := store.CreateFineRuleParams{ContributionID: cr.ID, IsActive: true,
				SmsNotifications: r.Str("fine_sms_notifications_enabled") == "1", EmailNotifications: r.Str("fine_email_notifications_enabled") == "1"}
			if l, err := strconv.Atoi(r.Str("fine_limit")); err == nil {
				l32 := int32(l)
				p.FineLimit = &l32
			}
			var mode, freq string
			switch r.Str("fine_type") {
			case "1":
				amt := appAmount(r.Str("fixed_amount"))
				if amt == "" {
					return s.fineRowsInvalid(), nil
				}
				p.Kind, p.FixedAmount, p.ChargeableOn = store.FineKindFixed, &amt, r.Str("fixed_fine_chargeable_on")
				mode, freq = r.Str("fixed_fine_mode"), r.Str("fixed_fine_frequency")
			case "2":
				rate, on := r.Str("percentage_rate"), r.Str("percentage_fine_on")
				p.Kind, p.PercentageRate, p.PercentageOf, p.ChargeableOn = store.FineKindPercentage, &rate, &on, r.Str("percentage_fine_chargeable_on")
				mode, freq = r.Str("percentage_fine_mode"), r.Str("percentage_fine_frequency")
			default:
				return s.fineRowsInvalid(), nil
			}
			if freq == "" {
				freq = "0"
			}
			p.Mode, p.Frequency = &mode, &freq
			fr, err := billing.RuleFromRow(p.Kind, deref(p.FixedAmount), deref(p.PercentageRate), deref(p.PercentageOf),
				p.ChargeableOn, p.Mode, p.Frequency, p.FineLimit)
			if err != nil {
				return s.fineRowsInvalid(), nil
			}
			rules = append(rules, rule{p, fr})
		}
	}
	err = s.inTx(ctx, func(q *store.Queries) error {
		if _, err := q.SetContributionFinesEnabled(ctx, store.SetContributionFinesEnabledParams{ContributionID: cr.ID, FinesEnabled: enable}); err != nil {
			return err
		}
		if !enable {
			return nil
		}
		if err := q.DeleteFineRulesExcept(ctx, store.DeleteFineRulesExceptParams{ContributionID: cr.ID, Keep: []int64{}}); err != nil {
			return err
		}
		sch, err := q.GetContributionSchedule(ctx, cr.ID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		for _, r := range rules {
			if sch.ContributionDate != nil {
				next := r.fr.FirstFineDate(*sch.ContributionDate)
				r.p.NextFineDate = &next
			}
			if _, err := q.CreateFineRule(ctx, r.p); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return timed(status(1, "Successful."), s.now()), nil
}

func (s *Server) fineRowsInvalid() map[string]any {
	return timed(status(0, "Kindly ensure all fine entries are valid"), s.now())
}

func numStr(n pgtype.Numeric) *string {
	if !n.Valid {
		return nil
	}
	v, ok := numText(n).(string)
	if !ok {
		return nil
	}
	return &v
}
