package httpapi

import (
	"context"
	"errors"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/swahome/legion-api/internal/billing"
	"github.com/swahome/legion-api/internal/store"
)

// fineRuleRequest is one late-payment fine setting of a regular
// contribution, in the legacy option codes the API also returns (see
// billing.FineRule): chargeable_on "1".."56", "last_day_of_the_month" or
// "first_day_of_the_month"; frequency "0".."6"; mode "1" (each unpaid
// invoice) or "2" (outstanding balance); percentage_of "1".."3".
type fineRuleRequest struct {
	ID                 *int64  `json:"id"`
	Kind               string  `json:"kind"`
	FixedAmount        *Amount `json:"fixed_amount"`
	PercentageRate     *string `json:"percentage_rate"`
	PercentageOf       *string `json:"percentage_of"`
	ChargeableOn       string  `json:"chargeable_on"`
	Mode               string  `json:"mode"`
	Frequency          string  `json:"frequency"`
	FineLimit          *int32  `json:"fine_limit"`
	SMSNotifications   bool    `json:"sms_notifications"`
	EmailNotifications bool    `json:"email_notifications"`
	IsActive           *bool   `json:"is_active"`
}

var bigHundred = big.NewRat(100, 1)

func (f *fineRuleRequest) validate() (code, msg string) {
	f.ChargeableOn = strings.TrimSpace(f.ChargeableOn)
	switch f.Kind {
	case "fixed":
		if f.FixedAmount == nil || cents(string(*f.FixedAmount)) <= 0 {
			return "invalid_fine_rule", "A fixed fine needs an amount more than zero."
		}
		f.PercentageRate, f.PercentageOf = nil, nil
	case "percentage":
		if f.PercentageRate == nil {
			return "invalid_fine_rule", "A percentage fine needs a rate."
		}
		r, err := billing.ParseRate(*f.PercentageRate)
		if err != nil || r.Sign() <= 0 || r.Cmp(bigHundred) > 0 {
			return "invalid_fine_rule", "The rate must be a percentage between 0 and 100."
		}
		if f.PercentageOf == nil || (*f.PercentageOf != "1" && *f.PercentageOf != "2" && *f.PercentageOf != "3") {
			return "invalid_fine_rule", `percentage_of must be "1" (contribution amount), "2" (balance) or "3" (balance and fines).`
		}
		f.FixedAmount = nil
	default:
		return "invalid_fine_rule", `kind must be "fixed" or "percentage".`
	}
	switch f.ChargeableOn {
	case "last_day_of_the_month", "first_day_of_the_month":
	default:
		n, err := strconv.Atoi(f.ChargeableOn)
		if err != nil || n < 1 || n > 56 {
			return "invalid_fine_rule", "chargeable_on must be 1 to 56 days, last_day_of_the_month or first_day_of_the_month."
		}
	}
	if f.Mode == "" {
		f.Mode = billing.ModeEachInvoice
	}
	if f.Mode != billing.ModeEachInvoice && f.Mode != billing.ModeBalance {
		return "invalid_fine_rule", `mode must be "1" (each unpaid invoice) or "2" (outstanding balance).`
	}
	if f.Frequency == "" {
		f.Frequency = "0"
	}
	if n, err := strconv.Atoi(f.Frequency); err != nil || n < 0 || n > 6 {
		return "invalid_fine_rule", `frequency must be "0" (once) to "6" (yearly).`
	}
	if f.FineLimit != nil && (*f.FineLimit < 0 || *f.FineLimit > 20) {
		return "invalid_fine_rule", "fine_limit must be 0 (no limit) to 20."
	}
	return "", ""
}

// firstFineDate is when a rule first charges: after the schedule's next
// contribution date, or nothing if the contribution has no schedule yet.
func firstFineDate(f billing.FineRule, due *time.Time) *time.Time {
	if due == nil {
		return nil
	}
	d := f.FirstFineDate(*due)
	return &d
}

// writeFineRules replaces a contribution's fine rules when rules is not nil,
// and restarts the fine dates of rules whose timing changed: new or edited
// rules, and all of them when the contribution date moved.
func writeFineRules(ctx context.Context, q *store.Queries, contributionID int64, rules *[]fineRuleRequest, dueMoved bool) error {
	sch, err := q.GetContributionSchedule(ctx, contributionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	due := sch.ContributionDate

	existing, err := q.ListContributionFineRules(ctx, contributionID)
	if err != nil {
		return err
	}
	if rules == nil {
		if !dueMoved {
			return nil
		}
		for _, e := range existing {
			fr, err := billing.RuleFromRow(e.Kind, deref(numericString(e.FixedAmount)), deref(numericString(e.PercentageRate)),
				deref(e.PercentageOf), e.ChargeableOn, e.Mode, e.Frequency, e.FineLimit)
			if err != nil {
				continue
			}
			if err := q.SetNextFineDate(ctx, store.SetNextFineDateParams{ID: e.ID, NextFineDate: firstFineDate(fr, due)}); err != nil {
				return err
			}
		}
		return nil
	}

	known := map[int64]bool{}
	for _, e := range existing {
		known[e.ID] = true
	}
	keep := []int64{}
	for _, f := range *rules {
		if f.ID != nil && !known[*f.ID] {
			return badRequest{Code: "invalid_fine_rule", Msg: "That fine rule is not on this contribution."}
		}
		active := f.IsActive == nil || *f.IsActive
		var fixed *string
		if f.FixedAmount != nil {
			v := string(*f.FixedAmount)
			fixed = &v
		}
		fr, err := billing.RuleFromRow(store.FineKind(f.Kind), deref(fixed), deref(f.PercentageRate), deref(f.PercentageOf),
			f.ChargeableOn, &f.Mode, &f.Frequency, f.FineLimit)
		if err != nil {
			return badRequest{Code: "invalid_fine_rule", Msg: err.Error()}
		}
		next := firstFineDate(fr, due)
		mode, freq := f.Mode, f.Frequency
		if f.ID == nil {
			id, err := q.CreateFineRule(ctx, store.CreateFineRuleParams{
				ContributionID: contributionID, Kind: store.FineKind(f.Kind), FixedAmount: fixed,
				PercentageRate: f.PercentageRate, PercentageOf: f.PercentageOf, ChargeableOn: f.ChargeableOn,
				Mode: &mode, Frequency: &freq, FineLimit: f.FineLimit, SmsNotifications: f.SMSNotifications,
				EmailNotifications: f.EmailNotifications, IsActive: active, NextFineDate: next,
			})
			if err != nil {
				return err
			}
			keep = append(keep, id)
			continue
		}
		// An unchanged rule keeps its place in the fine calendar.
		for _, e := range existing {
			if e.ID == *f.ID && !dueMoved && sameTiming(e, f) {
				next = e.NextFineDate
			}
		}
		if _, err := q.UpdateFineRule(ctx, store.UpdateFineRuleParams{
			ID: *f.ID, ContributionID: contributionID, Kind: store.FineKind(f.Kind), FixedAmount: fixed,
			PercentageRate: f.PercentageRate, PercentageOf: f.PercentageOf, ChargeableOn: f.ChargeableOn,
			Mode: &mode, Frequency: &freq, FineLimit: f.FineLimit, SmsNotifications: f.SMSNotifications,
			EmailNotifications: f.EmailNotifications, IsActive: active, NextFineDate: next,
		}); err != nil {
			return err
		}
		keep = append(keep, *f.ID)
	}
	return q.DeleteFineRulesExcept(ctx, store.DeleteFineRulesExceptParams{ContributionID: contributionID, Keep: keep})
}

func sameTiming(e store.ListContributionFineRulesRow, f fineRuleRequest) bool {
	return e.ChargeableOn == f.ChargeableOn && deref(e.Frequency) == f.Frequency && e.IsActive == (f.IsActive == nil || *f.IsActive)
}
