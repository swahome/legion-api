package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/swahome/legion-api/internal/auth"
	"github.com/swahome/legion-api/internal/books"
	"github.com/swahome/legion-api/internal/payments"
	"github.com/swahome/legion-api/internal/store"
)

// The bank panel (legacy /bank): staff of a partner bank see the groups
// that hold accounts with the bank, those accounts' balances and feeds, and
// per-branch figures. Platform admins can open any bank's panel.

type bankAccess struct {
	BankID int64
	Role   string // admin, staff
}

const bankKey ctxKey = iota + 200

func (s *Server) bankRoutes(r chi.Router) {
	r.Get("/banks", s.myBanks)
	r.Route("/{bankID}", func(r chi.Router) {
		r.Use(s.requireBank)
		r.Get("/", s.bankInfo)
		r.Get("/dashboard", s.bankDashboard)
		r.Get("/groups", s.bankGroups)
		r.Get("/groups/{groupID}", s.bankGroup)
		r.Get("/alerts", s.bankAlerts)
		r.Get("/alerts/{alertID}", s.bankAlert)
		r.Get("/users", s.bankUsers)
		r.Post("/users", s.addBankUser)
		r.Delete("/users/{userID}", s.removeBankUser)
	})
}

func (s *Server) requireBank(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		id, ok := pathID(r, "bankID")
		if !ok {
			writeError(w, http.StatusNotFound, "bank_not_found", "Bank not found.")
			return
		}
		claims := claimsFrom(ctx)
		a := bankAccess{BankID: id, Role: "admin"}
		if !claims.Admin {
			role, err := s.q.BankUserRole(ctx, store.BankUserRoleParams{BankID: id, UserID: claims.UserID})
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusNotFound, "bank_not_found", "Bank not found.")
				return
			}
			if err != nil {
				writeInternal(w, r, err)
				return
			}
			a.Role = string(role)
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, bankKey, a)))
	})
}

func bankFrom(ctx context.Context) bankAccess {
	a, _ := ctx.Value(bankKey).(bankAccess)
	return a
}

type bankPanelResponse struct {
	ID       int64           `json:"id"`
	Name     string          `json:"name"`
	Slug     string          `json:"slug"`
	Logo     string          `json:"logo"`
	Branding json.RawMessage `json:"branding"`
	Role     string          `json:"role"`
}

func rawJSON(b []byte) json.RawMessage {
	if len(b) == 0 {
		return json.RawMessage("{}")
	}
	return b
}

// myBanks: the banks whose panel the caller can open.
func (s *Server) myBanks(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	claims := claimsFrom(ctx)
	out := []bankPanelResponse{}
	if claims.Admin {
		rows, err := s.q.BanksWithAccounts(ctx)
		if err != nil {
			writeInternal(w, r, err)
			return
		}
		for _, b := range rows {
			out = append(out, bankPanelResponse{ID: b.ID, Name: b.Name, Slug: b.Slug, Logo: b.Logo, Branding: rawJSON(b.Branding), Role: "admin"})
		}
	} else {
		rows, err := s.q.UserBanks(ctx, claims.UserID)
		if err != nil {
			writeInternal(w, r, err)
			return
		}
		for _, b := range rows {
			out = append(out, bankPanelResponse{ID: b.ID, Name: b.Name, Slug: b.Slug, Logo: b.Logo, Branding: rawJSON(b.Branding), Role: string(b.Role)})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"banks": out})
}

func (s *Server) bankInfo(w http.ResponseWriter, r *http.Request) {
	a := bankFrom(r.Context())
	b, err := s.q.GetBankBasic(r.Context(), a.BankID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "bank_not_found", "Bank not found.")
		return
	}
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, bankPanelResponse{ID: b.ID, Name: b.Name, Slug: b.Slug, Logo: b.Logo, Branding: rawJSON(b.Branding), Role: a.Role})
}

// bankDashboard: per-branch figures for a period (default today), as the
// legacy daily KPIs: accounts, groups, new groups, money in and out.
func (s *Server) bankDashboard(w http.ResponseWriter, r *http.Request) {
	a := bankFrom(r.Context())
	today := time.Now().In(s.location)
	y, m, d := today.Date()
	from, to := time.Date(y, m, d, 0, 0, 0, 0, time.UTC), time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	f, ok1 := optionalDate(r, "from")
	t, ok2 := optionalDate(r, "to")
	if !ok1 || !ok2 {
		writeError(w, http.StatusBadRequest, "invalid_filter", "Dates look like 2026-11-05.")
		return
	}
	if f != nil {
		from = *f
	}
	if t != nil {
		to = *t
	}
	if to.Before(from) {
		writeError(w, http.StatusBadRequest, "invalid_filter", "from must be on or before to.")
		return
	}
	rows, err := s.q.BankBranchKPIs(r.Context(), store.BankBranchKPIsParams{BankID: &a.BankID, FromDate: from, ToDate: to})
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	type branch struct {
		BranchID  *int64 `json:"branch_id"`
		Branch    string `json:"branch"`
		Accounts  int32  `json:"accounts"`
		Groups    int32  `json:"groups"`
		NewGroups int32  `json:"new_groups"`
		MoneyIn   string `json:"money_in"`
		MoneyOut  string `json:"money_out"`
		Alerts    int32  `json:"alerts"`
	}
	branches := make([]branch, 0, len(rows))
	var accounts, groups, newGroups, alerts int32
	var in, out int64
	for _, b := range rows {
		branches = append(branches, branch{b.BankBranchID, b.Branch, b.Accounts, b.Groups, b.NewGroups, money(b.MoneyIn), money(b.MoneyOut), b.Alerts})
		accounts += b.Accounts
		groups += b.Groups
		newGroups += b.NewGroups
		alerts += b.Alerts
		in += books.Cents(b.MoneyIn)
		out += books.Cents(b.MoneyOut)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"from": Date{from}, "to": Date{to},
		"totals": map[string]any{"accounts": accounts, "groups": groups, "new_groups": newGroups, "alerts": alerts,
			"money_in": centsMoney(in), "money_out": centsMoney(out)},
		"branches": branches,
	})
}

func centsMoney(c int64) string {
	neg := c < 0
	if neg {
		c = -c
	}
	s := strconv.FormatInt(c/100, 10) + "." + strconv.FormatInt(c%100/10, 10) + strconv.FormatInt(c%10, 10)
	if neg {
		return "-" + s
	}
	return s
}

func (s *Server) bankGroups(w http.ResponseWriter, r *http.Request) {
	a := bankFrom(r.Context())
	pg := pageParams(r)
	branch, ok := optionalID(r, "branch_id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_filter", "branch_id must be an id.")
		return
	}
	rows, err := s.q.BankGroups(r.Context(), store.BankGroupsParams{BankID: &a.BankID, Search: queryPtr(r, "search"),
		BranchID: branch, PageSize: pg.size, PageOffset: pg.offset})
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	type groupRow struct {
		ID             int64     `json:"id"`
		Name           string    `json:"name"`
		Status         string    `json:"status"`
		CreatedAt      time.Time `json:"created_at"`
		Accounts       int32     `json:"accounts"`
		AccountNumbers string    `json:"account_numbers"`
		Branches       string    `json:"branches"`
		Members        int32     `json:"members"`
		Unreconciled   int32     `json:"unreconciled"`
		Balance        string    `json:"balance"`
	}
	items := make([]groupRow, 0, len(rows))
	var total int64
	for _, g := range rows {
		items = append(items, groupRow{g.ID, g.Name, string(g.Status), g.CreatedAt, g.Accounts, g.AccountNumbers,
			g.Branches, g.Members, g.Unreconciled, money(g.Balance)})
		total = g.Total
	}
	writeJSON(w, http.StatusOK, newPaged(items, total, pg))
}

func (s *Server) bankGroup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a := bankFrom(ctx)
	gid, ok := pathID(r, "groupID")
	if ok {
		ok, _ = s.q.BankGroupOK(ctx, store.BankGroupOKParams{GroupID: gid, BankID: &a.BankID})
	}
	if !ok {
		writeError(w, http.StatusNotFound, "group_not_found", "Group not found.")
		return
	}
	g, err := s.q.BankGroupBasic(ctx, gid)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	accts, err := s.q.BankGroupAccounts(ctx, store.BankGroupAccountsParams{GroupID: gid, BankID: &a.BankID})
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	type acct struct {
		ID            int64  `json:"id"`
		Name          string `json:"name"`
		AccountNumber string `json:"account_number"`
		Branch        string `json:"branch"`
		IsDefault     bool   `json:"is_default"`
		IsClosed      bool   `json:"is_closed"`
		Balance       string `json:"balance"`
	}
	list := make([]acct, 0, len(accts))
	for _, x := range accts {
		list = append(list, acct{x.ID, x.Name, x.AccountNumber, x.Branch, x.IsDefault, x.IsClosed, money(x.Balance)})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": g.ID, "name": g.Name, "status": g.Status, "created_at": g.CreatedAt, "currency": g.Currency,
		"members": g.Members, "accounts": list,
	})
}

// bankAlerts: the feed for the bank's accounts; group_id narrows it.
func (s *Server) bankAlerts(w http.ResponseWriter, r *http.Request) {
	a := bankFrom(r.Context())
	p := store.ListAlertsParams{BankID: &a.BankID}
	gid, ok := optionalID(r, "group_id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_filter", "group_id must be an id.")
		return
	}
	p.GroupID = gid
	if !alertFilters(w, r, &p) {
		return
	}
	s.writeAlerts(w, r, p)
}

func (s *Server) bankAlert(w http.ResponseWriter, r *http.Request) {
	a := bankFrom(r.Context())
	id, ok := pathID(r, "alertID")
	if !ok {
		writeError(w, http.StatusNotFound, "alert_not_found", "Transaction not found.")
		return
	}
	d, err := s.alertDetail(r, nil, id, false)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "alert_not_found", "Transaction not found.")
		return
	}
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	inBank := d.Bank != nil && d.Bank.ID == a.BankID
	if !inBank && d.Group != nil {
		inBank, _ = s.q.BankGroupOK(r.Context(), store.BankGroupOKParams{GroupID: d.Group.ID, BankID: &a.BankID})
	}
	if !inBank {
		writeError(w, http.StatusNotFound, "alert_not_found", "Transaction not found.")
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func requireBankAdmin(w http.ResponseWriter, a bankAccess) bool {
	if a.Role != "admin" {
		writeError(w, http.StatusForbidden, "bank_admins_only", "Only the bank's admins can manage its users.")
		return false
	}
	return true
}

func (s *Server) bankUsers(w http.ResponseWriter, r *http.Request) {
	a := bankFrom(r.Context())
	rows, err := s.q.ListBankUsers(r.Context(), a.BankID)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	type user struct {
		UserID    int64     `json:"user_id"`
		Name      string    `json:"name"`
		Phone     string    `json:"phone"`
		Email     string    `json:"email"`
		Role      string    `json:"role"`
		CreatedAt time.Time `json:"created_at"`
	}
	out := make([]user, 0, len(rows))
	for _, u := range rows {
		out = append(out, user{u.UserID, strings.TrimSpace(u.FirstName + " " + u.LastName), u.Phone, u.Email, string(u.Role), u.CreatedAt})
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out})
}

// addBankUser: POST /bank/{id}/users {"phone": "0712...", "role": "staff"}.
// The person must already have a Legion account.
func (s *Server) addBankUser(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a := bankFrom(ctx)
	if !requireBankAdmin(w, a) {
		return
	}
	var req struct {
		Phone string `json:"phone"`
		Role  string `json:"role"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	role := store.BankUserRole(req.Role)
	if req.Role == "" {
		role = store.BankUserRoleStaff
	}
	if !role.Valid() {
		writeError(w, http.StatusBadRequest, "invalid_role", "role is admin or staff.")
		return
	}
	phone, err := auth.NormalizePhone(req.Phone, s.defaultCallingCode)
	if err != nil || strings.TrimSpace(req.Phone) == "" {
		writeError(w, http.StatusBadRequest, "invalid_phone", "Enter the person's phone number.")
		return
	}
	uid, err := s.q.UserIDByPhone(ctx, &phone)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusBadRequest, "user_not_found", "Nobody has a Legion account with that phone number. Ask them to sign up first.")
		return
	}
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	me := claimsFrom(ctx).UserID
	if err := s.q.UpsertBankUser(ctx, store.UpsertBankUserParams{BankID: a.BankID, UserID: uid, Role: role, CreatedBy: &me}); err != nil {
		writeInternal(w, r, err)
		return
	}
	s.bankUsers(w, r)
}

func (s *Server) removeBankUser(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a := bankFrom(ctx)
	if !requireBankAdmin(w, a) {
		return
	}
	uid, ok := pathID(r, "userID")
	if !ok {
		writeError(w, http.StatusNotFound, "user_not_found", "User not found.")
		return
	}
	if uid == claimsFrom(ctx).UserID && !claimsFrom(ctx).Admin {
		writeError(w, http.StatusBadRequest, "cannot_remove_self", "You cannot remove yourself.")
		return
	}
	n, err := s.q.DeleteBankUser(ctx, store.DeleteBankUserParams{BankID: a.BankID, UserID: uid})
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	if n == 0 {
		writeError(w, http.StatusNotFound, "user_not_found", "User not found.")
		return
	}
	s.bankUsers(w, r)
}

// ---- Platform admin: every alert, payment and webhook ----

func (s *Server) adminListAlerts(w http.ResponseWriter, r *http.Request) {
	p := store.ListAlertsParams{Unassigned: r.URL.Query().Get("unassigned") == "true"}
	var ok1, ok2 bool
	p.GroupID, ok1 = optionalID(r, "group_id")
	p.BankID, ok2 = optionalID(r, "bank_id")
	if !ok1 || !ok2 {
		writeError(w, http.StatusBadRequest, "invalid_filter", "Filters must be ids or dates like 2026-11-05.")
		return
	}
	if !alertFilters(w, r, &p) {
		return
	}
	s.writeAlerts(w, r, p)
}

func (s *Server) adminGetAlert(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "alertID")
	if !ok {
		writeError(w, http.StatusNotFound, "alert_not_found", "Transaction not found.")
		return
	}
	s.writeAlertDetail(w, r, nil, id, http.StatusOK)
}

// adminAssignAlert: POST /admin/transaction-alerts/{id}/assign
// {"account_id": 12} gives an alert no account matched to a group account,
// then tries to reconcile it.
func (s *Server) adminAssignAlert(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := pathID(r, "alertID")
	if !ok {
		writeError(w, http.StatusNotFound, "alert_not_found", "Transaction not found.")
		return
	}
	var req struct {
		AccountID int64 `json:"account_id"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		a, err := q.LockAlert(ctx, id)
		if err != nil {
			return err
		}
		if a.GroupID != nil {
			return badRequest{Code: "already_assigned", Msg: "This transaction already belongs to a group."}
		}
		gid, err := q.AccountGroup(ctx, req.AccountID)
		if errors.Is(err, pgx.ErrNoRows) {
			return badRequest{Code: "invalid_account", Msg: "That account does not exist."}
		}
		if err != nil {
			return err
		}
		if err := q.AssignAlert(ctx, store.AssignAlertParams{ID: id, GroupID: &gid, AccountID: &req.AccountID}); err != nil {
			return err
		}
		_, err = payments.AutoReconcile(ctx, q, id, s.defaultCallingCode)
		return err
	})
	var bad badRequest
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "alert_not_found", "Transaction not found.")
	case errors.As(err, &bad):
		writeError(w, http.StatusBadRequest, bad.Code, bad.Msg)
	case err != nil:
		writeInternal(w, r, err)
	default:
		s.writeAlertDetail(w, r, nil, id, http.StatusOK)
	}
}

func (s *Server) adminListPayments(w http.ResponseWriter, r *http.Request) {
	gid, ok := optionalID(r, "group_id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_filter", "group_id must be an id.")
		return
	}
	s.writePayments(w, r, store.ListPaymentRequestsParams{GroupID: gid})
}

func (s *Server) adminListWebhooks(w http.ResponseWriter, r *http.Request) {
	pg := pageParams(r)
	rows, err := s.q.ListWebhookEvents(r.Context(), store.ListWebhookEventsParams{
		Source: queryPtr(r, "source"), Outcome: queryPtr(r, "outcome"), PageSize: pg.size, PageOffset: pg.offset,
	})
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	type event struct {
		ID        int64     `json:"id"`
		Source    string    `json:"source"`
		RemoteIP  string    `json:"remote_ip"`
		Body      string    `json:"body"`
		Outcome   string    `json:"outcome"`
		Detail    string    `json:"detail"`
		AlertID   *int64    `json:"alert_id"`
		CreatedAt time.Time `json:"created_at"`
	}
	items := make([]event, 0, len(rows))
	var total int64
	for _, e := range rows {
		items = append(items, event{e.ID, e.Source, e.RemoteIp, redactBody(e.Body), e.Outcome, e.Detail, e.AlertID, e.CreatedAt})
		total = e.Total
	}
	writeJSON(w, http.StatusOK, newPaged(items, total, pg))
}

// redactBody hides feed passwords when support reads the webhook log.
func redactBody(b string) string {
	m, ok := decodeLoose([]byte(b))
	if !ok {
		return b
	}
	if _, has := m["password"]; has {
		m["password"] = "***"
	}
	out, _ := json.Marshal(m)
	return string(out)
}
