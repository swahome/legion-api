package mobile

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/swahome/legion-api/internal/store"
)

// Milestone 3 read actions: accounts, categories, depositors, deposits,
// withdrawals, invoices and fines, in the legacy shapes. Ids are Legion's;
// the app only echoes them back. Accounts are named "bank-<id>" and so on
// when the app sends them, as before.

// accountLabel is accounts_m->get_group_account_options: how a deposit's or
// withdrawal's account is named in narratives.
func accountLabel(a store.ListGroupAccountsRow) string {
	switch a.Kind {
	case "bank", "sacco":
		return fmt.Sprintf("%s (%s) - %s (%s)", a.InstitutionName, a.BranchName, a.Name, a.AccountNumber)
	case "mobile_money":
		return fmt.Sprintf("%s - %s (%s)", a.InstitutionName, a.Name, a.AccountNumber)
	}
	return a.Name
}

// optionLabel is how the app's account pickers name an account.
func optionLabel(a store.ListGroupAccountsRow) string {
	switch a.Kind {
	case "bank", "sacco":
		return fmt.Sprintf("%s(%s) - %s(%s)", a.InstitutionName, a.BranchName, a.Name, a.AccountNumber)
	case "mobile_money":
		return fmt.Sprintf("%s-%s(%s)", a.InstitutionName, a.Name, a.AccountNumber)
	}
	return a.Name
}

func (s *Server) groupAccounts(ctx context.Context, g *member) ([]store.ListGroupAccountsRow, map[int64]store.ListGroupAccountsRow, error) {
	rows, err := s.q.ListGroupAccounts(ctx, g.group.ID)
	if err != nil {
		return nil, nil, err
	}
	byID := make(map[int64]store.ListGroupAccountsRow, len(rows))
	for _, a := range rows {
		byID[a.ID] = a
	}
	return rows, byID, nil
}

func (s *Server) accountOptions(activeOnly bool) func(ctx context.Context, c *call, g *member) (any, error) {
	return func(ctx context.Context, c *call, g *member) (any, error) {
		rows, _, err := s.groupAccounts(ctx, g)
		if err != nil {
			return nil, err
		}
		lists := map[store.AccountKind][]any{"bank": {}, "sacco": {}, "mobile_money": {}, "petty_cash": {}}
		for _, a := range rows {
			if activeOnly && a.IsClosed {
				continue
			}
			lists[a.Kind] = append(lists[a.Kind], map[string]any{"id": fmt.Sprint(a.ID), "name": optionLabel(a)})
		}
		return map[string]any{
			"status": 1, "message": "successful",
			"accounts": map[string]any{
				"bank_accounts": lists["bank"], "sacco_accounts": lists["sacco"],
				"mobile_money_accounts": lists["mobile_money"], "petty_cash_accounts": lists["petty_cash"],
			},
			"terms_and_conditions_link": "",
		}, nil
	}
}

func idString(p *int64) string {
	if p == nil {
		return ""
	}
	return fmt.Sprint(*p)
}

func (s *Server) accountsList(kind store.AccountKind) func(ctx context.Context, c *call, g *member) (any, error) {
	return func(ctx context.Context, c *call, g *member) (any, error) {
		rows, _, err := s.groupAccounts(ctx, g)
		if err != nil {
			return nil, err
		}
		partners := map[int64]bool{}
		if kind == "bank" {
			ids, err := s.q.PartnerBankIDs(ctx)
			if err != nil {
				return nil, err
			}
			for _, id := range ids {
				partners[id] = true
			}
		}
		out := []any{}
		for _, a := range rows {
			if a.Kind != kind {
				continue
			}
			item := map[string]any{
				"id": fmt.Sprint(a.ID), "account_name": a.Name, "account_balance": amount(a.Balance),
				"is_closed": boolInt(a.IsClosed),
			}
			switch kind {
			case "bank":
				partner := a.BankID != nil && partners[*a.BankID]
				item["description"] = fmt.Sprintf("%s (%s) - %s", a.InstitutionName, a.BranchName, a.AccountNumber)
				item["initial_balance"] = a.InitialBalance
				item["account_number"] = a.AccountNumber
				item["bank_id"], item["bank_branch_id"] = idString(a.BankID), idString(a.BankBranchID)
				item["bank_name"], item["bank_branch"] = a.InstitutionName, a.BranchName
				item["is_partner"] = boolInt(partner)
				item["is_verified"] = boolInt(partner && a.IsVerified)
				item["is_default"] = boolInt(a.IsDefault)
			case "sacco":
				item["description"] = fmt.Sprintf("%s (%s) - %s", a.InstitutionName, a.BranchName, a.AccountNumber)
				item["sacco_id"], item["sacco_branch_id"] = idString(a.SaccoID), idString(a.SaccoBranchID)
				item["account_number"] = a.AccountNumber
				item["sacco_name"], item["sacco_branch"] = a.InstitutionName, a.BranchName
			case "mobile_money":
				item["description"] = a.InstitutionName + " - " + a.AccountNumber
				item["mobile_money_provider_name"] = a.InstitutionName
				item["mobile_money_provider_id"] = idString(a.ProviderID)
				item["account_number"] = a.AccountNumber
			}
			out = append(out, item)
		}
		key := map[store.AccountKind]string{"bank": "banks", "sacco": "saccos", "mobile_money": "mobile_money_accounts", "petty_cash": "petty_accounts"}[kind]
		return map[string]any{"status": 1, "message": "successful", key: out}, nil
	}
}

// pageOf is the lower_limit/upper_limit paging with an action's own default size.
func pageOf(c *call, def int64) (limit, offset int32) {
	lower, upper := c.req.Int("lower_limit"), c.req.Int("upper_limit")
	if upper == 0 {
		upper = lower + def
	}
	n := upper - lower
	if lower < 0 || n <= 0 || n > 1000 {
		lower, n = 0, def
	}
	return int32(n), int32(lower)
}

func sliceRows[T any](rows []T, limit, offset int32) []T {
	if int(offset) >= len(rows) {
		return nil
	}
	end := int(offset) + int(limit)
	if end > len(rows) {
		end = len(rows)
	}
	return rows[offset:end]
}

type category struct {
	ID          int64
	Name        string
	Description string
	Amount      *string
	IsHidden    bool
	IsActive    bool
}

func (s *Server) categoryList(which string) func(ctx context.Context, c *call, g *member) (any, error) {
	return func(ctx context.Context, c *call, g *member) (any, error) {
		cats, err := s.categories(ctx, which, g.group.ID, true)
		if err != nil {
			return nil, err
		}
		limit, offset := pageOf(c, 100)
		out := []any{}
		for _, k := range sliceRows(cats, limit, offset) {
			item := map[string]any{"id": fmt.Sprint(k.ID), "name": k.Name, "is_hidden": boolInt(k.IsHidden), "active": boolInt(k.IsActive)}
			if which == "fine" {
				item["amount"] = amount(*k.Amount)
			} else {
				item["description"] = k.Description
			}
			out = append(out, item)
		}
		return map[string]any{"status": 1, "message": "success", which + "_categories": out}, nil
	}
}

func (s *Server) categoryOptions(which string) func(ctx context.Context, c *call, g *member) (any, error) {
	return func(ctx context.Context, c *call, g *member) (any, error) {
		cats, err := s.categories(ctx, which, g.group.ID, false)
		if err != nil {
			return nil, err
		}
		out := []any{}
		for _, k := range cats {
			item := map[string]any{"id": fmt.Sprint(k.ID), "name": k.Name}
			if which == "fine" {
				item["amount"], item["balance"] = 0, 0
			}
			out = append(out, item)
		}
		switch which {
		case "income":
			return map[string]any{"status": 1, "message": "income categories", "income_categories": out}, nil
		case "expense":
			return map[string]any{"status": 1, "message": "Expense Categories", "expense_category_options": out}, nil
		}
		return map[string]any{"status": 1, "message": "Fine Categories", "fine_category_options": out}, nil
	}
}

func (s *Server) categories(ctx context.Context, which string, groupID int64, all bool) ([]category, error) {
	var out []category
	switch which {
	case "income":
		rows, err := s.q.ListIncomeCategories(ctx, store.ListIncomeCategoriesParams{GroupID: groupID, IncludeInactive: all})
		for _, r := range rows {
			out = append(out, category{r.ID, r.Name, r.Description, nil, r.IsHidden, r.IsActive})
		}
		return out, err
	case "expense":
		rows, err := s.q.ListExpenseCategories(ctx, store.ListExpenseCategoriesParams{GroupID: groupID, IncludeInactive: all})
		for _, r := range rows {
			out = append(out, category{r.ID, r.Name, r.Description, nil, r.IsHidden, r.IsActive})
		}
		return out, err
	}
	rows, err := s.q.ListFineCategories(ctx, store.ListFineCategoriesParams{GroupID: groupID, IncludeInactive: all})
	for _, r := range rows {
		amt := "0"
		if v, err := r.Amount.Value(); err == nil && v != nil {
			amt = fmt.Sprint(v)
		}
		out = append(out, category{r.ID, r.Name, r.Description, &amt, r.IsHidden, r.IsActive})
	}
	return out, err
}

func (s *Server) depositorOptions(ctx context.Context, c *call, g *member) (any, error) {
	rows, err := s.q.ListDepositors(ctx, store.ListDepositorsParams{GroupID: g.group.ID, IncludeInactive: true})
	if err != nil {
		return nil, err
	}
	out := []any{}
	for _, d := range rows {
		identity := ""
		if d.Email != nil && strings.Contains(*d.Email, "@") {
			identity = *d.Email
		} else if d.Phone != nil {
			identity = *d.Phone
		}
		out = append(out, map[string]any{"id": fmt.Sprint(d.ID), "name": d.Name, "identity": identity})
	}
	return map[string]any{"status": 1, "message": "Depositors", "depositors": out}, nil
}

// visibleMembers applies member information privacy: an ordinary member of
// a private group sees only their own rows. Otherwise the app's "members"
// filter applies.
func visibleMembers(c *call, g *member) []int64 {
	if !g.official() && g.flag("enable_member_information_privacy") {
		return []int64{g.m.ID}
	}
	return c.req.IDs("members")
}

func sortOf(c *call) string {
	v := c.req.Str("sort_by")
	dir := ""
	switch {
	case strings.Contains(v, "desc"):
		dir = "_desc"
	case strings.Contains(v, "asc"):
		dir = "_asc"
	}
	switch {
	case strings.Contains(v, "amount") && dir != "":
		return "amount" + dir
	case strings.Contains(v, "date") && dir == "_asc":
		return "date_asc"
	}
	return ""
}

// depositFor is the legacy narrative of what a deposit paid for.
func depositFor(d store.MobileDepositsRow) string {
	name := depositNames[d.Kind]
	method := ""
	if d.Method.Valid {
		method = depositMethodNames[d.Method.PaymentMethod]
	}
	switch d.Kind {
	case "contribution":
		return fmt.Sprintf(`%s for "%s" contribution via %s`, name, d.ContributionName, method)
	case "fine":
		what := d.FineCategoryName
		if d.ContributionName != "" {
			what = d.ContributionName + " contribution late payment"
		}
		return fmt.Sprintf(`%s for "%s" via %s`, name, what, method)
	case "income":
		return name + " from " + d.IncomeCategoryName
	case "loan_repayment":
		return name + " via " + method
	}
	return name
}

func (s *Server) depositsList(ctx context.Context, c *call, g *member) (any, error) {
	var kinds []string
	for _, f := range c.req.Strs("status") {
		if k, ok := depositFilters[f]; ok {
			kinds = append(kinds, string(k))
		} else if f != "" && f != "0" {
			kinds = append(kinds, "none")
		}
	}
	limit, offset := pageOf(c, 20)
	rows, err := s.q.MobileDeposits(ctx, store.MobileDepositsParams{
		GroupID: g.group.ID, Kinds: kinds, MemberIds: visibleMembers(c, g), Sort: sortOf(c), Lim: limit, Off: offset,
	})
	if err != nil {
		return nil, err
	}
	_, accounts, err := s.groupAccounts(ctx, g)
	if err != nil {
		return nil, err
	}
	out := []any{}
	for _, d := range rows {
		typ := depositNames[d.Kind]
		switch d.Kind {
		case "income":
			typ += d.DepositorName
		case "loan_repayment":
			typ = d.MemberName + " made by " + d.MemberName
		case "contribution", "fine", "misc":
			typ += " made by " + d.MemberName
		}
		narrative := inWords(d.Amount, g.group.Currency) + " -- " + depositFor(d)
		if d.AccountID != nil {
			if a, ok := accounts[*d.AccountID]; ok {
				narrative += " Deposited to " + accountLabel(a)
			}
		}
		out = append(out, map[string]any{
			"type": typ, "date": mobileShortTime(d.DepositDate), "amount": d.Amount,
			"reconciliation": "Manually recorded", "narative": narrative, "id": fmt.Sprint(d.ID),
		})
	}
	return timed(map[string]any{"status": 1, "message": "deposit list", "deposits": out}, s.now()), nil
}

func (s *Server) groupDeposit(ctx context.Context, c *call, g *member) (any, error) {
	id := c.req.Int("deposit_id")
	rows, err := s.q.MobileDeposits(ctx, store.MobileDepositsParams{GroupID: g.group.ID, DepositID: &id, Lim: 1})
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 || id == 0 {
		return status(0, "Could not find group payment deposit"), nil
	}
	d := rows[0]
	paymentFor := depositFor(d)
	if d.Kind == "loan_repayment" && d.AccountID != nil {
		_, accounts, err := s.groupAccounts(ctx, g)
		if err != nil {
			return nil, err
		}
		paymentFor += " to " + accountLabel(accounts[*d.AccountID])
	}
	if d.Description != "" {
		paymentFor += " : " + d.Description
	}
	return map[string]any{"status": 1, "message": "successful", "data": map[string]any{
		"title": "Deposit Receipt", "member": d.MemberName, "amount_paid": inWords(d.Amount, g.group.Currency),
		"payment_for": paymentFor, "amount": d.Amount, "date": mobileShortTime(d.DepositDate),
	}}, nil
}

func withdrawalFor(w store.MobileWithdrawalsRow) string {
	name := withdrawalNames[w.Kind]
	out := name
	switch w.Kind {
	case "expense":
		out = name + " for " + w.ExpenseCategoryName
	case "loan_disbursement":
		if w.MemberName != "" {
			out += " to " + w.MemberName
		}
	case "contribution_refund":
		out = name + " to " + w.MemberName + " for " + w.ContributionName
	}
	if w.Description != "" {
		out += " : " + w.Description
	}
	return out
}

func (s *Server) withdrawalsList(ctx context.Context, c *call, g *member) (any, error) {
	var kinds []string
	for _, f := range c.req.Strs("status") {
		if k, ok := withdrawalFilters[f]; ok {
			kinds = append(kinds, string(k))
		} else if f != "" && f != "0" {
			kinds = append(kinds, "none")
		}
	}
	limit, offset := pageOf(c, 20)
	rows, err := s.q.MobileWithdrawals(ctx, store.MobileWithdrawalsParams{
		GroupID: g.group.ID, Kinds: kinds, MemberIds: visibleMembers(c, g), Sort: sortOf(c), Lim: limit, Off: offset,
	})
	if err != nil {
		return nil, err
	}
	out := []any{}
	for _, w := range rows {
		out = append(out, map[string]any{
			"id": fmt.Sprint(w.ID), "type": withdrawalNames[w.Kind] + " --- " + withdrawalFor(w),
			"withdrawal_date": mobileShortTime(w.WithdrawalDate), "recorded_on": mobileShortTime(w.CreatedAt.In(g.loc)),
			"narration": "", "reconcilation": "Manually Recorded", "amount": w.Amount,
		})
	}
	return timed(map[string]any{"status": 1, "message": "deposit list", "withdrawals": out}, s.now()), nil
}

func (s *Server) groupWithdrawal(ctx context.Context, c *call, g *member) (any, error) {
	id := c.req.Int("withdrawal_id")
	rows, err := s.q.MobileWithdrawals(ctx, store.MobileWithdrawalsParams{GroupID: g.group.ID, WithdrawalID: &id, Lim: 1})
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 || id == 0 {
		return status(0, "Could not find group withdrawal"), nil
	}
	w := rows[0]
	return map[string]any{"status": 1, "message": "successful", "data": map[string]any{
		"title": "Withdrawal Receipt", "member": w.MemberName, "amount_paid": inWords(w.Amount, g.group.Currency),
		"withdrawal_for": withdrawalFor(w), "amount": w.Amount, "date": mobileShortTime(w.WithdrawalDate),
	}}, nil
}

// invoiceTypes is invoice_type_options.
var invoiceTypes = map[store.LedgerEntryType]string{
	"contribution_invoice": "Contribution invoice", "contribution_fine_invoice": "Contribution fine invoice",
	"fine_invoice": "Fine invoice", "misc_invoice": "Miscellaneous invoice",
}

func invoiceText(i store.MobileInvoicesRow) string {
	t := invoiceTypes[i.EntryType]
	switch i.EntryType {
	case "contribution_invoice", "contribution_fine_invoice":
		return t + " for " + i.ContributionName + " contribution"
	case "fine_invoice":
		return t + " for " + i.FineCategoryName
	}
	return t + " for " + i.Description
}

func (s *Server) invoices(ctx context.Context, c *call, g *member, types []string, def int64) ([]store.MobileInvoicesRow, error) {
	var members []int64
	if !g.official() && g.flag("enable_member_information_privacy") {
		members = []int64{g.m.ID}
	}
	limit, offset := pageOf(c, def)
	return s.q.MobileInvoices(ctx, store.MobileInvoicesParams{GroupID: g.group.ID, Types: types, MemberIds: members, Lim: limit, Off: offset})
}

func (s *Server) invoicesList(ctx context.Context, c *call, g *member) (any, error) {
	rows, err := s.invoices(ctx, c, g, []string{"contribution_invoice", "contribution_fine_invoice", "fine_invoice", "misc_invoice"}, 100)
	if err != nil {
		return nil, err
	}
	out := []any{}
	for _, i := range rows {
		out = append(out, map[string]any{
			"id": fmt.Sprint(i.ID), "invoice_date": mobileShortTime(i.EntryDate), "due_date": mobileShortTime(dueOf(i)),
			"member": i.MemberName, "type": invoiceText(i), "amount_payable": i.Amount, "amount_paid": i.Paid,
		})
	}
	return map[string]any{"status": 1, "message": "success", "invoices": out}, nil
}

func dueOf(i store.MobileInvoicesRow) time.Time {
	if i.DueDate != nil {
		return *i.DueDate
	}
	return i.EntryDate
}

func (s *Server) groupInvoice(ctx context.Context, c *call, g *member) (any, error) {
	id := c.req.Int("invoice_id")
	rows, err := s.q.MobileInvoices(ctx, store.MobileInvoicesParams{
		GroupID: g.group.ID, InvoiceID: &id, Lim: 1,
		Types: []string{"contribution_invoice", "contribution_fine_invoice", "fine_invoice", "misc_invoice"},
	})
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 || id == 0 {
		return status(0, "Could not find invoice"), nil
	}
	i := rows[0]
	desc := i.Description
	typeDesc := invoiceText(i)
	if i.EntryType == "misc_invoice" {
		desc, typeDesc = "", invoiceTypes[i.EntryType]+" "+i.Description
	}
	typeDesc += " ---- " + inWords(i.Amount, g.group.Currency)
	return map[string]any{"status": 1, "message": "successful", "data": map[string]any{
		"member": i.MemberName, "due_date": mobileShortTime(dueOf(i)), "type": invoiceTypes[i.EntryType],
		"description": desc, "type_description": typeDesc, "amount": i.Amount,
	}}, nil
}

func (s *Server) finesList(ctx context.Context, c *call, g *member) (any, error) {
	rows, err := s.invoices(ctx, c, g, []string{"contribution_fine_invoice", "fine_invoice"}, 20)
	if err != nil {
		return nil, err
	}
	out := []any{}
	for _, i := range rows {
		out = append(out, map[string]any{
			"id": fmt.Sprint(i.ID), "fine_date": mobileShortTime(i.EntryDate), "amount_payable": i.Amount,
			"amount_paid": i.Paid, "description": i.MemberName + " - " + invoiceText(i), "member": i.MemberName,
		})
	}
	return map[string]any{"status": 1, "message": "deposit list", "fines": out}, nil
}

func (s *Server) fineOptions(ctx context.Context, c *call, g *member) (any, error) {
	balances, err := s.q.MobileFineBalances(ctx, store.MobileFineBalancesParams{GroupID: g.group.ID, MembershipID: &g.m.ID})
	if err != nil {
		return nil, err
	}
	byContribution, byCategory := map[int64]float64{}, map[int64]float64{}
	for _, b := range balances {
		switch {
		case b.ContributionID != nil:
			byContribution[*b.ContributionID] += amount(b.Balance)
		case b.FineCategoryID != nil:
			byCategory[*b.FineCategoryID] += amount(b.Balance)
		}
	}
	contribs, err := s.q.MobileFineContributions(ctx, g.group.ID)
	if err != nil {
		return nil, err
	}
	cOut := []any{}
	for _, k := range contribs {
		cOut = append(cOut, map[string]any{"id": fmt.Sprintf("contribution-%d", k.ID), "name": k.Name, "balance": byContribution[k.ID], "amount": 0})
	}
	cats, err := s.categories(ctx, "fine", g.group.ID, false)
	if err != nil {
		return nil, err
	}
	fOut := []any{}
	for _, k := range cats {
		fOut = append(fOut, map[string]any{"id": fmt.Sprintf("fine_category-%d", k.ID), "name": k.Name, "balance": byCategory[k.ID], "amount": amount(*k.Amount)})
	}
	return map[string]any{"status": 1, "message": "Fine types", "contribution_fine_options": cOut, "fine_category_options": fOut}, nil
}

// reportTime is timestamp_to_mobile_report_time: "5th Feb, 2024 ".
func reportTime(t time.Time) string {
	return ordinal(t.Day()) + t.Format(" Jan, 2006 ")
}

// recentDeposits is _get_member_recent_transactions: the latest deposits,
// of one member or of the whole group.
func (s *Server) recentDeposits(ctx context.Context, g *member, membershipID *int64, limit int64) ([]any, error) {
	var members []int64
	if membershipID != nil {
		members = []int64{*membershipID}
	}
	if limit <= 0 || limit > 100 {
		limit = 5
	}
	rows, err := s.q.MobileDeposits(ctx, store.MobileDepositsParams{GroupID: g.group.ID, MemberIds: members, Lim: int32(limit)})
	if err != nil {
		return nil, err
	}
	out := []any{}
	for _, d := range rows {
		desc := d.Description
		switch {
		case d.ContributionName != "":
			desc = d.ContributionName
		case d.FineCategoryName != "":
			desc = d.FineCategoryName
		}
		method := ""
		if d.Method.Valid {
			method = depositMethodNames[d.Method.PaymentMethod]
		}
		out = append(out, map[string]any{
			"type": depositNames[d.Kind], "date": reportTime(d.DepositDate), "amount": d.Amount,
			"payment_method": method, "description": desc,
		})
	}
	return out, nil
}

func (s *Server) recentTransactions(group bool) func(ctx context.Context, c *call, g *member) (any, error) {
	return func(ctx context.Context, c *call, g *member) (any, error) {
		if g.m.Status != store.MembershipStatusActive {
			return suspendedReply(s.now()), nil
		}
		who := &g.m.ID
		if group {
			who = nil
		}
		recent, err := s.recentDeposits(ctx, g, who, c.req.Int("limit"))
		if err != nil {
			return nil, err
		}
		return map[string]any{"status": 1, "recent_transactions": recent}, nil
	}
}

// cashBalances is what the group dashboard shows for money held: cash at
// bank (bank and sacco accounts), cash at hand (mobile money and petty
// cash), and each open bank and sacco account.
func (s *Server) cashBalances(ctx context.Context, g *member) (bank, cash float64, accounts []any, err error) {
	rows, err := s.q.DashboardCash(ctx, g.group.ID)
	if err != nil {
		return 0, 0, nil, err
	}
	for _, r := range rows {
		if r.Kind == "bank" || r.Kind == "sacco" {
			bank += amount(r.Balance)
		} else {
			cash += amount(r.Balance)
		}
	}
	list, _, err := s.groupAccounts(ctx, g)
	if err != nil {
		return 0, 0, nil, err
	}
	accounts = []any{}
	for _, a := range list {
		if a.IsClosed {
			continue
		}
		desc := a.Name
		if a.InstitutionName != "" {
			desc = a.InstitutionName + " -" + a.Name
		}
		accounts = append(accounts, map[string]any{"balance": amount(a.Balance), "description": desc})
	}
	return bank, cash, accounts, nil
}
