// Command legion-api is the single deployable binary: it serves the API and
// carries the operational subcommands. Settings come from the environment
// or a .env file (see .env.example).
//
//	legion-api                 serve the API (same as "serve")
//	legion-api migrate         apply pending database migrations
//	legion-api create-user     create a user, e.g. a first admin
//	legion-api import-legacy   load the LegionCore MySQL dump
//	legion-api billing-run     raise the invoices and fines due today (or -date)
//	legion-api version
package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // the binary carries its own time zone database

	"golang.org/x/crypto/bcrypt"

	"github.com/swahome/legion-api/internal/account"
	"github.com/swahome/legion-api/internal/auth"
	"github.com/swahome/legion-api/internal/billing"
	"github.com/swahome/legion-api/internal/config"
	"github.com/swahome/legion-api/internal/database"
	"github.com/swahome/legion-api/internal/httpapi"
	"github.com/swahome/legion-api/internal/legacy"
	"github.com/swahome/legion-api/internal/mobile"
	"github.com/swahome/legion-api/internal/payments"
	"github.com/swahome/legion-api/internal/sms"
	"github.com/swahome/legion-api/internal/store"
)

// version is set at build time: -ldflags "-X main.version=..."
var version = "dev"

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	cmd, args := "serve", os.Args[1:]
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "version", "-version", "--version":
		fmt.Println(version)
		return
	case "serve":
		err = serve()
	case "migrate":
		err = migrate()
	case "create-user":
		err = createUser(args)
	case "import-legacy":
		err = importLegacy(args)
	case "billing-run":
		err = billingRun(args)
	case "hook-urls":
		err = hookURLs()
	default:
		err = fmt.Errorf("unknown command %q (want serve, migrate, create-user, import-legacy, billing-run, hook-urls or version)", cmd)
	}
	if err != nil {
		slog.Error("fatal", "cmd", cmd, "err", err)
		os.Exit(1)
	}
}

func serve() error {
	if err := config.LoadEnvFile(); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := database.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if cfg.MigrateOnStart {
		if err := database.Migrate(ctx, pool); err != nil {
			return err
		}
	}

	authSvc := auth.NewService(pool, auth.NewTokenIssuer(cfg.JWTSecret, cfg.AccessTTL), cfg.RefreshTTL, cfg.DefaultCallingCode)
	engine := billing.New(pool, slog.Default())
	var sender sms.Sender = sms.LogSender{Log: slog.Default()}
	if cfg.ATUsername != "" {
		sender = &sms.AfricasTalking{Username: cfg.ATUsername, APIKey: cfg.ATAPIKey, SenderID: cfg.ATSenderID}
		slog.Info("sms via Africa's Talking", "username", cfg.ATUsername)
	} else {
		slog.Warn("sms: LEGION_AT_USERNAME not set, texts are written to the log instead of sent")
	}
	dispatcher := sms.NewDispatcher(pool, sender, slog.Default())
	go dispatcher.Run(ctx)
	acct := &account.Service{WebURL: cfg.WebURL}
	api := httpapi.New(pool, authSvc, cfg.CookieSecure).WithBilling(engine, cfg.Location).WithAccounts(acct, dispatcher.Kick)
	pay := &payments.Service{Pool: pool, PublicURL: cfg.PublicURL, HookKey: cfg.HookKey,
		DefaultCC: cfg.DefaultCallingCode, Location: cfg.Location, Log: slog.Default(), SMSKick: dispatcher.Kick}
	switch {
	case cfg.ChamasoftFake:
		pay.Gateway = &payments.FakeGateway{Delay: 5 * time.Second, Settle: func(ref, receipt string) {
			if _, _, err := pay.Settle(context.Background(), ref, payments.PaymentResult{
				Success: true, Code: "200", Description: "Paid (test mode)", Receipt: receipt}); err != nil {
				slog.Error("fake payment settle", "err", err)
			}
		}}
		slog.Warn("payments: LEGION_CHAMASOFT_FAKE is on, MPesa payments are simulated")
	case cfg.ChamasoftURL != "":
		gw := &payments.Chamasoft{BaseURL: cfg.ChamasoftURL, BasicAuth: cfg.ChamasoftAuth}
		if cfg.ChamasoftCert != "" {
			if gw.PublicKey, err = payments.LoadPublicKey(cfg.ChamasoftCert); err != nil {
				return fmt.Errorf("LEGION_CHAMASOFT_CERT: %w", err)
			}
		}
		pay.Gateway = gw
		slog.Info("payments via Chamasoft", "url", cfg.ChamasoftURL)
	default:
		slog.Warn("payments: LEGION_CHAMASOFT_URL not set, members cannot start MPesa payments")
	}
	if cfg.PublicURL == "" && pay.Gateway != nil {
		return errors.New("payments need LEGION_PUBLIC_URL (or LEGION_WEB_URL) for the MPesa callback")
	}
	go pay.RunExpiry(ctx)
	api.WithPayments(pay, httpapi.HookConfig{
		Equity:          httpapi.HookAuth{Username: cfg.EquityUsername, Password: cfg.EquityPassword, IPs: cfg.EquityIPs},
		Coop:            httpapi.HookAuth{Username: cfg.CoopUsername, Password: cfg.CoopPassword, IPs: cfg.CoopIPs},
		ChamasoftDirect: httpapi.HookAuth{IPs: cfg.ChamasoftIPs},
	})
	if cfg.BillingAt != "off" {
		at, err := billing.ParseClock(cfg.BillingAt)
		if err != nil {
			return fmt.Errorf("LEGION_BILLING_AT: %w", err)
		}
		go engine.Schedule(ctx, at, cfg.Location)
		slog.Info("billing scheduled", "at", cfg.BillingAt, "timezone", cfg.Location.String())
	}
	if cfg.MobileServerKey != "" {
		keys, err := mobile.LoadKeys(cfg.MobileServerKey, cfg.MobileAppCert)
		if err != nil {
			return err
		}
		api.WithMobile(mobile.New(pool, keys, authSvc, mobile.SMSOTP{Pool: pool, Kick: dispatcher.Kick}).WithAccounts(acct, dispatcher.Kick).WithPayments(pay).Routes())
		slog.Info("mobile app API on /v1/legacy")
	}
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           api.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      35 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errc := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", cfg.Addr, "version", version)
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func migrate() error {
	if err := config.LoadEnvFile(); err != nil {
		return err
	}
	ctx := context.Background()
	pool, err := database.Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := database.Migrate(ctx, pool); err != nil {
		return err
	}
	fmt.Println("migrations applied")
	return nil
}

// createUser makes a user who can log in straight away. Until the legacy
// import exists it is how a dev or first admin account gets created.
//
//	legion-api create-user -phone 0700000001 -password secret123 -admin
func createUser(args []string) error {
	fs := flag.NewFlagSet("create-user", flag.ContinueOnError)
	phone := fs.String("phone", "", "phone number (local or E.164)")
	email := fs.String("email", "", "email address")
	password := fs.String("password", "", "password")
	first := fs.String("first", "", "first name")
	last := fs.String("last", "", "last name")
	admin := fs.Bool("admin", false, "grant the platform admin role")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *password == "" || (*phone == "" && *email == "") {
		return errors.New("-password and one of -phone or -email are required")
	}
	if err := config.LoadEnvFile(); err != nil {
		return err
	}

	p := store.CreateUserParams{FirstName: *first, LastName: *last}
	if *phone != "" {
		cc := os.Getenv("LEGION_DEFAULT_CALLING_CODE")
		if cc == "" {
			cc = "254"
		}
		e164, err := auth.NormalizePhone(*phone, cc)
		if err != nil {
			return err
		}
		p.Phone = &e164
	}
	if *email != "" {
		p.Email = email
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(*password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	h := string(hash)
	p.PasswordHash = &h

	ctx := context.Background()
	pool, err := database.Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := database.Migrate(ctx, pool); err != nil {
		return err
	}
	q := store.New(pool)
	u, err := q.CreateUser(ctx, p)
	if err != nil {
		return err
	}
	if *admin {
		if err := q.AddPlatformRole(ctx, store.AddPlatformRoleParams{UserID: u.ID, Role: store.PlatformRoleAdmin}); err != nil {
			return err
		}
	}
	fmt.Printf("created user %d\n", u.ID)
	return nil
}

// importLegacy loads a LegionCore MySQL dump into DATABASE_URL and checks
// the result. It is the cutover data migration, so it is safe to re-run:
// with -replace it wipes the imported tables and loads them again.
//
//	legion-api import-legacy -dump anonymized_db.sql -users-groups users_groups.sql -synthetic-phones -replace
func importLegacy(args []string) error {
	fs := flag.NewFlagSet("import-legacy", flag.ContinueOnError)
	dump := fs.String("dump", "", "path to the legacy MySQL dump (.sql)")
	usersGroups := fs.String("users-groups", "", "separate dump holding users_groups, if the main dump's is unusable")
	synthetic := fs.Bool("synthetic-phones", false, "give every user a fake phone +2547000NNNNN from their id (anonymized dumps only)")
	replace := fs.Bool("replace", false, "wipe users, groups, memberships, contributions and the ledger before loading")
	dryRun := fs.Bool("dry-run", false, "parse and transform only; print the report without writing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dump == "" {
		return errors.New("-dump is required")
	}
	if err := config.LoadEnvFile(); err != nil {
		return err
	}

	start := time.Now()
	tables, err := legacy.ReadDump(*dump, legacy.DumpTables...)
	if err != nil {
		return err
	}
	roles := tables["users_groups"]
	if *usersGroups != "" {
		ug, err := legacy.ReadDump(*usersGroups, "users_groups")
		if err != nil {
			return err
		}
		roles = ug["users_groups"]
	}
	cc := os.Getenv("LEGION_DEFAULT_CALLING_CODE")
	plan, err := legacy.Transform(tables, roles, legacy.Options{SyntheticPhones: *synthetic, DefaultCallingCode: cc}, time.Now())
	if err != nil {
		return err
	}
	plan.Report.Write(os.Stdout)
	if *dryRun {
		return nil
	}

	ctx := context.Background()
	pool, err := database.Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := database.Migrate(ctx, pool); err != nil {
		return err
	}
	if err := legacy.Load(ctx, pool, plan, *replace); err != nil {
		return err
	}
	mismatches, err := legacy.Reconcile(ctx, pool, tables, os.Stdout)
	if err != nil {
		return err
	}
	fmt.Printf("done in %s\n", time.Since(start).Round(time.Millisecond))
	if mismatches > 0 {
		return fmt.Errorf("%d contribution balances differ from the legacy calculation", mismatches)
	}
	return nil
}

// billingRun raises the contribution invoices and fines due on a day, the
// same work the nightly scheduler does. Re-running a day adds nothing.
//
//	legion-api billing-run [-date 2026-10-31]
func billingRun(args []string) error {
	fs := flag.NewFlagSet("billing-run", flag.ContinueOnError)
	date := fs.String("date", "", "the day to bill (YYYY-MM-DD); default today in LEGION_TIMEZONE")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := config.LoadEnvFile(); err != nil {
		return err
	}
	loc, err := time.LoadLocation(cmp.Or(os.Getenv("LEGION_TIMEZONE"), "Africa/Nairobi"))
	if err != nil {
		return err
	}
	today := billing.LocalToday(time.Now(), loc)
	if *date != "" {
		if today, err = time.Parse(time.DateOnly, *date); err != nil {
			return fmt.Errorf("-date: %w", err)
		}
	}
	ctx := context.Background()
	pool, err := database.Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := database.Migrate(ctx, pool); err != nil {
		return err
	}
	sum, err := billing.New(pool, slog.Default()).Run(ctx, billing.RunOptions{Today: today, Trigger: "cli"})
	out, _ := json.MarshalIndent(sum, "", "  ")
	fmt.Println(string(out))
	return err
}

// hookURLs prints the addresses to give the banks and Chamasoft, with the
// keys the API checks.
func hookURLs() error {
	if err := config.LoadEnvFile(); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.PublicURL == "" {
		return errors.New("set LEGION_PUBLIC_URL (or LEGION_WEB_URL) first")
	}
	pay := &payments.Service{HookKey: cfg.HookKey}
	base := strings.TrimRight(cfg.PublicURL, "/") + "/v1/hooks"
	fmt.Printf("Equity Bank IPN:            %s/equity   (username and password in the body; LEGION_EQUITY_*)\n", base)
	fmt.Printf("Co-op Bank IPN:             %s/coop     (basic auth or LEGION_COOP_IPS)\n", base)
	fmt.Printf("Chamasoft direct paybill:   %s/chamasoft/direct?key=%s\n", base, pay.HookSecret("chamasoft_direct"))
	fmt.Printf("MPesa C2B validation:       %s/mpesa/validation?key=%s\n", base, pay.HookSecret("mpesa"))
	fmt.Printf("MPesa C2B confirmation:     %s/mpesa/confirmation?key=%s\n", base, pay.HookSecret("mpesa"))
	fmt.Println("Chamasoft payment callbacks are signed per payment; nothing to configure.")
	return nil
}
