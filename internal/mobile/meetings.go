package mobile

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/swahome/legion-api/internal/books"
	"github.com/swahome/legion-api/internal/store"
)

// Meetings the app records offline and syncs: who came, the agenda, any
// other business, and the money collected (contribution and fine payments,
// cash) and loans given out at the meeting. Each collection is recorded
// through internal/books and tagged with the meeting.
//
// Legacy differences: the legacy sync dropped the "late" list (a merge
// bug) and kept going when a payment failed; here every list is kept, and
// a payment that does not check out is skipped and counted in the reply.

func obj(v any) request {
	if m, ok := v.(map[string]any); ok {
		return request(m)
	}
	return request{}
}

func objects(v any) []request {
	xs, _ := v.([]any)
	out := make([]request, 0, len(xs))
	for _, x := range xs {
		if m, ok := x.(map[string]any); ok {
			out = append(out, request(m))
		}
	}
	return out
}

func strs(v any) []string {
	xs, _ := v.([]any)
	var out []string
	for _, x := range xs {
		if s := (request{"v": x}).Str("v"); s != "" {
			out = append(out, s)
		}
	}
	return out
}

var attendanceLists = []struct {
	key    string
	status int16
}{{"present", 1}, {"withApology", 2}, {"late", 3}, {"withoutApology", 4}}

// meetingTime reads the meeting's date and time as the group's local time.
func meetingTime(v string, loc *time.Location) *time.Time {
	for _, l := range []string{"2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02T15:04:05", "2006-01-02T15:04", "02-01-2006 15:04:05", "02-01-2006 15:04"} {
		if t, err := time.ParseInLocation(l, v, loc); err == nil {
			return &t
		}
	}
	if d := appDate(v, loc); d != nil {
		t := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, loc)
		return &t
	}
	return nil
}

func (s *Server) createGroupMeeting(ctx context.Context, c *call, g *member) (any, error) {
	if reply := s.checkForm(c, g, rule{"title", "Meeting name", ""}, rule{"venue", "Meeting Venue", ""},
		rule{"date", "Meeting Date", ""}); reply != nil {
		return reply, nil
	}
	at := meetingTime(c.req.Str("date"), g.loc)
	if at == nil {
		return s.formFailed(map[string]string{"date": "The Meeting Date field must contain a valid date."}), nil
	}
	day := appDate(c.req.Str("date"), g.loc)
	if day == nil {
		y, m, d := at.Date()
		t := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
		day = &t
	}
	members := obj(c.req["members"])
	collections := obj(c.req["collections"])
	skipped := 0
	var counts [5]int
	var totalContrib, totalFines, totalLoans int64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		id, err := q.CreateMeeting(ctx, store.CreateMeetingParams{GroupID: g.group.ID, Title: c.req.Str("title"),
			Venue: c.req.Str("venue"), Purpose: c.req.Str("purpose"), MeetingAt: *at, CreatedBy: &g.user.ID})
		if err != nil {
			return err
		}
		for _, l := range attendanceLists {
			for _, m := range objects(members[l.key]) {
				mid := m.Int("id")
				if mid <= 0 {
					continue
				}
				ok, err := q.ActiveMembershipInGroup(ctx, store.ActiveMembershipInGroupParams{GroupID: g.group.ID, ID: mid})
				if err != nil {
					return err
				}
				if !ok {
					skipped++
					continue
				}
				if err := q.AddMeetingAttendance(ctx, store.AddMeetingAttendanceParams{MeetingID: id, GroupID: g.group.ID,
					MembershipID: mid, Status: l.status}); err != nil {
					return err
				}
				counts[l.status]++
			}
		}
		for kind, key := range map[string]string{"agenda": "agenda", "aob": "aob"} {
			for i, name := range strs(c.req[key]) {
				if err := q.AddMeetingItem(ctx, store.AddMeetingItemParams{MeetingID: id, Kind: kind, Name: name, Position: int32(i)}); err != nil {
					return err
				}
			}
		}
		cash := "cash"
		// record runs one recording in a savepoint so a bad entry is
		// skipped without losing the rest.
		record := func(fn func(q *store.Queries) error) error {
			err := pgx.BeginFunc(ctx, tx, func(sp pgx.Tx) error { return fn(s.q.WithTx(sp)) })
			if _, ok := asBooksError(err); ok {
				skipped++
				return nil
			}
			return err
		}
		for _, it := range objects(collections["contributions"]) {
			d := books.Deposit{Kind: store.DepositKindContribution, AccountID: appAccount(obj(it["account"]).Str("account_id")),
				Amount: appAmount(it.Str("amount")), Date: day, Method: &cash,
				MemberID: appID(obj(it["member"]).Str("id")), ContributionID: appID(obj(it["contribution"]).Str("id"))}
			if d.MemberID == nil || d.ContributionID == nil || d.AccountID == 0 || d.Amount == "" {
				skipped++
				continue
			}
			if err := record(func(q *store.Queries) error {
				did, err := books.RecordDeposit(ctx, q, g.group.ID, g.user.ID, d)
				if err != nil {
					return err
				}
				totalContrib += books.Cents(d.Amount)
				return q.SetDepositLinks(ctx, store.SetDepositLinksParams{ID: did, MeetingID: &id})
			}); err != nil {
				return err
			}
		}
		for _, it := range objects(collections["fines"]) {
			fine := obj(it["fines"])
			d := books.Deposit{Kind: store.DepositKindFine, AccountID: appAccount(obj(it["account"]).Str("account_id")),
				Amount: appAmount(it.Str("amount")), Date: day, Method: &cash, Description: fine.Str("name"),
				MemberID: appID(obj(it["member"]).Str("id")), FineCategoryID: appID(fine.Str("id"))}
			if d.MemberID == nil || d.FineCategoryID == nil || d.AccountID == 0 || d.Amount == "" {
				skipped++
				continue
			}
			if err := record(func(q *store.Queries) error {
				did, err := books.RecordDeposit(ctx, q, g.group.ID, g.user.ID, d)
				if err != nil {
					return err
				}
				totalFines += books.Cents(d.Amount)
				return q.SetDepositLinks(ctx, store.SetDepositLinksParams{ID: did, MeetingID: &id})
			}); err != nil {
				return err
			}
		}
		for _, it := range objects(collections["disbursements"]) {
			typeID := appID(obj(it["loans"]).Str("id"))
			memberID := appID(obj(it["member"]).Str("id"))
			n := books.NewLoan{LoanTypeID: typeID, AccountID: appAccount(obj(it["account"]).Str("account_id")),
				Amount: appAmount(it.Str("amount")), Date: day, Method: &cash}
			if typeID == nil || memberID == nil || n.AccountID == 0 || n.Amount == "" {
				skipped++
				continue
			}
			n.MemberID = *memberID
			if err := record(func(q *store.Queries) error {
				lt, err := q.GetLoanType(ctx, store.GetLoanTypeParams{GroupID: g.group.ID, ID: *typeID})
				if err != nil {
					return books.Error{Code: "invalid_loan_type", Msg: "Unknown loan type"}
				}
				switch {
				case lt.FixedTerm != nil:
				case lt.MaxTerm != nil:
					n.Installments = int(*lt.MaxTerm)
				case lt.MinTerm != nil:
					n.Installments = int(*lt.MinTerm)
				default:
					n.Installments = 12
				}
				lid, err := books.RecordLoan(ctx, q, g.group.ID, g.user.ID, n)
				if err != nil {
					return err
				}
				totalLoans += books.Cents(n.Amount)
				return q.SetLoanMeeting(ctx, store.SetLoanMeetingParams{ID: lid, MeetingID: &id})
			}); err != nil {
				return err
			}
		}
		msg := fmt.Sprintf("%s at %s on %s. Present: %d, late: %d, absent with apology: %d, absent without apology: %d. "+
			"Contributions %s %s, fines %s %s, loans %s %s.", c.req.Str("title"), c.req.Str("venue"), shortDate(*at),
			counts[1], counts[3], counts[2], counts[4], g.group.Currency, commaMoney(books.Format(fmt.Sprintf("%.2f", float64(totalContrib)/100))),
			g.group.Currency, commaMoney(fmt.Sprintf("%.2f", float64(totalFines)/100)),
			g.group.Currency, commaMoney(fmt.Sprintf("%.2f", float64(totalLoans)/100)))
		return notifyGroup(ctx, q, g.group.ID, &g.user.ID, "Meeting summary", msg, categoryMeeting, map[string]any{"meeting_id": id})
	})
	if err != nil {
		return nil, err
	}
	reply := status(1, "Meetings successfully synced")
	if skipped > 0 {
		reply["skipped_entries"] = skipped
	}
	return reply, nil
}

func memberObject(id, userID int64, name, email string) map[string]any {
	return map[string]any{"id": id, "name": name, "identity": email, "user_id": userID}
}

func (s *Server) getGroupMeetings(ctx context.Context, c *call, g *member) (any, error) {
	limit, offset := pageOf(c, 20)
	meetings, err := s.q.ListMeetings(ctx, store.ListMeetingsParams{GroupID: g.group.ID, Lim: limit, Off: offset})
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(meetings))
	for _, m := range meetings {
		ids = append(ids, m.ID)
	}
	attendance, err := s.q.MeetingAttendance(ctx, ids)
	if err != nil {
		return nil, err
	}
	itemRows, err := s.q.MeetingItems(ctx, ids)
	if err != nil {
		return nil, err
	}
	deposits, err := s.q.MeetingDeposits(ctx, store.MeetingDepositsParams{GroupID: g.group.ID, MeetingIds: ids})
	if err != nil {
		return nil, err
	}
	loanRows, err := s.q.MeetingLoans(ctx, store.MeetingLoansParams{GroupID: g.group.ID, MeetingIds: ids})
	if err != nil {
		return nil, err
	}
	types, err := s.q.ListLoanTypes(ctx, store.ListLoanTypesParams{GroupID: g.group.ID, IncludeHidden: true})
	if err != nil {
		return nil, err
	}
	typeByID := map[int64]store.LoanType{}
	for _, t := range types {
		typeByID[t.ID] = t
	}
	people := map[int64]map[string][]any{}
	for _, a := range attendance {
		if people[a.MeetingID] == nil {
			people[a.MeetingID] = map[string][]any{}
		}
		key := attendanceLists[a.Status-1].key
		people[a.MeetingID][key] = append(people[a.MeetingID][key], memberObject(a.MembershipID, a.UserID, a.Name, a.Email))
	}
	agenda, aob := map[int64][]string{}, map[int64][]string{}
	for _, it := range itemRows {
		if it.Kind == "agenda" {
			agenda[it.MeetingID] = append(agenda[it.MeetingID], it.Name)
		} else {
			aob[it.MeetingID] = append(aob[it.MeetingID], it.Name)
		}
	}
	contribs, fines, repayments := map[int64][]any{}, map[int64][]any{}, map[int64][]any{}
	for _, d := range deposits {
		mid := *d.MeetingID
		mem := memberObject(*d.MembershipID, d.UserID, d.MemberName, d.Email)
		switch d.Kind {
		case store.DepositKindContribution:
			typ := "One-time Contribution"
			if d.ContributionKind.Valid && d.ContributionKind.ContributionKind == store.ContributionKindRegular {
				typ = "Regular Contribution"
			}
			contribs[mid] = append(contribs[mid], map[string]any{"type": "contributions", "account": idString(d.AccountID),
				"amount": int64(amount(d.Amount)), "member": mem,
				"contribution": map[string]any{"id": d.ContributionID, "name": d.ContributionName,
					"amount": int64(amount(d.ContributionAmount)), "type": typ}})
		case store.DepositKindFine:
			if d.FineCategoryID == nil {
				continue
			}
			fines[mid] = append(fines[mid], map[string]any{"type": "fines", "contribution": map[string]any{}, "loan": map[string]any{},
				"fines": map[string]any{"id": *d.FineCategoryID, "name": d.FineName, "is_hidden": boolInt(d.FineHidden),
					"active": 1, "amount": int64(amount(d.FineAmount))},
				"account": idString(d.AccountID), "amount": int64(amount(d.Amount)), "member": mem})
		}
	}
	for _, l := range loanRows {
		mid := *l.MeetingID
		info := map[string]any{"id": l.ID, "disbursementDate": dateOrEpoch(&l.DisbursedOn, g.loc).Unix(),
			"disbursement_date": mobileShortTime(l.DisbursedOn)}
		if l.LoanTypeID != nil {
			if t, ok := typeByID[*l.LoanTypeID]; ok {
				for k, v := range meetingLoanType(t) {
					info[k] = v
				}
			}
		}
		var membership int64
		if l.MembershipID != nil {
			membership = *l.MembershipID
		}
		repayments[mid] = append(repayments[mid], map[string]any{"type": "repayments", "account": idString(l.AccountID),
			"amount": int64(amount(l.Amount)), "member": memberObject(membership, l.UserID, l.MemberName, l.Email), "loans": info})
	}
	orEmpty := func(v []any) any {
		if v == nil {
			return ""
		}
		return v
	}
	strsOrEmpty := func(v []string) any {
		if v == nil {
			return ""
		}
		return v
	}
	nonNil := func(v []any) []any {
		if v == nil {
			return []any{}
		}
		return v
	}
	out := []any{}
	for _, m := range meetings {
		p := people[m.ID]
		out = append(out, map[string]any{
			"group_id": m.GroupID, "user_id": c.req.Str("user_id"), "title": m.Title, "venue": m.Venue, "purpose": m.Purpose,
			"synced_on": m.CreatedAt.Unix(), "date": m.MeetingAt.In(g.loc).Format("2006-01-02 3:04"),
			"members": map[string]any{"present": orEmpty(p["present"]), "withApology": orEmpty(p["withApology"]),
				"withoutApology": orEmpty(p["withoutApology"]), "late": orEmpty(p["late"])},
			"agenda": strsOrEmpty(agenda[m.ID]), "aob": strsOrEmpty(aob[m.ID]),
			"collections": map[string]any{"contributions": nonNil(contribs[m.ID]), "repayments": nonNil(repayments[m.ID]),
				"disbursements": []any{}, "fines": nonNil(fines[m.ID])},
			"time": m.MeetingAt.Unix(),
		})
	}
	return map[string]any{"status": 1, "message": "Successfull", "meetings": out, "time": s.now().Unix()}, nil
}

// meetingLoanType describes a loan type the way the meetings list did.
func meetingLoanType(t store.LoanType) map[string]any {
	enabled := func(b bool, yes, no string) string {
		if b {
			return yes
		}
		return no
	}
	amount := ""
	switch {
	case t.SavingsMultiple.Valid:
		amount = "Maximum Amount to borrow " + trimZeros(books.NumText(t.SavingsMultiple)) + " your savings"
	case t.MinAmount.Valid || t.MaxAmount.Valid:
		lo, hi := "0", "0"
		if t.MinAmount.Valid {
			lo = books.NumText(t.MinAmount)
		}
		if t.MaxAmount.Valid {
			hi = books.NumText(t.MaxAmount)
		}
		amount = "Between " + commaMoney(lo) + " - " + commaMoney(hi)
	}
	period := ""
	switch {
	case t.FixedTerm != nil:
		period = fmt.Sprintf("Fixed Repayment of %d Months", *t.FixedTerm)
	case t.MinTerm != nil && t.MaxTerm != nil:
		period = fmt.Sprintf("Repayment period between %d - %d Months", *t.MinTerm, *t.MaxTerm)
	}
	return map[string]any{
		"guarantors": enabled(t.Guarantors != "none", "Required", "Not Required"), "interestRate": books.NumText(t.InterestRate) + "%",
		"latePaymentFines": enabled(len(t.LateFine) > 4, "Enabled", "Disabled"), "loanAmount": amount,
		"loanProcessing":          enabled(len(t.ProcessingFee) > 4, "Charged", "No Charge"),
		"name":                    t.Name,
		"outstandingPaymentFines": enabled(len(t.BalanceFine) > 4, "Enabled", "Disabled"),
		"repaymentPeriod":         period, "isHidden": t.IsHidden,
	}
}
