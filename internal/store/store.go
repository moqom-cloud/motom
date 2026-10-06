// Copyright 2026 Elliott Wave LLC. All rights reserved.

// Package store is everything MOTOM keeps, as an interface with two
// implementations: memory (tests, development) and postgres (production). The
// storetest package is the contract both keep.
//
// The store owns the invariants that need a transaction: an idempotency key
// posts once, an account's version advances on every posting, a wallet or
// earnings account never goes below zero, an entry is reversed at most once,
// and every posting writes its audit record in the same transaction. Journal
// entries, postings and audit records are never updated or deleted.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/moqom-cloud/motom/ledger"
)

var (
	ErrNotFound      = errors.New("not found")
	ErrAlreadyExists = errors.New("already exists")
	// ErrVersionConflict is a write that lost an optimistic-concurrency race;
	// re-read and retry.
	ErrVersionConflict = errors.New("version conflict")
	// ErrInsufficientFunds is a posting that would take a wallet, earnings or
	// pending payout account below zero.
	ErrInsufficientFunds = errors.New("insufficient funds")
	// ErrIdempotencyMismatch is an idempotency key reused for a different
	// movement.
	ErrIdempotencyMismatch = errors.New("idempotency key reused with a different request")
	// ErrAlreadyReversed is a second reversal of one entry.
	ErrAlreadyReversed = errors.New("entry already reversed")
)

// Store is MOTOM's persistence.
type Store interface {
	CreateTenant(ctx context.Context, t Tenant) error
	GetTenant(ctx context.Context, id string) (Tenant, error)
	// SaveTenant writes t if the stored version equals t.Version, and
	// advances it.
	SaveTenant(ctx context.Context, t Tenant) error
	ListTenants(ctx context.Context) ([]Tenant, error)

	PutKey(ctx context.Context, k Key) error
	GetKey(ctx context.Context, id string) (Key, error)
	ListKeys(ctx context.Context, tenant string) ([]Key, error)
	RevokeKey(ctx context.Context, tenant, id string, at time.Time) error

	// EnsureAccount creates the account if it does not exist and returns it.
	EnsureAccount(ctx context.Context, a ledger.Account) (ledger.Account, error)
	GetAccount(ctx context.Context, id ledger.AccountID) (ledger.Account, error)
	ListAccounts(ctx context.Context, tenant string, owner string) ([]ledger.Account, error)
	// Post writes a balanced entry. It returns the stored entry and true, or,
	// when the idempotency key has been used for the same movement, that
	// entry and false.
	Post(ctx context.Context, e ledger.Entry) (ledger.Entry, bool, error)
	GetEntry(ctx context.Context, tenant, id string) (ledger.Entry, error)
	FindEntry(ctx context.Context, tenant, idempotencyKey string) (ledger.Entry, error)
	// FindReversal returns the entry that reverses id.
	FindReversal(ctx context.Context, tenant, id string) (ledger.Entry, error)
	// Balance is the signed sum of an account's postings and its version,
	// read together.
	Balance(ctx context.Context, id ledger.AccountID) (Balance, error)
	// CreditsSince is the total credited to an account at or after since, as
	// a positive amount. Payout holds subtract it from the balance.
	CreditsSince(ctx context.Context, id ledger.AccountID, since time.Time) (ledger.Amount, error)
	// ListEntries returns entries newest first.
	ListEntries(ctx context.Context, filter EntryFilter) ([]ledger.Entry, error)
	// TrialBalance sums every posting of a tenant by currency. Every value is
	// zero in a ledger that has only ever accepted balanced entries.
	TrialBalance(ctx context.Context, tenant string) (map[ledger.Currency]ledger.Amount, error)

	GetWallet(ctx context.Context, tenant, user string) (Wallet, error)
	// SaveWallet creates the wallet at version 0, or updates it if the stored
	// version equals w.Version.
	SaveWallet(ctx context.Context, w Wallet) error

	CreatePurchase(ctx context.Context, p Purchase) error
	GetPurchase(ctx context.Context, tenant, id string) (Purchase, error)
	FindPurchaseByIntent(ctx context.Context, intent string) (Purchase, error)
	SavePurchase(ctx context.Context, p Purchase) error
	// CountPurchases counts a user's purchases created at or after since.
	CountPurchases(ctx context.Context, tenant, user string, since time.Time) (int, error)
	ListPurchases(ctx context.Context, tenant, user string) ([]Purchase, error)

	CreateTip(ctx context.Context, t Tip) error
	ListTips(ctx context.Context, q TipQuery) ([]Tip, error)
	// TipStats counts and sums the tips matching q.
	TipStats(ctx context.Context, q TipQuery) (count int, sum ledger.Amount, err error)
	// ScrubTips blanks the free text of every tip a user sent. The amounts
	// and the entries behind them are financial records and stay.
	ScrubTips(ctx context.Context, tenant, user string) (int, error)

	SavePayee(ctx context.Context, p Payee) error
	GetPayee(ctx context.Context, tenant, creator string) (Payee, error)
	FindPayeeByStripeAccount(ctx context.Context, account string) (Payee, error)
	ListPayees(ctx context.Context, tenant string) ([]Payee, error)

	CreatePayout(ctx context.Context, p Payout) error
	GetPayout(ctx context.Context, tenant, id string) (Payout, error)
	FindPayoutByTransfer(ctx context.Context, transfer string) (Payout, error)
	SavePayout(ctx context.Context, p Payout) error
	ListPayouts(ctx context.Context, q PayoutQuery) ([]Payout, error)

	CreateReview(ctx context.Context, r Review) error
	GetReview(ctx context.Context, tenant, id string) (Review, error)
	SaveReview(ctx context.Context, r Review) error
	ListReviews(ctx context.Context, tenant string, status ReviewStatus, subject string) ([]Review, error)

	// RecordWebhook notes a webhook event ID and reports whether it is new.
	RecordWebhook(ctx context.Context, id, kind string, at time.Time) (bool, error)
	// ForgetWebhook removes a recorded event whose handling failed, so
	// Stripe's retry is processed rather than skipped.
	ForgetWebhook(ctx context.Context, id string) error

	AppendAudit(ctx context.Context, r AuditRecord) error
	ListAudit(ctx context.Context, tenant, subject string, limit int) ([]AuditRecord, error)
}

// Balance is an account's signed balance at a version.
type Balance struct {
	Amount  ledger.Amount
	Version int64
}

// EntryFilter selects journal entries. Empty fields match everything.
type EntryFilter struct {
	Tenant  string
	Account ledger.AccountID
	// Owner matches entries touching any account the owner holds.
	Owner string
	Kind  ledger.EntryKind
	Limit int
}

// Tenant is a MOQOM tenant using MOTOM, with its commercial terms.
type Tenant struct {
	ID string
	// Currencies the tenant transacts in; the first is the default.
	Currencies []ledger.Currency
	// PlatformFee is MOTOM's cut of each tip, set by the operator and not
	// by the tenant.
	PlatformFee ledger.BasisPoints
	Config      TenantConfig
	Version     int64
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Liability is who absorbs a chargeback the user's wallet cannot cover.
type Liability string

const (
	LiabilityPlatform Liability = "platform"
	LiabilityTenant   Liability = "tenant"
)

// TenantConfig is what a tenant sets for its own app.
type TenantConfig struct {
	TenantFee ledger.BasisPoints `json:"tenant_fee_bps"`
	Packs     []Pack             `json:"packs"`

	TipMin ledger.Amount `json:"tip_min"`
	TipMax ledger.Amount `json:"tip_max"`
	// SenderDailyCap and RoomDailyCap bound tips over a rolling 24 hours.
	SenderDailyCap   ledger.Amount `json:"sender_daily_cap"`
	RoomDailyCap     ledger.Amount `json:"room_daily_cap"`
	SenderTipsPerMin int           `json:"sender_tips_per_minute"`
	RoomTipsPerMin   int           `json:"room_tips_per_minute"`

	PurchasesPerHour int `json:"purchases_per_hour"`
	// ReviewTipAt queues a review of the creator for any tip at or above it.
	ReviewTipAt ledger.Amount `json:"review_tip_at"`
	// RepeatPairPerHour flags a sender tipping one creator more often than
	// this in an hour.
	RepeatPairPerHour int `json:"repeat_pair_per_hour"`

	HoldDays  int           `json:"hold_days"`
	MinPayout ledger.Amount `json:"min_payout"`
	// ReviewFirstPayoutAt queues a creator's first payout for review when
	// it is at or above this.
	ReviewFirstPayoutAt ledger.Amount `json:"review_first_payout_at"`

	ChargebackLiability Liability `json:"chargeback_liability"`
}

// Pack is a top-up a tenant sells: Price charged, Price+Bonus credited. The
// bonus is funded by the tenant.
type Pack struct {
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Currency ledger.Currency `json:"currency"`
	Price    ledger.Amount   `json:"price"`
	Bonus    ledger.Amount   `json:"bonus"`
}

// Scope is something a key may do.
type Scope string

const (
	ScopeAdmin          Scope = "admin"
	ScopeWalletsRead    Scope = "wallets:read"
	ScopeWalletsWrite   Scope = "wallets:write"
	ScopePurchasesWrite Scope = "purchases:write"
	ScopeRefundsWrite   Scope = "refunds:write"
	ScopeTipsRead       Scope = "tips:read"
	ScopeTipsWrite      Scope = "tips:write"
	ScopePayoutsRead    Scope = "payouts:read"
	ScopePayoutsWrite   Scope = "payouts:write"
	ScopeReviewsRead    Scope = "reviews:read"
	ScopeReviewsWrite   Scope = "reviews:write"
	ScopeDSAR           Scope = "dsar"
)

// Scopes lists every scope.
var Scopes = []Scope{ScopeAdmin, ScopeWalletsRead, ScopeWalletsWrite, ScopePurchasesWrite, ScopeRefundsWrite,
	ScopeTipsRead, ScopeTipsWrite, ScopePayoutsRead, ScopePayoutsWrite, ScopeReviewsRead, ScopeReviewsWrite, ScopeDSAR}

// Key is an API key minus its secret, which is never stored.
type Key struct {
	ID        string
	Tenant    string
	Name      string
	Scopes    []Scope
	Hash      string
	CreatedAt time.Time
	RevokedAt time.Time
}

// Revoked reports whether the key has been revoked.
func (k Key) Revoked() bool { return !k.RevokedAt.IsZero() }

// Allows reports whether the key holds scope, or admin.
func (k Key) Allows(scope Scope) bool {
	for _, s := range k.Scopes {
		if s == scope || s == ScopeAdmin {
			return true
		}
	}
	return false
}

// Wallet is the non-ledger state of an end user's wallet. Its money is in
// the ledger.
type Wallet struct {
	Tenant       string
	User         string
	Frozen       bool
	FrozenReason string
	Version      int64
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// PurchaseStatus is where a credit purchase is.
type PurchaseStatus string

const (
	PurchasePending   PurchaseStatus = "pending"
	PurchaseSucceeded PurchaseStatus = "succeeded"
	PurchaseFailed    PurchaseStatus = "failed"
	PurchaseRefunded  PurchaseStatus = "refunded"
	PurchaseDisputed  PurchaseStatus = "disputed"
	// PurchaseFlagged is a payment whose amount or currency did not match
	// what was sold; it is credited by nobody until reviewed.
	PurchaseFlagged PurchaseStatus = "flagged"
)

// Purchase is a credit top-up.
type Purchase struct {
	ID       string
	Tenant   string
	User     string
	Pack     string
	Currency ledger.Currency
	Price    ledger.Amount
	Credits  ledger.Amount
	Status   PurchaseStatus
	// Mode is "checkout" or "payment_intent".
	Mode           string
	StripeIntent   string
	StripeCheckout string
	EntryID        string
	Refunded       ledger.Amount
	Version        int64
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Tip is a viewer tipping a creator in a room. The money is the entry.
type Tip struct {
	ID       string
	Tenant   string
	Room     string
	Sender   string
	Creator  string
	Currency ledger.Currency
	Amount   ledger.Amount
	// Message is free text shown in the room. Personal data: erasable.
	Message   string
	EntryID   string
	CreatedAt time.Time
}

// TipQuery selects tips. Empty fields match everything.
type TipQuery struct {
	Tenant  string
	Room    string
	Sender  string
	Creator string
	Since   time.Time
	Limit   int
}

// KYC is a creator's verification state as Stripe reports it.
type KYC string

const (
	KYCNone       KYC = "none"
	KYCPending    KYC = "pending"
	KYCVerified   KYC = "verified"
	KYCRestricted KYC = "restricted"
)

// Payee is a creator who can be paid out through Stripe Connect.
type Payee struct {
	Tenant        string
	Creator       string
	StripeAccount string
	KYC           KYC
	// PayoutsEnabled is Stripe's payouts_enabled for the connected account.
	PayoutsEnabled bool
	DisabledReason string
	Frozen         bool
	PaidOnce       bool
	Version        int64
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Payable reports whether money may be sent to the payee.
func (p Payee) Payable() bool {
	return p.StripeAccount != "" && p.KYC == KYCVerified && p.PayoutsEnabled && !p.Frozen
}

// PayoutStatus is where a payout is.
type PayoutStatus string

const (
	// PayoutPending has its ledger entry and has not been confirmed by
	// Stripe. A run retries it with the same idempotency key.
	PayoutPending PayoutStatus = "pending"
	PayoutPaid    PayoutStatus = "paid"
	PayoutFailed  PayoutStatus = "failed"
	// PayoutReturned was transferred and then reversed by Stripe.
	PayoutReturned PayoutStatus = "returned"
)

// Payout moves a creator's earnings to their connected account.
type Payout struct {
	ID       string
	Tenant   string
	Creator  string
	Currency ledger.Currency
	Amount   ledger.Amount
	Status   PayoutStatus
	// Run is the scheduled run this belongs to, e.g. "2026-10-06".
	Run            string
	EntryID        string
	SettleEntryID  string
	StripeTransfer string
	Failure        string
	Attempts       int
	Version        int64
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// PayoutQuery selects payouts. Empty fields match everything.
type PayoutQuery struct {
	Tenant  string
	Creator string
	Status  PayoutStatus
	Limit   int
}

// ReviewStatus is where a review is.
type ReviewStatus string

const (
	ReviewOpen     ReviewStatus = "open"
	ReviewApproved ReviewStatus = "approved"
	ReviewRejected ReviewStatus = "rejected"
)

// Review is an item in the manual review queue. Subject names what is under
// review: "wallet:<user>", "payee:<creator>", "purchase:<id>".
type Review struct {
	ID         string
	Tenant     string
	Subject    string
	Reason     string
	Detail     string
	Reference  string
	Status     ReviewStatus
	ResolvedBy string
	Resolution string
	Version    int64
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// AuditRecord is one line of the audit log: who did what to which subject.
// Append-only; financial retention (seven years) applies.
type AuditRecord struct {
	ID      string
	Tenant  string
	Actor   string
	Action  string
	Subject string
	Detail  string
	At      time.Time
}
