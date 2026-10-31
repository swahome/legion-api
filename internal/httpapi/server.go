// Package httpapi is the REST API.
package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/swahome/legion-api/internal/auth"
	"github.com/swahome/legion-api/internal/billing"
	"github.com/swahome/legion-api/internal/store"
)

type Server struct {
	pool               *pgxpool.Pool
	q                  *store.Queries
	auth               *auth.Service
	cookieSecure       bool
	defaultCallingCode string
	mobile             http.Handler
	billing            *billing.Engine
	location           *time.Location
}

func New(pool *pgxpool.Pool, authSvc *auth.Service, cookieSecure bool) *Server {
	return &Server{
		pool: pool, q: store.New(pool), auth: authSvc, cookieSecure: cookieSecure,
		defaultCallingCode: authSvc.DefaultCallingCode(), location: nairobi(),
	}
}

// WithMobile serves the legacy mobile app API under /v1/legacy, so the
// app's base URL becomes https://<host>/v1/legacy/.
func (s *Server) WithMobile(h http.Handler) *Server {
	s.mobile = h
	return s
}

func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(requestLogger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))

	r.Get("/healthz", s.health)

	r.Route("/v1", func(r chi.Router) {
		if s.mobile != nil {
			r.Mount("/legacy", s.mobile)
		}
		r.Route("/auth", func(r chi.Router) {
			r.Post("/login", s.login)
			r.Post("/refresh", s.refresh)
			r.Post("/logout", s.logout)
		})
		r.Group(func(r chi.Router) {
			r.Use(s.requireAuth)
			r.Get("/me", s.me)

			r.Route("/reference", func(r chi.Router) {
				r.Get("/countries", s.listCountries)
				r.Get("/counties", s.listCounties)
				r.Get("/banks", s.listBanks)
				r.Get("/banks/{bankID}/branches", s.listBranches)
				r.Get("/saccos", s.listSaccos)
				r.Get("/saccos/{saccoID}/branches", s.listSaccoBranches)
				r.Get("/mobile-money-providers", s.listMobileMoneyProviders)
			})

			r.Route("/admin", func(r chi.Router) {
				r.Use(s.requirePlatformAdmin)
				r.Get("/stats", s.adminStats)
				r.Get("/audit", s.adminAudit)
				r.Get("/groups", s.adminListGroups)
				r.Patch("/groups/{groupID}", s.adminUpdateGroup)
				r.Get("/users", s.adminListUsers)
				r.Post("/users", s.adminCreateUser)
				r.Get("/users/{userID}", s.adminGetUser)
				r.Patch("/users/{userID}", s.adminUpdateUser)
				r.Post("/users/{userID}/password", s.adminSetPassword)
				r.Get("/transactions", s.adminListTransactions)
				r.Get("/settings", s.listSettings)
				r.Patch("/settings", s.updateSettings)
				r.Get("/countries", s.listCountries)
				r.Post("/countries", s.createCountry)
				r.Patch("/countries/{countryID}", s.updateCountry)
				r.Get("/counties", s.listCounties)
				r.Post("/counties", s.createCounty)
				r.Patch("/counties/{countyID}", s.updateCounty)
				r.Get("/banks", s.listBanks)
				r.Post("/banks", s.createBank)
				r.Patch("/banks/{bankID}", s.updateBank)
				r.Get("/banks/{bankID}/branches", s.listBranches)
				r.Post("/banks/{bankID}/branches", s.createBranch)
				r.Patch("/banks/{bankID}/branches/{branchID}", s.updateBranch)
				r.Get("/jobs", s.adminListJobRuns)
				r.Post("/jobs/billing/run", s.adminRunBilling)
			})

			r.Route("/groups/{groupID}", func(r chi.Router) {
				r.Use(s.requireGroup)
				r.Get("/", s.getGroup)
				r.Get("/roles", s.listRoles)

				r.Get("/members", s.listMembers)
				r.Get("/members/{memberID}", s.getMember)
				r.Get("/members/{memberID}/statement", s.memberStatement)

				r.Get("/contributions", s.listContributions)
				r.Get("/contributions/{contributionID}", s.getContribution)

				r.Get("/income-categories", s.listCategories(s.incomeCategories))
				r.Get("/expense-categories", s.listCategories(s.expenseCategories))
				r.Get("/fine-categories", s.listCategories(s.fineCategories))

				// Signatories who are not officials answer withdrawal requests too.
				r.Get("/withdrawal-requests", s.listWithdrawalRequests)
				r.Get("/withdrawal-requests/{requestID}", s.getWithdrawalRequest)
				r.Post("/withdrawal-requests/{requestID}/approve", s.approveWithdrawalRequest)
				r.Post("/withdrawal-requests/{requestID}/decline", s.declineWithdrawalRequest)
				r.Get("/fines", s.listFines)
				r.Get("/fine-balances", s.fineBalances)

				r.Group(func(r chi.Router) {
					r.Use(requireOfficial)
					r.Post("/members", s.addMember)
					r.Patch("/members/{memberID}", s.updateMember)
					r.Post("/contributions", s.createContribution)
					r.Patch("/contributions/{contributionID}", s.updateContribution)
					r.Get("/contribution-balances", s.contributionBalances)

					r.Get("/accounts", s.listAccounts)
					r.Post("/accounts", s.createAccount)
					r.Patch("/accounts/{accountID}", s.updateAccount)
					r.Delete("/accounts/{accountID}", s.deleteAccount)
					r.Get("/cash-book", s.cashBook)

					r.Post("/income-categories", s.createCategory(s.incomeCategories))
					r.Patch("/income-categories/{categoryID}", s.updateCategory(s.incomeCategories))
					r.Post("/expense-categories", s.createCategory(s.expenseCategories))
					r.Patch("/expense-categories/{categoryID}", s.updateCategory(s.expenseCategories))
					r.Post("/fine-categories", s.createCategory(s.fineCategories))
					r.Patch("/fine-categories/{categoryID}", s.updateCategory(s.fineCategories))
					r.Get("/depositors", s.listDepositors)
					r.Post("/depositors", s.createDepositor)
					r.Patch("/depositors/{depositorID}", s.updateDepositor)

					r.Get("/deposits", s.listDeposits)
					r.Post("/deposits", s.createDeposit)
					r.Get("/deposits/{depositID}", s.getDeposit)
					r.Post("/deposits/{depositID}/void", s.voidDeposit)

					r.Get("/withdrawals", s.listWithdrawals)
					r.Post("/withdrawals", s.createWithdrawal)
					r.Get("/withdrawals/{withdrawalID}", s.getWithdrawal)
					r.Post("/withdrawals/{withdrawalID}/void", s.voidWithdrawal)
					r.Get("/transfers", s.listTransfers)
					r.Post("/transfers", s.createTransfer)
					r.Post("/transfers/{transferID}/void", s.voidTransfer)
					r.Post("/withdrawal-requests", s.createWithdrawalRequest)
					r.Post("/withdrawal-requests/{requestID}/cancel", s.cancelWithdrawalRequest)
					r.Post("/withdrawal-requests/{requestID}/pay", s.payWithdrawalRequest)
					r.Post("/fines", s.fineMembers)
					r.Post("/fines/{fineID}/void", s.voidFine)
				})
			})
		})
	})
	return r
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.pool.Ping(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "db_unavailable", "Database is unreachable.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		slog.InfoContext(r.Context(), "request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", ww.Status(),
			"duration_ms", time.Since(start).Milliseconds(),
			"request_id", middleware.GetReqID(r.Context()),
		)
	})
}
