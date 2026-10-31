package mobile

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/swahome/legion-api/internal/auth"
	"github.com/swahome/legion-api/internal/store"
)

// Setting up the group's accounts from the app: create, edit, close,
// activate and delete bank, sacco, mobile money and petty cash accounts.

type accountWords struct {
	noun         string // "bank account", for the legacy sentences
	title        string // "Bank Account"
	institution  string // request field naming the bank, sacco or provider
	branch       string // request field naming the branch, if any
	notFound     string // "Could not group bank account. Select a different account"
	activateWord string // "activated" for banks, "activate" (sic) elsewhere
}

var accountText = map[store.AccountKind]accountWords{
	store.AccountKindBank: {noun: "bank account", title: "Bank Account", institution: "bank_id", branch: "bank_branch_id",
		notFound: "Could not group bank account. Select a different account", activateWord: "activated"},
	store.AccountKindSacco: {noun: "sacco account", title: "Sacco account", institution: "sacco_id", branch: "sacco_branch_id",
		notFound: "Could not %s group sacco account. Select a different account", activateWord: "activate"},
	store.AccountKindMobileMoney: {noun: "mobile money account", title: "Mobile money account", institution: "mobile_money_provider_id",
		notFound: "Could not %s group mobile money account. Select a different account", activateWord: "activate"},
	store.AccountKindPettyCash: {noun: "petty cash account", title: "Petty cash account",
		notFound: "Could not %s group petty cash account. Select a different account", activateWord: "activate"},
}

func (w accountWords) missing(verb string) string {
	if strings.Contains(w.notFound, "%s") {
		return strings.Replace(w.notFound, "%s", verb, 1)
	}
	return w.notFound
}

// walletLocked is the legacy guard on the group's e-wallet: a verified
// default bank account is run by Chamasoft and can't be changed from the app.
func walletLocked(a store.GroupAccount) bool {
	return a.Kind == store.AccountKindBank && a.IsDefault && a.IsVerified
}

const walletMessage = "No action can be performed on this account. Kindly contact support to disable E-Wallet services"

// accountForm validates the create and edit forms and returns the values.
func (s *Server) accountForm(ctx context.Context, c *call, kind store.AccountKind) (p store.CreateGroupAccountParams, reply map[string]any, err error) {
	words := accountText[kind]
	fields := [][2]string{{"account_name", "Account Name"}}
	if words.institution != "" {
		fields = append(fields, [2]string{words.institution, "Institution"})
	}
	if words.branch != "" {
		fields = append(fields, [2]string{words.branch, "Branch"})
	}
	if kind != store.AccountKindPettyCash {
		fields = append(fields, [2]string{"account_number", "Account Number"})
	}
	if reply := s.required(c, fields...); reply != nil {
		return p, reply, nil
	}
	p = store.CreateGroupAccountParams{Kind: kind, Name: c.req.Str("account_name"), AccountNumber: c.req.Str("account_number")}
	balance := "0"
	if v := c.req.Str("initial_balance"); v != "" {
		if balance = appAmount(v); balance == "" {
			return p, s.formFailed(map[string]string{"initial_balance": "The Initial Balance field must be a valid amount."}), nil
		}
	}
	_ = p.InitialBalance.Scan(balance)
	if kind == store.AccountKindBank {
		n := p.AccountNumber
		if len(n) < 5 || len(n) > 20 || strings.Trim(n, "0123456789") != "" {
			return p, s.formFailed(map[string]string{"account_number": "The Account Number field must be 5 to 20 digits."}), nil
		}
	}
	if kind == store.AccountKindPettyCash {
		return p, nil, nil
	}
	inst := appID(c.req.Str(words.institution))
	if inst == nil {
		return p, s.formFailed(map[string]string{words.institution: "Select a valid institution"}), nil
	}
	found, err := s.q.InstitutionExists(ctx, store.InstitutionExistsParams{Kind: kind, InstitutionID: *inst})
	if err != nil {
		return p, nil, err
	}
	if found == nil || !*found {
		return p, s.formFailed(map[string]string{words.institution: "Select a valid institution"}), nil
	}
	var branch *int64
	if words.branch != "" {
		branch = appID(c.req.Str(words.branch))
		ok := branch != nil
		if ok && kind == store.AccountKindBank {
			ok, err = s.q.BankBranchBelongs(ctx, store.BankBranchBelongsParams{ID: *branch, BankID: *inst})
		} else if ok {
			ok, err = s.q.SaccoBranchBelongs(ctx, store.SaccoBranchBelongsParams{ID: *branch, SaccoID: *inst})
		}
		if err != nil {
			return p, nil, err
		}
		if !ok {
			return p, s.formFailed(map[string]string{words.branch: "The branch does not belong to the selected institution"}), nil
		}
	}
	switch kind {
	case store.AccountKindBank:
		p.BankID, p.BankBranchID = inst, branch
	case store.AccountKindSacco:
		p.SaccoID, p.SaccoBranchID = inst, branch
	case store.AccountKindMobileMoney:
		p.ProviderID = inst
	}
	return p, nil, nil
}

func (s *Server) accountCreated(ctx context.Context, kind store.AccountKind, id int64, name string, p store.CreateGroupAccountParams) (map[string]any, error) {
	if kind != store.AccountKindBank {
		return map[string]any{"status": 1, "message": "success", "id": id, "name": name}, nil
	}
	partners, err := s.q.PartnerBankIDs(ctx)
	if err != nil {
		return nil, err
	}
	partner := false
	for _, b := range partners {
		partner = partner || (p.BankID != nil && b == *p.BankID)
	}
	return map[string]any{"status": 1, "id": id, "bank_id": deref64(p.BankID), "is_partner": boolInt(partner)}, nil
}

func deref64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func (s *Server) createAccount(kind store.AccountKind) groupFn {
	return s.officialsOnly(func(ctx context.Context, c *call, g *member) (any, error) {
		p, reply, err := s.accountForm(ctx, c, kind)
		if err != nil || reply != nil {
			return reply, err
		}
		p.GroupID = g.group.ID
		id, err := s.q.CreateGroupAccount(ctx, p)
		if err != nil {
			return nil, err
		}
		resp, err := s.accountCreated(ctx, kind, id, p.Name, p)
		if kind == store.AccountKindBank && resp != nil {
			resp["message"] = "Successfully added"
		}
		return resp, err
	})
}

// groupAccount loads the account the request names by "id", of this kind.
func (s *Server) groupAccount(ctx context.Context, c *call, g *member, kind store.AccountKind) (*store.GroupAccount, error) {
	id := appAccount(c.req.Str("id"))
	a, err := s.q.GetGroupAccount(ctx, store.GetGroupAccountParams{GroupID: g.group.ID, ID: id})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && a.Kind != kind) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (s *Server) editAccount(kind store.AccountKind) groupFn {
	return s.officialsOnly(func(ctx context.Context, c *call, g *member) (any, error) {
		acct, err := s.groupAccount(ctx, c, g, kind)
		if err != nil {
			return nil, err
		}
		if acct == nil {
			if kind == store.AccountKindBank {
				return status(0, "Group bank account to edit not found"), nil
			}
			return status(0, accountText[kind].missing("find")), nil
		}
		if walletLocked(*acct) {
			return status(0, walletMessage), nil
		}
		p, reply, err := s.accountForm(ctx, c, kind)
		if err != nil || reply != nil {
			return reply, err
		}
		if (p.BankID != nil && (acct.BankID == nil || *p.BankID != *acct.BankID)) ||
			(p.SaccoID != nil && (acct.SaccoID == nil || *p.SaccoID != *acct.SaccoID)) ||
			(p.ProviderID != nil && (acct.ProviderID == nil || *p.ProviderID != *acct.ProviderID)) {
			return s.formFailed(map[string]string{accountText[kind].institution: "An account's institution can't be changed; open a new account instead."}), nil
		}
		u := store.UpdateGroupAccountParams{
			GroupID: g.group.ID, ID: acct.ID, Name: &p.Name, AccountNumber: &p.AccountNumber, InitialBalance: p.InitialBalance,
		}
		if accountText[kind].branch != "" {
			u.SetBranch, u.BranchID = true, firstNonNil(p.BankBranchID, p.SaccoBranchID)
		}
		if _, err := s.q.UpdateGroupAccount(ctx, u); err != nil {
			return nil, err
		}
		resp, err := s.accountCreated(ctx, kind, acct.ID, p.Name, p)
		if kind == store.AccountKindBank && resp != nil {
			resp["message"] = "Successfully updated"
		}
		return resp, err
	})
}

func firstNonNil(ids ...*int64) *int64 {
	for _, id := range ids {
		if id != nil {
			return id
		}
	}
	return nil
}

// closeAccount is close (closed true) and activate (false).
func (s *Server) closeAccount(kind store.AccountKind, closed bool) groupFn {
	return s.officialsOnly(func(ctx context.Context, c *call, g *member) (any, error) {
		words := accountText[kind]
		verb := map[bool]string{true: "close", false: "activate"}[closed]
		acct, err := s.groupAccount(ctx, c, g, kind)
		if err != nil {
			return nil, err
		}
		if acct == nil {
			return status(0, words.missing(verb)), nil
		}
		if walletLocked(*acct) {
			return status(0, walletMessage), nil
		}
		if acct.IsClosed == closed {
			if closed {
				return status(0, "Sorry, the "+words.noun+" is already closed"), nil
			}
			return status(0, "Sorry, the "+words.noun+" is already "+words.activateWord), nil
		}
		u := store.UpdateGroupAccountParams{GroupID: g.group.ID, ID: acct.ID, IsClosed: &closed}
		if closed {
			no := false
			u.IsDefault = &no
		}
		if _, err := s.q.UpdateGroupAccount(ctx, u); err != nil {
			return nil, err
		}
		if closed {
			return status(1, words.title+" successfully closed"), nil
		}
		return status(1, words.title+" successfully activated"), nil
	})
}

// deleteAccount needs the caller's password and an account with no
// transactions, as before.
func (s *Server) deleteAccount(kind store.AccountKind) groupFn {
	return s.officialsOnly(func(ctx context.Context, c *call, g *member) (any, error) {
		words := accountText[kind]
		err := s.auth.ConfirmPassword(ctx, g.user.ID, c.req.Str("password"), clientInfo(c.r))
		var locked *auth.LockedOutError
		switch {
		case errors.Is(err, auth.ErrInvalidCredentials):
			return status(0, "You entered the wrong password."), nil
		case errors.As(err, &locked):
			return status(0, "Too many wrong passwords. Try again later."), nil
		case err != nil:
			return nil, err
		}
		acct, err := s.groupAccount(ctx, c, g, kind)
		if err != nil {
			return nil, err
		}
		if acct == nil {
			return status(0, words.missing("delete")), nil
		}
		if walletLocked(*acct) {
			return status(0, "Bank account can not be deleted. Kindly contact support to disable E-Wallet services"), nil
		}
		n, err := s.q.CountAccountEntries(ctx, &acct.ID)
		if err != nil {
			return nil, err
		}
		if n > 0 {
			return status(0, "The "+words.noun+" has transactions associated to it, void all transactions associated to this account before deleting it"), nil
		}
		if err := s.q.DeleteGroupAccount(ctx, store.DeleteGroupAccountParams{GroupID: g.group.ID, ID: acct.ID}); err != nil {
			return nil, err
		}
		return status(1, strings.ToUpper(words.noun[:1])+words.noun[1:]+" deleted successfully"), nil
	})
}

// Pickers for opening accounts. Saccos and providers have no logos in
// Legion, so logo is empty.

func (s *Server) saccoOptions(ctx context.Context, c *call, g *member) (any, error) {
	rows, err := s.q.ListSaccos(ctx, false)
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, map[string]any{"id": fmt.Sprint(r.ID), "name": r.Name, "logo": ""})
	}
	return map[string]any{"status": 1, "message": "successful", "saccos": out}, nil
}

func (s *Server) saccoBranchOptions(ctx context.Context, c *call, g *member) (any, error) {
	id, err := strconv.ParseInt(c.req.Str("sacco_id"), 10, 64)
	if err != nil {
		return timed(status(0, "Request submitted is invalid"), s.now()), nil
	}
	rows, err := s.q.ListSaccoBranches(ctx, store.ListSaccoBranchesParams{SaccoID: id})
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, map[string]any{"id": fmt.Sprint(r.ID), "name": r.Name})
	}
	return map[string]any{"status": 1, "message": "successful", "sacco_branches": out}, nil
}

func (s *Server) mobileMoneyProviderOptions(ctx context.Context, c *call, g *member) (any, error) {
	rows, err := s.q.ListMobileMoneyProviders(ctx, false)
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, map[string]any{"id": fmt.Sprint(r.ID), "name": r.Name, "logo": ""})
	}
	return map[string]any{"status": 1, "message": "successful", "mobile_money_providers": out}, nil
}
