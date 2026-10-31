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

	"github.com/swahome/legion-api/internal/store"
)

// Legacy codes and labels (libraries/Contribution_invoices.php).
var (
	kindCodes     = map[store.ContributionKind]string{"regular": "1", "one_time": "2", "non_scheduled": "3"}
	kindLabels    = map[store.ContributionKind]string{"regular": "Regular Contribution", "one_time": "One Time Contribution", "non_scheduled": "Non Scheduled Contribution"}
	categoryCodes = map[store.ContributionCategory]string{"share_capital": "1", "savings": "2", "welfare": "3", "membership_fee": "4", "investment": "5", "other": "6"}
	frequencyCode = map[store.ContributionFrequency]string{
		"monthly": "1", "bimonthly": "2", "quarterly": "3", "biannually": "4", "annually": "5",
		"weekly": "6", "fortnightly": "7", "daily": "8", "twice_monthly": "9",
	}
	frequencyLabels = map[string]string{
		"1": "Once a Month (Monthly)", "6": "Once a Week (Weekly)", "8": "Once a Day (Daily)",
		"7": "Once Every Two Weeks (Fortnightly)", "2": "Once Every Two Months (Bimonthly)",
		"3": "Once Every Three Months (Quarterly)", "4": "Once Every Six Months (Biannually)",
		"5": "Once a Year (Annually)", "9": "Twice Every  Month (After A Date)",
	}
	monthDays    = []string{"Day of the Month", "Sunday of the Month", "Monday of the Month", "Tuesday of the Month", "Wednesday of the Month", "Thursday of the Month", "Friday of the Month", "Saturday of the Month"}
	weekdayNames = []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}
	weekNumbers  = map[string]string{"1": "of the First Week", "2": "of the Second Week"}
)

func dayOfMonth(v string) string {
	switch n, _ := strconv.Atoi(v); {
	case n == 32:
		return "Every Last"
	case n >= 1 && n <= 30:
		return "Every " + ordinal(n)
	}
	return ""
}

func pick(list []string, v string, offset int) string {
	n, err := strconv.Atoi(v)
	if err != nil || n-offset < 0 || n-offset >= len(list) {
		return ""
	}
	return list[n-offset]
}

// frequencyText is how the legacy list described a regular schedule, e.g.
// "Once a Month (Monthly), Every 5th Day of the Month".
func frequencyText(code string, rule map[string]string) string {
	label := frequencyLabels[code]
	monthDay := func(key string) string {
		if rule[key] == "" {
			return monthDays[0]
		}
		return pick(monthDays, rule[key], 0)
	}
	switch code {
	case "1":
		return label + ", " + dayOfMonth(rule["month_day_monthly"]) + " " + monthDay("week_day_monthly")
	case "6":
		if d := pick(weekdayNames, rule["week_day_weekly"], 1); d != "" {
			return label + ", Every " + d + " of the Week"
		}
		return label + ", "
	case "7":
		d := pick(weekdayNames, rule["week_day_fortnight"], 1)
		if d != "" {
			d = "Every " + d
		}
		return label + ", " + d + " " + weekNumbers[rule["week_number_fortnight"]]
	case "2", "3", "4", "5":
		start := ""
		if m, err := strconv.Atoi(rule["start_month_multiple"]); err == nil && m >= 1 && m <= 12 {
			start = "starting in " + time.Month(m).String()
		}
		return label + ", " + dayOfMonth(rule["month_day_multiple"]) + " " + monthDay("week_day_multiple") + ", " + start
	case "8":
		return label
	}
	return ""
}

func (s *Server) getGroupContributions(ctx context.Context, c *call, g *member) (any, error) {
	rows, err := s.q.MobileGroupContributions(ctx, g.group.ID)
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(rows))
	for _, r := range rows {
		typ, frequency, invoiceDate, contributionDate := "Invoicing Disabled", "", "", ""
		oneTimeSetting := 0
		scheduled := r.Frequency.Valid || r.ContributionDate != nil || r.InvoiceDate != nil
		if r.InvoicingActive {
			typ = kindLabels[r.Kind]
			switch {
			case r.Kind == store.ContributionKindRegular && scheduled:
				var rule map[string]string
				_ = json.Unmarshal(r.Rule, &rule)
				frequency = frequencyText(frequencyCode[r.Frequency.ContributionFrequency], rule)
				invoiceDate = mobileTime(dateOrEpoch(r.InvoiceDate, g.loc))
				contributionDate = mobileTime(dateOrEpoch(r.ContributionDate, g.loc))
			case r.Kind == store.ContributionKindOneTime && scheduled:
				invoiceDate = mobileTime(dateOrEpoch(r.InvoiceDate, g.loc))
				contributionDate = mobileTime(dateOrEpoch(r.ContributionDate, g.loc))
				frequency = "One Time Contribution"
				oneTimeSetting = int(r.ID)
			}
		}
		out = append(out, map[string]any{
			"id":                            fmt.Sprint(r.ID),
			"name":                          r.Name,
			"amount":                        r.Amount,
			"type":                          typ,
			"contribution_type":             kindCodes[r.Kind],
			"frequency":                     frequency,
			"invoice_date":                  invoiceDate,
			"contribution_date":             contributionDate,
			"one_time_contribution_setting": oneTimeSetting,
			"is_hidden":                     boolInt(r.IsHidden),
			"active":                        boolInt(r.IsActive),
		})
	}
	return map[string]any{"status": 1, "message": "Group contributions", "contributions": out}, nil
}

// getGroupContribution returns one contribution with its schedule merged
// in, under the legacy column names, plus its fines and members.
func (s *Server) getGroupContribution(ctx context.Context, c *call, g *member) (any, error) {
	id := c.req.Int("id")
	if id == 0 {
		return status(0, "Bad request: Missing contribution id"), nil
	}
	ct, err := s.q.GetContribution(ctx, store.GetContributionParams{GroupID: g.group.ID, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return status(0, "Group contribution not available"), nil
	}
	if err != nil {
		return nil, err
	}
	str := func(b bool) string { return fmt.Sprint(boolInt(b)) }
	settings := map[string]any{}
	sched, err := s.q.GetContributionSchedule(ctx, id)
	hasSchedule := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if hasSchedule && ct.Kind != store.ContributionKindNonScheduled {
		epoch := func(d *time.Time) any {
			if d == nil {
				return nil
			}
			return fmt.Sprint(dateOrEpoch(d, g.loc).Unix())
		}
		settings["contribution_id"] = fmt.Sprint(id)
		settings["group_id"] = fmt.Sprint(g.group.ID)
		settings["invoice_date"] = epoch(sched.InvoiceDate)
		settings["contribution_date"] = epoch(sched.ContributionDate)
		settings["last_invoice_date"] = epoch(sched.LastInvoicedOn)
		settings["sms_notifications_enabled"] = str(sched.SmsNotifications)
		settings["email_notifications_enabled"] = str(sched.EmailNotifications)
		settings["sms_template"] = sched.SmsTemplate
		settings["disable_overpayments"] = str(sched.DisableOverpayments)
		settings["enable_fines"] = str(sched.FinesEnabled)
		settings["enable_contribution_member_list"] = str(!ct.AppliesToAllMembers)
		settings["active"] = "1"
		if ct.Kind == store.ContributionKindRegular {
			settings["contribution_frequency"] = nil
			if sched.Frequency.Valid {
				settings["contribution_frequency"] = frequencyCode[sched.Frequency.ContributionFrequency]
			}
			settings["invoice_days"] = nil
			if sched.InvoiceDaysBefore != nil {
				settings["invoice_days"] = fmt.Sprint(*sched.InvoiceDaysBefore)
			}
			for _, k := range ruleKeys {
				settings[k] = nil
			}
			var rule map[string]string
			_ = json.Unmarshal(sched.Rule, &rule)
			for k, v := range rule {
				settings[k] = v
			}
		}
	} else if ct.Kind == store.ContributionKindNonScheduled {
		settings["contribution_id"] = fmt.Sprint(id)
	}
	// The contribution's own columns win, as in PHP's array_merge.
	var category any
	if ct.Category.Valid {
		category = categoryCodes[ct.Category.ContributionCategory]
	}
	invoicing := hasSchedule && sched.InvoicingActive
	for k, v := range map[string]any{
		"id":                        fmt.Sprint(ct.ID),
		"type":                      kindCodes[ct.Kind],
		"name":                      ct.Name,
		"amount":                    ct.Amount,
		"group_id":                  fmt.Sprint(g.group.ID),
		"category":                  category,
		"regular_invoicing_active":  str(invoicing && ct.Kind == store.ContributionKindRegular),
		"one_time_invoicing_active": str(invoicing && ct.Kind == store.ContributionKindOneTime),
		"active":                    str(ct.IsActive),
		"is_hidden":                 str(ct.IsHidden),
		"is_closed":                 str(ct.IsClosed),
		"is_non_refundable":         str(ct.IsNonRefundable),
		"is_equity":                 str(ct.IsEquity),
		"hidden_in_statement":       str(ct.HiddenInStatement),
		"display_contribution_arrears_cumulatively": str(ct.ShowArrearsCumulatively),
		"created_on":  fmt.Sprint(ct.CreatedAt.Unix()),
		"modified_on": fmt.Sprint(ct.UpdatedAt.Unix()),
	} {
		settings[k] = v
	}

	fines, err := s.q.ListContributionFineRules(ctx, id)
	if err != nil {
		return nil, err
	}
	fineOut := make([]any, 0, len(fines))
	for _, f := range fines {
		row := map[string]any{
			"id": fmt.Sprint(f.ID), "contribution_id": fmt.Sprint(id), "group_id": fmt.Sprint(g.group.ID),
			"fine_type": "1", "fixed_amount": numText(f.FixedAmount), "fixed_fine_mode": nil,
			"fixed_fine_chargeable_on": nil, "fixed_fine_frequency": nil,
			"percentage_rate": numText(f.PercentageRate), "percentage_fine_on": f.PercentageOf,
			"percentage_fine_chargeable_on": nil, "percentage_fine_mode": nil, "percentage_fine_frequency": nil,
			"fine_limit": nil, "active": str(f.IsActive),
		}
		prefix := "fixed_fine_"
		if f.Kind == store.FineKindPercentage {
			row["fine_type"], prefix = "2", "percentage_fine_"
		}
		row[prefix+"chargeable_on"] = f.ChargeableOn
		row[prefix+"mode"] = f.Mode
		row[prefix+"frequency"] = f.Frequency
		if f.FineLimit != nil {
			row["fine_limit"] = fmt.Sprint(*f.FineLimit)
		}
		fineOut = append(fineOut, row)
	}

	var ids []int64
	if ct.AppliesToAllMembers {
		ids, err = s.q.ActiveMembershipIDs(ctx, g.group.ID)
	} else {
		ids, err = s.q.ListContributionMemberIDs(ctx, id)
	}
	if err != nil {
		return nil, err
	}
	members := make([]any, 0, len(ids))
	for _, m := range ids {
		members = append(members, fmt.Sprint(m))
	}
	return map[string]any{
		"status": 1, "message": "success",
		"contribution_settings":      settings,
		"contribution_fine_settings": fineOut,
		"selected_group_members":     members,
	}, nil
}

var ruleKeys = []string{
	"month_day_monthly", "week_day_monthly", "week_day_weekly", "week_day_fortnight",
	"week_number_fortnight", "month_day_multiple", "week_day_multiple", "start_month_multiple",
	"after_first_starting_day", "after_second_starting_day", "after_second_contribution_date",
	"after_first_contribution_day_option", "after_first_day_week_multiple",
	"after_second_day_week_multiple", "after_second_contribution_day_option",
}

func numText(n pgtype.Numeric) any {
	if !n.Valid {
		return nil
	}
	f, err := n.Float64Value()
	if err != nil {
		return nil
	}
	return strconv.FormatFloat(f.Float64, 'f', -1, 64)
}

func (s *Server) getGroupContributionOptions(ctx context.Context, c *call, g *member) (any, error) {
	return s.contributionOptions(ctx, g, false)
}

func (s *Server) getMemberContributionOptions(ctx context.Context, c *call, g *member) (any, error) {
	return s.contributionOptions(ctx, g, true)
}

// contributionOptions lists active, visible contributions, with the
// caller's balance on each when asked.
func (s *Server) contributionOptions(ctx context.Context, g *member, withBalance bool) (any, error) {
	rows, err := s.q.MobileContributionOptions(ctx, g.group.ID)
	if err != nil {
		return nil, err
	}
	balances := map[int64]float64{}
	if withBalance {
		bs, err := s.q.MobileContributionBalances(ctx, store.MobileContributionBalancesParams{GroupID: g.group.ID, MembershipID: &g.m.ID})
		if err != nil {
			return nil, err
		}
		for _, b := range bs {
			balances[b.ContributionID] = amount(b.Balance)
		}
	}
	out := make([]any, 0, len(rows))
	for _, r := range rows {
		o := map[string]any{"id": r.ID, "name": r.Name}
		if withBalance {
			o["balance"] = balances[r.ID]
		}
		out = append(out, o)
	}
	return timed(map[string]any{"status": 1, "message": "Contribution List", "contributions": out}, s.now()), nil
}
