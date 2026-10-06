// Copyright 2026 Elliott Wave LLC. All rights reserved.

// Package postgres is store.Store on Postgres (Cloud SQL in deployment).
//
// Concurrency: Post locks every account it touches by bumping its version, in
// sorted ID order so two entries cannot deadlock, and only then reads the
// balances it checks for overdraft. Under READ COMMITTED each statement after
// the lock sees every posting committed before it, so two parallel spends on
// one wallet serialise on the wallet row and the second sees the first.
package postgres

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/moqom-cloud/motom/internal/store"
	"github.com/moqom-cloud/motom/ledger"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Store is store.Store on a pool the caller owns.
type Store struct {
	pool *pgxpool.Pool
}

var _ store.Store = (*Store)(nil)

// migrationLock is "motom" as an advisory lock key, so replicas starting
// together migrate once.
const migrationLock = 0x6d6f746f6d

// New applies pending migrations and returns a store on pool.
func New(ctx context.Context, pool *pgxpool.Pool) (*Store, error) {
	if err := Migrate(ctx, pool); err != nil {
		return nil, err
	}
	return &Store{pool: pool}, nil
}

// Migrate applies every embedded migration not yet recorded, in name order,
// each in its own transaction.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	connection, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer connection.Release()
	if _, err := connection.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLock); err != nil {
		return fmt.Errorf("taking the migration lock: %w", err)
	}
	defer func() {
		_, _ = connection.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", migrationLock)
	}()
	if _, err := connection.Exec(ctx, `CREATE TABLE IF NOT EXISTS motom_schema_migrations (
		name text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return fmt.Errorf("creating the migrations table: %w", err)
	}
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		var applied bool
		if err := connection.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM motom_schema_migrations WHERE name = $1)", name).Scan(&applied); err != nil {
			return err
		}
		if applied {
			continue
		}
		body, err := migrations.ReadFile(name)
		if err != nil {
			return err
		}
		err = pgx.BeginFunc(ctx, connection.Conn(), func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(body)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, "INSERT INTO motom_schema_migrations (name) VALUES ($1)", name)
			return err
		})
		if err != nil {
			return fmt.Errorf("applying %s: %w", name, err)
		}
	}
	return nil
}

func uniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && (constraint == "" || pgErr.ConstraintName == constraint)
}

func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return store.ErrNotFound
	}
	return err
}

func nullable(at time.Time) *time.Time {
	if at.IsZero() {
		return nil
	}
	utc := at.UTC()
	return &utc
}

func timeOf(at *time.Time) time.Time {
	if at == nil {
		return time.Time{}
	}
	return *at
}

func nullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func stringOf(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// versioned turns an UPDATE ... WHERE version = $n that touched nothing into
// the right error: the row is missing, or somebody else saved first.
func (s *Store) versioned(ctx context.Context, tag pgconn.CommandTag, err error, exists string, args ...any) error {
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var found bool
	if err := s.pool.QueryRow(ctx, "SELECT EXISTS ("+exists+")", args...).Scan(&found); err != nil {
		return err
	}
	if found {
		return store.ErrVersionConflict
	}
	return store.ErrNotFound
}

// Tenants.

func (s *Store) CreateTenant(ctx context.Context, t store.Tenant) error {
	config, err := json.Marshal(t.Config)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO motom_tenants (id, currencies, platform_fee_bps, config, version, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 0, $5, $6)`,
		t.ID, currencyStrings(t.Currencies), int(t.PlatformFee), config, t.CreatedAt.UTC(), t.UpdatedAt.UTC())
	if uniqueViolation(err, "") {
		return store.ErrAlreadyExists
	}
	return err
}

func currencyStrings(cs []ledger.Currency) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = string(c)
	}
	return out
}

const tenantColumns = `id, currencies, platform_fee_bps, config, version, created_at, updated_at`

func scanTenant(row pgx.Row) (store.Tenant, error) {
	var t store.Tenant
	var currencies []string
	var fee int
	var config []byte
	if err := row.Scan(&t.ID, &currencies, &fee, &config, &t.Version, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return store.Tenant{}, notFound(err)
	}
	for _, c := range currencies {
		t.Currencies = append(t.Currencies, ledger.Currency(c))
	}
	t.PlatformFee = ledger.BasisPoints(fee)
	if err := json.Unmarshal(config, &t.Config); err != nil {
		return store.Tenant{}, fmt.Errorf("tenant %s config: %w", t.ID, err)
	}
	return t, nil
}

func (s *Store) GetTenant(ctx context.Context, id string) (store.Tenant, error) {
	return scanTenant(s.pool.QueryRow(ctx, `SELECT `+tenantColumns+` FROM motom_tenants WHERE id = $1`, id))
}

func (s *Store) SaveTenant(ctx context.Context, t store.Tenant) error {
	config, err := json.Marshal(t.Config)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `UPDATE motom_tenants SET currencies = $2, platform_fee_bps = $3, config = $4,
		updated_at = $5, version = version + 1 WHERE id = $1 AND version = $6`,
		t.ID, currencyStrings(t.Currencies), int(t.PlatformFee), config, t.UpdatedAt.UTC(), t.Version)
	return s.versioned(ctx, tag, err, "SELECT 1 FROM motom_tenants WHERE id = $1", t.ID)
}

func (s *Store) ListTenants(ctx context.Context) ([]store.Tenant, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+tenantColumns+` FROM motom_tenants ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Tenant
	for rows.Next() {
		t, err := scanTenant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Keys.

func (s *Store) PutKey(ctx context.Context, k store.Key) error {
	scopes := make([]string, len(k.Scopes))
	for i, scope := range k.Scopes {
		scopes[i] = string(scope)
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO motom_keys (id, tenant, name, scopes, hash, created_at, revoked_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, k.ID, k.Tenant, k.Name, scopes, k.Hash, k.CreatedAt.UTC(), nullable(k.RevokedAt))
	if uniqueViolation(err, "") {
		return store.ErrAlreadyExists
	}
	return err
}

func scanKey(row pgx.Row) (store.Key, error) {
	var k store.Key
	var scopes []string
	var revoked *time.Time
	if err := row.Scan(&k.ID, &k.Tenant, &k.Name, &scopes, &k.Hash, &k.CreatedAt, &revoked); err != nil {
		return store.Key{}, notFound(err)
	}
	for _, scope := range scopes {
		k.Scopes = append(k.Scopes, store.Scope(scope))
	}
	k.RevokedAt = timeOf(revoked)
	return k, nil
}

const keyColumns = `id, tenant, name, scopes, hash, created_at, revoked_at`

func (s *Store) GetKey(ctx context.Context, id string) (store.Key, error) {
	return scanKey(s.pool.QueryRow(ctx, `SELECT `+keyColumns+` FROM motom_keys WHERE id = $1`, id))
}

func (s *Store) ListKeys(ctx context.Context, tenant string) ([]store.Key, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+keyColumns+` FROM motom_keys WHERE tenant = $1 ORDER BY created_at, id`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Key
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *Store) RevokeKey(ctx context.Context, tenant, id string, at time.Time) error {
	tag, err := s.pool.Exec(ctx, `UPDATE motom_keys SET revoked_at = COALESCE(revoked_at, $3) WHERE id = $1 AND tenant = $2`, id, tenant, at.UTC())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

// Accounts and the journal.

const accountColumns = `id, tenant, kind, owner, currency, version, created_at`

func scanAccount(row pgx.Row) (ledger.Account, error) {
	var a ledger.Account
	var id, kind, currency string
	if err := row.Scan(&id, &a.Tenant, &kind, &a.Owner, &currency, &a.Version, &a.CreatedAt); err != nil {
		return ledger.Account{}, notFound(err)
	}
	a.ID, a.Kind, a.Currency = ledger.AccountID(id), ledger.AccountKind(kind), ledger.Currency(currency)
	return a, nil
}

func (s *Store) EnsureAccount(ctx context.Context, a ledger.Account) (ledger.Account, error) {
	parsed, err := ledger.NewAccount(a.ID, a.CreatedAt)
	if err != nil {
		return ledger.Account{}, err
	}
	created := parsed.CreatedAt
	if created.IsZero() {
		created = time.Now()
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO motom_accounts (id, tenant, kind, owner, currency, version, created_at)
		VALUES ($1, $2, $3, $4, $5, 0, $6) ON CONFLICT (id) DO NOTHING`,
		string(parsed.ID), parsed.Tenant, string(parsed.Kind), parsed.Owner, string(parsed.Currency), created.UTC()); err != nil {
		return ledger.Account{}, err
	}
	return s.GetAccount(ctx, parsed.ID)
}

func (s *Store) GetAccount(ctx context.Context, id ledger.AccountID) (ledger.Account, error) {
	return scanAccount(s.pool.QueryRow(ctx, `SELECT `+accountColumns+` FROM motom_accounts WHERE id = $1`, string(id)))
}

func (s *Store) ListAccounts(ctx context.Context, tenant, owner string) ([]ledger.Account, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+accountColumns+` FROM motom_accounts
		WHERE tenant = $1 AND ($2::text = '' OR owner = $2) ORDER BY id`, tenant, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ledger.Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

var errRaced = errors.New("idempotency race")

func (s *Store) Post(ctx context.Context, e ledger.Entry) (ledger.Entry, bool, error) {
	if err := e.Validate(); err != nil {
		return ledger.Entry{}, false, err
	}
	for attempt := 0; ; attempt++ {
		stored, created, err := s.post(ctx, e)
		if errors.Is(err, errRaced) && attempt == 0 {
			// Another transaction committed the same key between our check
			// and our insert; the retry finds it and compares fingerprints.
			continue
		}
		return stored, created, err
	}
}

func (s *Store) post(ctx context.Context, e ledger.Entry) (ledger.Entry, bool, error) {
	var stored ledger.Entry
	var created bool
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// Lock first, then look for the key: a replay that raced its original
		// waits here and then finds it, rather than finding the money gone.
		locked := make([]ledger.Account, 0, len(e.Postings))
		for _, id := range e.Accounts() {
			account, err := scanAccount(tx.QueryRow(ctx, `UPDATE motom_accounts SET version = version + 1 WHERE id = $1
				RETURNING `+accountColumns, string(id)))
			if err != nil {
				return err
			}
			account.Version--
			locked = append(locked, account)
		}
		existing, err := entryWhere(ctx, tx, "e.tenant = $1 AND e.idempotency_key = $2", e.Tenant, e.IdempotencyKey)
		if err == nil {
			if existing.fingerprint != e.Fingerprint() {
				return store.ErrIdempotencyMismatch
			}
			stored = existing.Entry
			return nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if e.Reverses != "" {
			var tenant string
			if err := tx.QueryRow(ctx, "SELECT tenant FROM motom_entries WHERE id = $1", e.Reverses).Scan(&tenant); err != nil || tenant != e.Tenant {
				if err != nil && !errors.Is(err, pgx.ErrNoRows) {
					return err
				}
				return store.ErrNotFound
			}
			var reversed bool
			if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM motom_entries WHERE reverses = $1)", e.Reverses).Scan(&reversed); err != nil {
				return err
			}
			if reversed {
				return store.ErrAlreadyReversed
			}
		}
		deltas := map[ledger.AccountID]ledger.Amount{}
		for _, p := range e.Postings {
			deltas[p.Account] = p.Amount
		}
		for _, account := range locked {
			if expected, ok := e.Expect[account.ID]; ok && expected != account.Version {
				return store.ErrVersionConflict
			}
			if account.Kind.MayOverdraw() {
				continue
			}
			var signed int64
			if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(amount), 0)::bigint FROM motom_postings WHERE account_id = $1`, string(account.ID)).Scan(&signed); err != nil {
				return err
			}
			if err := store.CheckOverdraw(account, ledger.Amount(signed), deltas[account.ID]); err != nil {
				return err
			}
		}
		metadata := e.Metadata
		if metadata == nil {
			metadata = map[string]string{}
		}
		metadataJSON, err := json.Marshal(metadata)
		if err != nil {
			return err
		}
		at := e.CreatedAt.UTC()
		_, err = tx.Exec(ctx, `INSERT INTO motom_entries (id, tenant, kind, idempotency_key, fingerprint, reverses,
			reference, actor, memo, metadata, created_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
			e.ID, e.Tenant, string(e.Kind), e.IdempotencyKey, e.Fingerprint(), nullString(e.Reverses),
			e.Reference, e.Actor, e.Memo, metadataJSON, at)
		switch {
		case uniqueViolation(err, "motom_entries_idempotency"):
			return errRaced
		case uniqueViolation(err, "motom_entries_reverses"):
			return store.ErrAlreadyReversed
		case uniqueViolation(err, "motom_entries_pkey"):
			return store.ErrAlreadyExists
		case err != nil:
			return err
		}
		batch := &pgx.Batch{}
		for i, p := range e.Postings {
			_, _, _, currency, _ := p.Account.Parse()
			batch.Queue(`INSERT INTO motom_postings (entry_id, ordinal, account_id, amount, currency, created_at)
				VALUES ($1, $2, $3, $4, $5, $6)`, e.ID, i, string(p.Account), int64(p.Amount), string(currency), at)
		}
		audit := store.EntryAudit(e)
		batch.Queue(`INSERT INTO motom_audit (id, tenant, actor, action, subject, detail, at) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			audit.ID, audit.Tenant, audit.Actor, audit.Action, audit.Subject, audit.Detail, audit.At.UTC())
		if err := tx.SendBatch(ctx, batch).Close(); err != nil {
			return err
		}
		stored, created = e, true
		stored.Expect = nil
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		err = store.ErrNotFound
	}
	if err != nil {
		return ledger.Entry{}, false, err
	}
	return stored, created, nil
}

type queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type storedEntry struct {
	ledger.Entry
	fingerprint string
}

const entryColumns = `e.id, e.tenant, e.kind, e.idempotency_key, e.fingerprint, e.reverses, e.reference, e.actor, e.memo, e.metadata, e.created_at`

func scanEntry(row pgx.Row) (storedEntry, error) {
	var e storedEntry
	var kind string
	var reverses *string
	var metadata []byte
	if err := row.Scan(&e.ID, &e.Tenant, &kind, &e.IdempotencyKey, &e.fingerprint, &reverses,
		&e.Reference, &e.Actor, &e.Memo, &metadata, &e.CreatedAt); err != nil {
		return storedEntry{}, notFound(err)
	}
	e.Kind, e.Reverses = ledger.EntryKind(kind), stringOf(reverses)
	if len(metadata) > 2 {
		if err := json.Unmarshal(metadata, &e.Metadata); err != nil {
			return storedEntry{}, err
		}
	}
	return e, nil
}

// entriesWhere loads entries and then their postings in one more query.
func entriesWhere(ctx context.Context, q queryer, where, tail string, args ...any) ([]storedEntry, error) {
	rows, err := q.Query(ctx, `SELECT `+entryColumns+` FROM motom_entries e WHERE `+where+` ORDER BY e.seq DESC `+tail, args...)
	if err != nil {
		return nil, err
	}
	var out []storedEntry
	index := map[string]int{}
	var ids []string
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		index[e.ID] = len(out)
		ids = append(ids, e.ID)
		out = append(out, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	postings, err := q.Query(ctx, `SELECT entry_id, account_id, amount FROM motom_postings WHERE entry_id = ANY($1) ORDER BY entry_id, ordinal`, ids)
	if err != nil {
		return nil, err
	}
	defer postings.Close()
	for postings.Next() {
		var entry, account string
		var amount int64
		if err := postings.Scan(&entry, &account, &amount); err != nil {
			return nil, err
		}
		i := index[entry]
		out[i].Postings = append(out[i].Postings, ledger.Posting{Account: ledger.AccountID(account), Amount: ledger.Amount(amount)})
	}
	return out, postings.Err()
}

func entryWhere(ctx context.Context, q queryer, where string, args ...any) (storedEntry, error) {
	found, err := entriesWhere(ctx, q, where, "LIMIT 1", args...)
	if err != nil {
		return storedEntry{}, err
	}
	if len(found) == 0 {
		return storedEntry{}, store.ErrNotFound
	}
	return found[0], nil
}

func (s *Store) GetEntry(ctx context.Context, tenant, id string) (ledger.Entry, error) {
	e, err := entryWhere(ctx, s.pool, "e.tenant = $1 AND e.id = $2", tenant, id)
	return e.Entry, err
}

func (s *Store) FindEntry(ctx context.Context, tenant, key string) (ledger.Entry, error) {
	e, err := entryWhere(ctx, s.pool, "e.tenant = $1 AND e.idempotency_key = $2", tenant, key)
	return e.Entry, err
}

func (s *Store) FindReversal(ctx context.Context, tenant, id string) (ledger.Entry, error) {
	e, err := entryWhere(ctx, s.pool, "e.tenant = $1 AND e.reverses = $2", tenant, id)
	return e.Entry, err
}

func (s *Store) Balance(ctx context.Context, id ledger.AccountID) (store.Balance, error) {
	var b store.Balance
	var amount int64
	err := s.pool.QueryRow(ctx, `SELECT a.version,
		COALESCE((SELECT SUM(p.amount) FROM motom_postings p WHERE p.account_id = a.id), 0)::bigint
		FROM motom_accounts a WHERE a.id = $1`, string(id)).Scan(&b.Version, &amount)
	b.Amount = ledger.Amount(amount)
	return b, notFound(err)
}

func (s *Store) CreditsSince(ctx context.Context, id ledger.AccountID, since time.Time) (ledger.Amount, error) {
	account, err := s.GetAccount(ctx, id)
	if err != nil {
		return 0, err
	}
	sign := int64(-1)
	if account.Kind.DebitNormal() {
		sign = 1
	}
	var total int64
	err = s.pool.QueryRow(ctx, `SELECT COALESCE(SUM($2::bigint * amount), 0)::bigint FROM motom_postings
		WHERE account_id = $1 AND created_at >= $3 AND $2::bigint * amount > 0`, string(id), sign, since.UTC()).Scan(&total)
	return ledger.Amount(total), err
}

func (s *Store) ListEntries(ctx context.Context, f store.EntryFilter) ([]ledger.Entry, error) {
	where := []string{"e.tenant = $1"}
	args := []any{f.Tenant}
	if f.Kind != "" {
		args = append(args, string(f.Kind))
		where = append(where, fmt.Sprintf("e.kind = $%d", len(args)))
	}
	if f.Account != "" {
		args = append(args, string(f.Account))
		where = append(where, fmt.Sprintf("EXISTS (SELECT 1 FROM motom_postings p WHERE p.entry_id = e.id AND p.account_id = $%d)", len(args)))
	}
	if f.Owner != "" {
		args = append(args, f.Owner)
		where = append(where, fmt.Sprintf(`EXISTS (SELECT 1 FROM motom_postings p JOIN motom_accounts a ON a.id = p.account_id
			WHERE p.entry_id = e.id AND a.owner = $%d)`, len(args)))
	}
	tail := ""
	if f.Limit > 0 {
		tail = fmt.Sprintf("LIMIT %d", f.Limit)
	}
	found, err := entriesWhere(ctx, s.pool, strings.Join(where, " AND "), tail, args...)
	if err != nil {
		return nil, err
	}
	out := make([]ledger.Entry, len(found))
	for i, e := range found {
		out[i] = e.Entry
	}
	return out, nil
}

func (s *Store) TrialBalance(ctx context.Context, tenant string) (map[ledger.Currency]ledger.Amount, error) {
	rows, err := s.pool.Query(ctx, `SELECT p.currency, SUM(p.amount)::bigint FROM motom_postings p
		JOIN motom_entries e ON e.id = p.entry_id WHERE e.tenant = $1 GROUP BY p.currency`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[ledger.Currency]ledger.Amount{}
	for rows.Next() {
		var currency string
		var sum int64
		if err := rows.Scan(&currency, &sum); err != nil {
			return nil, err
		}
		out[ledger.Currency(currency)] = ledger.Amount(sum)
	}
	return out, rows.Err()
}

// Wallets.

func (s *Store) GetWallet(ctx context.Context, tenant, user string) (store.Wallet, error) {
	var w store.Wallet
	err := s.pool.QueryRow(ctx, `SELECT tenant, user_id, frozen, frozen_reason, version, created_at, updated_at
		FROM motom_wallets WHERE tenant = $1 AND user_id = $2`, tenant, user).
		Scan(&w.Tenant, &w.User, &w.Frozen, &w.FrozenReason, &w.Version, &w.CreatedAt, &w.UpdatedAt)
	return w, notFound(err)
}

func (s *Store) SaveWallet(ctx context.Context, w store.Wallet) error {
	if w.Version == 0 {
		tag, err := s.pool.Exec(ctx, `INSERT INTO motom_wallets (tenant, user_id, frozen, frozen_reason, version, created_at, updated_at)
			VALUES ($1, $2, $3, $4, 1, $5, $6) ON CONFLICT DO NOTHING`, w.Tenant, w.User, w.Frozen, w.FrozenReason, w.CreatedAt.UTC(), w.UpdatedAt.UTC())
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return store.ErrVersionConflict
		}
		return nil
	}
	tag, err := s.pool.Exec(ctx, `UPDATE motom_wallets SET frozen = $3, frozen_reason = $4, updated_at = $5, version = version + 1
		WHERE tenant = $1 AND user_id = $2 AND version = $6`, w.Tenant, w.User, w.Frozen, w.FrozenReason, w.UpdatedAt.UTC(), w.Version)
	return s.versioned(ctx, tag, err, "SELECT 1 FROM motom_wallets WHERE tenant = $1 AND user_id = $2", w.Tenant, w.User)
}

// Purchases.

const purchaseColumns = `id, tenant, user_id, pack, currency, price, credits, status, mode, stripe_intent,
	stripe_checkout, entry_id, refunded, version, created_at, updated_at`

func scanPurchase(row pgx.Row) (store.Purchase, error) {
	var p store.Purchase
	var currency, status string
	var price, credits, refunded int64
	var intent *string
	if err := row.Scan(&p.ID, &p.Tenant, &p.User, &p.Pack, &currency, &price, &credits, &status, &p.Mode, &intent,
		&p.StripeCheckout, &p.EntryID, &refunded, &p.Version, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return store.Purchase{}, notFound(err)
	}
	p.Currency, p.Status, p.StripeIntent = ledger.Currency(currency), store.PurchaseStatus(status), stringOf(intent)
	p.Price, p.Credits, p.Refunded = ledger.Amount(price), ledger.Amount(credits), ledger.Amount(refunded)
	return p, nil
}

func (s *Store) CreatePurchase(ctx context.Context, p store.Purchase) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO motom_purchases (`+purchaseColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,0,$14,$15)`,
		p.ID, p.Tenant, p.User, p.Pack, string(p.Currency), int64(p.Price), int64(p.Credits), string(p.Status), p.Mode,
		nullString(p.StripeIntent), p.StripeCheckout, p.EntryID, int64(p.Refunded), p.CreatedAt.UTC(), p.UpdatedAt.UTC())
	if uniqueViolation(err, "") {
		return store.ErrAlreadyExists
	}
	return err
}

func (s *Store) GetPurchase(ctx context.Context, tenant, id string) (store.Purchase, error) {
	return scanPurchase(s.pool.QueryRow(ctx, `SELECT `+purchaseColumns+` FROM motom_purchases WHERE tenant = $1 AND id = $2`, tenant, id))
}

func (s *Store) FindPurchaseByIntent(ctx context.Context, intent string) (store.Purchase, error) {
	if intent == "" {
		return store.Purchase{}, store.ErrNotFound
	}
	return scanPurchase(s.pool.QueryRow(ctx, `SELECT `+purchaseColumns+` FROM motom_purchases WHERE stripe_intent = $1`, intent))
}

func (s *Store) SavePurchase(ctx context.Context, p store.Purchase) error {
	tag, err := s.pool.Exec(ctx, `UPDATE motom_purchases SET status = $3, stripe_intent = $4, stripe_checkout = $5,
		entry_id = $6, refunded = $7, updated_at = $8, version = version + 1
		WHERE tenant = $1 AND id = $2 AND version = $9`,
		p.Tenant, p.ID, string(p.Status), nullString(p.StripeIntent), p.StripeCheckout, p.EntryID, int64(p.Refunded),
		p.UpdatedAt.UTC(), p.Version)
	if uniqueViolation(err, "") {
		return store.ErrAlreadyExists
	}
	return s.versioned(ctx, tag, err, "SELECT 1 FROM motom_purchases WHERE tenant = $1 AND id = $2", p.Tenant, p.ID)
}

func (s *Store) CountPurchases(ctx context.Context, tenant, user string, since time.Time) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM motom_purchases WHERE tenant = $1 AND user_id = $2 AND created_at >= $3`,
		tenant, user, since.UTC()).Scan(&n)
	return n, err
}

func (s *Store) ListPurchases(ctx context.Context, tenant, user string) ([]store.Purchase, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+purchaseColumns+` FROM motom_purchases
		WHERE tenant = $1 AND ($2::text = '' OR user_id = $2) ORDER BY created_at DESC, id DESC`, tenant, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Purchase
	for rows.Next() {
		p, err := scanPurchase(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Tips.

func (s *Store) CreateTip(ctx context.Context, t store.Tip) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO motom_tips (id, tenant, room, sender, creator, currency, amount, message, entry_id, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, t.ID, t.Tenant, t.Room, t.Sender, t.Creator, string(t.Currency),
		int64(t.Amount), t.Message, t.EntryID, t.CreatedAt.UTC())
	if uniqueViolation(err, "") {
		return store.ErrAlreadyExists
	}
	return err
}

func tipWhere(q store.TipQuery) (string, []any) {
	return `tenant = $1 AND ($2::text = '' OR room = $2) AND ($3::text = '' OR sender = $3) AND ($4::text = '' OR creator = $4) AND created_at >= $5`,
		[]any{q.Tenant, q.Room, q.Sender, q.Creator, q.Since.UTC()}
}

func (s *Store) ListTips(ctx context.Context, q store.TipQuery) ([]store.Tip, error) {
	where, args := tipWhere(q)
	tail := ""
	if q.Limit > 0 {
		tail = fmt.Sprintf(" LIMIT %d", q.Limit)
	}
	rows, err := s.pool.Query(ctx, `SELECT id, tenant, room, sender, creator, currency, amount, message, entry_id, created_at
		FROM motom_tips WHERE `+where+` ORDER BY created_at DESC, id DESC`+tail, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Tip
	for rows.Next() {
		var t store.Tip
		var currency string
		var amount int64
		if err := rows.Scan(&t.ID, &t.Tenant, &t.Room, &t.Sender, &t.Creator, &currency, &amount, &t.Message, &t.EntryID, &t.CreatedAt); err != nil {
			return nil, err
		}
		t.Currency, t.Amount = ledger.Currency(currency), ledger.Amount(amount)
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) TipStats(ctx context.Context, q store.TipQuery) (int, ledger.Amount, error) {
	where, args := tipWhere(q)
	var n int
	var sum int64
	err := s.pool.QueryRow(ctx, `SELECT COUNT(*), COALESCE(SUM(amount), 0)::bigint FROM motom_tips WHERE `+where, args...).Scan(&n, &sum)
	return n, ledger.Amount(sum), err
}

func (s *Store) ScrubTips(ctx context.Context, tenant, user string) (int, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE motom_tips SET message = '' WHERE tenant = $1 AND sender = $2 AND message <> ''`, tenant, user)
	return int(tag.RowsAffected()), err
}

// Payees.

const payeeColumns = `tenant, creator, stripe_account, kyc, payouts_enabled, disabled_reason, frozen, paid_once, version, created_at, updated_at`

func scanPayee(row pgx.Row) (store.Payee, error) {
	var p store.Payee
	var account *string
	var kyc string
	if err := row.Scan(&p.Tenant, &p.Creator, &account, &kyc, &p.PayoutsEnabled, &p.DisabledReason, &p.Frozen,
		&p.PaidOnce, &p.Version, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return store.Payee{}, notFound(err)
	}
	p.StripeAccount, p.KYC = stringOf(account), store.KYC(kyc)
	return p, nil
}

func (s *Store) SavePayee(ctx context.Context, p store.Payee) error {
	var tag pgconn.CommandTag
	var err error
	if p.Version == 0 {
		tag, err = s.pool.Exec(ctx, `INSERT INTO motom_payees (`+payeeColumns+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,1,$9,$10)
			ON CONFLICT (tenant, creator) DO NOTHING`, p.Tenant, p.Creator, nullString(p.StripeAccount), string(p.KYC),
			p.PayoutsEnabled, p.DisabledReason, p.Frozen, p.PaidOnce, p.CreatedAt.UTC(), p.UpdatedAt.UTC())
		if uniqueViolation(err, "") {
			return store.ErrAlreadyExists
		}
		if err == nil && tag.RowsAffected() == 0 {
			return store.ErrVersionConflict
		}
		return err
	}
	tag, err = s.pool.Exec(ctx, `UPDATE motom_payees SET stripe_account = $3, kyc = $4, payouts_enabled = $5,
		disabled_reason = $6, frozen = $7, paid_once = $8, updated_at = $9, version = version + 1
		WHERE tenant = $1 AND creator = $2 AND version = $10`, p.Tenant, p.Creator, nullString(p.StripeAccount),
		string(p.KYC), p.PayoutsEnabled, p.DisabledReason, p.Frozen, p.PaidOnce, p.UpdatedAt.UTC(), p.Version)
	if uniqueViolation(err, "") {
		return store.ErrAlreadyExists
	}
	return s.versioned(ctx, tag, err, "SELECT 1 FROM motom_payees WHERE tenant = $1 AND creator = $2", p.Tenant, p.Creator)
}

func (s *Store) GetPayee(ctx context.Context, tenant, creator string) (store.Payee, error) {
	return scanPayee(s.pool.QueryRow(ctx, `SELECT `+payeeColumns+` FROM motom_payees WHERE tenant = $1 AND creator = $2`, tenant, creator))
}

func (s *Store) FindPayeeByStripeAccount(ctx context.Context, account string) (store.Payee, error) {
	if account == "" {
		return store.Payee{}, store.ErrNotFound
	}
	return scanPayee(s.pool.QueryRow(ctx, `SELECT `+payeeColumns+` FROM motom_payees WHERE stripe_account = $1`, account))
}

func (s *Store) ListPayees(ctx context.Context, tenant string) ([]store.Payee, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+payeeColumns+` FROM motom_payees WHERE tenant = $1 ORDER BY creator`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Payee
	for rows.Next() {
		p, err := scanPayee(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Payouts.

const payoutColumns = `id, tenant, creator, currency, amount, status, run, entry_id, settle_entry_id, stripe_transfer,
	failure, attempts, version, created_at, updated_at`

func scanPayout(row pgx.Row) (store.Payout, error) {
	var p store.Payout
	var currency, status string
	var amount int64
	var transfer *string
	if err := row.Scan(&p.ID, &p.Tenant, &p.Creator, &currency, &amount, &status, &p.Run, &p.EntryID, &p.SettleEntryID,
		&transfer, &p.Failure, &p.Attempts, &p.Version, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return store.Payout{}, notFound(err)
	}
	p.Currency, p.Amount, p.Status, p.StripeTransfer = ledger.Currency(currency), ledger.Amount(amount), store.PayoutStatus(status), stringOf(transfer)
	return p, nil
}

func (s *Store) CreatePayout(ctx context.Context, p store.Payout) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO motom_payouts (`+payoutColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,0,$13,$14)`,
		p.ID, p.Tenant, p.Creator, string(p.Currency), int64(p.Amount), string(p.Status), p.Run, p.EntryID, p.SettleEntryID,
		nullString(p.StripeTransfer), p.Failure, p.Attempts, p.CreatedAt.UTC(), p.UpdatedAt.UTC())
	if uniqueViolation(err, "") {
		return store.ErrAlreadyExists
	}
	return err
}

func (s *Store) GetPayout(ctx context.Context, tenant, id string) (store.Payout, error) {
	return scanPayout(s.pool.QueryRow(ctx, `SELECT `+payoutColumns+` FROM motom_payouts WHERE tenant = $1 AND id = $2`, tenant, id))
}

func (s *Store) FindPayoutByTransfer(ctx context.Context, transfer string) (store.Payout, error) {
	if transfer == "" {
		return store.Payout{}, store.ErrNotFound
	}
	return scanPayout(s.pool.QueryRow(ctx, `SELECT `+payoutColumns+` FROM motom_payouts WHERE stripe_transfer = $1`, transfer))
}

func (s *Store) SavePayout(ctx context.Context, p store.Payout) error {
	tag, err := s.pool.Exec(ctx, `UPDATE motom_payouts SET status = $3, settle_entry_id = $4, stripe_transfer = $5,
		failure = $6, attempts = $7, updated_at = $8, version = version + 1 WHERE tenant = $1 AND id = $2 AND version = $9`,
		p.Tenant, p.ID, string(p.Status), p.SettleEntryID, nullString(p.StripeTransfer), p.Failure, p.Attempts, p.UpdatedAt.UTC(), p.Version)
	if uniqueViolation(err, "") {
		return store.ErrAlreadyExists
	}
	return s.versioned(ctx, tag, err, "SELECT 1 FROM motom_payouts WHERE tenant = $1 AND id = $2", p.Tenant, p.ID)
}

func (s *Store) ListPayouts(ctx context.Context, q store.PayoutQuery) ([]store.Payout, error) {
	tail := ""
	if q.Limit > 0 {
		tail = fmt.Sprintf(" LIMIT %d", q.Limit)
	}
	rows, err := s.pool.Query(ctx, `SELECT `+payoutColumns+` FROM motom_payouts
		WHERE tenant = $1 AND ($2::text = '' OR creator = $2) AND ($3::text = '' OR status = $3)
		ORDER BY created_at DESC, id DESC`+tail, q.Tenant, q.Creator, string(q.Status))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Payout
	for rows.Next() {
		p, err := scanPayout(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Reviews.

const reviewColumns = `id, tenant, subject, reason, detail, reference, status, resolved_by, resolution, version, created_at, updated_at`

func scanReview(row pgx.Row) (store.Review, error) {
	var r store.Review
	var status string
	if err := row.Scan(&r.ID, &r.Tenant, &r.Subject, &r.Reason, &r.Detail, &r.Reference, &status, &r.ResolvedBy,
		&r.Resolution, &r.Version, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return store.Review{}, notFound(err)
	}
	r.Status = store.ReviewStatus(status)
	return r, nil
}

func (s *Store) CreateReview(ctx context.Context, r store.Review) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO motom_reviews (`+reviewColumns+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,0,$10,$11)`,
		r.ID, r.Tenant, r.Subject, r.Reason, r.Detail, r.Reference, string(r.Status), r.ResolvedBy, r.Resolution,
		r.CreatedAt.UTC(), r.UpdatedAt.UTC())
	if uniqueViolation(err, "") {
		return store.ErrAlreadyExists
	}
	return err
}

func (s *Store) GetReview(ctx context.Context, tenant, id string) (store.Review, error) {
	return scanReview(s.pool.QueryRow(ctx, `SELECT `+reviewColumns+` FROM motom_reviews WHERE tenant = $1 AND id = $2`, tenant, id))
}

func (s *Store) SaveReview(ctx context.Context, r store.Review) error {
	tag, err := s.pool.Exec(ctx, `UPDATE motom_reviews SET status = $3, resolved_by = $4, resolution = $5, detail = $6,
		updated_at = $7, version = version + 1 WHERE tenant = $1 AND id = $2 AND version = $8`,
		r.Tenant, r.ID, string(r.Status), r.ResolvedBy, r.Resolution, r.Detail, r.UpdatedAt.UTC(), r.Version)
	return s.versioned(ctx, tag, err, "SELECT 1 FROM motom_reviews WHERE tenant = $1 AND id = $2", r.Tenant, r.ID)
}

func (s *Store) ListReviews(ctx context.Context, tenant string, status store.ReviewStatus, subject string) ([]store.Review, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+reviewColumns+` FROM motom_reviews
		WHERE tenant = $1 AND ($2::text = '' OR status = $2) AND ($3::text = '' OR subject = $3) ORDER BY created_at, id`,
		tenant, string(status), subject)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Review
	for rows.Next() {
		r, err := scanReview(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Webhooks and audit.

func (s *Store) RecordWebhook(ctx context.Context, id, kind string, at time.Time) (bool, error) {
	if id == "" {
		return false, errors.New("a webhook event needs an id")
	}
	tag, err := s.pool.Exec(ctx, `INSERT INTO motom_webhook_events (id, kind, received_at) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
		id, kind, at.UTC())
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (s *Store) ForgetWebhook(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM motom_webhook_events WHERE id = $1`, id)
	return err
}

func (s *Store) AppendAudit(ctx context.Context, r store.AuditRecord) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO motom_audit (id, tenant, actor, action, subject, detail, at) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		r.ID, r.Tenant, r.Actor, r.Action, r.Subject, r.Detail, r.At.UTC())
	if uniqueViolation(err, "") {
		return store.ErrAlreadyExists
	}
	return err
}

func (s *Store) ListAudit(ctx context.Context, tenant, subject string, limit int) ([]store.AuditRecord, error) {
	tail := ""
	if limit > 0 {
		tail = fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := s.pool.Query(ctx, `SELECT id, tenant, actor, action, subject, detail, at FROM motom_audit
		WHERE tenant = $1 AND ($2::text = '' OR subject = $2 OR left(subject, length($2::text) + 1) = $2::text || ':')
		ORDER BY seq DESC`+tail, tenant, subject)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.AuditRecord
	for rows.Next() {
		var r store.AuditRecord
		if err := rows.Scan(&r.ID, &r.Tenant, &r.Actor, &r.Action, &r.Subject, &r.Detail, &r.At); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
