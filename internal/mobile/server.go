package mobile

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/swahome/legion-api/internal/auth"
	"github.com/swahome/legion-api/internal/store"
)

const (
	// Builds at or below this version are told to update, as before.
	minVersionCode = 73
	// Above this version, responses drop the outer {"response": ...}.
	unwrapVersionCode = 77

	tokenTTL      = 100 * 24 * time.Hour // a new token, as legacy Chamasoft
	tokenSlideTTL = 30 * 24 * time.Hour  // each use keeps it alive at least this long
	maxBodyBytes  = 2 << 20
)

// Server answers the mobile app. Actions are registered by their legacy
// path, like "login" or "contributions/get_group_contributions".
type Server struct {
	pool    *pgxpool.Pool
	q       *store.Queries
	keys    *Keys
	auth    *auth.Service
	otp     OTPSender
	now     func() time.Time
	actions map[string]action
}

// action handles one decrypted request and returns the legacy "response".
type action struct {
	public bool // reachable without an access token (login, PIN sign-in)
	fn     func(ctx context.Context, c *call) (any, error)
}

// call is one decrypted request.
type call struct {
	req     request
	userID  int64 // from the access token; 0 on public actions
	r       *http.Request
	version int
}

func New(pool *pgxpool.Pool, keys *Keys, authSvc *auth.Service, otp OTPSender) *Server {
	if otp == nil {
		otp = noOTPSender{}
	}
	s := &Server{pool: pool, q: store.New(pool), keys: keys, auth: authSvc, otp: otp, now: time.Now}
	s.actions = map[string]action{
		"login":                 {public: true, fn: s.login},
		"generate_pin":          {public: true, fn: s.generatePin},
		"resend_pin":            {public: true, fn: s.resendPin},
		"verify_pin":            {public: true, fn: s.verifyPin},
		"get_user_checkin_data": {fn: s.getUserCheckinData},

		"get_group_data":                                {fn: s.groupAction(0, s.getGroupData)},
		"get_member_dashboard_data":                     {fn: s.groupAction(0, s.getMemberDashboardData)},
		"get_group_dashboard_data":                      {fn: s.groupAction(0, s.getGroupDashboardData)},
		"get_group_member_contribution_summary":         {fn: s.groupAction(0, s.getGroupMemberContributionSummary)},
		"countries/get_country_options":                 {fn: s.userAction(s.getCountryOptions)},
		"countries/get_currency_options":                {fn: s.userAction(s.getCurrencyOptions)},
		"banks/get_bank_options":                        {fn: s.groupAction(6, s.getBankOptions)},
		"bank_branches/get_bank_branches_options":       {fn: s.groupAction(6, s.getBankBranchOptions)},
		"group_roles/group_roles_list":                  {fn: s.groupAction(6, s.groupRolesList)},
		"members/get_members_and_details":               {fn: s.groupAction(6, s.getMembersAndDetails)},
		"members/get_group_member_options":              {fn: s.groupAction(6, s.getGroupMemberOptions)},
		"contributions/get_group_contributions":         {fn: s.groupAction(6, s.getGroupContributions)},
		"contributions/get_group_contribution":          {fn: s.groupAction(6, s.getGroupContribution)},
		"contributions/get_group_contribution_options":  {fn: s.groupAction(6, s.getGroupContributionOptions)},
		"contributions/get_member_contribution_options": {fn: s.groupAction(6, s.getMemberContributionOptions)},
		"statements/contribution_statement":             {fn: s.groupAction(0, s.contributionStatement)},

		// Milestone 3: money, read side.
		"get_member_recent_transactions":                             {fn: s.groupAction(0, s.recentTransactions(false))},
		"get_group_recent_transactions":                              {fn: s.groupAction(0, s.recentTransactions(true))},
		"accounts/get_group_account_options":                         {fn: s.groupAction(6, s.accountOptions(false))},
		"accounts/get_group_active_account_options":                  {fn: s.groupAction(6, s.accountOptions(true))},
		"bank_accounts/get_group_bank_accounts_list":                 {fn: s.groupAction(6, s.accountsList("bank"))},
		"sacco_accounts/get_group_sacco_accounts_list":               {fn: s.groupAction(6, s.accountsList("sacco"))},
		"mobile_money_accounts/get_group_mobile_money_accounts_list": {fn: s.groupAction(6, s.accountsList("mobile_money"))},
		"petty_cash_accounts/get_group_petty_cash_accounts_list":     {fn: s.groupAction(6, s.accountsList("petty_cash"))},
		"income_categories/group_income_categories_list":             {fn: s.groupAction(6, s.categoryList("income"))},
		"income_categories/get_group_income_category_options":        {fn: s.groupAction(6, s.categoryOptions("income"))},
		"expense_categories/get_group_expense_categories":            {fn: s.groupAction(6, s.categoryList("expense"))},
		"expense_categories/get_group_expense_category_options":      {fn: s.groupAction(6, s.categoryOptions("expense"))},
		"fine_categories/group_fine_categories_list":                 {fn: s.groupAction(6, s.categoryList("fine"))},
		"fine_categories/get_group_fine_category_options":            {fn: s.groupAction(6, s.categoryOptions("fine"))},
		"depositors/get_group_depositor_options":                     {fn: s.groupAction(6, s.depositorOptions)},
		"deposits/get_deposits_list":                                 {fn: s.groupAction(6, s.depositsList)},
		"deposits/get_group_deposit":                                 {fn: s.groupAction(6, s.groupDeposit)},
		"withdrawals/get_group_withdrawal_list":                      {fn: s.groupAction(6, s.withdrawalsList)},
		"withdrawals/get_group_withdrawal":                           {fn: s.groupAction(6, s.groupWithdrawal)},
		"invoices/get_group_invoices":                                {fn: s.groupAction(6, s.invoicesList)},
		"invoices/get_group_invoice":                                 {fn: s.groupAction(6, s.groupInvoice)},
		"fines/get_group_fines_list":                                 {fn: s.groupAction(6, s.finesList)},
		"fines/get_group_fine_options":                               {fn: s.groupAction(6, s.fineOptions)},

		// Milestone 3: recording money from the app.
		"deposits/record_contribution_payments":                    {fn: s.groupAction(6, s.recordDeposits("contribution", "Error contribution payments array empty."))},
		"deposits/record_fine_payments":                            {fn: s.groupAction(6, s.recordDeposits("fine", "Error fine payments array empty."))},
		"deposits/record_miscellaneous_payments":                   {fn: s.groupAction(6, s.recordDeposits("misc", "Error miscellaneous payments array empty."))},
		"deposits/record_income":                                   {fn: s.groupAction(6, s.recordDeposits("income", "Error income payments array empty."))},
		"deposits/new_record_contribution_payments":                {fn: s.groupAction(6, s.newMemberDeposits("contribution"))},
		"deposits/new_record_fine_payments":                        {fn: s.groupAction(6, s.newMemberDeposits("fine"))},
		"deposits/new_record_miscellaneous_payments":               {fn: s.groupAction(6, s.newDeposit("misc"))},
		"deposits/new_record_income_payment":                       {fn: s.groupAction(6, s.newDeposit("income"))},
		"deposits/void":                                            {fn: s.groupAction(6, s.officialsOnly(s.voidDeposit))},
		"withdrawals/record_expenses":                              {fn: s.groupAction(6, s.officialsOnly(s.recordExpenses))},
		"withdrawals/new_record_expenses":                          {fn: s.groupAction(6, s.officialsOnly(s.newRecordExpense))},
		"withdrawals/record_contribution_refund":                   {fn: s.groupAction(6, s.officialsOnly(s.recordContributionRefund))},
		"withdrawals/record_funds_transfer":                        {fn: s.groupAction(6, s.officialsOnly(s.recordFundsTransfer))},
		"withdrawals/void":                                         {fn: s.groupAction(6, s.officialsOnly(s.voidWithdrawal))},
		"fines/fine_members":                                       {fn: s.groupAction(6, s.officialsOnly(s.fineMembers))},
		"fines/new_fine_members":                                   {fn: s.groupAction(6, s.officialsOnly(s.newFineMembers))},
		"invoices/create":                                          {fn: s.groupAction(6, s.officialsOnly(s.createInvoices))},
		"invoices/void":                                            {fn: s.groupAction(6, s.officialsOnly(s.voidInvoice))},
		"saccos/get_sacco_options":                                 {fn: s.groupAction(6, s.saccoOptions)},
		"sacco_branches/get_sacco_branches_options":                {fn: s.groupAction(6, s.saccoBranchOptions)},
		"mobile_money_providers/get_mobile_money_provider_options": {fn: s.groupAction(6, s.mobileMoneyProviderOptions)},
		"withdrawals/withdrawal_request_list":                      {fn: s.groupAction(6, s.withdrawalRequestList)},
		"withdrawals/view_withdrawal_request":                      {fn: s.groupAction(6, s.viewWithdrawalRequest)},
		"withdrawals/request_funds_transfer":                       {fn: s.groupAction(6, s.officialsOnly(s.requestFundsTransfer))},
		"withdrawals/respond_to_withdrawal_request":                {fn: s.groupAction(6, s.respondToWithdrawalRequest)},
		"withdrawals/cancel_withdrawal_request":                    {fn: s.groupAction(6, s.cancelWithdrawalRequest)},
		"reports/contribution_summary":                             {fn: s.groupAction(6, s.memberSummary(false))},
		"reports/fines_summary":                                    {fn: s.groupAction(6, s.memberSummary(true))},
		"reports/expenses_summary":                                 {fn: s.groupAction(6, s.expensesSummary)},
		"reports/transaction_statement":                            {fn: s.groupAction(6, s.transactionStatement)},
		"depositors/create":                                        {fn: s.groupAction(6, s.officialsOnly(s.createDepositor))},
	}
	for module, kind := range map[string]store.AccountKind{
		"bank_accounts": store.AccountKindBank, "sacco_accounts": store.AccountKindSacco,
		"mobile_money_accounts": store.AccountKindMobileMoney, "petty_cash_accounts": store.AccountKindPettyCash,
	} {
		s.actions[module+"/create"] = action{fn: s.groupAction(6, s.createAccount(kind))}
		s.actions[module+"/edit"] = action{fn: s.groupAction(6, s.editAccount(kind))}
		s.actions[module+"/close"] = action{fn: s.groupAction(6, s.closeAccount(kind, true))}
		s.actions[module+"/activate"] = action{fn: s.groupAction(6, s.closeAccount(kind, false))}
		s.actions[module+"/delete"] = action{fn: s.groupAction(6, s.deleteAccount(kind))}
	}
	for module, kind := range map[string]string{"income_categories": "income", "expense_categories": "expense", "fine_categories": "fine"} {
		s.actions[module+"/create"] = action{fn: s.groupAction(6, s.createCategory(kind))}
		s.actions[module+"/edit"] = action{fn: s.groupAction(6, s.editCategory(kind))}
		s.actions[module+"/hide"] = action{fn: s.groupAction(6, s.hideCategory(kind, true))}
		s.actions[module+"/unhide"] = action{fn: s.groupAction(6, s.hideCategory(kind, false))}
		s.actions[module+"/delete"] = action{fn: s.groupAction(6, s.deleteCategory(kind))}
	}
	return s
}

// Routes serves mobile/<action> and mobile/<module>/<action>. Mount it
// where the app's base URL points, e.g. /v1/legacy.
func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()
	r.Post("/mobile/{action}", s.serve)
	r.Post("/mobile/{module}/{action}", s.serve)
	return r
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "action")
	if m := chi.URLParam(r, "module"); m != "" {
		name = m + "/" + name
	}
	version, _ := strconv.Atoi(firstHeader(r, "Versioncode", "VersionCode"))
	out := &responder{w: w, keys: s.keys, version: version}

	act, known := s.actions[name]
	c := &call{r: r, version: version}

	// The checks run in the legacy order, so the app sees the same
	// answers: token, Secret header, version, body.
	if !act.public || !known {
		token := bearer(r)
		if token == "" {
			out.send(http.StatusOK, status("401", "You are currently using an outdated application. Update to the latest version"))
			return
		}
		uid, err := s.q.UseMobileToken(r.Context(), store.UseMobileTokenParams{
			TokenHash: hashToken(token), MinExpiresAt: s.now().Add(tokenSlideTTL),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			out.send(http.StatusOK, status(400, "Authentication token expired"))
			return
		}
		if err != nil {
			out.internal(r, err)
			return
		}
		c.userID = uid
	}
	secret := r.Header.Get("Secret")
	if secret == "" {
		out.send(http.StatusOK, status("401", "You are using an outdated application. Update to the latest application"))
		return
	}
	if version <= minVersionCode {
		// Sent unencrypted, as before: an old build may not read the new scheme.
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": 0, "message": "You are using an outdated application. Update to the latest application"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil || len(body) == 0 {
		out.send(http.StatusOK, status(2, "Empty file sent from the client"))
		return
	}
	plain, err := s.keys.DecryptRequest(secret, body)
	if err == nil {
		err = json.Unmarshal(plain, &c.req)
	}
	if err != nil || c.req == nil {
		out.send(http.StatusOK, status(3, "You are currently using an outdated application. Kindly update to latest version. Your request could not be handled at the moment. if the error persist, contact admin"))
		return
	}
	// Recording actions carry a request id so a retry isn't recorded twice
	// (legacy _request_id_required). It is checked before the user id, as before.
	var requestID string
	if known && requestIDActions[name] {
		reply, err := s.claimRequestID(r.Context(), c.req.Str("request_id"))
		if err != nil {
			out.internal(r, err)
			return
		}
		if reply != nil {
			out.sendUnwrapped(http.StatusOK, reply)
			return
		}
		requestID = c.req.Str("request_id")
	}
	if c.userID != 0 && c.req.Int("user_id") != c.userID {
		out.send(http.StatusOK, status(400, "user Authentication token is invalid."))
		return
	}
	if !known {
		out.send(http.StatusNotFound, map[string]any{"status": 0, "error": "404 Method Not Found for URI: mobile/" + name})
		return
	}
	resp, err := act.fn(r.Context(), c)
	if requestID != "" && (err != nil || failed(resp)) {
		// Let the app retry a request that wasn't recorded.
		if err := s.q.ReleaseMobileRequestID(context.WithoutCancel(r.Context()), requestID); err != nil {
			slog.ErrorContext(r.Context(), "mobile: release request id", "err", err)
		}
	}
	if err != nil {
		out.internal(r, err)
		return
	}
	out.send(http.StatusOK, resp)
}

type responder struct {
	w       http.ResponseWriter
	keys    *Keys
	version int
}

// send encrypts resp, wrapped in {"response": ...} for builds up to 77.
func (o *responder) send(code int, resp any) {
	var v any = resp
	if o.version <= unwrapVersionCode {
		v = map[string]any{"response": resp}
	}
	o.write(code, v)
}

// sendUnwrapped encrypts resp as it is, for the answers the legacy
// controller sent before reaching an action.
func (o *responder) sendUnwrapped(code int, resp any) { o.write(code, resp) }

func (o *responder) write(code int, v any) {
	b, err := o.keys.EncryptResponse(v)
	if err != nil {
		slog.Error("mobile: encrypt response", "err", err)
		http.Error(o.w, "", http.StatusInternalServerError)
		return
	}
	o.w.Header().Set("Content-Type", "application/json")
	o.w.WriteHeader(code)
	_, _ = o.w.Write(b)
}

func (o *responder) internal(r *http.Request, err error) {
	slog.ErrorContext(r.Context(), "mobile: internal error", "err", err, "path", r.URL.Path)
	o.send(http.StatusOK, status(0, "Something went wrong. Try again later."))
}

func status(code any, msg string) map[string]any {
	return map[string]any{"status": code, "message": msg}
}

func bearer(r *http.Request) string {
	v := strings.TrimSpace(r.Header.Get("Authorization"))
	for _, p := range []string{"Bearer", "Basic"} {
		v = strings.TrimSpace(strings.TrimPrefix(v, p))
	}
	return strings.Trim(v, `"`)
}

func firstHeader(r *http.Request, names ...string) string {
	for _, n := range names {
		if v := r.Header.Get(n); v != "" {
			return v
		}
	}
	return ""
}

func hashToken(t string) []byte {
	h := sha256.Sum256([]byte(t))
	return h[:]
}

func clientInfo(r *http.Request) auth.ClientInfo {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	var ip *netip.Addr
	if a, err := netip.ParseAddr(strings.TrimSpace(host)); err == nil {
		a = a.Unmap()
		ip = &a
	}
	return auth.ClientInfo{IP: ip, UserAgent: r.UserAgent()}
}

// request is a decrypted request body. The app sends numbers both as JSON
// numbers and as strings, so the getters accept either.
type request map[string]any

func (q request) Str(key string) string {
	switch v := q[key].(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		if v {
			return "1"
		}
	}
	return ""
}

func (q request) Int(key string) int64 {
	n, _ := strconv.ParseInt(q.Str(key), 10, 64)
	return n
}

// Strs reads a list the app sends as a JSON array (or a single value).
func (q request) Strs(key string) []string {
	switch v := q[key].(type) {
	case []any:
		out := make([]string, 0, len(v))
		for _, x := range v {
			out = append(out, request{"v": x}.Str("v"))
		}
		return out
	case nil:
		return nil
	}
	if s := q.Str(key); s != "" {
		return []string{s}
	}
	return nil
}

// IDs reads a list of positive ids, ignoring anything else.
func (q request) IDs(key string) []int64 {
	var out []int64
	for _, s := range q.Strs(key) {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 {
			out = append(out, n)
		}
	}
	return out
}
