// Copyright 2026 Elliott Wave LLC. All rights reserved.

package postgres_test

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/moqom-cloud/motom/internal/store"
	"github.com/moqom-cloud/motom/internal/store/postgres"
	"github.com/moqom-cloud/motom/internal/store/storetest"
)

// dsn is the database the conformance suite runs against: a dev Cloud SQL
// instance on GCP, never production. Each case gets its own schema, dropped
// afterwards.
func dsn(t *testing.T) string {
	for _, name := range []string{"MOTOM_TEST_DATABASE_URL", "DATABASE_URL"} {
		if value := os.Getenv(name); value != "" {
			return value
		}
	}
	t.Skip("DATABASE_URL is not set; the Postgres conformance suite runs against a GCP dev database")
	return ""
}

func open(t *testing.T, url string, counter *atomic.Uint64) *pgxpool.Pool {
	ctx := context.Background()
	schema := fmt.Sprintf("motom_test_%d_%d", time.Now().UnixNano(), counter.Add(1))
	settings, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	settings.AfterConnect = func(ctx context.Context, connection *pgx.Conn) error {
		_, err := connection.Exec(ctx, "SET search_path TO "+pgx.Identifier{schema}.Sanitize())
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		pool.Close()
	})
	return pool
}

func TestConformance(t *testing.T) {
	url := dsn(t)
	var counter atomic.Uint64
	storetest.Run(t, func(t *testing.T) store.Store {
		subject, err := postgres.New(context.Background(), open(t, url, &counter))
		if err != nil {
			t.Fatal(err)
		}
		return subject
	})
}

// The journal is append-only below the application too.
func TestJournalRefusesEdits(t *testing.T) {
	url := dsn(t)
	var counter atomic.Uint64
	pool := open(t, url, &counter)
	ctx := context.Background()
	if _, err := postgres.New(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrating twice: %v", err)
	}
	for _, statement := range []string{
		"UPDATE motom_entries SET memo = 'x'",
		"DELETE FROM motom_postings",
		"DELETE FROM motom_audit",
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("%s on an empty table: %v", statement, err)
		}
	}
	exec := func(sql string, args ...any) error {
		_, err := pool.Exec(ctx, sql, args...)
		return err
	}
	at := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	for _, id := range []string{"t/stripe_clearing/-/usd", "t/wallet/u/usd"} {
		if err := exec(`INSERT INTO motom_accounts (id, tenant, kind, owner, currency, created_at) VALUES ($1, 't', split_part($1, '/', 2), '', 'usd', $2)`, id, at); err != nil {
			t.Fatal(err)
		}
	}
	if err := exec(`INSERT INTO motom_entries (id, tenant, kind, idempotency_key, fingerprint, created_at) VALUES ('je', 't', 'adjustment', 'k', 'f', $1)`, at); err != nil {
		t.Fatal(err)
	}
	unbalanced := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO motom_postings (entry_id, ordinal, account_id, amount, currency, created_at)
			VALUES ('je', 0, 't/stripe_clearing/-/usd', 5, 'usd', $1)`, at)
		return err
	})
	if unbalanced == nil {
		t.Fatal("an unbalanced entry committed")
	}
	for _, statement := range []string{"UPDATE motom_entries SET memo = 'x'", "DELETE FROM motom_entries", "TRUNCATE motom_audit"} {
		if err := exec(statement); err == nil {
			t.Fatalf("%s succeeded", statement)
		}
	}
}
