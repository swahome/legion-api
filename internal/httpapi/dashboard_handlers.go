package httpapi

import (
	"net/http"
	"strconv"
	"time"

	"github.com/swahome/legion-api/internal/billing"
	"github.com/swahome/legion-api/internal/books"
	"github.com/swahome/legion-api/internal/store"
)

// The group dashboard: the legacy group home page (Group::index) and the
// mobile get_group_dashboard_data, in one response. Any member may read the
// group totals, as in the mobile app; the latest cash book movements and the
// pending withdrawal requests are for officials.

type dashboardMonth struct {
	Month         string `json:"month"` // 2026-10
	Contributions string `json:"contributions"`
	Fines         string `json:"fines"`
	Misc          string `json:"misc"`
	Income        string `json:"income"`
	Expenses      string `json:"expenses"`
}

type dashboardYear struct {
	Year          int32  `json:"year"`
	Contributions string `json:"contributions"`
	Fines         string `json:"fines"`
	Misc          string `json:"misc"`
	Income        string `json:"income"`
	Expenses      string `json:"expenses"`
}

type dashboardMovement struct {
	ID          int64  `json:"id"`
	Date        Date   `json:"date"`
	Direction   string `json:"direction"` // in | out
	Kind        string `json:"kind"`      // deposit or withdrawal kind, transfer, or "" for legacy rows
	Amount      string `json:"amount"`
	Description string `json:"description"`
	Account     string `json:"account"`
	AccountKind string `json:"account_kind"`
	Member      string `json:"member"`
}

type dashboardOwed struct {
	Owed    string `json:"owed"`
	Members int64  `json:"members"`
}

func (s *Server) groupDashboard(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	ctx := r.Context()
	months := 12
	if v := r.URL.Query().Get("months"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 36 {
			writeError(w, http.StatusBadRequest, "invalid_months", "months must be between 1 and 36.")
			return
		}
		months = n
	}
	today := billing.LocalToday(time.Now(), s.location)
	from := time.Date(today.Year(), today.Month()-time.Month(months-1), 1, 0, 0, 0, 0, time.UTC)

	cashRows, err := s.q.DashboardCash(ctx, a.Group.ID)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	byKind := map[string]string{}
	accounts := map[string]int64{}
	var atBank, atHand int64
	for _, c := range cashRows {
		byKind[string(c.Kind)] = money(c.Balance)
		accounts[string(c.Kind)] = c.Accounts
		switch c.Kind {
		case store.AccountKindBank, store.AccountKindSacco:
			atBank += cents(c.Balance)
		default:
			atHand += cents(c.Balance)
		}
	}

	yearly, err := s.q.DashboardYearly(ctx, a.Group.ID)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	years := make([]dashboardYear, 0, len(yearly))
	var sum [5]int64
	for _, y := range yearly {
		for i, v := range []string{y.Contributions, y.Fines, y.Misc, y.Income, y.Expenses} {
			sum[i] += cents(v)
		}
		years = append(years, dashboardYear{
			Year: y.Year, Contributions: money(y.Contributions), Fines: money(y.Fines),
			Misc: money(y.Misc), Income: money(y.Income), Expenses: money(y.Expenses),
		})
	}
	monthly, err := s.q.DashboardMonthly(ctx, store.DashboardMonthlyParams{GroupID: a.Group.ID, FromMonth: from, Today: today})
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	series := make([]dashboardMonth, 0, len(monthly))
	for _, m := range monthly {
		series = append(series, dashboardMonth{
			Month: m.Month.Format("2006-01"), Contributions: money(m.Contributions), Fines: money(m.Fines),
			Misc: money(m.Misc), Income: money(m.Income), Expenses: money(m.Expenses),
		})
	}

	owedRows, err := s.q.DashboardOwed(ctx, a.Group.ID)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	owed := map[string]dashboardOwed{
		"contributions": {Owed: "0.00"}, "fines": {Owed: "0.00"}, "misc": {Owed: "0.00"},
	}
	bucketNames := map[string]string{"contribution": "contributions", "fine": "fines", "misc": "misc"}
	for _, o := range owedRows {
		if name, ok := bucketNames[o.Bucket]; ok {
			owed[name] = dashboardOwed{Owed: money(o.Owed), Members: o.Members}
		}
	}
	behind, err := s.q.DashboardMembersBehind(ctx, a.Group.ID)
	if err != nil {
		writeInternal(w, r, err)
		return
	}

	out := map[string]any{
		"currency": a.Group.Currency,
		"as_of":    Date{today},
		"cash": map[string]any{
			"at_bank": formatCents(atBank), "at_hand": formatCents(atHand), "total": formatCents(atBank + atHand),
			"by_kind": byKind, "accounts": accounts,
		},
		"totals": map[string]string{
			"contributions": formatCents(sum[0]), "fines": formatCents(sum[1]), "misc": formatCents(sum[2]),
			"income": formatCents(sum[3]), "expenses": formatCents(sum[4]),
		},
		"months":         series,
		"years":          years,
		"owed":           owed,
		"members_behind": behind,
	}

	if a.IsOfficial {
		recent, err := s.q.DashboardRecent(ctx, store.DashboardRecentParams{GroupID: a.Group.ID, Today: today, RowLimit: 10})
		if err != nil {
			writeInternal(w, r, err)
			return
		}
		moves := make([]dashboardMovement, 0, len(recent))
		for _, e := range recent {
			dir := "in"
			if e.Direction < 0 {
				dir = "out"
			}
			moves = append(moves, dashboardMovement{
				ID: e.ID, Date: Date{e.EntryDate}, Direction: dir, Kind: e.Kind, Amount: money(e.Amount),
				Description: e.Description, Account: e.AccountName, AccountKind: string(e.AccountKind), Member: e.MemberName,
			})
		}
		pending, err := s.q.DashboardPendingRequests(ctx, a.Group.ID)
		if err != nil {
			writeInternal(w, r, err)
			return
		}
		out["recent"] = moves
		out["pending_withdrawal_requests"] = pending
		lt, err := s.q.LoanTotals(ctx, a.Group.ID)
		if err != nil {
			writeInternal(w, r, err)
			return
		}
		out["loans"] = map[string]any{"count": lt.Loans, "lent": money(lt.Lent), "repaid": money(lt.Repaid),
			"balance": centsText(books.Cents(lt.Payable) - books.Cents(lt.Repaid))}
	}
	writeJSON(w, http.StatusOK, out)
}
