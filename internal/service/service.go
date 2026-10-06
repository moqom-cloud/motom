// Copyright 2026 Elliott Wave LLC. All rights reserved.

// Package service is MOTOM's business logic: wallets and credits, tipping,
// refunds and chargebacks, Connect payouts, fraud controls and DSAR export.
//
// Two rules hold everywhere. Money moves in the ledger before it moves at
// Stripe: a refund or transfer is posted first, and unwound by a reversal if
// Stripe definitively refuses it. And every write carries an idempotency key,
// so a retried request, a redelivered webhook or a re-run payout job moves
// money once.
package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/moqom-cloud/motom/events"
	"github.com/moqom-cloud/motom/internal/store"
	"github.com/moqom-cloud/motom/internal/stripe"
	"github.com/moqom-cloud/motom/ledger"
)

var (
	// ErrInvalid is a request that cannot be satisfied as asked.
	ErrInvalid = errors.New("invalid request")
	// ErrFrozen is a wallet or payee frozen pending review.
	ErrFrozen = errors.New("frozen pending review")
	// ErrRateLimited is a request over a per-minute or per-hour velocity limit.
	ErrRateLimited = errors.New("rate limited")
	// ErrLimitExceeded is a request over a daily cap.
	ErrLimitExceeded = errors.New("daily limit exceeded")
	// ErrPaymentsUnavailable is a deployment with no Stripe key.
	ErrPaymentsUnavailable = errors.New("payments are not configured on this deployment")
	// ErrInsufficientFunds is a wallet without the credit asked for.
	ErrInsufficientFunds = store.ErrInsufficientFunds
	// ErrConflict is a write that lost a race it could not retry.
	ErrConflict = errors.New("conflict: retry the request")
	// ErrStripeRefused is Stripe definitively refusing a refund or
	// transfer; the ledger entry made for it has been reversed.
	ErrStripeRefused = errors.New("stripe refused the request")
)

// Config is everything the service needs.
type Config struct {
	Store store.Store
	// Stripe is nil on a deployment that takes no payments.
	Stripe *stripe.Client
	// WebhookSecrets verify Stripe deliveries: the platform endpoint's and
	// the Connect endpoint's.
	WebhookSecrets []string
	// Broadcaster tells rooms about tips. Nil discards.
	Broadcaster events.Broadcaster
	// Defaults is the configuration a new tenant starts with.
	Defaults store.TenantConfig
	// MinHoldDays is the shortest payout hold a tenant may choose.
	MinHoldDays int

	Now    func() time.Time
	Random io.Reader
	Logger *slog.Logger
}

// Service is MOTOM.
type Service struct {
	store          store.Store
	stripe         *stripe.Client
	webhookSecrets []string
	broadcaster    events.Broadcaster
	defaults       store.TenantConfig
	minHoldDays    int
	clock          func() time.Time
	random         io.Reader
	log            *slog.Logger

	// senders serialises one sender's tips within a replica, so the daily
	// cap a sender's tips are checked against cannot be raced past by the
	// sender's own parallel requests. Across replicas the cap can be
	// overshot by at most one tip per replica; the wallet can never be.
	senders [256]sync.Mutex
}

// New validates the configuration.
func New(c Config) (*Service, error) {
	if c.Store == nil {
		return nil, errors.New("service: a store is required")
	}
	if c.Stripe != nil && len(c.WebhookSecrets) == 0 {
		return nil, errors.New("service: taking payments needs a webhook secret")
	}
	s := &Service{
		store: c.Store, stripe: c.Stripe, webhookSecrets: c.WebhookSecrets, broadcaster: c.Broadcaster,
		defaults: c.Defaults, minHoldDays: c.MinHoldDays, clock: c.Now, random: c.Random, log: c.Logger,
	}
	if s.broadcaster == nil {
		s.broadcaster = events.Discard
	}
	if s.defaults.HoldDays == 0 && s.defaults.TipMax == 0 {
		s.defaults = DefaultConfig()
	}
	if s.clock == nil {
		s.clock = time.Now
	}
	if s.random == nil {
		s.random = rand.Reader
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if err := validateConfig(s.defaults, nil, 0, s.minHoldDays); err != nil {
		return nil, fmt.Errorf("service: default tenant config: %w", err)
	}
	return s, nil
}

// Store exposes the store, for the operator CLI.
func (s *Service) Store() store.Store { return s.store }

// now is the clock in UTC at the precision Postgres keeps, so a time read
// back compares equal to the one written.
func (s *Service) now() time.Time { return s.clock().UTC().Truncate(time.Microsecond) }

func (s *Service) newID(prefix string) string {
	var b [12]byte
	if _, err := io.ReadFull(s.random, b[:]); err != nil {
		panic(fmt.Sprintf("service: reading randomness: %v", err))
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}

// derivedID names a record after the request that created it, so a replay
// with the same idempotency key finds the same record.
func derivedID(prefix string, parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return prefix + "_" + hex.EncodeToString(h.Sum(nil)[:12])
}

func (s *Service) senderLock(tenant, sender string) *sync.Mutex {
	h := fnv.New32a()
	h.Write([]byte(tenant + "\x00" + sender))
	return &s.senders[h.Sum32()%uint32(len(s.senders))]
}

// audit records a non-ledger action. Ledger postings write their own audit
// record inside the posting transaction.
func (s *Service) audit(ctx context.Context, tenant, actor, action, subject, detail string) {
	record := store.AuditRecord{ID: s.newID("au"), Tenant: tenant, Actor: actor, Action: action,
		Subject: subject, Detail: detail, At: s.now()}
	if err := s.store.AppendAudit(ctx, record); err != nil {
		s.log.ErrorContext(ctx, "audit write failed", "event", "motom_audit_failed", "action", action, "error", err)
	}
	s.log.InfoContext(ctx, "audit", "event", "motom_audit", "tenant", tenant, "actor", actor, "action", action, "subject", subject)
}

// account ensures an account exists and returns its ID.
func (s *Service) account(ctx context.Context, tenant string, kind ledger.AccountKind, owner string, currency ledger.Currency) (ledger.AccountID, error) {
	id := ledger.AccountFor(tenant, kind, owner, currency)
	if _, err := s.store.EnsureAccount(ctx, ledger.Account{ID: id, CreatedAt: s.now()}); err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return id, nil
}

func (s *Service) accounts(ctx context.Context, tenant string, currency ledger.Currency, specs ...accountSpec) ([]ledger.AccountID, error) {
	out := make([]ledger.AccountID, len(specs))
	for i, spec := range specs {
		id, err := s.account(ctx, tenant, spec.kind, spec.owner, currency)
		if err != nil {
			return nil, err
		}
		out[i] = id
	}
	return out, nil
}

type accountSpec struct {
	kind  ledger.AccountKind
	owner string
}

// post writes an entry and logs the movement.
func (s *Service) post(ctx context.Context, e ledger.Entry) (ledger.Entry, bool, error) {
	if e.ID == "" {
		e.ID = s.newID("je")
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = s.now()
	}
	stored, created, err := s.store.Post(ctx, e)
	if err != nil {
		return ledger.Entry{}, false, err
	}
	if created {
		s.log.InfoContext(ctx, "ledger entry posted", "event", "motom_ledger_posted", "tenant", stored.Tenant,
			"entry", stored.ID, "kind", stored.Kind, "reference", stored.Reference, "actor", stored.Actor)
	}
	return stored, created, nil
}

// postings builds an entry's lines, merging lines to one account and
// dropping lines that net to zero.
type postings struct {
	order []ledger.AccountID
	sums  map[ledger.AccountID]ledger.Amount
}

func (p *postings) add(account ledger.AccountID, amount ledger.Amount) *postings {
	if p.sums == nil {
		p.sums = map[ledger.AccountID]ledger.Amount{}
	}
	if _, ok := p.sums[account]; !ok {
		p.order = append(p.order, account)
	}
	p.sums[account] += amount
	return p
}

func (p *postings) lines() []ledger.Posting {
	var out []ledger.Posting
	for _, account := range p.order {
		if amount := p.sums[account]; amount != 0 {
			out = append(out, ledger.Posting{Account: account, Amount: amount})
		}
	}
	return out
}

// normalBalance reads an account's natural balance and version.
func (s *Service) normalBalance(ctx context.Context, id ledger.AccountID) (ledger.Amount, int64, error) {
	account, err := s.store.GetAccount(ctx, id)
	if err != nil {
		return 0, 0, err
	}
	balance, err := s.store.Balance(ctx, id)
	if err != nil {
		return 0, 0, err
	}
	return account.Normal(balance.Amount), balance.Version, nil
}

func validID(field, value string) error {
	if !ledger.ValidSegment(value) {
		return fmt.Errorf("%w: %s must be 1-%d printable characters without '/' or spaces", ErrInvalid, field, ledger.MaxSegment)
	}
	return nil
}

func validKey(key string) error {
	if key == "" || len(key) > 200 {
		return fmt.Errorf("%w: an Idempotency-Key of 1-200 bytes is required", ErrInvalid)
	}
	return nil
}
