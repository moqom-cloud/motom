// Copyright 2026 Elliott Wave LLC. All rights reserved.

// Command motom runs the MOTOM API and its operator tasks.
//
//	motom serve                  serve the API on MOTOM_ADDR (default :8080)
//	motom migrate                apply database migrations and exit
//	motom tenant create -id t1 -currencies usd -platform-fee-bps 500
//	motom key create -tenant t1 -name bootstrap -scopes admin
//	motom payouts run [-run 2026-10-06]
//
// Configuration comes from the environment, which a deployment fills from
// Secret Manager. Nothing secret is read from a file or a flag.
//
//	MOTOM_DATABASE_URL or DATABASE_URL   Postgres
//	STRIPE_SECRET_KEY                    Stripe platform key; unset takes no payments
//	STRIPE_WEBHOOK_SECRET                signing secret of the platform webhook endpoint
//	STRIPE_CONNECT_WEBHOOK_SECRET        signing secret of the Connect webhook endpoint
//	MOTOM_ADDR                           listen address
//	MOTOM_MIN_HOLD_DAYS                  shortest payout hold a tenant may set (default 2)
//	MOTOM_REQUESTS_PER_MINUTE            per-key API rate limit (default 600)
//	MOTOM_DEV=1                          allow the in-memory store, for local trials only
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/moqom-cloud/motom/internal/httpapi"
	"github.com/moqom-cloud/motom/internal/service"
	"github.com/moqom-cloud/motom/internal/store"
	"github.com/moqom-cloud/motom/internal/store/memory"
	"github.com/moqom-cloud/motom/internal/store/postgres"
	"github.com/moqom-cloud/motom/internal/stripe"
	"github.com/moqom-cloud/motom/ledger"
)

const usage = `usage:
  motom serve
  motom migrate
  motom tenant create -id ID [-currencies usd,eur] [-platform-fee-bps 500]
  motom key create -tenant ID [-name NAME] -scopes admin[,tips:write,...]
  motom payouts run [-run NAME]`

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, log, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "motom:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, log *slog.Logger, args []string) error {
	command := strings.Join(args[:min(2, len(args))], " ")
	switch {
	case len(args) > 0 && args[0] == "serve":
		return serve(ctx, log)
	case len(args) > 0 && args[0] == "migrate":
		pool, err := openPool(ctx)
		if err != nil {
			return err
		}
		defer pool.Close()
		if err := postgres.Migrate(ctx, pool); err != nil {
			return err
		}
		log.Info("migrations applied", "event", "motom_migrated")
		return nil
	case command == "tenant create":
		return createTenant(ctx, log, args[2:])
	case command == "key create":
		return createKey(ctx, log, args[2:])
	case command == "payouts run":
		return runPayouts(ctx, log, args[2:])
	}
	return errors.New(usage)
}

func openPool(ctx context.Context) (*pgxpool.Pool, error) {
	dsn := os.Getenv("MOTOM_DATABASE_URL")
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	if dsn == "" {
		return nil, errors.New("MOTOM_DATABASE_URL or DATABASE_URL is required")
	}
	return pgxpool.New(ctx, dsn)
}

// open builds the service on Postgres, or on memory when MOTOM_DEV=1 and no
// database is configured. close releases the database.
func open(ctx context.Context, log *slog.Logger) (svc *service.Service, close func(), err error) {
	var st store.Store
	close = func() {}
	if os.Getenv("MOTOM_DATABASE_URL") == "" && os.Getenv("DATABASE_URL") == "" && os.Getenv("MOTOM_DEV") == "1" {
		log.Warn("using the in-memory store: every balance is lost on exit", "event", "motom_memory_store")
		st = memory.New()
	} else {
		pool, err := openPool(ctx)
		if err != nil {
			return nil, nil, err
		}
		pg, err := postgres.New(ctx, pool)
		if err != nil {
			pool.Close()
			return nil, nil, err
		}
		st, close = pg, pool.Close
	}

	config := service.Config{Store: st, Logger: log, MinHoldDays: 2}
	if v := os.Getenv("MOTOM_MIN_HOLD_DAYS"); v != "" {
		if config.MinHoldDays, err = strconv.Atoi(v); err != nil || config.MinHoldDays < 0 {
			close()
			return nil, nil, errors.New("MOTOM_MIN_HOLD_DAYS is a whole number of days")
		}
	}
	if secret := os.Getenv("STRIPE_SECRET_KEY"); secret != "" {
		if !strings.HasPrefix(secret, "sk_") && !strings.HasPrefix(secret, "rk_") {
			close()
			return nil, nil, errors.New("STRIPE_SECRET_KEY is not a Stripe secret or restricted key")
		}
		if strings.HasPrefix(secret, "sk_live_") && os.Getenv("MOTOM_DEV") == "1" {
			close()
			return nil, nil, errors.New("refusing a live Stripe key with MOTOM_DEV=1")
		}
		config.Stripe = &stripe.Client{SecretKey: secret, HTTP: &http.Client{Timeout: 30 * time.Second}}
		for _, name := range []string{"STRIPE_WEBHOOK_SECRET", "STRIPE_CONNECT_WEBHOOK_SECRET"} {
			if v := os.Getenv(name); v != "" {
				config.WebhookSecrets = append(config.WebhookSecrets, v)
			}
		}
	}
	svc, err = service.New(config)
	if err != nil {
		close()
		return nil, nil, err
	}
	return svc, close, nil
}

func serve(ctx context.Context, log *slog.Logger) error {
	svc, close, err := open(ctx, log)
	if err != nil {
		return err
	}
	defer close()
	perMinute := 0
	if v := os.Getenv("MOTOM_REQUESTS_PER_MINUTE"); v != "" {
		if perMinute, err = strconv.Atoi(v); err != nil || perMinute <= 0 {
			return errors.New("MOTOM_REQUESTS_PER_MINUTE is a positive number")
		}
	}
	addr := os.Getenv("MOTOM_ADDR")
	if addr == "" {
		addr = ":" + envOr("PORT", "8080")
	}
	server := &http.Server{
		Addr:              addr,
		Handler:           httpapi.Handler(svc, httpapi.Options{RequestsPerMinute: perMinute, Logger: log}),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
	failed := make(chan error, 1)
	go func() { failed <- server.ListenAndServe() }()
	log.Info("serving", "event", "motom_serving", "addr", addr, "payments", os.Getenv("STRIPE_SECRET_KEY") != "")
	select {
	case err := <-failed:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return server.Shutdown(shutdown)
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func createTenant(ctx context.Context, log *slog.Logger, args []string) error {
	flags := flag.NewFlagSet("tenant create", flag.ContinueOnError)
	id := flags.String("id", "", "tenant ID")
	currencies := flags.String("currencies", "usd", "comma-separated ISO 4217 codes, lower case")
	fee := flags.Int("platform-fee-bps", 500, "MOTOM's fee on tips and spends, in basis points")
	if err := flags.Parse(args); err != nil {
		return err
	}
	svc, close, err := open(ctx, log)
	if err != nil {
		return err
	}
	defer close()
	var list []ledger.Currency
	for _, c := range strings.Split(*currencies, ",") {
		list = append(list, ledger.Currency(strings.TrimSpace(c)))
	}
	t, err := svc.CreateTenant(ctx, "operator:"+operator(), *id, list, ledger.BasisPoints(*fee))
	if err != nil {
		return err
	}
	fmt.Printf("created tenant %s (%v, platform fee %d bps)\n", t.ID, t.Currencies, t.PlatformFee)
	return nil
}

func createKey(ctx context.Context, log *slog.Logger, args []string) error {
	flags := flag.NewFlagSet("key create", flag.ContinueOnError)
	tenant := flags.String("tenant", "", "tenant ID")
	name := flags.String("name", "bootstrap", "what the key is for")
	scopes := flags.String("scopes", "", "comma-separated scopes: "+strings.Join(scopeNames(), ", "))
	if err := flags.Parse(args); err != nil {
		return err
	}
	svc, close, err := open(ctx, log)
	if err != nil {
		return err
	}
	defer close()
	var list []store.Scope
	for _, s := range strings.Split(*scopes, ",") {
		if s = strings.TrimSpace(s); s != "" {
			list = append(list, store.Scope(s))
		}
	}
	key, plaintext, err := svc.IssueKey(ctx, "operator:"+operator(), *tenant, *name, list)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "created key %s for tenant %s. It is printed once on stdout; put it straight into a secret manager.\n", key.ID, *tenant)
	fmt.Println(plaintext)
	return nil
}

func runPayouts(ctx context.Context, log *slog.Logger, args []string) error {
	flags := flag.NewFlagSet("payouts run", flag.ContinueOnError)
	name := flags.String("run", "", "run name; a run pays each creator at most once (default: today, UTC)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	svc, close, err := open(ctx, log)
	if err != nil {
		return err
	}
	defer close()
	reports, err := svc.RunAllPayouts(ctx, *name)
	for _, r := range reports {
		log.Info("payout run", "event", "motom_payout_run", "tenant", r.Tenant, "run", r.Run,
			"paid", len(r.Paid), "failed", len(r.Failed), "pending", len(r.Pending), "skipped", r.Skipped)
	}
	return err
}

func scopeNames() []string {
	var names []string
	for _, s := range store.Scopes {
		names = append(names, string(s))
	}
	return names
}

// operator names who ran a command, for the audit log.
func operator() string {
	if u := os.Getenv("MOTOM_OPERATOR"); u != "" {
		return u
	}
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "unknown"
}
